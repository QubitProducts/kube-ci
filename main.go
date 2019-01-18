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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	clientset "github.com/argoproj/argo/pkg/client/clientset/versioned"
	argoScheme "github.com/argoproj/argo/pkg/client/clientset/versioned/scheme"
	informers "github.com/argoproj/argo/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo/pkg/client/listers/workflow/v1alpha1"
	yaml "gopkg.in/yaml.v2"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v22/github"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

var ()

func rootHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"hello": "world",
	}

	bs, _ := json.Marshal(resp)
	buf := bytes.NewBuffer(bs)

	io.Copy(w, buf)
}

type httpErrorFunc func(w http.ResponseWriter, r *http.Request) (int, string)

func (f httpErrorFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code, status := f(w, r)

	if code == 0 {
		// We'll use 0 to indicate that the request output as already been handled.
		return
	}

	if code == 0 {
		code = http.StatusBadRequest
	}

	if status == "" {
		status = http.StatusText(code)
	}

	http.Error(w, status, code)
}

// updateWorkflow, lots of these settings shoud come in from some config.
func updateWorkflow(wf *workflow.Workflow, event *github.CheckSuiteEvent, cr *github.CheckRun) *workflow.Workflow {
	newWf := wf.DeepCopy()

	newWf.GenerateName = ""
	newWf.Namespace = "argo"
	newWf.Name = fmt.Sprintf("%s.%s.%s.%d",
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		*event.CheckSuite.HeadBranch,
		time.Now().Unix(),
	)

	ttl := int32((3 * 24 * time.Hour) / time.Second)
	newWf.Spec.TTLSecondsAfterFinished = &ttl

	newWf.Spec.Tolerations = append(
		newWf.Spec.Tolerations,
		v1.Toleration{
			Effect: "NoSchedule",
			Key:    "dedicated",
			Value:  "ci",
		},
	)

	newWf.Spec.NodeSelector = map[string]string{
		"jenkins": "true",
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

	newWf.Spec.Arguments.Parameters = parms

	if newWf.Labels == nil {
		newWf.Labels = make(map[string]string)
	}
	newWf.Labels["managedBy"] = "kube-ci"

	if newWf.Annotations == nil {
		newWf.Annotations = make(map[string]string)
	}

	newWf.Annotations["kube-ci.qutics.com/sha"] = *event.CheckSuite.HeadSHA
	newWf.Annotations["kube-ci.qutics.com/branch"] = *event.CheckSuite.HeadBranch
	newWf.Annotations["kube-ci.qutics.com/repo"] = *event.Repo.Name
	newWf.Annotations["kube-ci.qutics.com/org"] = *event.Repo.Owner.Login

	newWf.Annotations["kube-ci.qutics.com/check-run-name"] = *cr.Name
	newWf.Annotations["kube-ci.qutics.com/check-run-id"] = strconv.Itoa(int(*cr.ID))
	newWf.Annotations["kube-ci.qutics.com/github-install-id"] = strconv.Itoa(int(*event.Installation.ID))

	return newWf
}

type githubKeyStore struct {
	baseTransport http.RoundTripper
	appID         int

	keys map[int][]byte
}

func (ks *githubKeyStore) getClient(installID int) (*github.Client, error) {
	key, ok := ks.keys[installID]
	if !ok {
		return nil, fmt.Errorf("unknown installation %d", installID)
	}

	itr, err := ghinstallation.New(ks.baseTransport, ks.appID, installID, key)
	if err != nil {
		return nil, err
	}

	return github.NewClient(&http.Client{Transport: itr}), nil
}

type githubClientSource interface {
	getClient(installID int) (*github.Client, error)
}

type workflowSyncer struct {
	ghSecret []byte

	ghClientSrc githubClientSource

	kubeclient kubernetes.Interface
	client     clientset.Interface
	lister     listers.WorkflowLister
	synced     cache.InformerSynced
	workqueue  workqueue.RateLimitingInterface
	recorder   record.EventRecorder

	argoUIBase string
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
				//Images: []*github.CheckRunImage{{
				//	Alt:      &imgAlt,
				// ImageURL: &imgURL,
				// Caption:  &imgCapt,
				//}},
			},
			//Actions: []*github.CheckRunAction{
			//	{
			//		Label:       "Click Me",
			//		Description: "This brings joy",
			//		Identifier:  "joyBringer",
			//	},
			//},
		},
	)

	if err != nil {
		log.Printf("Unable to update check run, %v", err)
	}
	return nil
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

