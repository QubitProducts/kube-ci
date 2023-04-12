package kubeci

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QubitProducts/kube-ci/cistarlark"
	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v45/github"
	"go.starlark.net/starlark"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/scheme"
)

// GithubEvent is a copy of the type in cistarlark and
// can probably be replaced by that one in future
type GithubEvent interface {
	GetInstallation() *github.Installation
	GetRepo() *github.Repository
	GetSender() *github.User
}

// WorkflowContext is a copy of the type in cistarlark and
// can probably be replaced by that one in future
type WorkflowContext struct {
	Repo        *github.Repository
	Entrypoint  string
	Ref         string
	RefType     string
	SHA         string
	ContextData map[string]string
	PRs         []*github.PullRequest
	Event       GithubEvent
}

func wfName(org, repo, entrypoint, ref string) string {
	// We'll generate some random info so that they are
	// guaranteed unique
	c := 10
	b := make([]byte, c)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}

	h := sha1.New()
	h.Write(b)
	// hash the rest of the job name
	fmt.Fprintf(h, "%s#%s#%s#%s", org, repo, entrypoint, ref)
	// base64 the hash so we get a few stringt dense random bits
	str := base64.RawURLEncoding.EncodeToString(h.Sum(nil))

	//  we'll grab the first few bytes for a unique name
	hashStr := "x" + str[0:8]

	// 8 chars used for hash
	maxLen := 50 - len(hashStr)
	maxLen -= 1 // for the dot

	maxRepoName := 20
	if entrypoint != "" {
		maxRepoName = 15
	}
	if len(repo) > maxRepoName {
		repo = repo[0:maxRepoName]
	}

	maxLen = maxLen - len(repo)
	maxLen -= 1 // for the dot
	parts := []string{repo}

	if entrypoint != "" {
		if len(entrypoint) > 15 {
			entrypoint = entrypoint[0:15]
		}
		maxLen = maxLen - len(entrypoint)
		maxLen -= 1 // for the dot
		parts = append(parts, entrypoint)
	}

	if len(ref) > maxLen {
		ref = ref[0:maxLen]
	}
	parts = append(parts, ref)

	parts = append(parts, hashStr)

	return labelSafe(parts...)
}

// GithubStatus is a pseudo-ugly mapping of github checkrun
// and deployment status into a common struct to make workflow
// running agnostic to the reason the workflow was launched
type GithubStatus struct {
	Status     string
	Conclusion string
	DetailsURL string
	Actions    []*github.CheckRunAction

	// output
	Title       string
	Summary     string
	Text        string
	Annotations []*github.CheckRunAnnotation
}

func (ws *workflowSyncer) cancelRunningWorkflows(org, repo, branch string) {
	ctx := context.Background()
	ls := labels.Set(
		map[string]string{
			labelOrg:  labelSafe(org),
			labelRepo: labelSafe(repo),
		})

	sel := ls.AsSelector()
	if sel.Empty() {
		log.Printf("failed clearing existing workflows, invalid labels selector, %#v", ls)
		return
	}

	// We'll cancel all in-progress checks for this
	// repo/branch
	wfs, err := ws.lister.Workflows(ws.config.Namespace).List(sel)
	if err != nil {
		log.Printf("failed clearing existing workflows, %v", err)
		return
	}

	for _, wf := range wfs {
		if wf.Annotations != nil {
			b := wf.Annotations[annBranch]
			if b != branch {
				continue
			}
		}
		wf = wf.DeepCopy()
		ads := int64(0)
		wf.Spec.ActiveDeadlineSeconds = &ads
		ws.client.ArgoprojV1alpha1().Workflows(wf.Namespace).Update(ctx, wf, metav1.UpdateOptions{})
	}
}

