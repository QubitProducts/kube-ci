// Copyright 2019 Qubit Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at //
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// controller.go: the controller (TODO: also currently syncer), updates
// a github check run to match the current state of a workflow created
// in kubernetes.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	clientset "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	informers "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	listers "github.com/argoproj/argo-workflows/v3/pkg/client/listers/workflow/v1alpha1"

	"github.com/google/go-github/v32/github"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var (
	annCommit               = "kube-ci.qutics.com/sha"
	annBranch               = "kube-ci.qutics.com/branch"
	annRepo                 = "kube-ci.qutics.com/repo"
	annOrg                  = "kube-ci.qutics.com/org"
	annInstID               = "kube-ci.qutics.com/github-install-id"
	annCheckRunName         = "kube-ci.qutics.com/check-run-name"
	annCheckRunID           = "kube-ci.qutics.com/check-run-id"
	annAnnotationsPublished = "kube-ci.qutics.com/annotations-published"
	annDeploymentIDs        = "kube-ci.qutics.com/deployment-ids"

	annCacheVolumeName             = "kube-ci.qutics.com/cacheName"
	annCacheVolumeScope            = "kube-ci.qutics.com/cacheScope"
	annCacheVolumeStorageSize      = "kube-ci.qutics.com/cacheSize"
	annCacheVolumeStorageClassName = "kube-ci.qutics.com/cacheStorageClassName"

	annRunBranch = "kube-ci.qutics.com/runForBranch"
	annRunTag    = "kube-ci.qutics.com/runForTag"

	annFeatures = "kube-ci.qutics.com/features"

	annWorkflowLint = "kube-ci.qutics.com/lint"

	labelManagedBy = "managedBy"
	labelOrg       = "org"
	labelRepo      = "repo"
	labelBranch    = "branch"
	labelScope     = "scope"
)

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

func (ts TemplateSet) Help() string {
	keys := []string{}
	for name := range ts {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	body := ""
	for _, name := range keys {
		t := ts[name]
		body += fmt.Sprintf("- *%s*: %s\n", name, t.Description)
	}
	return body
}

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

	ActionTemplates        string `yaml:"actionTemplates"`
	DeployTemplates        string `yaml:"deployTemplates"`
	ProductionEnvironments string `yaml:"productionEnvironments"`
	EnvironmentParameter   string `yaml:"environmentParameter"`

	ExtraParameters map[string]string `yaml:"extraParameters"`

	buildBranches          *regexp.Regexp
	actionTemplates        *regexp.Regexp
	deployTemplates        *regexp.Regexp
	productionEnvironments *regexp.Regexp
}

type storageManager interface {
	ensurePVC(wf *workflow.Workflow, org, repo, branch string, defaults CacheSpec) error
	deletePVC(org, repo, branch string, action string) error
}

// workflowSyncer watches argo workflows as they run and publishes updates
// udpates back to github.

type workflowSyncer struct {
	appID    int64
	ghSecret []byte

	ghClientSrc githubClientSource

	config     Config
	kubeclient kubernetes.Interface
	client     clientset.Interface
	lister     listers.WorkflowLister
	synced     cache.InformerSynced
	workqueue  workqueue.RateLimitingInterface

	storage storageManager

	argoUIBase string
}

var sanitize = regexp.MustCompile(`[^-a-z0-9]`)
var sanitizeToDNS = regexp.MustCompile(`^[-0-9.]*`)
var sanitizeToDNSSuff = regexp.MustCompile(`[-.]+$`)

func escape(str string) string {
	str = sanitize.ReplaceAllString(strings.ToLower(str), "-")
	str = sanitizeToDNS.ReplaceAllString(str, "")
	str = sanitizeToDNSSuff.ReplaceAllString(str, "")
	return str
}

func labelSafeLen(maxLen int, strs ...string) string {
	escStrs := make([]string, 0, len(strs))

	for i := 0; i < len(strs); i++ {
		str := escape(strs[i])
		if len(str) == 0 {
			continue
		}
		escStrs = append(escStrs, str)
	}

	str := strings.Join(escStrs, ".")

	if len(str) > maxLen {
		strOver := maxLen - len(str)
		str = str[strOver*-1:]
	}

	// Need to tidy up the end incase we got unlucky.
	str = sanitizeToDNSSuff.ReplaceAllString(str, "")
	return str
}

