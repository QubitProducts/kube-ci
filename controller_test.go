package kubeci

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	workflowfake "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/fake"
	informers "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v45/github"
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

	// Objects from here preloaded into NewSimpleFake.
	k8sObjects []runtime.Object
	wfObjects  []runtime.Object

	// Actions expected to happen on the client.
	k8sActions            []k8stesting.Action
	wfActions             []k8stesting.Action
	githubCalls           map[string][]githubCall
	githubCheckRunStatus  github.CheckRun
	githubCheckRunActions []*github.CheckRunAction

	wf *workflow.Workflow
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.wfObjects = []runtime.Object{}
	f.k8sObjects = []runtime.Object{}
	f.wf = baseTestWorkflow()
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

	c := NewWorkflowSyner(
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

func compareActions(text string, want, got []k8stesting.Action, t *testing.T) {
	actions := filterInformerActions(got)
	if len(want) > len(actions) {
		diff := cmp.Diff(want[len(actions):], nil)
		t.Errorf("missing %d %s: %+v", len(want)-len(actions), text, diff)
	}

	for i, action := range actions {
		if len(want) < i+1 {
			diff := cmp.Diff(nil, actions[i:])
			t.Errorf("%d unexpected %s:\n%s", len(actions)-len(want), text, diff)
			break
		}

		expectedAction := want[i]
		checkAction(expectedAction, action, t)
	}
}

func (f *fixture) run(obj interface{}, t *testing.T) {
	f.runController(obj, true, false, t)
}

//lint:ignore U1000 we will need this at some point
func (f *fixture) runExpectError(obj interface{}, t *testing.T) {
	f.runController(obj, true, true, t)
}

func (f *fixture) runController(obj interface{}, startInformers bool, expectError bool, t *testing.T) {
	c, i, k8sI, gh := f.newController(f.config, t)
	if startInformers {
		stopCh := make(chan struct{})
		defer close(stopCh)
		i.Start(stopCh)
		k8sI.Start(stopCh)
	}

	var err error
	var wf *workflow.Workflow
	switch obj := obj.(type) {
	case *workflow.Workflow:
		wf, err = c.sync(obj)
		if !expectError && err != nil {
			f.t.Errorf("error syncing workflow: %v", err)
		} else if expectError && err == nil {
			f.t.Error("expected error syncing workflow, got nil")
		}
	default:
	}

	compareActions("workflow actions", f.wfActions, f.wfClient.Actions(), t)
	compareActions("kubernetes actions", f.k8sActions, f.k8sClient.Actions(), t)

	crid, err := strconv.Atoi(wf.Annotations[annCheckRunID])
	if err != nil {
		f.t.Errorf("final workflow has invalid check-run id, %v", err)
	}

	status, actions := gh.getCheckRunStatus(int64(crid))
	// Because most of the creates are don't from the webhooks, not the
	// controller, we don't see them in the mock calls, so we have to
	// stub it out here (except in the cases where we do do the create)
	if status.HeadSHA == nil {
		status.HeadSHA = github.String("50dbe643f76dcd92c4c935455a46687c903e1b7d")
	}

	compare("wrong Github Check Run status", f.githubCheckRunStatus, status, f.t)
	compare("wrong check-run action buttons", f.githubCheckRunActions, actions, f.t)

	compareGithubActions("wrong github calls", f.githubCalls, gh.calls, f.t)
}

func compare[K any](text string, expected, actual K, t *testing.T) {
	if diff := cmp.Diff(expected, actual); diff != "" {
		t.Fatalf("%s\n(-want +got):\n%s", text, diff)
	}
}

func copyGithubActions(in map[string][]githubCall) map[string][]githubCall {
	tmp := map[string][]githubCall{}
	for k, calls := range in {
		// ignore update_check_run as we check that via the final check run status
		if k == "update_check_run" {
			continue
		}

		for j := range calls {
			// All the github calls here are mocked, there's no point in comparing
			// the results
			calls[j].Res = nil
		}

		tmp[k] = calls
	}
	return tmp
}

func compareGithubActions(text string, want, got map[string][]githubCall, t *testing.T) {
	want = copyGithubActions(want)
	got = copyGithubActions(got)

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
			t.Errorf("action: %s\n(-want +got):\n%s", verb, diff)
		}
	case "update":
		e, _ := expected.(k8stesting.UpdateAction)
		expObject := e.GetObject()
		act := actual.(k8stesting.UpdateAction)
		object := act.GetObject()

		if diff := cmp.Diff(expObject, object); diff != "" {
			t.Errorf("action: %s\n(-want +got):\n%s", verb, diff)
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
  - name: release-staging
    steps:
    - - arguments:
          parameters:
          - name: env
            value: staging
        name: release-staging
        template: release
  - name: very-long-action-button-name
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

// volumeAction creates a CheckRunAction which matches the one
// kube-ci sets up for volume deletion.
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

// userAction creates a CheckRunAction as per the ones
// kube-ci creates for user instigated actions
func userAction(task string) *github.CheckRunAction {
	return &github.CheckRunAction{
		Label:       task,
		Description: fmt.Sprintf("Run the %s template", task),
		Identifier:  task,
	}
}

// githubStatus creates a GithubStatus that matches the output for the above workflow
// this should really pull more info from the workflow
func githubStatus(status, conclusion string, warnings []string) github.CheckRun {
	text := "wf[1].release-staging(Failed): Error (exit code 2) \n"
	parts := append(warnings, text)
	text = strings.Join(parts, "\n")
	want := github.CheckRun{
		Name:       github.String("Argo Workflow"),
		Status:     github.String(status),
		HeadSHA:    github.String("50dbe643f76dcd92c4c935455a46687c903e1b7d"),
		DetailsURL: github.String("http://example.com/ui/workflows/default/wf"),

		Output: &github.CheckRunOutput{
			Title:       github.String("Workflow Run (default/wf))"),
			Summary:     github.String("child 'wf-1' failed"),
			Text:        github.String(text),
			Annotations: nil,
		},
	}
	if conclusion != "" {
		want.Conclusion = &conclusion
	}
	return want
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

func (f *fixture) expectCheckRunAnnotationsUpdate(wf *workflow.Workflow) *workflow.Workflow {
	wf = wf.DeepCopy()
	wf.Annotations[annAnnotationsPublished] = "true"
	f.expectUpdateWorkflowsAction(wf)
	for _, n := range wf.Status.Nodes {
		if n.Type != "Pod" {
			continue
		}
		f.expectPodGetLogs(wf.Namespace, n.ID)
	}

	return wf
}

// When we are creating deployments on-demand for running steps,
// we expect to get a deployment update for each step
func (f *fixture) expectDeploymentIDsUpdate(wf *workflow.Workflow) *workflow.Workflow {
	wf = wf.DeepCopy()
	wf.Annotations[annDeploymentIDs] = `{"wf-1":1}`

	f.expectUpdateWorkflowsAction(wf)

	return wf
}

func (f *fixture) expectWorkflowReset(wf *workflow.Workflow) *workflow.Workflow {
	wf = wf.DeepCopy()

	wf.Annotations[annAnnotationsPublished] = "false"
	// it would be nice if we could get the ID in from the
	// github fake, but at least we know it starts as not "1"
	// so much have been reset.
	anns := wf.Annotations
	anns[annCheckRunID] = "1"
	if _, ok := anns[annDeploymentIDs]; ok {
		anns[annDeploymentIDs] = `{}`
	}
	wf.Annotations = anns

	f.expectUpdateWorkflowsAction(wf)

	return wf
}

func (f *fixture) expectDeploymentIDsUpdateAfterWorkflowReset(wf *workflow.Workflow) *workflow.Workflow {
	wf = wf.DeepCopy()

	wf = f.expectWorkflowReset(wf)
	wf = f.expectDeploymentIDsUpdate(wf)

	return wf
}

func (f *fixture) expectGithubRawCall(call string, err error, res interface{}, args ...interface{}) {
	if f.githubCalls == nil {
		f.githubCalls = map[string][]githubCall{}
	}
	f.githubCalls[call] = append(f.githubCalls[call], githubCall{
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

type setupf func(f *fixture)

func createCheckRunRaw(opt github.CreateCheckRunOptions) setupf {
	return func(f *fixture) {
		res := &github.CheckRun{
			Name:    &opt.Name,
			HeadSHA: &opt.HeadSHA,
			Status:  opt.Status,
			Output:  opt.Output,
		}
		f.expectGithubCall("create_check_run", res, opt)
	}
}

func readRepoDetails() setupf {
	return func(f *fixture) {
		f.expectGithubCall("get_repo", nil)
	}
}

func createDeploymentStatus(opts *github.DeploymentStatusRequest) setupf {
	return func(f *fixture) {
		id := int64(1)
		f.expectGithubCall("create_deployment_status", &github.DeploymentStatus{ID: &id}, opts)
	}
}

func createDeployment(opts *github.DeploymentRequest) setupf {
	return func(f *fixture) {
		id := int64(1)
		f.expectGithubCall("create_deployment", &github.Deployment{ID: &id}, opts)
	}
}

func addAnnotations(anns map[string]string) setupf {
	return func(f *fixture) {
		for k, v := range anns {
			if f.wf.Annotations == nil {
				f.wf.Annotations = map[string]string{}
			}
			f.wf.Annotations[k] = v
		}
	}
}

func deploymentStatusRequest(_ *workflow.Workflow) *github.DeploymentStatusRequest {
	return &github.DeploymentStatusRequest{
		State:        github.String("failure"),
		LogURL:       github.String("http://example.com/ui/workflows/default/wf?nodeId=wf-1&sidePanel=logs%3Awf-1%3Amain&tab=workflow"),
		Description:  github.String("deploying qubitdigital/qubit-grafana (release) to staging: failure"),
		Environment:  github.String("staging"),
		AutoInactive: github.Bool(true),
	}
}

func enableUserActions(regex string) setupf {
	return func(f *fixture) {
		f.config.manualTemplates = regexp.MustCompile(regex)
	}
}

func enableDeploys() setupf {
	return func(f *fixture) {
		f.config.deployTemplates = regexp.MustCompile("^release$")
		f.config.productionEnvironments = regexp.MustCompile("^production$")
		f.config.EnvironmentParameter = "env"
	}
}

func createsDeployment() setupf {
	return func(f *fixture) {
		createDeployment(deploymentRequest(f.wf))(f)
	}
}

func createsDeploymentStatus() setupf {
	return func(f *fixture) {
		createDeploymentStatus(deploymentStatusRequest(f.wf))(f)
	}
}

func createsCheckRun(status, summary string) setupf {
	return createCheckRunRaw(
		github.CreateCheckRunOptions{
			Name:       "Argo Workflow",
			HeadSHA:    "50dbe643f76dcd92c4c935455a46687c903e1b7d",
			ExternalID: github.String("build"),
			Status:     github.String(status),
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String(summary),
			},
		},
	)
}

func createsNextCheckRun(task string) setupf {
	return createCheckRunRaw(
		github.CreateCheckRunOptions{
			Name:       "Workflow - " + task,
			HeadSHA:    "50dbe643f76dcd92c4c935455a46687c903e1b7d",
			Conclusion: github.String("action_required"),
			ExternalID: github.String(task),
			Output: &github.CheckRunOutput{
				Title:   github.String("Manual Step"),
				Summary: github.String("Use the button above to manually trigger this workflow template"),
			},
			Actions: []*github.CheckRunAction{
				{
					Label:       "Run",
					Description: "run this workflow template manually",
					Identifier:  "run",
				},
				{
					Label:       "Skip",
					Description: "mark as complete without running",
					Identifier:  "skip",
				},
			},
		},
	)
}

func updatesCheckRunAnnotations() setupf {
	return func(f *fixture) {
		f.expectCheckRunAnnotationsUpdate(f.wf)
	}
}

func resetsWorkflow() setupf {
	return func(f *fixture) {
		f.expectWorkflowReset(f.wf)
	}
}

func updatesDeploymentID() setupf {
	return func(f *fixture) {
		f.expectDeploymentIDsUpdate(f.wf)
	}
}

func resetsWorkflowAndDeploymentID() setupf {
	return func(f *fixture) {
		f.expectDeploymentIDsUpdateAfterWorkflowReset(f.wf)
	}
}

func expectGithubCalls(fs ...setupf) []setupf {
	return fs
}

func expectCheckRunActions(acts ...*github.CheckRunAction) setupf {
	return func(f *fixture) {
		f.githubCheckRunActions = acts
	}
}

func deploymentRequest(wf *workflow.Workflow) *github.DeploymentRequest {
	noRequired := []string{}
	return &github.DeploymentRequest{
		Ref:       github.String("heads/testdeploy"),
		Task:      github.String("release"),
		AutoMerge: github.Bool(false),
		Payload: DeploymentPayload{
			KubeCI: KubeCIPayload{
				Run:     false,
				SHA:     "50dbe643f76dcd92c4c935455a46687c903e1b7d",
				RefType: "branch",
				RefName: "testdeploy",
			},
		},
		Environment:           github.String("staging"),
		Description:           github.String("deploying qubitdigital/qubit-grafana (release) to staging"),
		TransientEnvironment:  nil,
		ProductionEnvironment: github.Bool(false),
		RequiredContexts:      &noRequired,
	}
}

func TestCreateWorkflow(t *testing.T) {
	var config Config
	config.deployTemplates = regexp.MustCompile("^$")
	config.manualTemplates = regexp.MustCompile("^$")
	config.productionEnvironments = regexp.MustCompile("^$")
	config.nonInteractiveBranches = regexp.MustCompile("^(production|staging)$")

	alreadyPublished := map[string]string{annAnnotationsPublished: "true"}
	alreadyPublishedWithDeploy := map[string]string{
		annAnnotationsPublished: "true",
		annDeploymentIDs:        `{"wf-1":4}`,
	}

	var tests = []struct {
		name         string
		phase        workflow.WorkflowPhase
		setup        []setupf
		expectStatus github.CheckRun
	}{
		{
			"normal_pending",
			workflow.WorkflowPending,
			nil,
			githubStatus("queued", "", nil),
		},
		{
			"normal_running",
			workflow.WorkflowRunning,
			nil,
			githubStatus("in_progress", "", nil),
		},
		{
			"normal_succeeded",
			workflow.WorkflowSucceeded,
			[]setupf{
				updatesCheckRunAnnotations(),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_branch_volume",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{"kube-ci.qutics.com/cacheScope": "branch"},
				),
				updatesCheckRunAnnotations(),
				expectCheckRunActions(volumeAction("branch")),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_project_volume",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{"kube-ci.qutics.com/cacheScope": "project"},
				),
				updatesCheckRunAnnotations(),
				expectCheckRunActions(volumeAction("project")),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_action_buttons",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{"kube-ci.qutics.com/cacheScope": "project"},
				),
				updatesCheckRunAnnotations(),
				enableUserActions("^release-staging$"),
				expectCheckRunActions(volumeAction("project")),
				createsNextCheckRun("release-staging"),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_action_buttons_defaultbranch",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{
						"kube-ci.qutics.com/cacheScope": "project",
						"kube-ci.qutics.com/branch":     "master",
					},
				),
				updatesCheckRunAnnotations(),
				enableUserActions("^release-staging$"),
				expectCheckRunActions(volumeAction("project")),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_action_buttons_noninteractivebranch",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{
						"kube-ci.qutics.com/cacheScope": "project",
						"kube-ci.qutics.com/branch":     "staging",
					},
				),
				updatesCheckRunAnnotations(),
				enableUserActions("^release-staging$"),
				expectCheckRunActions(volumeAction("project")),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_action_buttons_noninteractivebranchannotation",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{
						"kube-ci.qutics.com/cacheScope":             "project",
						"kube-ci.qutics.com/branch":                 "mybranch",
						"kube-ci.qutics.com/nonInteractiveBranches": "^mybranch$",
					},
				),
				updatesCheckRunAnnotations(),
				enableUserActions("^release-staging$"),
				expectCheckRunActions(volumeAction("project")),
				readRepoDetails(),
			},
			githubStatus("completed", "success", nil),
		},
		{
			"normal_succeeded_with_long_action_buttons",
			workflow.WorkflowSucceeded,
			[]setupf{
				addAnnotations(
					map[string]string{"kube-ci.qutics.com/cacheScope": "project"},
				),
				updatesCheckRunAnnotations(),
				enableUserActions("^very-long-"),
				expectCheckRunActions(volumeAction("project")),
				readRepoDetails(),
			},
			githubStatus(
				"completed",
				"success",
				[]string{"**WARNING:** Action button very-long-action-button-name dropped, only 20 chars permitted"},
			),
		},
		{
			"normal_failure",
			workflow.WorkflowFailed,
			[]setupf{
				updatesCheckRunAnnotations(),
			},
			githubStatus("completed", "failure", nil),
		},
		{
			// TODO: This might want further thought, a workflow errors if something
			// went wrong that was not he fault of the workflow, the workflow may be
			// retried. so it may not be sensile to mark it as completed.
			"normal_error",
			workflow.WorkflowError,
			nil,
			githubStatus("completed", "failure", nil),
		},
		{
			"restart_pending",
			workflow.WorkflowPending,
			expectGithubCalls(
				addAnnotations(alreadyPublished),
				createsCheckRun("queued", "Creating workflow"),
				resetsWorkflow(),
			),
			githubStatus("queued", "", nil),
		},
		{
			"restart_running",
			workflow.WorkflowRunning,
			expectGithubCalls(
				addAnnotations(alreadyPublished),
				createsCheckRun("queued", "Creating workflow"),
				resetsWorkflow(),
			),
			githubStatus("in_progress", "", nil),
		},
		{
			"deploy_running_external_trigger",
			workflow.WorkflowRunning,
			[]setupf{
				addAnnotations(
					map[string]string{annDeploymentIDs: `{"wf-1":1}`},
				),
				enableDeploys(),
				createsDeploymentStatus(),
			},
			githubStatus("in_progress", "", nil),
		},
		{
			"deploy_running_ondemand",
			workflow.WorkflowRunning,
			[]setupf{
				enableDeploys(),
				createsDeployment(),
				createsDeploymentStatus(),
				updatesDeploymentID(),
			},
			githubStatus("in_progress", "", nil),
		},
		{
			// A workflow that was triggered by an external deployment
			// creation has been resubmitted
			"deploy_restart_external_trigger",
			workflow.WorkflowRunning,
			[]setupf{
				addAnnotations(
					alreadyPublishedWithDeploy,
				),
				enableDeploys(),
				createsCheckRun("queued", "Creating workflow"),
				createsDeployment(),
				createsDeploymentStatus(),
				resetsWorkflowAndDeploymentID(),
			},
			githubStatus("in_progress", "", nil),
		},
		{
			// A normal workflow that creates deployments on-demand has
			// has been resubmitted
			"deploy_restart_ondemand",
			workflow.WorkflowRunning,
			[]setupf{
				addAnnotations(
					alreadyPublishedWithDeploy,
				),
				enableDeploys(),
				createsCheckRun("queued", "Creating workflow"),
				createsDeployment(),
				createsDeploymentStatus(),
				resetsWorkflowAndDeploymentID(),
			},
			githubStatus("in_progress", "", nil),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.config = config

			f.wf.Status.Phase = tt.phase
			pod := newPod("default", "wf-1")

			for _, setup := range tt.setup {
				setup(f)
			}

			f.k8sObjects = append(f.k8sObjects, pod)
			f.wfObjects = append(f.wfObjects, f.wf)

			f.githubCheckRunStatus = tt.expectStatus

			f.run(f.wf, t)
		})
	}
}

