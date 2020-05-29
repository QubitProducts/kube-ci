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
	"strings"
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

var (
	annCommit                      = "kube-ci.qutics.com/sha"
	annBranch                      = "kube-ci.qutics.com/branch"
	annRepo                        = "kube-ci.qutics.com/repo"
	annOrg                         = "kube-ci.qutics.com/org"
	annInstID                      = "kube-ci.qutics.com/github-install-id"
	annCheckRunName                = "kube-ci.qutics.com/check-run-name"
	annCheckRunID                  = "kube-ci.qutics.com/check-run-id"
	annAnnotationsPublished        = "kube-ci.qutics.com/annotations-published"
	annCacheVolumeName             = "kube-ci.qutics.com/cacheName"
	annCacheVolumeScope            = "kube-ci.qutics.com/cacheScope"
	annCacheVolumeStorageSize      = "kube-ci.qutics.com/cacheSize"
	annCacheVolumeStorageClassName = "kube-ci.qutics.com/cacheStorageClassName"

	labelManagedBy   = "managedBy"
	labelWFType      = "wfType"
	labelOrg         = "org"
	labelRepo        = "repo"
	labelBranch      = "branch"
	labelDetailsHash = "detailsHash"
)

type githubKeyStore struct {
	baseTransport http.RoundTripper
	appID         int
	ids           []int
	key           []byte
	orgs          *regexp.Regexp
}

func (ks *githubKeyStore) getClient(org string, installID int) (*github.Client, error) {
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

	if ks.orgs != nil && !ks.orgs.MatchString(org) {
		return nil, fmt.Errorf("refusing event from untrusted org %s", org)
	}

	itr, err := ghinstallation.New(ks.baseTransport, ks.appID, installID, ks.key)
	if err != nil {
		return nil, err
	}

	return github.NewClient(&http.Client{Transport: itr}), nil
}

type githubClientSource interface {
	getClient(org string, installID int) (*github.Client, error)
}

// CacheSpec lets you choose the default settings for a
// per-job cache volume.
type CacheSpec struct {
	Scope            string `yaml:"scope"`
	Size             string `yaml:"size"`
	StorageClassName string `yaml:"storageClassName"`
}

// TemplateSpec gives the description, and location, of a set
// of config files for use by the setup slash command
type TemplateSpec struct {
	Description string `yaml:"description"`
	CI          string `yaml:"ci"`
	Deploy      string `yaml:"deploy"`
}

// TemplateSet describes a set of templates
type TemplateSet map[string]TemplateSpec

// Config defines our configuration file format
type Config struct {
	CIFilePath    string            `yaml:"ciFilePath"`
	Namespace     string            `yaml:"namespace"`
	Tolerations   []v1.Toleration   `yaml:"tolerations"`
	NodeSelector  map[string]string `yaml:"nodeSelector"`
	TemplateSet   TemplateSet       `yaml:"templates"`
	CacheDefaults CacheSpec         `yaml:"cacheDefaults"`
	BuildDraftPRs bool              `yaml:"buildDraftPRs"`
	BuildBranches string            `yaml:"buildBranches"`

	buildBranches *regexp.Regexp
	secret        []byte
}

type workflowSyncer struct {
	ghSecret []byte

	ghClientSrc githubClientSource

	config     Config
	kubeclient kubernetes.Interface
	client     clientset.Interface
	lister     listers.WorkflowLister
	informer   cache.SharedIndexInformer
	synced     cache.InformerSynced
	workqueue  workqueue.RateLimitingInterface
	recorder   record.EventRecorder

	argoUIBase string
}

var sanitize = regexp.MustCompile(`[^-a-z0-9]`)
var sanitizeToDNS = regexp.MustCompile(`^[-.0-9]*`)

func escape(str string) string {
	return sanitize.ReplaceAllString(strings.ToLower(str), "-")
}

func labelSafe(strs ...string) string {
	escStrs := make([]string, len(strs))
	for i := 0; i < len(strs); i++ {
		escStrs[i] = escape(strs[i])
	}

	str := strings.Join(escStrs, ".")

	maxLen := 50
	if len(str) > maxLen {
		strOver := maxLen - len(str)
		str = str[strOver*-1 : len(str)]
	}

	return sanitizeToDNS.ReplaceAllString(str, "")
}