// decorateWorkflow, this amends the workflow from the repo with the
// details we need to track it and update the status on external
// sources.
func (ws *workflowSyncer) decorateWorkflow(
	wf *workflow.Workflow,
	instID int,
	wctx *WorkflowContext) {

	owner := wctx.Repo.GetOwner().GetLogin()
	repoName := wctx.Repo.GetName()
	gitURL := wctx.Repo.GetGitURL()
	sshURL := wctx.Repo.GetSSHURL()
	httpsURL := wctx.Repo.GetCloneURL()

	wf.GenerateName = ""
	wf.Name = wfName(owner, repoName, wctx.Entrypoint, wctx.Ref)

	if ws.config.Namespace != "" {
		wf.Namespace = ws.config.Namespace
	}

	ttl := int32((3 * 24 * time.Hour) / time.Second)
	wf.Spec.TTLStrategy = &workflow.TTLStrategy{
		SecondsAfterCompletion: &ttl,
	}

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
			p.Name == "orgnNme" ||
			p.Name == "repoName" ||
			p.Name == "repoDefaultBranch" {
			continue
		}
		parms = append(parms, p)
	}

	defaultBranch := "main"
	if wctx.Repo.DefaultBranch != nil {
		defaultBranch = *wctx.Repo.DefaultBranch
	}

	parms = append(parms, []workflow.Parameter{
		{
			Name:  "repo",
			Value: workflow.AnyStringPtr(sshURL),
		},
		{
			Name:  "repo_git_url",
			Value: workflow.AnyStringPtr(gitURL),
		},
		{
			Name:  "repo_https_url",
			Value: workflow.AnyStringPtr(httpsURL),
		},
		{
			Name:  "repoName",
			Value: workflow.AnyStringPtr(repoName),
		},
		{
			Name:  "orgName",
			Value: workflow.AnyStringPtr(owner),
		},
		{
			Name:  "revision",
			Value: workflow.AnyStringPtr(wctx.SHA),
		},
		{
			Name:  "refType",
			Value: workflow.AnyStringPtr(wctx.RefType),
		},
		{
			Name:  "refName",
			Value: workflow.AnyStringPtr(wctx.Ref),
		},
		{
			Name:  wctx.RefType,
			Value: workflow.AnyStringPtr(wctx.Ref),
		},
		{
			Name:  "repoDefaultBranch",
			Value: workflow.AnyStringPtr(defaultBranch),
		},
	}...)

	prIDArg := workflow.AnyStringPtr("")
	prBaseArg := workflow.AnyStringPtr("")

	if len(wctx.PRs) != 0 {
		pr := wctx.PRs[0]
		prid := strconv.Itoa(pr.GetNumber())
		prIDArg = workflow.AnyStringPtr(prid)
		prBaseArg = workflow.AnyStringPtr(*pr.Base.Ref)
		if wf.Annotations == nil {
			wf.Annotations = make(map[string]string)
		}
		// This sets the workflow title in the UI to be the PR title
		wf.Annotations["workflows.argoproj.io/title"] = pr.GetTitle()
		wf.Annotations[annPRURL] = pr.GetURL()
	}

	parms = append(parms, []workflow.Parameter{
		{
			Name:  "pullRequestID",
			Value: prIDArg,
		},
		{
			Name:  "pullRequestBaseBranch",
			Value: prBaseArg,
		},
	}...)

	wf.Spec.Arguments.Parameters = parms

	if wf.Labels == nil {
		wf.Labels = make(map[string]string)
	}
	wf.Labels[labelManagedBy] = ws.config.ManagedBy
	wf.Labels[labelOrg] = labelSafe(owner)
	wf.Labels[labelRepo] = labelSafe(repoName)
	wf.Labels[labelBranch] = labelSafe(wctx.Ref)

	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}

	wf.Annotations[annCommit] = wctx.SHA
	wf.Annotations[annBranch] = wctx.Ref
	wf.Annotations[annRepo] = repoName
	wf.Annotations[annOrg] = owner

	wf.Annotations[annInstID] = strconv.Itoa(int(instID))
}

type policyRejection struct {
	message, log string
}

