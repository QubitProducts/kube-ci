// Copyright 2019 Qubit Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	clientset "github.com/argoproj/argo/pkg/client/clientset/versioned"
	informers "github.com/argoproj/argo/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo/pkg/client/listers/workflow/v1alpha1"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v22/github"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

var ()

type githubKeyStore struct {
	baseTransport http.RoundTripper
	appID         int
	ids           []int
	key           []byte
}

func (ks *githubKeyStore) getClient(installID int) (*github.Client, error) {
	validID := false
	for _, id := range ks.ids {
		if installID == id {
			validID = true
			break
		}
	}

	if len(ks.ids) > 0 && !validID {
		return nil, fmt.Errorf("unknown installation %d", installID)
	}

	itr, err := ghinstallation.New(ks.baseTransport, ks.appID, installID, ks.key)
	if err != nil {
		return nil, err
	}

	return github.NewClient(&http.Client{Transport: itr}), nil
}

type githubClientSource interface {
	getClient(installID int) (*github.Client, error)
}

// Config defines our configuration file format
type Config struct {
	CIFilePath   string            `yaml:"ciFilePath"`
	Namespace    string            `yaml:"namespace"`
	Tolerations  []v1.Toleration   `yaml:"tolerations"`
	NodeSelector map[string]string `yaml:"nodeSelector"`
}

type workflowSyncer struct {
	ghSecret []byte

	ghClientSrc githubClientSource

	config     Config
	kubeclient kubernetes.Interface
	client     clientset.Interface
	lister     listers.WorkflowLister
	synced     cache.InformerSynced
	workqueue  workqueue.RateLimitingInterface
	recorder   record.EventRecorder

	argoUIBase string
}

var sanitize = regexp.MustCompile(`[^-A-Za-z0-9]`)

func wfName(prefix, owner, repo, sha string) string {
	return fmt.Sprintf("%s.%s.%s.%s.%d",
		prefix,
		sanitize.ReplaceAllString(owner, "-"),
		sanitize.ReplaceAllString(repo, "-"),
		sanitize.ReplaceAllString(sha, "-"),
		time.Now().Unix(),
	)
}

// updateWorkflow, lots of these settings shoud come in from some config.
func (ws *workflowSyncer) updateWorkflow(wf *workflow.Workflow, event *github.CheckSuiteEvent, cr *github.CheckRun) {
	wfType := "ci"
	wf.GenerateName = ""
	wf.Name = wfName(wfType, *event.Repo.Owner.Login, *event.Repo.Name, *event.CheckSuite.HeadBranch)

	if ws.config.Namespace != "" {
		wf.Namespace = ws.config.Namespace
	}

	ttl := int32((3 * 24 * time.Hour) / time.Second)
	wf.Spec.TTLSecondsAfterFinished = &ttl

	wf.Spec.Tolerations = append(
		wf.Spec.Tolerations,
		ws.config.Tolerations...,
	)

	if wf.Spec.NodeSelector == nil {
		wf.Spec.NodeSelector = map[string]string{}
	}
	for k, v := range ws.config.NodeSelector {
		wf.Spec.NodeSelector[k] = v
	}

	var parms []workflow.Parameter
	for _, p := range wf.Spec.Arguments.Parameters {
		if p.Name == "repo" ||
			p.Name == "revision" ||
			p.Name == "orgName" ||
			p.Name == "repoName" {
			continue
		}
		parms = append(parms, p)
	}

	gitURL := event.Repo.GetSSHURL()
	parms = append(parms, []workflow.Parameter{
		{
			Name:  "repo",
			Value: &gitURL,
		},
		{
			Name:  "repoName",
			Value: event.Repo.Name,
		},
		{
			Name:  "orgName",
			Value: event.Repo.Owner.Login,
		},
		{
			Name:  "revision",
			Value: event.CheckSuite.HeadSHA,
		},
	}...)

	wf.Spec.Arguments.Parameters = parms

	if wf.Labels == nil {
		wf.Labels = make(map[string]string)
	}
	wf.Labels["managedBy"] = "kube-ci"
	wf.Labels["wfType"] = wfType

	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}

	wf.Annotations["kube-ci.qutics.com/sha"] = *event.CheckSuite.HeadSHA
	wf.Annotations["kube-ci.qutics.com/branch"] = *event.CheckSuite.HeadBranch
	wf.Annotations["kube-ci.qutics.com/repo"] = *event.Repo.Name
	wf.Annotations["kube-ci.qutics.com/org"] = *event.Repo.Owner.Login

	wf.Annotations["kube-ci.qutics.com/github-install-id"] = strconv.Itoa(int(*event.Installation.ID))

	wf.Annotations["kube-ci.qutics.com/check-run-name"] = *cr.Name
	wf.Annotations["kube-ci.qutics.com/check-run-id"] = strconv.Itoa(int(*cr.ID))
}