func TestLabelSafe(t *testing.T) {
	var tests = []struct {
		strs []string
	}{
		{[]string{"QubitProducts", "kube-ci", "", "master", "kube-ci.master"}},
		{[]string{"ERBC-96-post-release-stripping-of-locale-null-from-the-db"}},
		{[]string{"ERBC-967post-release-stripping-of-locale-null-from-the-db"}},
	}
	for i, tt := range tests {
		tt := tt
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			actual := labelSafe(tt.strs...)

			parts := strings.Split(actual, ".")

			for _, p := range parts {
				if !rfc1035Re.MatchString(p) {
					t.Errorf("%s in %s to match rfc1035", p, actual)
				}
			}

			if !workflowRegexp.MatchString(actual) {
				t.Errorf("%s does not match workflowRegexp", actual)
			}
		})
	}
}

func FuzzLabelSafe(f *testing.F) {
	f.Add("QubitProducts")
	f.Add("QubitProducts,something")
	f.Add("ERBC-96-post-release-stripping-of-locale-null-from-the-db")

	f.Fuzz(func(t *testing.T, str string) {
		// since fuzz functions can't take []string, we'll split the input
		// on `,`

		strs := strings.Split(str, ",")
		actual := labelSafe(strs...)
		if actual == "" {
			return
		}

		parts := strings.Split(actual, ".")

		for _, p := range parts {
			if !rfc1035Re.MatchString(p) {
				t.Errorf("%s in %s to match rfc1035", p, actual)
			}
		}

		if !workflowRegexp.MatchString(actual) {
			t.Errorf("%s does not match workflowRegexp", actual)
		}
	})
}