// policy enforces the main build policy:
// - only build Draft PRs if the configured to do so.
// - only build PRs that are targeted at one of our valid base branches
func (ws *workflowSyncer) policy(
	repo *github.Repository,
	headBranch string,
	title string,
	prs []*github.PullRequest,
) *policyRejection {
	if len(prs) > 0 {
		for _, pr := range prs {
			// TODO(tcolgate): I think this is never actually the case for web events. External PRs aren't
			// included
			if *pr.Head.Repo.URL != *pr.Base.Repo.URL {
				return &policyRejection{
					message: "refusing to build non-local PR, org members can run them manually using `/kube-ci run`",
					log:     fmt.Sprintf("not running %s %s, as it for a PR from a non-local branch", repo.GetFullName(), headBranch),
				}
			}
		}

		if ws.config.buildBranches != nil {
			baseMatched := false
			for _, pr := range prs {
				if ws.config.buildBranches.MatchString(pr.GetBase().GetRef()) {
					baseMatched = true
					break
				}
			}

			if !baseMatched {
				return &policyRejection{
					message: fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
					log:     fmt.Sprintf("not running %s %s, base branch did not match %s", repo.GetFullName(), headBranch, ws.config.buildBranches.String()),
				}
			}
		}

		onlyDrafts := true
		for _, pr := range prs {
			onlyDrafts = onlyDrafts && pr.GetDraft()
		}

		if onlyDrafts && !ws.config.BuildDraftPRs {
			return &policyRejection{
				message: "auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`",
				log:     fmt.Sprintf("not running %s %s, as it is only used in draft PRs", repo.GetFullName(), headBranch),
			}
		}

		return nil
	}

	// this commit was not for a PR, so we confirm we should build for the head branch it was targettted at
	if ws.config.buildBranches != nil && !ws.config.buildBranches.MatchString(headBranch) {
		return &policyRejection{
			message: fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
			log:     fmt.Sprintf("not running %s %s, as it target unmatched base branches", repo.GetFullName(), headBranch),
		}
	}

	return nil
}

func runBranchOrTag(reftype string, wf *workflow.Workflow) bool {
	runBranch := true
	runTag := false

	for k, vstr := range wf.GetAnnotations() {
		switch k {
		case annRunBranch:
			v, err := strconv.ParseBool(vstr)
			if err != nil {
				log.Printf(`ignoring bad values %q for %s, should be "true" or "false"`, vstr, annRunBranch)
				continue
			}
			runBranch = v
		case annRunTag:
			v, err := strconv.ParseBool(vstr)
			if err != nil {
				log.Printf(`ignoring bad values %q for %s, should be "true" or "false"`, vstr, annRunBranch)
				continue
			}
			runTag = v
		}
	}

	switch reftype {
	case "branch":
		return runBranch
	case "tag":
		return runTag
	default:
		return false
	}
}

type baseGHClient interface {
	GetInstallID() int
	GetHTTPClient() *http.Client
}

type wfGHClient interface {
	baseGHClient
	contentDownloader
	CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error)
	GithubClientInterface
}

func checkRunError(ctx context.Context, info *githubInfo, err error, title string) {
	if err == nil {
		return
	}

	StatusUpdate(
		ctx,
		info,
		GithubStatus{
			Title:      title,
			Summary:    err.Error(),
			Status:     "completed",
			Conclusion: "failure",
		},
	)
}