func (ws *workflowSyncer) enqueue(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	ws.workqueue.AddRateLimited(key)
}

func newWorkflowSyncer(
	kubeclient kubernetes.Interface,
	clientset clientset.Interface,
	sinf informers.SharedInformerFactory,
	ghClientSrc githubClientSource,
	ghSecret []byte,
	baseURL string,
	config Config,
) *workflowSyncer {

	informer := sinf.Argoproj().V1alpha1().Workflows()

	syncer := &workflowSyncer{
		ghClientSrc: ghClientSrc,
		ghSecret:    ghSecret,

		kubeclient: kubeclient,
		client:     clientset,
		lister:     informer.Lister(),
		synced:     informer.Informer().HasSynced,
		workqueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "workflows"),
		argoUIBase: baseURL,
		config:     config,
	}

	log.Print("Setting up event handlers")
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: syncer.enqueue,
		UpdateFunc: func(old, new interface{}) {
			syncer.enqueue(new)
		},
	})

	return syncer
}

func (ws *workflowSyncer) runWorker() {
	for ws.process() {
	}

	log.Print("worker stopped")
}

func (ws *workflowSyncer) process() bool {
	obj, shutdown := ws.workqueue.Get()

	if shutdown {
		return false
	}

	defer ws.workqueue.Done(obj)

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("couldn't split workflow cache key %q, %v", key, err))
		return true
	}

	wf, err := ws.lister.Workflows(namespace).Get(name)
	if err != nil {
		ws.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("couldn't get workflow %q, %v ", key, err))
		return true
	}

	err = ws.sync(wf)
	if err != nil {
		runtime.HandleError(err)
		return true
	}

	ws.workqueue.Forget(obj)

	return true
}

type crInfo struct {
	orgName  string
	repoName string
	instID   int

	headSHA    string
	headBranch string

	checkRunName string
	checkRunID   int64
}

func crInfoFromWorkflow(wf *workflow.Workflow) (*crInfo, error) {
	instIDStr, ok := wf.Annotations["kube-ci.qutics.com/github-install-id"]
	if !ok {
		return nil, fmt.Errorf("could not get github installation id for  %s/%s", wf.Namespace, wf.Name)
	}

	instID, err := strconv.Atoi(instIDStr)
	if err != nil {
		return nil, fmt.Errorf("could not convert installation id for %s/%s to int", wf.Namespace, wf.Name)
	}

	headSHA, ok := wf.Annotations["kube-ci.qutics.com/sha"]
	if !ok {
		return nil, fmt.Errorf("could not get commit sha for %s/%s", wf.Namespace, wf.Name)
	}
	headBranch, ok := wf.Annotations["kube-ci.qutics.com/branch"]
	if !ok {
		return nil, fmt.Errorf("could not get commit branch for %s/%s", wf.Namespace, wf.Name)
	}
	orgName, ok := wf.Annotations["kube-ci.qutics.com/org"]
	if !ok {
		return nil, fmt.Errorf("could not get github org for %s/%s", wf.Namespace, wf.Name)
	}
	repoName, ok := wf.Annotations["kube-ci.qutics.com/repo"]
	if !ok {
		return nil, fmt.Errorf("could not get github repo name for %s/%s", wf.Namespace, wf.Name)
	}
	checkRunName, ok := wf.Annotations["kube-ci.qutics.com/check-run-name"]
	if !ok {
		return nil, fmt.Errorf("could not get check run name for %s/%s", wf.Namespace, wf.Name)
	}

	checkRunIDStr, ok := wf.Annotations["kube-ci.qutics.com/check-run-id"]
	if !ok {
		return nil, fmt.Errorf("could not get check run id for  %s/%s", wf.Namespace, wf.Name)
	}
	checkRunID, err := strconv.Atoi(checkRunIDStr)
	if err != nil {
		return nil, fmt.Errorf("could not convert check  run id for %s/%s to int", wf.Namespace, wf.Name)
	}

	return &crInfo{
		headSHA:      headSHA,
		instID:       instID,
		headBranch:   headBranch,
		orgName:      orgName,
		repoName:     repoName,
		checkRunName: checkRunName,
		checkRunID:   int64(checkRunID),
	}, nil
}

