package main

import (
	"reflect"
	"regexp"
	"testing"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	workflowfake "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/fake"
	informers "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v32/github"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeStorageManager struct {
}

func (f *fakeStorageManager) ensurePVC(wf *workflow.Workflow, org, repo, branch string, defaults CacheSpec) error {
	panic("not implemented")
}

func (f *fakeStorageManager) deletePVC(org, repo, branch string, action string) error {
	panic("not implemented")
}

type fixture struct {
	t *testing.T

	wfClient  *workflowfake.Clientset
	k8sClient *k8sfake.Clientset

	config Config

	// Objects to put in the store.
	workflowsLister []*workflow.Workflow

	// Actions expected to happen on the client.
	k8sActions    []k8stesting.Action
	wfActions     []k8stesting.Action
	githubActions map[string][]githubCall
	githubStatus  GithubStatus
	// Objects from here preloaded into NewSimpleFake.
	k8sObjects []runtime.Object
	wfObjects  []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.wfObjects = []runtime.Object{}
	f.k8sObjects = []runtime.Object{}
	return f
}

var (
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

func (f *fixture) newController(config Config, t *testing.T) (*workflowSyncer, informers.SharedInformerFactory, k8sinformers.SharedInformerFactory, *testGHClientSrc) {
	f.wfClient = workflowfake.NewSimpleClientset(f.wfObjects...)
	f.k8sClient = k8sfake.NewSimpleClientset(f.k8sObjects...)

	i := informers.NewSharedInformerFactory(f.wfClient, noResyncPeriodFunc())
	k8sI := k8sinformers.NewSharedInformerFactory(f.k8sClient, noResyncPeriodFunc())

	storage := &fakeStorageManager{}
	clients := &testGHClientSrc{t: t}

	c := newWorkflowSyncer(
		f.k8sClient,
		f.wfClient,
		i,
		storage,
		clients,
		1234,
		[]byte("secret"),
		"http://example.com/ui",
		config,
	)

	for _, wf := range f.workflowsLister {
		f.t.Logf("adding workflow %s/%s", wf.Namespace, wf.Name)
		err := i.Argoproj().V1alpha1().Workflows().Informer().GetIndexer().Add(wf)
		if err != nil {
			f.t.Errorf("couldn't setup test, error adding workflow %s/%s, %v", wf.Namespace, wf.Name, err)
		}
	}

	return c, i, k8sI, clients
}

func (f *fixture) run(obj interface{}, t *testing.T) {
	f.runController(obj, true, false, t)
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) runExpectError(obj interface{}, t *testing.T) {
	f.runController(obj, true, true, t)
}

func compareActions(text string, want, got []k8stesting.Action, t *testing.T) {
	actions := filterInformerActions(got)
	for i, action := range actions {
		if len(want) < i+1 {
			diff := cmp.Diff(nil, actions[i:])
			t.Errorf("%d unexpected %s:\n%s", len(actions)-len(want), text, diff)
			break
		}

		expectedAction := want[i]
		checkAction(expectedAction, action, t)
	}

	if len(want) > len(actions) {
		diff := cmp.Diff(want[len(got):], nil)
		t.Errorf("missing %d %s: %+v", len(want)-len(got), text, diff)
	}
}

func (f *fixture) runController(obj interface{}, startInformers bool, expectError bool, t *testing.T) {
	c, i, k8sI, gh := f.newController(f.config, t)
	if startInformers {
		stopCh := make(chan struct{})
		defer close(stopCh)
		i.Start(stopCh)
		k8sI.Start(stopCh)
	}

	switch obj := obj.(type) {
	case *workflow.Workflow:
		err := c.sync(obj)
		if !expectError && err != nil {
			f.t.Errorf("error syncing workflow: %v", err)
		} else if expectError && err == nil {
			f.t.Error("expected error syncing workflow, got nil")
		}
	default:
	}

	compareActions("workflow actions", f.wfActions, f.wfClient.Actions(), t)
	compareActions("kubernetes actions", f.k8sActions, f.k8sClient.Actions(), t)
	compareGithubActions("wrong github calls", f.githubActions, gh.actions, f.t)
	compare("wrong Github Check Run status", f.githubStatus, gh.getCheckRunStatus(), f.t)
}

func compare[K any](text string, expected, actual K, t *testing.T) {
	if diff := cmp.Diff(expected, actual); diff != "" {
		t.Fatalf("%s\n(-want +got):\n%s", text, diff)
	}
}

func compareGithubActions(text string, want, got map[string][]githubCall, t *testing.T) {
	// All the github calls here are mocked, there's no point in comparing
	// the results
	for i := range got {
		for j := range got[i] {
			got[i][j].Res = nil
		}
	}
	for i := range want {
		for j := range want[i] {
			want[i][j].Res = nil
		}
	}

	compare(text, want, got, t)
}

// checkAction verifies that expected and actual actions are equal and both have
// same attached resources
func checkAction(expected, actual k8stesting.Action, t *testing.T) {
	if !(expected.Matches(actual.GetVerb(), actual.GetResource().Resource) && actual.GetSubresource() == expected.GetSubresource()) {
		t.Errorf("Expected\n\t%#v\ngot\n\t%#v", expected, actual)
		return
	}

	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		t.Errorf("Action has wrong type. Expected: %t. Got: %t", expected, actual)
		return
	}

	verb := actual.GetVerb()
	switch verb {
	case "create":
		e, _ := expected.(k8stesting.CreateAction)
		act := actual.(k8stesting.CreateAction)
		expObject := e.GetObject()
		object := act.GetObject()

		if diff := cmp.Diff(expObject, object); diff != "" {
			t.Fatalf("\n(-want +got):\n%s", diff)
		}
	case "update":
		e, _ := expected.(k8stesting.UpdateAction)
		expObject := e.GetObject()
		act := actual.(k8stesting.UpdateAction)
		object := act.GetObject()

		if diff := cmp.Diff(expObject, object); diff != "" {
			t.Fatalf("\n(-want +got):\n%s", diff)
		}
	}
}

// filterInformerActions filters list and watch actions for testing resources.
// Since list and watch don't change resource state we can filter it to lower
// nose level in our tests.
func filterInformerActions(actions []k8stesting.Action) []k8stesting.Action {
	ret := []k8stesting.Action{}
	for _, action := range actions {
		if action.Matches("get", "workflows") ||
			action.Matches("list", "workflows") ||
			action.Matches("watch", "workflows") {
			continue
		}
		ret = append(ret, action)
	}

	return ret
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) expectCreateWorkflowAction(rs *workflow.Workflow) {
	f.wfActions = append(f.wfActions,
		k8stesting.NewCreateAction(schema.GroupVersionResource{
			Resource: "workflows",
			Group:    workflow.SchemeGroupVersion.Group,
			Version:  workflow.SchemeGroupVersion.Version,
		}, rs.Namespace, rs),
	)
}

func (f *fixture) expectUpdateWorkflowsAction(rs *workflow.Workflow) {
	f.wfActions = append(f.wfActions, k8stesting.NewUpdateAction(schema.GroupVersionResource{
		Resource: "workflows",
		Group:    workflow.SchemeGroupVersion.Group,
		Version:  workflow.SchemeGroupVersion.Version,
	}, rs.Namespace, rs))
}

func newWorkflow(str string) *workflow.Workflow {
	return workflow.MustUnmarshalWorkflow(str)
}

func baseTestWorkflow() *workflow.Workflow {
	return newWorkflow(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    kube-ci.qutics.com/branch: testdeploy
    kube-ci.qutics.com/cacheSize: 20Gi
    kube-ci.qutics.com/check-run-id: "7319927949"
    kube-ci.qutics.com/check-run-name: Argo Workflow
    kube-ci.qutics.com/github-install-id: "593693"
    kube-ci.qutics.com/org: qubitdigital
    kube-ci.qutics.com/repo: qubit-grafana
    kube-ci.qutics.com/sha: 50dbe643f76dcd92c4c935455a46687c903e1b7d
    workflows.argoproj.io/pod-name-format: v1
  creationTimestamp: "2022-07-13T11:26:59Z"
  generation: 5
  labels:
    branch: testdeploy
    managedBy: kube-ci
    org: myorg
    repo: myrepo
  name: wf
  namespace: default
  resourceVersion: "891608799"
  uid: 69963d6a-bba8-4a83-bf57-aabb96df9217
spec:
  arguments:
    parameters:
    - name: repo
      value: git@github.com:myorg/myrepo.git
    - name: repo_git_url
      value: git://github.com/myorg/myrepo.git
    - name: repo_https_url
      value: https://github.com/myorg/myrepo.git
    - name: repoName
      value: myrepo
    - name: orgName
      value: myorg
    - name: revision
      value: 50dbe643f76dcd92c4c935455a46687c903e1b7d
    - name: refType
      value: branch
    - name: refName
      value: testdeploy
    - name: branch
      value: testdeploy
    - name: repoDefaultBranch
      value: master
    - name: pullRequestID
      value: ""
    - name: pullRequestBaseBranch
      value: ""
    - name: cacheVolumeClaimName
      value: cacheVol
  entrypoint: build
  templates:
  - name: build
    steps:
    - - arguments:
          parameters:
          - name: env
            value: production
        name: release-production
        template: release
        when: '"{{workflow.parameters.branch}}" == master'
    - - arguments:
          parameters:
          - name: env
            value: staging
        name: release-staging
        template: release
        when: '"{{workflow.parameters.branch}}" != master'
  - name: release
    container:
      command:
      - /bin/true
      image: alpine
      name: ""
      workingDir: /src
    inputs:
      parameters:
      - name: env
        value: staging
    metadata: {}
    outputs: {}
status:
  conditions:
  - status: "False"
    type: PodRunning
  - status: "True"
    type: Completed
  message: child 'wf-1' failed
  phase: Pending
  nodes:
    wf:
      displayName: wf
      id: wf
      message: child 'wf-1' failed
      name: wf
      phase: Failed
      templateName: build
      type: Steps
    wf-1:
      displayName: release-staging
      id: wf-1
      inputs:
        parameters:
        - name: env
          value: staging
      message: Error (exit code 2)
      name: wf[1].release-staging
      outputs:
        artifacts:
        - name: main-logs
          s3:
            key: wf/wf-1/main.log
        exitCode: "2"
      phase: Failed
      templateName: release
      templateScope: local/wf
      type: Pod
    wf-2:
      displayName: '[1]'
      id: wf-2
      message: child 'wf-1' failed
      name: wf[1]
      phase: Failed
      type: StepGroup
    wf-3:
      displayName: '[0]'
      id: wf-3
      name: wf[0]
      phase: Succeeded
      type: StepGroup
    wf-4:
      displayName: release-production
      id: wf-4
      message: when '"testdeploy" == master' evaluated false
      name: wf[0].release-production
      phase: Skipped
      templateName: release
      type: Skipped`)
}

func newPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) expectWorkflowUpdate(wf *workflow.Workflow) {
	f.wfActions = append(f.wfActions,
		k8stesting.NewUpdateAction(schema.GroupVersionResource{
			Resource: "workflows",
			Group:    workflow.SchemeGroupVersion.Group,
			Version:  workflow.SchemeGroupVersion.Version,
		}, wf.Namespace, wf),
	)
}

func (f *fixture) expectPodGetLogs(namespace, name string) {
	action := k8stesting.GenericActionImpl{}
	action.Verb = "get"
	action.Namespace = namespace
	action.Resource = schema.GroupVersionResource{
		Resource: "pods",
		Group:    corev1.SchemeGroupVersion.Group,
		Version:  corev1.SchemeGroupVersion.Version,
	}
	action.Subresource = "log"
	action.Value = &corev1.PodLogOptions{Container: "main"}

	f.k8sActions = append(f.k8sActions, action)
}

func (f *fixture) expectAnnotationsUpdate(wf *workflow.Workflow) {
	wf = wf.DeepCopy()
	wf.Annotations[annAnnotationsPublished] = "true"
	f.expectUpdateWorkflowsAction(wf)
	for _, n := range wf.Status.Nodes {
		if n.Type != "Pod" {
			continue
		}
		f.expectPodGetLogs(wf.Namespace, n.ID)
	}
}

// When we are creating deployments on-demand for running steps,
// we expect to get a deployment update for each step
func (f *fixture) expectDeploymentIDsUpdate(wf *workflow.Workflow) {
	wf = wf.DeepCopy()
	wf.Annotations[annDeploymentIDs+"wf-1"] = "1"
	f.expectUpdateWorkflowsAction(wf)
}

func (f *fixture) expectWorkflowReset(wf *workflow.Workflow) {
	wf = wf.DeepCopy()

	wf.Annotations[annAnnotationsPublished] = "false"
	// it would be nice if we could get the ID in from the
	// github fake, but at least we know it starts as not "1"
	// so much have been reset.
	wf.Annotations[annCheckRunID] = "1"
	f.expectUpdateWorkflowsAction(wf)
}

func (f *fixture) expectGithubRawCall(call string, err error, res interface{}, args ...interface{}) {
	if f.githubActions == nil {
		f.githubActions = map[string][]githubCall{}
	}
	f.githubActions[call] = append(f.githubActions[call], githubCall{
		Args: args,
		Err:  err,
		Res:  res,
	})
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) expectGithubCallErr(call string, err error, args ...interface{}) {
	f.expectGithubRawCall(call, err, nil, args...)
}

func (f *fixture) expectGithubCall(call string, res interface{}, args ...interface{}) {
	f.expectGithubRawCall(call, nil, res, args...)
}

type setupf func(f *fixture, wf *workflow.Workflow)

func createCheckRunRaw(opt github.CreateCheckRunOptions) setupf {
	return func(f *fixture, _ *workflow.Workflow) {
		f.expectGithubCall("create_check_run", nil, opt)
	}
}

func createDeploymentStatus(opts *github.DeploymentStatusRequest, wf *workflow.Workflow) setupf {
	return func(f *fixture, _ *workflow.Workflow) {
		id := int64(1)
		f.expectGithubCall("create_deployment_status", &github.DeploymentStatus{ID: &id}, opts)
	}
}

func createDeployment(opts *github.DeploymentRequest, wf *workflow.Workflow) setupf {
	return func(f *fixture, _ *workflow.Workflow) {
		id := int64(1)
		f.expectGithubCall("create_deployment", &github.Deployment{ID: &id}, opts)
	}
}

func deploymentStatusRequest(_ *workflow.Workflow) *github.DeploymentStatusRequest {
	return &github.DeploymentStatusRequest{
		State:        github.String("failure"),
		LogURL:       github.String("http://example.com/ui/workflows/default/wf/wf-1?nodeId=wf-1&sidePanel=logs%3Awf-1%3Amain&tab=workflow"),
		Description:  github.String("deploying qubitdigital/qubit-grafana (release) to staging: failure"),
		Environment:  github.String("staging"),
		AutoInactive: github.Bool(true),
	}
}

func enableDeploys(createsDeployment bool) setupf {
	return func(f *fixture, wf *workflow.Workflow) {
		f.config.deployTemplates = regexp.MustCompile("^release$")
		f.config.productionEnvironments = regexp.MustCompile("^production$")
		f.config.EnvironmentParameter = "env"

		// TODO(tcm): This would be testing a restart of a deploy (I think)
		//createCheckRun("queued", "Creating workflow")(f)

		if createsDeployment {
			f.expectDeploymentIDsUpdate(wf)
			createDeployment(deploymentRequest(wf), wf)(f, wf)
		}
		createDeploymentStatus(deploymentStatusRequest(wf), wf)(f, wf)
	}
}

func createCheckRun(status, summary string) setupf {
	return createCheckRunRaw(
		github.CreateCheckRunOptions{
			Name:    "Argo Workflow",
			HeadSHA: "50dbe643f76dcd92c4c935455a46687c903e1b7d",
			Status:  github.String(status),
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String(summary),
			},
		},
	)
}

func expectGithubCalls(fs ...setupf) []setupf {
	return fs
}

func volumeAction(scope string) *github.CheckRunAction {
	if scope == "" {
		return nil
	}
	task := "clearCache"
	if scope == "branch" {
		task = "clearCacheBranch"
	}

	return &github.CheckRunAction{
		Label:       "Clear Cache",
		Description: "delete the cache volume for this build",
		Identifier:  task,
	}
}

func githubStatus(status, conclusion string, actions ...*github.CheckRunAction) GithubStatus {
	return GithubStatus{
		Status:     status,
		Conclusion: conclusion,
		Actions:    actions,
		Title:      "Workflow Run (default/wf))",

		DetailsURL: "http://example.com/ui/workflows/default/wf",
		Summary:    "child 'wf-1' failed",
		Text:       "wf[1].release-staging(Failed): Error (exit code 2) \n",

		Annotations: nil,
	}
}

func deploymentRequest(wf *workflow.Workflow) *github.DeploymentRequest {
	return &github.DeploymentRequest{
		Ref:                   github.String("50dbe643f76dcd92c4c935455a46687c903e1b7d"),
		Task:                  github.String("release"),
		AutoMerge:             github.Bool(false),
		Payload:               DeploymentPayload{Passive: true},
		Environment:           github.String("staging"),
		Description:           github.String("deploying qubitdigital/qubit-grafana (release) to staging"),
		TransientEnvironment:  nil,
		ProductionEnvironment: github.Bool(false),
	}
}

func TestCreateWorkflow(t *testing.T) {
	var config Config
	config.deployTemplates = regexp.MustCompile("^$")
	config.actionTemplates = regexp.MustCompile("^$")
	config.productionEnvironments = regexp.MustCompile("^$")

	alreadyPublished := map[string]string{annAnnotationsPublished: "true"}

	var tests = []struct {
		name                string
		phase               workflow.WorkflowPhase
		extraAnnotations    map[string]string
		expectLogs          bool
		expectWorkflowReset bool
		setup               []setupf
		expectStatus        GithubStatus
	}{
		{
			"normal_pending",
			workflow.WorkflowPending,
			nil,
			false,
			false,
			nil,
			githubStatus("queued", ""),
		},
		{
			"normal_running",
			workflow.WorkflowRunning,
			nil,
			false,
			false,
			nil,
			githubStatus("in_progress", ""),
		},
		{
			"normal_succeeded",
			workflow.WorkflowSucceeded,
			nil,
			true,
			false,
			nil,
			githubStatus("completed", "success"),
		},
		{
			"normal_succeeded_with_branch_volume",
			workflow.WorkflowSucceeded,
			map[string]string{"kube-ci.qutics.com/cacheScope": "branch"},
			true,
			false,
			nil,
			githubStatus("completed", "success", volumeAction("branch")),
		},
		{
			"normal_succeeded_with_project_volume",
			workflow.WorkflowSucceeded,
			map[string]string{"kube-ci.qutics.com/cacheScope": "project"},
			true,
			false,
			nil,
			githubStatus("completed", "success", volumeAction("project")),
		},
		{
			"normal_failure",
			workflow.WorkflowFailed,
			nil,
			true,
			false,
			nil,
			githubStatus("completed", "failure"),
		},
		{
			// TODO: This might want further thought, a workflow errors if something
			// went wrong that was not he fault of the workflow, the workflow may be
			// retried. so it may not be sensile to mark it as completed.
			"normal_error",
			workflow.WorkflowError,
			nil,
			false,
			false,
			nil,
			githubStatus("completed", "failure"),
		},
		{
			"restart_pending",
			workflow.WorkflowPending,
			alreadyPublished,
			false,
			true,
			expectGithubCalls(createCheckRun("queued", "Creating workflow")),
			githubStatus("queued", ""),
		},
		{
			"restart_running",
			workflow.WorkflowPending,
			alreadyPublished,
			false,
			true,
			expectGithubCalls(createCheckRun("queued", "Creating workflow")),
			githubStatus("queued", ""),
		},
		{
			"deploy_running_external_trigger",
			workflow.WorkflowRunning,
			map[string]string{"kube-ci.qutics.com/deployment-ids/wf-1": "1"},
			false,
			false,
			[]setupf{enableDeploys(false)},
			githubStatus("in_progress", ""),
		},
		{
			"deploy_running_ondemand",
			workflow.WorkflowRunning,
			nil,
			false,
			false,
			[]setupf{enableDeploys(true)},
			githubStatus("in_progress", ""),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.config = config

			wf := baseTestWorkflow()
			wf.Status.Phase = tt.phase
			for k, v := range tt.extraAnnotations {
				wf.Annotations[k] = v
			}
			pod := newPod("default", "wf-1")

			for _, setup := range tt.setup {
				setup(f, wf)
			}

			f.k8sObjects = append(f.k8sObjects, pod)
			f.wfObjects = append(f.wfObjects, wf)

			if tt.expectLogs {
				f.expectAnnotationsUpdate(wf)
			}

			if tt.expectWorkflowReset {
				f.expectWorkflowReset(wf)
			}

			f.githubStatus = tt.expectStatus

			f.run(wf, t)
		})
	}
}
