package main

import (
	"reflect"
	"regexp"
	"testing"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	workflowfake "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned/fake"
	informers "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/diff"
)

type fakeStorageManager struct {
}

func (f *fakeStorageManager) ensurePVC(wf *workflow.Workflow, org, repo, branch string, defaults CacheSpec) error {
	panic("not implemented")
}

func (f *fakeStorageManager) deletePVC(org, repo, branch string, action string) error {
	panic("not implemented")
}

func TestWFName(t *testing.T) {
	t.Logf("name: %q", wfName("ci", "qubitdigital", "yak", "mytests/tester"))
}

type fixture struct {
	t *testing.T

	client     *workflowfake.Clientset
	kubeclient *k8sfake.Clientset

	config Config

	// Objects to put in the store.
	workflowsLister []*workflow.Workflow
	// Actions expected to happen on the client.
	kubeactions []k8stesting.Action
	actions     []k8stesting.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object
	objects     []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.objects = []runtime.Object{}
	f.kubeobjects = []runtime.Object{}
	return f
}

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

func (f *fixture) newController(config Config) (*workflowSyncer, informers.SharedInformerFactory, k8sinformers.SharedInformerFactory, *testGHClientSrc) {
	f.client = workflowfake.NewSimpleClientset(f.objects...)
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)

	i := informers.NewSharedInformerFactory(f.client, noResyncPeriodFunc())
	k8sI := k8sinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	storage := &fakeStorageManager{}
	clients := &testGHClientSrc{}

	c := newWorkflowSyncer(
		f.kubeclient,
		f.client,
		i,
		storage,
		clients,
		1234,
		[]byte("secret"),
		"http://example.com/ui",
		config,
	)

	/*
		for _, f := range f.workflowLister {
			i.Config().V1beta1().RuleGroups().Informer().GetIndexer().Add(f)
		}
	*/

	return c, i, k8sI, clients
}

func (f *fixture) run(obj interface{}, t *testing.T) {
	f.runController(obj, true, false, t)
}

func (f *fixture) runExpectError(obj interface{}, t *testing.T) {
	f.runController(obj, true, true, t)
}

func (f *fixture) runController(obj interface{}, startInformers bool, expectError bool, t *testing.T) {
	c, i, k8sI, gh := f.newController(f.config)
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

	actions := filterInformerActions(f.client.Actions())
	f.t.Logf("actions: %#v", actions)
	for i, action := range actions {
		if len(f.actions) < i+1 {
			f.t.Errorf("%d unexpected actions: %#v", len(actions)-len(f.actions), actions[i:])
			break
		}

		expectedAction := f.actions[i]
		checkAction(expectedAction, action, f.t)
	}

	if len(f.actions) > len(actions) {
		f.t.Errorf("%d additional expected actions:%+v", len(f.actions)-len(actions), f.actions[len(actions):])
	}

	k8sActions := filterInformerActions(f.kubeclient.Actions())
	f.t.Logf("k8s actions: %#v", k8sActions)
	for i, action := range k8sActions {
		if len(f.kubeactions) < i+1 {
			f.t.Errorf("%d unexpected k8s actions: %+v", len(k8sActions)-len(f.kubeactions), k8sActions[i:])
			break
		}

		expectedAction := f.kubeactions[i]
		checkAction(expectedAction, action, f.t)
	}

	if len(f.kubeactions) > len(k8sActions) {
		f.t.Errorf("%d additional expected k8s actions:%+v", len(f.kubeactions)-len(k8sActions), f.kubeactions[len(k8sActions):])
	}

	f.t.Logf("githubStatus: %#v", gh.statusUpdates)
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

	switch a := actual.(type) {
	case k8stesting.CreateAction:
		e, _ := expected.(k8stesting.CreateAction)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintDiff(expObject, object))
		}
	case k8stesting.UpdateAction:
		e, _ := expected.(k8stesting.UpdateAction)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintDiff(expObject, object))
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

func (f *fixture) expectCreateWorkflowAction(rs *workflow.Workflow) {
	f.actions = append(f.actions,
		k8stesting.NewCreateAction(schema.GroupVersionResource{
			Resource: "workflows",
			Group:    workflow.SchemeGroupVersion.Group,
			Version:  workflow.SchemeGroupVersion.Version,
		}, rs.Namespace, rs),
	)
}

func (f *fixture) expectUpdateWorkflowsAction(rs *workflow.Workflow) {
	f.actions = append(f.actions, k8stesting.NewUpdateAction(schema.GroupVersionResource{
		Resource: "workflows",
		Group:    workflow.SchemeGroupVersion.Group,
		Version:  workflow.SchemeGroupVersion.Version,
	}, rs.Namespace, rs))
}

func getKey(obj interface{}, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		t.Errorf("Unexpected error getting key for %v: %v", obj, err)
		return ""
	}
	return key
}

func newWorkflow(str string) *workflow.Workflow {
	return workflow.MustUnmarshalWorkflow(str)
}

func TestCreateWorkflow(t *testing.T) {
	f := newFixture(t)
	f.config.deployTemplates = regexp.MustCompile("^$")
	f.config.actionTemplates = regexp.MustCompile("^$")
	f.config.productionEnvironments = regexp.MustCompile("^$")

	wf := newWorkflow(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    kube-ci.qutics.com/branch: testdeploy
    kube-ci.qutics.com/cacheScope: project
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
    wfType: ci
  name: wf
  namespace: argo
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
  message: child 'wf-522619196' failed
  phase: Pending
  nodes:
    wf:
      displayName: wf
      id: wf
      message: child 'wf-522619196' failed
      name: wf
      phase: Failed
      templateName: build
      type: Steps
    wf-522619196:
      displayName: release-staging
      id: wf-522619196
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
            key: wf/wf-522619196/main.log
        exitCode: "2"
      phase: Failed
      templateName: release
      templateScope: local/wf
      type: Pod
    wf-3533601351:
      displayName: '[1]'
      id: wf-3533601351
      message: child 'wf-522619196' failed
      name: wf[1]
      phase: Failed
      type: StepGroup
    wf-3600858922:
      displayName: '[0]'
      id: wf-3600858922
      name: wf[0]
      phase: Succeeded
      type: StepGroup
    wf-3673952239:
      displayName: release-production
      id: wf-3673952239
      message: when '"testdeploy" == master' evaluated false
      name: wf[0].release-production
      phase: Skipped
      templateName: release
      type: Skipped`)

	f.workflowsLister = append(f.workflowsLister, wf)
	f.objects = append(f.objects, wf)

	f.run(wf, t)
}