func (ws *workflowSyncer) sync(wf *workflow.Workflow) error {
	if _, ok := wf.Annotations["kube-ci.qutics.com/annotations-published"]; ok {
		log.Printf("ignoring %s/%s, already completed", wf.Namespace, wf.Name)
		return nil
	}

	cr, err := crInfoFromWorkflow(wf)
	if err != nil {
		log.Printf("ignoring %s/%s, %v", wf.Namespace, wf.Name, err)
		return nil
	}

	ghClient, err := ws.ghClientSrc.getClient(int(cr.instID))
	if err != nil {
		return err
	}

	status := ""

	failure := "failure"
	success := "success"
	neutral := "neutral"
	var conclusion *string

	var completedAt *github.Timestamp
	now := &github.Timestamp{
		Time: time.Now(),
	}

	switch wf.Status.Phase {
	case workflow.NodePending:
		status = "queued"
	case workflow.NodeRunning:
		status = "in_progress"
	case workflow.NodeFailed:
		status = "completed"
		conclusion = &failure
		completedAt = now
	case workflow.NodeError:
		status = "completed"
		conclusion = &failure
		completedAt = now
	case workflow.NodeSucceeded:
		status = "completed"
		conclusion = &success
		completedAt = now
	case workflow.NodeSkipped:
		status = "completed"
		conclusion = &neutral
		completedAt = now
	default:
		log.Printf("ignoring %s/%s, unknown node phase %q", wf.Namespace, wf.Name, wf.Status.Phase)
		return nil
	}

	summary := wf.Status.Message

	title := fmt.Sprintf("Workflow Run (%s/%s))", wf.Namespace, wf.Name)
	text := ""
	var names []string
	namesToNodes := make(map[string]string)
	for k, v := range wf.Status.Nodes {
		if v.Type != "Pod" {
			continue
		}
		names = append(names, v.Name)
		namesToNodes[v.Name] = k
	}
	sort.Strings(names)
	for _, k := range names {
		n := namesToNodes[k]
		node := wf.Status.Nodes[n]
		text += fmt.Sprintf("%s(%s): %s \n", k, node.Phase, node.Message)
	}

	wfURL := fmt.Sprintf(
		"%s/workflows/%s/%s",
		ws.argoUIBase,
		wf.Namespace,
		wf.Name)

	_, _, err = ghClient.Checks.UpdateCheckRun(
		context.Background(),
		cr.orgName,
		cr.repoName,
		cr.checkRunID,
		github.UpdateCheckRunOptions{
			Name:        cr.checkRunName,
			HeadBranch:  &cr.headBranch,
			HeadSHA:     &cr.headSHA,
			DetailsURL:  &wfURL,
			Status:      &status,
			Conclusion:  conclusion,
			CompletedAt: completedAt,

			Output: &github.CheckRunOutput{
				Title:   &title,
				Summary: &summary,
				Text:    &text,
			},
		},
	)

	if err != nil {
		log.Printf("Unable to update check run, %v", err)
	}

	if status == "completed" {
		go ws.completeCheckRun(&title, &summary, &text, wf, cr)
	}

	return nil
}

// completeCheckRun is used to publish any annotations found in the logs from a check run.
// There are a bunch of reasons this could fail.
func (ws *workflowSyncer) completeCheckRun(title, summary, text *string, wf *workflow.Workflow, cri *crInfo) {
	if wf.Status.Phase != workflow.NodeFailed &&
		wf.Status.Phase != workflow.NodeSucceeded {
		return
	}

	var allAnns []*github.CheckRunAnnotation
	for _, n := range wf.Status.Nodes {
		if n.Type != "Pod" {
			continue
		}
		logr, err := getPodLogs(ws.kubeclient, n.ID, wf.Namespace, "main")
		if err != nil {
			log.Printf("getting pod logs failed, %v", err)
			continue
		}
		anns, err := parseAnnotations(logr, "")
		if err != nil {
			log.Printf("parsing annotations failed, %v", err)
			return
		}
		allAnns = append(allAnns, anns...)
	}

	ghClient, err := ws.ghClientSrc.getClient(int(cri.instID))
	if err != nil {
		return
	}

	var actions []*github.CheckRunAction
	if wf.Status.Phase == workflow.NodeSucceeded {
		actions = []*github.CheckRunAction{
			{
				Label:       "Deploy Me",
				Description: "Do the thing ",
				Identifier:  "deploy",
			},
		}
	}

	_, _, err = ghClient.Checks.UpdateCheckRun(
		context.Background(),
		cri.orgName,
		cri.repoName,
		cri.checkRunID,
		github.UpdateCheckRunOptions{
			Name:       cri.checkRunName,
			HeadBranch: &cri.headBranch,
			HeadSHA:    &cri.headSHA,
			Actions:    actions,
		},
	)

	batchSize := 50 // github API allows 50 at a time
	for i := 0; i < len(allAnns); i += batchSize {
		start := i
		end := i + batchSize
		if end > len(allAnns) {
			end = len(allAnns)
		}
		anns := allAnns[start:end]
		_, _, err = ghClient.Checks.UpdateCheckRun(
			context.Background(),
			cri.orgName,
			cri.repoName,
			cri.checkRunID,
			github.UpdateCheckRunOptions{
				Name:       cri.checkRunName,
				HeadBranch: &cri.headBranch,
				HeadSHA:    &cri.headSHA,

				Output: &github.CheckRunOutput{
					Title:       title,
					Summary:     summary,
					Text:        text,
					Annotations: anns,
				},
				Actions: actions,
			},
		)
		if err != nil {
			log.Printf("upload annotations for %s/%s failed, %v", wf.Namespace, wf.Name, err)

		}
	}

	// We need to update the API object so that we know we've published the
	// logs, we'll grab the latest one incase it has changed since we got here.
	newwf, err := ws.client.Argoproj().Workflows("argo").Get(wf.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("getting workflow %s/%s for annotations update failed, %v", newwf.Namespace, newwf.Name, err)
		return
	}

	upwf := newwf.DeepCopy()
	if upwf.Annotations == nil {
		upwf.Annotations = map[string]string{}
	}
	upwf.Annotations["kube-ci.qutics.com/annotations-published"] = "true"

	_, err = ws.client.Argoproj().Workflows("argo").Update(upwf)
	if err != nil {
		log.Printf("workflow %s/%s update for annotations update failed, %v", upwf.Namespace, upwf.Name, err)
	}

	return
}

