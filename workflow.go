package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v32/github"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func wfName(org, repo, entrypoint, ref string) string {
	// We'll generate some random info so that they are
	// guaranteed unique
	c := 10
	b := make([]byte, c)
	_, _ = rand.Read(b)

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

// StatusUpdater in an interface for informing something about the an update on
// the progress of a workflow run. The crid arg shoul dbe factored out so this
// closes over amore abstract concept.
type StatusUpdater interface {
	StatusUpdate(
		ctx context.Context,
		info *githubInfo,
		status GithubStatus,
	)
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
	repo *github.Repository,
	prs []*github.PullRequest,
	sha string,
	refType string,
	ref string,
	cr *github.CheckRun,
	entrypoint string) {

	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()
	gitURL := repo.GetGitURL()
	sshURL := repo.GetSSHURL()
	httpsURL := repo.GetCloneURL()

	wf.GenerateName = ""
	wf.Name = wfName(owner, repoName, entrypoint, ref)

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
	if repo.DefaultBranch != nil {
		defaultBranch = *repo.DefaultBranch
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
			Value: workflow.AnyStringPtr(sha),
		},
		{
			Name:  "refType",
			Value: workflow.AnyStringPtr(refType),
		},
		{
			Name:  "refName",
			Value: workflow.AnyStringPtr(ref),
		},
		{
			Name:  refType,
			Value: workflow.AnyStringPtr(ref),
		},
		{
			Name:  "repoDefaultBranch",
			Value: workflow.AnyStringPtr(defaultBranch),
		},
	}...)

	prIDArg := workflow.AnyStringPtr("")
	prBaseArg := workflow.AnyStringPtr("")

	if len(prs) != 0 {
		pr := prs[0]
		prid := strconv.Itoa(pr.GetNumber())
		prIDArg = workflow.AnyStringPtr(prid)
		prBaseArg = workflow.AnyStringPtr(*pr.Base.Ref)
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
	wf.Labels[labelManagedBy] = "kube-ci"
	wf.Labels[labelOrg] = labelSafe(owner)
	wf.Labels[labelRepo] = labelSafe(repoName)
	wf.Labels[labelBranch] = labelSafe(ref)

	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}

	wf.Annotations[annCommit] = sha
	wf.Annotations[annBranch] = ref
	wf.Annotations[annRepo] = repoName
	wf.Annotations[annOrg] = owner

	wf.Annotations[annInstID] = strconv.Itoa(int(instID))

	wf.Annotations[annCheckRunName] = *cr.Name
	wf.Annotations[annCheckRunID] = strconv.Itoa(int(*cr.ID))
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
}

type wfGHClient interface {
	baseGHClient
	contentDownloader
	CreateCheckRun(ctx context.Context, opts github.CreateCheckRunOptions) (*github.CheckRun, error)
}