func wfName(prefix, owner, repo, branch string) string {
	timeStr := strconv.Itoa(int(time.Now().Unix()))
	if len(prefix) > 0 {
		return labelSafe(prefix, owner, repo, branch, timeStr)
	}
	return labelSafe(owner, repo, branch, timeStr)
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
			p.Name == "pullRequestID" ||
			p.Name == "pullRequestBaseBranch" ||
			p.Name == "branch" ||
			p.Name == "revision" ||
			p.Name == "orgname" ||
			p.Name == "reponame" {
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
		{
			Name:  "branch",
			Value: event.CheckSuite.HeadBranch,
		},
	}...)
	if len(event.CheckSuite.PullRequests) != 0 {
		pr := event.CheckSuite.PullRequests[0]
		prid := strconv.Itoa(pr.GetNumber())
		parms = append(parms, []workflow.Parameter{
			{
				Name:  "pullRequestID",
				Value: &prid,
			},
			{
				Name:  "pullRequestBaseBranch",
				Value: pr.Base.Ref,
			},
		}...)
	}

	wf.Spec.Arguments.Parameters = parms

	if wf.Labels == nil {
		wf.Labels = make(map[string]string)
	}
	wf.Labels[labelManagedBy] = "kube-ci"
	wf.Labels[labelWFType] = wfType
	wf.Labels[labelOrg] = labelSafe(*event.Repo.Owner.Login)
	wf.Labels[labelRepo] = labelSafe(*event.Repo.Name)
	wf.Labels[labelBranch] = labelSafe(*event.CheckSuite.HeadBranch)
	wf.Labels[labelDetailsHash] = detailsHash(*event.Repo.Owner.Login, *event.Repo.Name, *event.CheckSuite.HeadBranch)

	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}

	wf.Annotations[annCommit] = *event.CheckSuite.HeadSHA
	wf.Annotations[annBranch] = *event.CheckSuite.HeadBranch
	wf.Annotations[annRepo] = *event.Repo.Name
	wf.Annotations[annOrg] = *event.Repo.Owner.Login

	wf.Annotations[annInstID] = strconv.Itoa(int(*event.Installation.ID))

	wf.Annotations[annCheckRunName] = *cr.Name
	wf.Annotations[annCheckRunID] = strconv.Itoa(int(*cr.ID))
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

func (ws *workflowSyncer) doDelete(obj interface{}) {
	// we want to update the check run status for any
	// workflows that are deleted while still
	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return
	}

	cri, err := crInfoFromWorkflow(wf)
	if err != nil {
		return
	}

	if !wf.Status.FinishedAt.IsZero() {
		return
	}

	ghClient, err := ws.ghClientSrc.getClient(cri.orgName, int(cri.instID))
	if err != nil {
		return
	}

	status := "completed"
	conclusion := "cancelled"
	ghClient.Checks.UpdateCheckRun(
		context.Background(),
		cri.orgName,
		cri.repoName,
		cri.checkRunID,
		github.UpdateCheckRunOptions{
			Name:        cri.checkRunName,
			HeadBranch:  &cri.headBranch,
			HeadSHA:     &cri.headSHA,
			Status:      &status,
			Conclusion:  &conclusion,
			CompletedAt: &github.Timestamp{Time: time.Now()},
		},
	)
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
		DeleteFunc: syncer.doDelete,
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
	instIDStr, ok := wf.Annotations[annInstID]
	if !ok {
		return nil, fmt.Errorf("could not get github installation id for  %s/%s", wf.Namespace, wf.Name)
	}

	instID, err := strconv.Atoi(instIDStr)
	if err != nil {
		return nil, fmt.Errorf("could not convert installation id for %s/%s to int", wf.Namespace, wf.Name)
	}

	headSHA, ok := wf.Annotations[annCommit]
	if !ok {
		return nil, fmt.Errorf("could not get commit sha for %s/%s", wf.Namespace, wf.Name)
	}
	headBranch, ok := wf.Annotations[annBranch]
	if !ok {
		return nil, fmt.Errorf("could not get commit branch for %s/%s", wf.Namespace, wf.Name)
	}
	orgName, ok := wf.Annotations[annOrg]
	if !ok {
		return nil, fmt.Errorf("could not get github org for %s/%s", wf.Namespace, wf.Name)
	}
	repoName, ok := wf.Annotations[annRepo]
	if !ok {
		return nil, fmt.Errorf("could not get github repo name for %s/%s", wf.Namespace, wf.Name)
	}
	checkRunName, ok := wf.Annotations[annCheckRunName]
	if !ok {
		return nil, fmt.Errorf("could not get check run name for %s/%s", wf.Namespace, wf.Name)
	}

	checkRunIDStr, ok := wf.Annotations[annCheckRunID]
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