func (ws *workflowSyncer) Run(stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer ws.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, ws.synced); !ok {
		log.Printf("failed waiting for cache sync")
		return fmt.Errorf("caches did not sync")
	}

	go wait.Until(ws.runWorker, time.Second, stopCh)

	log.Print("Started workers")
	<-stopCh
	log.Print("Shutting down workers")

	return nil
}

func (ws *workflowSyncer) getWorkflow(
	ctx context.Context,
	ghClient *github.Client,
	owner string,
	name string,
	sha string,
	filename string) (*workflow.Workflow, error) {

	file, err := ghClient.Repositories.DownloadContents(
		ctx,
		owner,
		name,
		filename,
		&github.RepositoryContentGetOptions{
			Ref: sha,
		})
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode == http.StatusNotFound {
				log.Printf("no %s in %s/%s (%s)",
					filename,
					owner,
					name,
					sha,
				)
				return nil, os.ErrNotExist
			}
		}
		return nil, err
	}
	defer file.Close()

	bs := &bytes.Buffer{}

	_, err = io.Copy(bs, file)
	if err != nil {
		return nil, err
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs.Bytes(), nil, nil)

	if err != nil {
		return nil, fmt.Errorf("failed to decode %s, %v", filename, err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}

	return wf, nil
}

func (ws *workflowSyncer) webhook(w http.ResponseWriter, r *http.Request) (int, string) {
	payload, err := github.ValidatePayload(r, ws.ghSecret)
	if err != nil {
		return http.StatusBadRequest, "request did not validate"
	}

	rawEvent, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		return http.StatusBadRequest, "could not parse request"
	}

	ctx := r.Context()

	switch event := rawEvent.(type) {
	case *github.CheckSuiteEvent:
		switch *event.Action {
		case "requested", "rerequested":
			return ws.webhookCheckSuite(ctx, event)
		default:
			return http.StatusOK, "unknown action ignore"
		}
	case *github.CheckRunEvent:
		switch *event.Action {
		case "requested_action":
			return ws.webhookCheckRunRequestAction(ctx, event)
		default:
			return http.StatusOK, "unknown action ignore"
		}
	case *github.DeploymentEvent:
		return ws.webhookDeployment(ctx, event)
	case *github.DeploymentStatusEvent:
		return ws.webhookDeploymentStatus(ctx, event)
	default:
		return http.StatusOK, fmt.Sprintf("unknown event type %T", event)
	}
}