func labelSafe(strs ...string) string {
	return labelSafeLen(50, strs...)
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

	if !wf.Status.FinishedAt.IsZero() {
		return
	}

	ghInfo, err := githubInfoFromWorkflow(wf, ws.ghClientSrc)
	if err != nil {
		return
	}

	ctx := context.Background()
	// Status: Complete check run, cancelled
	status := "completed"
	conclusion := "cancelled"
	StatusUpdate(
		ctx,
		ghInfo,
		GithubStatus{
			Status:     status,
			Conclusion: conclusion,
		},
	)
}

func newWorkflowSyncer(
	kubeclient kubernetes.Interface,
	clientset clientset.Interface,
	sinf informers.SharedInformerFactory,
	storage storageManager,
	ghClientSrc githubClientSource,
	appID int64,
	ghSecret []byte,
	baseURL string,
	config Config,
) *workflowSyncer {

	informer := sinf.Argoproj().V1alpha1().Workflows()

	syncer := &workflowSyncer{
		appID:       appID,
		ghClientSrc: ghClientSrc,
		ghSecret:    ghSecret,

		kubeclient: kubeclient,
		storage:    storage,
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
		UpdateFunc: func(_, new interface{}) {
			syncer.enqueue(new)
		},
		DeleteFunc: syncer.doDelete,
	})

	return syncer
}

// handleCheckRunResetRun returns true if we should continue
func (ws *workflowSyncer) handleCheckRunResetRun(ctx context.Context, wf *workflow.Workflow) (*workflow.Workflow, bool) {
	var err error
	if v, ok := wf.Annotations[annAnnotationsPublished]; ok && v == "true" {
		switch wf.Status.Phase {
		case workflow.WorkflowPending, workflow.WorkflowRunning: // attempt create new checkrun for a resubmitted job
			wf, err = ws.resetCheckRun(ctx, wf)
			if err != nil {
				log.Printf("failed checkrun reset, %v", err)
				return nil, false
			}
		default:
			// The workflow is not yet running, ignore it
			return nil, false
		}
	}
	return wf, true
}

func (ws *workflowSyncer) nodesText(wf *workflow.Workflow) string {
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
	return text
}

func completionStatus(wf *workflow.Workflow) (string, string) {
	completed := "completed"
	inProgress := "in_progress"
	status := ""

	failure := "failure"
	success := "success"
	cancelled := "cancelled"
	var conclusion string

	switch wf.Status.Phase {
	case workflow.WorkflowPending:
		status = *defaultCheckRunStatus
	case workflow.WorkflowRunning:
		status = inProgress
	case workflow.WorkflowFailed:
		status = completed
		conclusion = failure
		if wf.Spec.ActiveDeadlineSeconds != nil && *wf.Spec.ActiveDeadlineSeconds == 0 {
			conclusion = cancelled
		}
	case workflow.WorkflowError:
		// TODO: This might want further thought, a workflow errors if something
		// went wrong that was not he fault of the workflow, the workflow may be
		// retried. so it may not be sensile to mark it as completed.
		status = completed
		conclusion = failure
	case workflow.WorkflowSucceeded:
		status = completed
		conclusion = success
	default:
		log.Printf("ignoring %s/%s, unknown node phase %q", wf.Namespace, wf.Name, wf.Status.Phase)
		return "", ""
	}
	return status, conclusion
}

func deployStatusFromPhase(p workflow.NodePhase) string {
	switch p {
	case workflow.NodeRunning:
		return "in_progress"
	case workflow.NodeSucceeded:
		return "success"
	case workflow.NodeFailed:
		return "failure"
	case workflow.NodeError:
		return "error"
	default:
		return ""
	}
}

func (ws *workflowSyncer) isProdEnvironment(env string) bool {
	if ws.config.productionEnvironments == nil {
		return false
	}
	return ws.config.productionEnvironments.MatchString(env)
}