func (ws *workflowSyncer) webhook(w http.ResponseWriter, r *http.Request) (int, string) {
	payload, err := github.ValidatePayload(r, ws.ghSecret)
	if err != nil {
		return http.StatusBadRequest, "request did not validate"
	}

	rawEvent, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		return http.StatusBadRequest, "could not parse request"
	}

	switch event := rawEvent.(type) {
	case *github.CheckSuiteEvent:
		if *event.Action != "requested" && *event.Action != "rerequested" {
			return http.StatusOK, ""
		}
	default:
		return http.StatusOK, ""
	}

	event := rawEvent.(*github.CheckSuiteEvent)

	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	ctx := r.Context()

	// All is good (return an error to fail)
	file, err := ghClient.Repositories.DownloadContents(
		ctx,
		*event.Org.Login,
		*event.Repo.Name,
		".kube-ci/ci.yaml",
		&github.RepositoryContentGetOptions{
			Ref: *event.CheckSuite.HeadSHA,
		})
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode == http.StatusNotFound {
				log.Printf("no .kube-ci/ci.yaml in %s/%s (%s)",
					*event.Org.Login,
					*event.Repo.Name,
					*event.CheckSuite.HeadSHA,
				)
				return http.StatusOK, ""
			}
		}
		log.Printf("github call failed, %v", err)
		return http.StatusInternalServerError, ""
	}
	defer file.Close()

	bs := &bytes.Buffer{}

	_, err = io.Copy(bs, file)
	if err != nil {
		log.Printf("copy failed, %v", err)
		return http.StatusInternalServerError, ""
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bs.Bytes(), nil, nil)

	if err != nil {
		log.Printf("Unable to decode content, %v", err)
		msg := fmt.Sprintf("unable to parse workflow, %v", err)
		log.Print(msg)
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

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		msg := fmt.Sprintf("could not use %T as workflow", wf)
		log.Print(msg)
		status := "completed"
		conclusion := "failure"
		title := "Workflow Setup"
		ghClient.Checks.CreateCheckRun(ctx,
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
		return http.StatusBadRequest, "unable convert to workflow"
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

	_, err = ws.client.Argoproj().Workflows("argo").Create(updateWorkflow(wf, event, cr))
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

func main() {
	argoScheme.AddToScheme(scheme.Scheme)

	kubeconfig := flag.String("kubeconfig", "", "Path to a kubeconfig file")
	masterURL := flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	shutdownGrace := flag.Duration("grace.shutdown", 2*time.Second, "delay before server shuts down")
	requestGrace := flag.Duration("grace.requests", 1*time.Second, "delay before server starts shut down")
	keysfile := flag.String("keysfile", "keys.yaml", "file of github installation keys")
	secretfile := flag.String("secretfile", "webhook-secret", "file containing your webhook secret")
	appID := flag.Int("github.appid", 0, "github application ID")
	argoUIBaseURL := flag.String("argo.ui.base", "http://argo", "file containing your webhook secret")

	flag.Parse()

	if *appID == 0 {
		log.Fatal("github appID must be set")
	}

	config, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	wfClient, err := clientset.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create argo client, %v", err)
	}

	namespace := "argo"
	selector := "managedBy=kube-ci"

	sinf := informers.NewFilteredSharedInformerFactory(
		wfClient,
		0,
		namespace,
		func(opt *metav1.ListOptions) {
			opt.LabelSelector = selector
		},
	)

	keysbs, err := ioutil.ReadFile(*keysfile)
	if err != nil {
		log.Fatalf("failed to read keys files, %v", err)
	}

	var keyList []struct {
		ID  int    `json:"id" yaml:"id"`
		Key string `json:"key" yaml:"key"`
	}

	err = yaml.Unmarshal(keysbs, &keyList)
	if err != nil {
		log.Fatalf("failed to parse keys file, %v", err)
	}

	keys := map[int][]byte{}
	for _, k := range keyList {
		if _, ok := keys[k.ID]; ok {
			log.Fatalf("duplicate key id %v", k.ID)
		}
		keys[k.ID] = []byte(k.Key)
	}

	ghSrc := &githubKeyStore{
		baseTransport: http.DefaultTransport,
		appID:         *appID,
		keys:          keys,
	}

	secret, err := ioutil.ReadFile(*secretfile)
	if err != nil {
		log.Fatalf("failed to read secrets file, %v", err)
	}

	wfSyncer := newWorkflowSyncer(
		kubeClient,
		wfClient,
		sinf,
		ghSrc,
		secret,
		*argoUIBaseURL,
	)

	shutdownInt := int32(200)
	sh := &statusHandler{
		shutdown: &shutdownInt,
	}

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "A histogram of latencies for requests.",
			Buckets: []float64{.001, .005, 0.010, 0.05, 0.5, 1.0},
		},
		[]string{},
	)
	prometheus.MustRegister(duration)

	mux := http.NewServeMux()
	mux.HandleFunc("/",
		promhttp.InstrumentHandlerDuration(
			duration,
			http.HandlerFunc(rootHandler)))

	mux.HandleFunc("/webhooks/github",
		promhttp.InstrumentHandlerDuration(
			duration,
			httpErrorFunc(wfSyncer.webhook)))

	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/status", sh)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	done := make(chan struct{})
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	signal.Notify(quit, syscall.SIGTERM)

	go func() {
		sinf.Start(done)
		go wfSyncer.Run(done)

		<-quit

		log.Println("setting status to shutdown")
		sh.Shutdown()

		log.Printf("sleeping for %s before shutdown", shutdownGrace)
		time.Sleep(*shutdownGrace)

		ctx, cancel := context.WithTimeout(context.Background(), *requestGrace)
		defer cancel()

		log.Printf("shutting down the server")
		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		log.Printf("server shutdown")

		close(done)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %s: %v\n", ":8080", err)
	}
}