func (ws *workflowSyncer) resetCheckRun(wf *workflow.Workflow) (*workflow.Workflow, error) {
	newWf := wf.DeepCopy()

	cr, err := crInfoFromWorkflow(wf)
	if err != nil {
		return nil, fmt.Errorf("no check-run info found in restarted workflow (%s/%s)", wf.Namespace, wf.Name)
	}

	ghClient, err := ws.ghClientSrc.getClient(cr.orgName, int(cr.instID))
	if err != nil {
		return nil, err
	}

	newCR, _, err := ghClient.Checks.CreateCheckRun(context.TODO(),
		cr.orgName,
		cr.repoName,
		github.CreateCheckRunOptions{
			Name:       "Argo Workflow",
			HeadBranch: cr.headBranch,
			HeadSHA:    cr.headSHA,
			Status:     github.String("queued"),
		},
	)

	newWf.Annotations[annAnnotationsPublished] = "false"
	newWf.Annotations[annCheckRunName] = newCR.GetName()
	newWf.Annotations[annCheckRunID] = strconv.Itoa(int(newCR.GetID()))

	return ws.client.ArgoprojV1alpha1().Workflows(newWf.GetNamespace()).Update(newWf)
}

func (ws *workflowSyncer) sync(wf *workflow.Workflow) error {
	var err error

	log.Printf("got workflow: %v/%v %v", wf.Namespace, wf.Name, wf.Status.Phase)

	if v, ok := wf.Annotations[annAnnotationsPublished]; ok && v == "true" {
		switch wf.Status.Phase {
		case workflow.NodePending, workflow.NodeRunning: // attempt create new checkrun for a resubmitted job
			wf, err = ws.resetCheckRun(wf)
			if err != nil {
				log.Printf("failed checkrun reset, %v", err)
				return nil
			}
		default:
			log.Printf("ignoring %s/%s, already completed", wf.Namespace, wf.Name)
			return nil
		}
	}

	cr, err := crInfoFromWorkflow(wf)
	if err != nil {
		log.Printf("ignoring %s/%s, %v", wf.Namespace, wf.Name, err)
		return nil
	}

	ghClient, err := ws.ghClientSrc.getClient(cr.orgName, int(cr.instID))
	if err != nil {
		return err
	}

	status := ""

	failure := "failure"
	success := "success"
	neutral := "neutral"
	cancelled := "cancelled"
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
		if wf.Spec.ActiveDeadlineSeconds != nil && *wf.Spec.ActiveDeadlineSeconds == 0 {
			conclusion = &cancelled
		}
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

	ghClient, err := ws.ghClientSrc.getClient(cri.orgName, int(cri.instID))
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
	newwf, err := ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Get(wf.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("getting workflow %s/%s for annotations update failed, %v", newwf.Namespace, newwf.Name, err)
		return
	}

	upwf := newwf.DeepCopy()
	if upwf.Annotations == nil {
		upwf.Annotations = map[string]string{}
	}
	upwf.Annotations[annAnnotationsPublished] = "true"

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Update(upwf)
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

func (ws *workflowSyncer) getFile(
	ctx context.Context,
	ghClient *github.Client,
	owner string,
	name string,
	sha string,
	filename string) (io.ReadCloser, error) {

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
				return nil, os.ErrNotExist
			}
		}
		return nil, err
	}
	return file, nil
}

func (ws *workflowSyncer) getWorkflow(
	ctx context.Context,
	ghClient *github.Client,
	owner string,
	name string,
	sha string,
	filename string) (*workflow.Workflow, error) {

	file, err := ws.getFile(
		ctx,
		ghClient,
		owner,
		name,
		sha,
		filename)
	if err != nil {
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

	eventType := github.WebHookType(r)
	rawEvent, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return http.StatusBadRequest, "could not parse request"
	}

	log.Printf("webhook event of type %s", eventType)

	ctx := r.Context()

	switch event := rawEvent.(type) {
	case *github.CheckSuiteEvent:
		log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckSuite.HeadBranch, event.Sender.GetLogin())
		log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "requested", "rerequested":
			log.Printf("payload: %s", payload)
			return ws.webhookCheckSuite(ctx, event)
		default:
			log.Printf("unknown cheksuite action %q ignored", *event.Action)
			return http.StatusOK, "unknown cheksuite action ignored"
		}
	case *github.CheckRunEvent:
		log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckRun.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "rerequested":
			ev := &github.CheckSuiteEvent{
				Org:          event.Org,
				Repo:         event.Repo,
				CheckSuite:   event.CheckRun.GetCheckSuite(),
				Installation: event.Installation,
				Action:       event.Action,
			}
			return ws.webhookCheckSuite(ctx, ev)
		case "requested_action":
			return ws.webhookCheckRunRequestAction(ctx, event)
		default:
			log.Printf("unknown chekrun action %q ignored", *event.Action)
			return http.StatusOK, "unknown checkrun action ignored"
		}
	case *github.DeploymentEvent:
		return ws.webhookDeployment(ctx, event)
	case *github.DeploymentStatusEvent:
		return ws.webhookDeploymentStatus(ctx, event)
	case *github.IssueCommentEvent:
		return ws.webhookIssueComment(ctx, event)
	default:
		return http.StatusOK, fmt.Sprintf("unknown event type %T", event)
	}
}