func (ws *workflowSyncer) createOnDemandDeployment(ctx context.Context, wf *workflow.Workflow, info *githubInfo, n workflow.NodeStatus, env string) (int64, *workflow.Workflow, error) {
	desc := fmt.Sprintf("deploying %s/%s (%s) to %s", info.orgName, info.repoName, n.TemplateName, env)
	deployID := info.deploymentIDs[n.ID]

	params := wf.Spec.Arguments.Parameters
	refType := ""
	refPrefix := ""
	refName := ""
	refSHA := ""
	for _, p := range params {
		switch p.Name {
		case "refType":
			refType = p.GetValue()
			switch refType {
			case "branch":
				refPrefix = "heads"
			case "tag":
				refPrefix = "tags"
			}
		case "refName":
			refName = p.GetValue()
		case "revision":
			refSHA = p.GetValue()
		}
	}
	if refPrefix == "" || refName == "" {
		return deployID, nil, fmt.Errorf("can't create deployment, could not determine ref type or name from workflow")
	}
	ref := fmt.Sprintf("%s/%s", refPrefix, refName)

	if deployID == 0 {
		// we need to create a new deployment and record it here
		requiredContexts := []string{}
		opts := &github.DeploymentRequest{
			Environment:           github.String(env),
			Task:                  github.String(n.TemplateName),
			ProductionEnvironment: github.Bool(ws.isProdEnvironment(env)),
			Ref:                   github.String(ref),
			Payload: DeploymentPayload{
				KubeCI: KubeCIPayload{
					Run:     false, // don't need to run a workflow, we already have one
					RefType: refType,
					RefName: refName,
					SHA:     refSHA,
				},
			},
			Description:          github.String(desc),
			TransientEnvironment: nil,                // TODO(tcm): support transient environments
			AutoMerge:            github.Bool(false), // TODO(tcm): support auto-merge
			RequiredContexts:     &requiredContexts,  // TODO(tcm): at the moment this trips up because the check we have created has finished
		}
		dep, err := info.ghClient.CreateDeployment(ctx, opts)
		if err != nil {
			return 0, nil, fmt.Errorf("could not create on-demand deployment, %w", err)
		}
		deployID = dep.GetID()
		info.deploymentIDs[n.ID] = deployID

		deploymentIDsJSON, err := json.Marshal(info.deploymentIDs)
		if err == nil {
			wf, err = ws.updateWorkflow(ctx, wf, func(wf *workflow.Workflow) {
				if wf.Annotations == nil {
					wf.Annotations = map[string]string{}
				}
				wf.Annotations[annDeploymentIDs] = string(deploymentIDsJSON)
			})
		}
		if err != nil {
			// TODO(tcm): We should probably scrap the deployment we just created
			return 0, nil, fmt.Errorf("could not update with on-demand deployment, %w", err)
		}
	}

	return deployID, wf, nil
}

func (ws *workflowSyncer) getDeployNodeEnv(n workflow.NodeStatus) string {
	env := ""
	if n.Inputs != nil {
		for _, p := range n.Inputs.Parameters {
			if p.Name == ws.config.EnvironmentParameter && p.Value != nil {
				env = p.Value.String()
				break
			}
		}
	}
	return env
}

func (ws *workflowSyncer) syncDeployments(ctx context.Context, wf *workflow.Workflow, info *githubInfo) (*workflow.Workflow, error) {
	for _, n := range wf.Status.Nodes {
		if n.TemplateName == "" || !ws.config.deployTemplates.MatchString(n.TemplateName) {
			continue
		}

		state := deployStatusFromPhase(n.Phase)
		if state == "" {
			// not a phase we care about
			continue
		}

		env := ws.getDeployNodeEnv(n)
		if env == "" {
			// TODO(tcm): need to report to the user that the deploy failed as we
			// couldn't work out which env was being deployed to
			log.Printf("couldn't determine env for %s/%s", wf.Namespace, wf.Name)
			continue
		}

		deployID := info.deploymentIDs[n.ID]
		if deployID == 0 {
			id, _, err := ws.createOnDemandDeployment(ctx, wf, info, n, env)
			if err != nil {
				// TODO(tcm): need to report to the user that the deploy failed as we
				// couldn't work out which env was being deployed to
				log.Printf("couldn't create deployment for %s/%s,%v", wf.Namespace, wf.Name, err)
				continue
			}
			deployID = id
		}

		desc := fmt.Sprintf("deploying %s/%s (%s) to %s: %s", info.orgName, info.repoName, n.TemplateName, env, state)
		opts := &github.DeploymentStatusRequest{
			State:        github.String(state),
			LogURL:       github.String(ws.nodeURL(wf, n)),
			Description:  github.String(desc),
			Environment:  github.String(env),
			AutoInactive: github.Bool(true), // TODO(tcm): we should probably make auto-inactive controllable
		}

		_, err := info.ghClient.CreateDeploymentStatus(ctx, deployID, opts)
		if err != nil {
			return nil, fmt.Errorf("couldn't set workflow status, %w", err)
		}
	}
	return wf, nil
}