func (ws *workflowSyncer) setupEntrypoint(entrypoint string, wf *workflow.Workflow, ev GithubEvent) error {
	if wf == nil {
		return nil
	}

	var dEv *github.DeploymentEvent
	if ev != nil {
		switch ev := ev.(type) {
		case *github.DeploymentEvent:
			dEv = ev
		}
	}

	if dEv != nil {
		entrypoint = dEv.Deployment.GetTask()
		dregex := ws.config.deployTemplates
		if dregex == nil {
			return fmt.Errorf("deployment templates are not configured")
		}
		if str := wf.Annotations[annDeployTemplates]; str != "" {
			var err error
			dregex, err = regexp.Compile(str)
			if err != nil {
				return fmt.Errorf("annotation %s contains invalid regex, %s", annDeployTemplates, err)
			}
		}
		if dregex != nil && !dregex.MatchString(entrypoint) {
			return fmt.Errorf("requested deployment action %s does not match configured deployment templates, %s", entrypoint, dregex.String())
		}
	}

	var entrypointTemplateIndex int
	var entrypointTemplate workflow.Template
	if entrypoint != "" {
		wf.Spec.Entrypoint = entrypoint
	}

	found := false
	for i, t := range wf.Spec.Templates {
		if t.Name == wf.Spec.Entrypoint {
			entrypointTemplateIndex = i
			entrypointTemplate = t
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("requested entrypoint %q not found in workflow templates", entrypoint)
	}

	if dEv == nil {
		return nil
	}

	// if this is for a deployment we need to update the environment parameter
	envParam := ws.config.EnvironmentParameter
	env := dEv.Deployment.GetEnvironment()
	found = false

	for i, p := range entrypointTemplate.Inputs.Parameters {
		if p.Name == envParam {
			found = true
			p.Value = workflow.AnyStringPtr(env)
			wf.Spec.Templates[entrypointTemplateIndex].Inputs.Parameters[i] = p
			break
		}
	}
	if !found {
		return fmt.Errorf("did not find deployment environment parameter %s in template %s", envParam, entrypointTemplate.Name)
	}

	return nil
}

func (ws *workflowSyncer) lintWorkflow(wf *workflow.Workflow) (*workflow.Workflow, []string) {
	return nil, nil
}

type workflowGetter interface {
	contentDownloader
	GetInstallID() int
	GetHTTPClient() *http.Client
}

func (ws *workflowSyncer) getRepoCIContext(
	ctx context.Context,
	cd workflowGetter,
	wctx *WorkflowContext,
) (map[string]string, error) {
	cnts, err := cd.GetContents(ctx, ws.config.CIContextPath, &github.RepositoryContentGetOptions{
		Ref: wctx.Ref,
	})
	if err != nil {
		return nil, fmt.Errorf("could not fetch repository CI context, %w", err)
	}

	res := make(map[string]string)
	for _, cnt := range cnts {
		bs, err := cnt.GetContent()
		if err != nil {
			return nil, fmt.Errorf("could not fetch repository CI context, %w", err)
		}
		res[cnt.GetName()] = bs
	}

	return res, nil
}

func (ws *workflowSyncer) getCIStarlark(
	ctx context.Context,
	wctx *WorkflowContext,
	hc *http.Client,
) (*workflow.Workflow, string, error) {
	_, ok := wctx.ContextData[ws.config.CIStarlarkFile]
	if !ok {
		return nil, "", fmt.Errorf("file %s not found in CI context, %w", ws.config.CIStarlarkFile, os.ErrNotExist)
	}

	sCtx := cistarlark.WorkflowContext{
		Repo:        wctx.Repo,
		Ref:         wctx.Ref,
		SHA:         wctx.SHA,
		Entrypoint:  wctx.Entrypoint,
		RefType:     wctx.RefType,
		ContextData: wctx.ContextData,
		PRs:         wctx.PRs,
		Event:       wctx.Event,
	}

	var output []string
	var outputM sync.Mutex

	print := func(_ *starlark.Thread, msg string) {
		outputM.Lock()
		defer outputM.Unlock()
		output = append(output, msg)
	}

	sCfg := cistarlark.Config{
		Print: print,
	}
	wf, err := cistarlark.LoadWorkflow(ctx, hc, ws.config.CIStarlarkFile, sCtx, sCfg)
	outStr := strings.Join(output, "\n")
	if err != nil {
		return nil, outStr, err
	}
	return wf, outStr, nil
}

func (ws *workflowSyncer) getCIYAML(
	ctx context.Context,
	wctx *WorkflowContext,
) (*workflow.Workflow, error) {
	var err error

	ciStr, ok := wctx.ContextData[ws.config.CIYAMLFile]
	if !ok {
		return nil, fmt.Errorf("file %s not found in CI context, %w", ws.config.CIYAMLFile, os.ErrNotExist)
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode([]byte(ciStr), nil, nil)

	if err != nil {
		return nil, fmt.Errorf("failed to decode %s, %v", ws.config.CIYAMLFile, err)
	}

	wf, ok := obj.(*workflow.Workflow)
	if !ok {
		return nil, fmt.Errorf("could not use %T as workflow", wf)
	}
	return wf, nil
}

func (ws *workflowSyncer) getWorkflow(
	ctx context.Context,
	cd workflowGetter,
	wctx *WorkflowContext,
) (wf *workflow.Workflow, debug string, err error) {
	defer func() {
		if err == nil {
			ws.decorateWorkflow(wf, cd.GetInstallID(), wctx)
			return
		}

		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("no workflow definition found in context, %w", os.ErrNotExist)
		}
		err = fmt.Errorf("failed to load workflow, %w", err)
	}()

	ciFiles := wctx.ContextData
	if ciFiles == nil {
		ciFiles, err = ws.getRepoCIContext(ctx, cd, wctx)
		if err != nil {
			return nil, "", fmt.Errorf("could not read context from repo, %w", err)
		}
		wctx.ContextData = ciFiles
	}

	wf, debug, err = ws.getCIStarlark(ctx, wctx, cd.GetHTTPClient())

	if errors.Is(err, os.ErrNotExist) {
		wf, err = ws.getCIYAML(ctx, wctx)
	}

	return
}

func (ws *workflowSyncer) runWorkflow(ctx context.Context, ghClient wfGHClient, wctx *WorkflowContext) (*workflow.Workflow, error) {
	org := wctx.Repo.GetOwner().GetLogin()
	name := wctx.Repo.GetName()
	wf, _, wfErr := ws.getWorkflow(
		ctx,
		ghClient,
		wctx,
	)

	if wfErr != nil && errors.Is(wfErr, os.ErrNotExist) {
		return nil, wfErr
	}

	if wf != nil {
		wf = wf.DeepCopy()
	}

	if wf != nil && !runBranchOrTag(wctx.RefType, wf) {
		log.Printf("not running %s/%s (%s) for reftype %s", org, name, wctx.SHA, wctx.RefType)
		return nil, nil
	}

	epErr := ws.setupEntrypoint(wctx.Entrypoint, wf, wctx.Event)
	if epErr != nil {
		log.Printf("not running %s/%s (%s), %v",
			org,
			name,
			wctx.SHA,
			epErr,
		)
		// can't return here, we need to report the error
	}

	crName := defaultCheckRunName
	if wfErr == nil && epErr == nil {
		crName = fmt.Sprintf("Workflow - %s", wf.Spec.Entrypoint)
		if wctx.Event != nil {
			switch wctx.Event.(type) {
			case *github.DeploymentEvent:
				crName += " (deployment)"
			}
		}
	}

	var externalID *string
	if wf != nil {
		externalID = github.String(wf.Spec.Entrypoint)
	}

	title := github.String("Workflow Setup")
	cr, crErr := ghClient.CreateCheckRun(ctx,
		github.CreateCheckRunOptions{
			Name:       crName,
			HeadSHA:    wctx.SHA,
			ExternalID: externalID,
			Status:     defaultCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String("Creating workflow"),
			},
		},
	)

	if crErr != nil {
		log.Printf("Unable to create check run, %v", crErr)
		return nil, fmt.Errorf("creating check run failed, %w", crErr)
	}

	if wf != nil && cr != nil {
		wf.Annotations[annCheckRunName] = *cr.Name
		wf.Annotations[annCheckRunID] = strconv.Itoa(int(*cr.ID))
	}

	info := &githubInfo{
		orgName:  org,
		repoName: name,
		instID:   ghClient.GetInstallID(),

		checkRunID:   cr.GetID(),
		checkRunName: crName,
		ghClient:     ghClient,
	}

	// Status: initialise CheckRun info
	StatusUpdate(
		ctx,
		info,
		GithubStatus{
			Title:      *title,
			Summary:    "Creating Workflow",
			Status:     "queued",
			Conclusion: "",
		},
	)

	if wfErr != nil {
		msg := fmt.Errorf("unable to parse workflow, %v", wfErr)
		checkRunError(ctx, info, msg, *title)
		log.Printf("unable to parse workflow for %s (%s), %v", wctx.Repo.GetFullName(), wctx.Ref, wfErr)
		return nil, nil
	}

	if epErr != nil {
		msg := fmt.Errorf("error finding workflow endpoint, %v", epErr)
		checkRunError(ctx, info, msg, *title)
		log.Printf("error finding workflow endpoint for %s (%s), %v", wctx.Repo, wctx.Ref, epErr)
		return nil, nil
	}

	if err := ws.policy(wctx.Repo, wctx.Ref, *title, wctx.PRs); err != nil {
		// Status: error to checkrun info failed - policy error
		checkRunError(ctx, info, fmt.Errorf(err.message), *title)
		log.Printf(err.log)
		return nil, nil
	}

	ws.cancelRunningWorkflows(
		org,
		name,
		wctx.Ref,
	)

	err := ws.storage.ensurePVC(
		wf,
		org,
		name,
		wctx.Ref,
		ws.config.CacheDefaults,
	)

	if err != nil {
		// Status: error to checkrun info failed - pvc create error
		checkRunError(ctx, info, fmt.Errorf("creation of cache volume failed, %v", err), *title)
		return nil, err
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(ctx, wf, metav1.CreateOptions{})
	if err != nil {
		checkRunError(ctx, info, fmt.Errorf("argo workflow creation failed, %v", err), *title)

		return nil, fmt.Errorf("workflow creation failed, %w", err)
	}

	return wf, nil
}