func (ws *workflowSyncer) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	wf, err := ws.getWorkflow(
		ctx,
		ghClient,
		*event.Org.Login,
		*event.Repo.Name,
		*event.CheckSuite.HeadSHA,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)",
			ws.config.CIFilePath,
			*event.Org.Login,
			*event.Repo.Name,
			*event.CheckSuite.HeadSHA,
		)
		return http.StatusOK, ""
	}

	if err != nil {
		msg := fmt.Sprintf("unable to parse workflow, %v", err)
		status := "completed"
		conclusion := "failure"
		title := "Workflow Setup"
		_, _, err := ghClient.Checks.CreateCheckRun(ctx,
			*event.Org.Login,
			*event.Repo.Name,
			github.CreateCheckRunOptions{
				Name:       "Argo Workflow",
				HeadBranch: *event.CheckSuite.HeadBranch,
				HeadSHA:    *event.CheckSuite.HeadSHA,
				Status:     &status,
				Conclusion: &conclusion,
				CompletedAt: &github.Timestamp{
					Time: time.Now(),
				},
				Output: &github.CheckRunOutput{
					Title:   &title,
					Summary: &msg,
				},
			},
		)
		if err != nil {
			log.Printf("failed to create CR, %v", err)
		}
		return http.StatusBadRequest, msg
	}

	cr, _, err := ghClient.Checks.CreateCheckRun(ctx,
		*event.Org.Login,
		*event.Repo.Name,
		github.CreateCheckRunOptions{
			Name:       "Argo Workflow",
			HeadBranch: *event.CheckSuite.HeadBranch,
			HeadSHA:    *event.CheckSuite.HeadSHA,
		},
	)
	if err != nil {
		log.Printf("Unable to create check run, %v", err)
		return http.StatusInternalServerError, ""
	}

	wf = wf.DeepCopy()
	ws.updateWorkflow(wf, event, cr)
	_, err = ws.client.Argoproj().Workflows("argo").Create(wf)
	if err != nil {
		msg := fmt.Sprintf("argo workflow creation failed, %v", err)
		log.Print(msg)
		status := "completed"
		conclusion := "failure"
		_, _, err = ghClient.Checks.UpdateCheckRun(
			ctx,
			*event.Org.Login,
			*event.Repo.Name,
			*cr.ID,
			github.UpdateCheckRunOptions{
				Status:     &status,
				Conclusion: &conclusion,
				Output: &github.CheckRunOutput{
					Summary: &msg,
				},
				CompletedAt: &github.Timestamp{
					Time: time.Now(),
				},
			})
		if err != nil {
			log.Printf("Update of aborted check run failed, %v", err)
		}

		return http.StatusInternalServerError, ""
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookCheckRunRequestAction(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	/*
			// All is good (return an error to fail)
			ciFile := ".kube-ci/deploy.yaml"

			wf, err := ws.getWorkflow(
				ctx,
				ghClient,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
				ciFile,
			)

		if os.IsNotExist(err) {
			log.Printf("no %s in %s/%s (%s)",
				ciFile,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
			)
			return http.StatusOK, ""
		}
	*/

	env := "staging"
	msg := fmt.Sprintf("deploying the thing to %v", env)
	dep, _, err := ghClient.Repositories.CreateDeployment(
		ctx,
		*event.Org.Login,
		*event.Repo.Name,
		&github.DeploymentRequest{
			Ref:         event.CheckRun.HeadSHA,
			Description: &msg,
			Environment: &env,
		},
	)

	if err != nil {
		log.Printf("create deployment ailed, %v", err)
		return http.StatusInternalServerError, ""
	}

	log.Printf("Deployment created, %v", *dep.ID)

	return http.StatusOK, "blah"
}

func (ws *workflowSyncer) webhookDeployment(ctx context.Context, event *github.DeploymentEvent) (int, string) {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	logURL := fmt.Sprintf(
		"%s/workflows/%s/%s",
		ws.argoUIBase,
		"blah",
		"blah")

	pending := "pending"
	_, _, err = ghClient.Repositories.CreateDeploymentStatus(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		*event.Deployment.ID,
		&github.DeploymentStatusRequest{
			State:  &pending,
			LogURL: &logURL,
		},
	)

	if err != nil {
		log.Printf("create deployment state failed, %v", err)
		return http.StatusInternalServerError, ""
	}

	go func() {
		time.Sleep(10 * time.Second)
		success := "success"
		_, _, err := ghClient.Repositories.CreateDeploymentStatus(
			context.Background(),
			*event.Repo.Owner.Login,
			*event.Repo.Name,
			*event.Deployment.ID,
			&github.DeploymentStatusRequest{
				State:  &success,
				LogURL: &logURL,
			},
		)

		if err != nil {
			log.Printf("create deployment state failed, %v", err)
		}
	}()

	return http.StatusOK, ""
}

func (ws *workflowSyncer) webhookDeploymentStatus(ctx context.Context, event *github.DeploymentStatusEvent) (int, string) {
	log.Printf("status: %v is %v", *event.DeploymentStatus.ID, *event.DeploymentStatus.State)
	return http.StatusOK, ""
}