func (ws *workflowSyncer) wfURL(wf *workflow.Workflow) string {
	return fmt.Sprintf(
		"%s/workflows/%s/%s",
		ws.argoUIBase,
		wf.Namespace,
		wf.Name)
}
func (ws *workflowSyncer) nodeURL(wf *workflow.Workflow, n workflow.NodeStatus) string {
	base := ws.wfURL(wf)
	u, _ := url.Parse(base)
	vs := u.Query()
	vs.Set("tab", "workflow")
	vs.Set("nodeId", n.ID)
	vs.Set("sidePanel", fmt.Sprintf("logs:%s:main", n.ID))
	u.RawQuery = vs.Encode()
	return u.String()
}

func (ws *workflowSyncer) sync(wf *workflow.Workflow) (*workflow.Workflow, error) {
	var err error
	ctx := context.Background()

	// we may modify this, so we'll just assume we will
	wf = wf.DeepCopy()

	log.Printf("got workflow phase: %v/%v %v", wf.Namespace, wf.Name, wf.Status.Phase)

	wf, cont := ws.handleCheckRunResetRun(ctx, wf)
	if !cont {
		return wf, nil
	}

	info, err := githubInfoFromWorkflow(wf, ws.ghClientSrc)
	if err != nil {
		log.Printf("ignoring %s/%s, %v", wf.Namespace, wf.Name, err)
		return wf, nil
	}

	wf, err = ws.syncDeployments(ctx, wf, info)
	if err != nil {
		return wf, err
	}

	status, conclusion := completionStatus(wf)
	if status == "" {
		return wf, nil
	}

	summary := wf.Status.Message

	title := fmt.Sprintf("Workflow Run (%s/%s))", wf.Namespace, wf.Name)

	text := ws.nodesText(wf)

	ghStatus := GithubStatus{
		Title:      title,
		Summary:    summary,
		Status:     status,
		Conclusion: conclusion,
		DetailsURL: ws.wfURL(wf),
		Text:       text,
	}
	// Status: Progress Update
	StatusUpdate(
		ctx,
		info,
		ghStatus,
	)

	if status == "completed" {
		_, err = ws.completeCheckRun(ctx, wf, info, ghStatus)
	}

	return wf, err
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

	_, err = ws.sync(wf)
	if err != nil {
		log.Printf("processing event for %s/%s failed, %v", wf.Namespace, wf.Name, err)
		runtime.HandleError(err)
		return true
	}

	ws.workqueue.Forget(obj)

	return true
}

func (ws *workflowSyncer) runWorker() {
	for ws.process() {
	}

	log.Print("worker stopped")
}

type githubInfo struct {
	orgName  string
	repoName string
	instID   int

	headSHA    string
	headBranch string

	checkRunName string
	checkRunID   int64

	deploymentIDs map[string]int64

	ghClient ghClientInterface
}

type deployIDs map[string]int64

func githubInfoFromWorkflow(wf *workflow.Workflow, ghClientSrc githubClientSource) (*githubInfo, error) {
	var err error

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

	deploymentIDs := deployIDs{}
	deploymentIDsJSON := wf.Annotations[annDeploymentIDs]
	json.Unmarshal([]byte(deploymentIDsJSON), &deploymentIDs)

	ghClient, err := ghClientSrc.getClient(orgName, int(instID), repoName)
	if err != nil {
		return nil, fmt.Errorf("could not get github client, %w", err)
	}

	return &githubInfo{
		headSHA:       headSHA,
		instID:        instID,
		headBranch:    headBranch,
		orgName:       orgName,
		repoName:      repoName,
		checkRunName:  checkRunName,
		checkRunID:    int64(checkRunID),
		deploymentIDs: deploymentIDs,
		ghClient:      ghClient,
	}, nil
}