func checkRunError(ctx context.Context, info *githubInfo, err error, title string, upd StatusUpdater) {
	if err == nil {
		return
	}

	upd.StatusUpdate(
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

func (ws *workflowSyncer) setupEntrypoint(entrypoint string, wf *workflow.Workflow, ev *github.DeploymentEvent) error {
	if ev != nil {
		entrypoint = ev.Deployment.GetTask()
		dregex := ws.config.deployTemplates
		if dregex == nil {
			return fmt.Errorf("deployment templates are not configured")
		}
		if dregex != nil && !dregex.MatchString(entrypoint) {
			return fmt.Errorf("requested deployment action %s does not match configured deployment templates, %s", entrypoint, dregex.String())
		}
	}

	var entrypointTemplateIndex int
	var entrypointTemplate workflow.Template
	if entrypoint != "" {
		found := false
		for i, t := range wf.Spec.Templates {
			if t.Name == entrypoint {
				entrypointTemplateIndex = i
				entrypointTemplate = t
				found = true
			}
		}
		if !found {
			return fmt.Errorf("requested entrypoint %q found in workflow templates", entrypoint)
		}
		wf.Spec.Entrypoint = entrypoint
	}

	if ev == nil {
		return nil
	}

	// if this is for a deployment we need to update the environment parameter
	envParam := ws.config.EnvironmentParameter
	env := ev.Deployment.GetEnvironment()
	found := false

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

func (ws *workflowSyncer) runWorkflow(ctx context.Context, ghClient wfGHClient, repo *github.Repository, sha, refType, ref, entrypoint string, prs []*github.PullRequest, updater StatusUpdater, de *github.DeploymentEvent) error {
	org := repo.GetOwner().GetLogin()
	name := repo.GetName()
	wf, err := getWorkflow(
		ctx,
		ghClient,
		sha,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)", ws.config.CIFilePath, org, name, sha)
		return nil
	}

	if wf != nil && !runBranchOrTag(refType, wf) {
		log.Printf("not running %s/%s (%s) for reftype %s", org, name, sha, refType)
		return nil
	}

	crName := defaultCheckRunName
	if entrypoint != "" {
		wf.Spec.Entrypoint = entrypoint
		crName = defaultCheckRunName
		if de != nil {
			crName += " (deployment)"
		}
	}

	title := github.String("Workflow Setup")
	cr, crerr := ghClient.CreateCheckRun(ctx,
		github.CreateCheckRunOptions{
			Name:    crName,
			HeadSHA: sha,
			Status:  defaultCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if crerr != nil {
		log.Printf("Unable to create check run, %v", err)
		return fmt.Errorf("creating check run failed, %w", err)
	}

	info := &githubInfo{
		orgName:  org,
		repoName: name,
		instID:   ghClient.GetInstallID(),

		checkRunID:   cr.GetID(),
		checkRunName: defaultCheckRunName,
	}

	// Status: initialise CheckRun info
	updater.StatusUpdate(
		ctx,
		info,
		GithubStatus{
			Title:      *title,
			Summary:    "Creating Workflow",
			Status:     "queued",
			Conclusion: "",
		},
	)

	if err != nil {
		// Status: error to checkrun info failed
		msg := fmt.Errorf("unable to parse workflow, %v", err)
		checkRunError(ctx, info, msg, *title, updater)
		log.Printf("unable to parse workflow for %s (%s), %v", repo, ref, err)
		return nil
	}

	if err := ws.setupEntrypoint(entrypoint, wf, de); err != nil {
		log.Printf("not running %s/%s (%s), %v",
			org,
			name,
			sha,
			err,
		)
		checkRunError(ctx, info, err, *title, updater)

		return nil
	}

	if err := ws.policy(repo, ref, *title, prs); err != nil {
		// Status: error to checkrunrun info failed - policy error
		checkRunError(ctx, info, fmt.Errorf(err.message), *title, updater)
		log.Printf(err.log)
		return nil
	}

	ws.cancelRunningWorkflows(
		org,
		name,
		ref,
	)

	wf = wf.DeepCopy()
	ws.decorateWorkflow(
		wf,
		ghClient.GetInstallID(),
		repo,
		prs,
		sha,
		refType,
		ref,
		cr,
		entrypoint,
	)

	err = ws.storage.ensurePVC(
		wf,
		org,
		name,
		ref,
		ws.config.CacheDefaults,
	)
	if err != nil {
		// Status: error to checkrun info failed - pvc create error
		updater.StatusUpdate(
			ctx,
			info,
			GithubStatus{
				Title:      *title,
				Summary:    fmt.Sprintf("creation of cache volume failed, %v", err),
				Status:     "completed",
				Conclusion: "failure",
			},
		)
		checkRunError(ctx, info, fmt.Errorf("creation of cache volume failed, %v", err), *title, updater)
		return err
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(ctx, wf, metav1.CreateOptions{})
	if err != nil {
		checkRunError(ctx, info, fmt.Errorf("argo workflow creation failed, %v", err), *title, updater)

		return fmt.Errorf("workflow creation failed, %w", err)
	}

	return nil
}