func (ws *workflowSyncer) resetCheckRun(ctx context.Context, wf *workflow.Workflow) (*workflow.Workflow, error) {
	ghInfo, err := githubInfoFromWorkflow(wf, ws.ghClientSrc)
	if err != nil {
		return nil, fmt.Errorf("no check-run info found in restarted workflow (%s/%s)", wf.Namespace, wf.Name)
	}

	newCR, err := ghInfo.ghClient.CreateCheckRun(context.Background(),
		github.CreateCheckRunOptions{
			Name:    ghInfo.checkRunName,
			HeadSHA: ghInfo.headSHA,
			Status:  defaultCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed creating new check run, %w", err)
	}

	ghInfo.checkRunID = newCR.GetID()

	for k := range wf.Annotations {
		switch k {
		case annAnnotationsPublished:
			wf.Annotations[k] = "false"
		case annCheckRunName:
			wf.Annotations[k] = newCR.GetName()
		case annCheckRunID:
			wf.Annotations[k] = strconv.Itoa(int(newCR.GetID()))
		case annDeploymentIDs:
			wf.Annotations[k] = `{}`
		}
	}

	return ws.client.ArgoprojV1alpha1().Workflows(wf.GetNamespace()).Update(ctx, wf, metav1.UpdateOptions{})
}

func getWFVolumeScope(wf *workflow.Workflow) string {
	if wf.Annotations == nil {
		return scopeNone
	}
	switch wf.Annotations[annCacheVolumeScope] {
	case scopeBranch:
		return scopeBranch
	case scopeProject:
		return scopeProject
	default:
		return scopeNone
	}
}

var ()

func usesCacheVolume(wf *workflow.Workflow) bool {
	if wf.Annotations == nil {
		return false
	}
	scope := wf.Annotations[annCacheVolumeScope]
	if scope == scopeNone || scope == "" {
		return false
	}
	return true
}

func clearCacheAction(wf *workflow.Workflow) *github.CheckRunAction {
	if wf.Status.Phase == workflow.WorkflowFailed ||
		wf.Status.Phase == workflow.WorkflowError {
		return nil
	}

	if !usesCacheVolume(wf) {
		return nil
	}

	clearCacheAction := "clearCache"
	if getWFVolumeScope(wf) == scopeBranch {
		clearCacheAction = "clearCacheBranch"
	}
	return &github.CheckRunAction{
		Label:       "Clear Cache",
		Description: "delete the cache volume for this build",
		Identifier:  clearCacheAction,
	}
}

func (ws *workflowSyncer) buttonsForWorkflow(wf *workflow.Workflow) ([]*github.CheckRunAction, []string) {
	var warnings []string
	var actions []*github.CheckRunAction
	if action := clearCacheAction(wf); action != nil {
		actions = append(actions, action)
	}

	return actions, warnings
}

func (ws *workflowSyncer) nextCheckRunsForWorkflow(ctx context.Context, wf *workflow.Workflow, ghInfo *githubInfo) []string {
	var warnings []string

	var nextTasks []string
	for _, t := range wf.Spec.Templates {
		if t.Name != "" && ws.config.actionTemplates.MatchString(t.Name) {
			if len(t.Name) > 20 {
				warnings = append(warnings, fmt.Sprintf("Action button %s dropped, only 20 chars permitted", t.Name))
				continue
			}
			nextTasks = append(nextTasks, t.Name)
		}
	}

	// if we are already one of the valid next tasks, don't create next check runs.
	for _, task := range nextTasks {
		if task == wf.Spec.Entrypoint {
			return nil
		}
	}

	for _, task := range nextTasks {
		ghInfo.ghClient.CreateCheckRun(ctx, github.CreateCheckRunOptions{
			Name:       fmt.Sprintf("Workflow - %s", task),
			HeadSHA:    ghInfo.headSHA,
			Conclusion: github.String("action_required"),
			ExternalID: github.String(task),
			Actions: []*github.CheckRunAction{{
				Label:       "Run",
				Description: "run this workflow template manually",
				Identifier:  "run",
			}},
			Output: &github.CheckRunOutput{
				Summary: github.String("Use the button above to manually trigger this workflow template"),
			},
		})
	}

	return warnings
}

func (ws *workflowSyncer) ghCompleteCheckRun(ctx context.Context, wf *workflow.Workflow, ghInfo *githubInfo, ghStatus GithubStatus) error {
	var err error
	var warnings []string

	allAnns := ghStatus.Annotations
	ghStatus.Actions, warnings = ws.buttonsForWorkflow(wf)

	if wf.Status.Phase == workflow.WorkflowSucceeded {
		warnings = append(warnings, ws.nextCheckRunsForWorkflow(ctx, wf, ghInfo)...)
	}

	output := ghStatus.Text
	for _, w := range warnings {
		output = fmt.Sprintf("**WARNING:** %s\n%s", w, output)
	}
	ghStatus.Text = output

	ghStatus.Annotations = nil
	// Status: Add actions
	StatusUpdate(
		ctx,
		ghInfo,
		ghStatus,
	)
	if err != nil {
		return fmt.Errorf("error, failed updating check run status, %w", err)
	}

	batchSize := 50 // github API allows 50 at a time
	for i := 0; i < len(allAnns); i += batchSize {
		start := i
		end := i + batchSize
		if end > len(allAnns) {
			end = len(allAnns)
		}
		// Status: Add annotations
		anns := allAnns[start:end]
		ghStatus.Annotations = anns
		StatusUpdate(
			ctx,
			ghInfo,
			ghStatus,
		)
	}
	return nil
}

// completeCheckRun is used to publish any annotations found in the logs from a check run.
// There are a bunch of reasons this could fail.
func (ws *workflowSyncer) completeCheckRun(ctx context.Context, wf *workflow.Workflow, ghInfo *githubInfo, ghStatus GithubStatus) (*workflow.Workflow, error) {
	if wf.Status.Phase != workflow.WorkflowFailed &&
		wf.Status.Phase != workflow.WorkflowSucceeded {
		return wf, nil
	}

	var allAnns []*github.CheckRunAnnotation
	containerName := "main"
	for _, n := range wf.Status.Nodes {
		if n.Type != "Pod" {
			continue
		}
		logr, err := getPodLogs(ctx, ws.kubeclient, n.ID, wf.Namespace, containerName)
		if err != nil {
			log.Printf("getting pod logs failed, %v", err)
			continue
		}
		anns, err := parseAnnotations(logr, "")
		if err != nil {
			log.Printf("parsing annotations for %s/%s (%s) failed, %v", wf.Namespace, wf.Name, containerName, err)
			continue
		}
		allAnns = append(allAnns, anns...)
	}

	ghStatus.Annotations = allAnns
	err := ws.ghCompleteCheckRun(ctx, wf, ghInfo, ghStatus)
	if err != nil {
		return nil, fmt.Errorf("completing github check-run failed, %w", err)
	}

	wf, err = ws.updateWorkflow(ctx, wf, func(wf *workflow.Workflow) {
		if wf.Annotations == nil {
			wf.Annotations = map[string]string{}
		}
		wf.Annotations[annAnnotationsPublished] = "true"
	})

	if err != nil {
		return nil, fmt.Errorf("adding annotations-published annotation failed, %w", err)
	}

	return wf, nil
}

func (ws *workflowSyncer) updateWorkflow(ctx context.Context, wf *workflow.Workflow, mutate func(wf *workflow.Workflow)) (*workflow.Workflow, error) {
	// We need to update the API object so that we know we've published the
	// logs, we'll grab the latest one in case it has changed since we got here.
	newwf, err := ws.client.ArgoprojV1alpha1().Workflows(wf.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting workflow %s/%s update failed, %w", wf.Namespace, wf.Name, err)
	}

	upwf := newwf.DeepCopy()
	mutate(upwf)

	upwf, err = ws.client.ArgoprojV1alpha1().Workflows(upwf.Namespace).Update(ctx, upwf, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("workflow %s/%s update failed, %w", upwf.Namespace, upwf.Name, err)
	}
	return upwf, nil
}

func (ws *workflowSyncer) Run(stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer ws.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, ws.synced); !ok {
		return fmt.Errorf("caches did not sync")
	}

	go wait.Until(ws.runWorker, time.Second, stopCh)

	log.Print("Started workers")
	<-stopCh
	log.Print("Shutting down workers")

	return nil
}
