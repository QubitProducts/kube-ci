package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	workflow "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v32/github"
)

func wfName(prefix, owner, repo, branch string) string {
	timeStr := strconv.Itoa(int(time.Now().Unix()))
	if len(prefix) > 0 {
		return labelSafe(prefix, owner, repo, branch, timeStr)
	}
	return labelSafe(owner, repo, branch, timeStr)
}

// updateWorkflow, lots of these settings shoud come in from some config.
func (ws *workflowSyncer) updateWorkflow(
	wf *workflow.Workflow,
	instID int64,
	repo *github.Repository,
	prs []*github.PullRequest,
	headSHA string,
	headRefType string,
	headRefName string,
	cr *github.CheckRun) {

	owner := *repo.Owner.Login
	repoName := *repo.Name
	gitURL := repo.GetGitURL()
	sshURL := repo.GetSSHURL()
	httpsURL := repo.GetCloneURL()

	wfType := "ci"
	wf.GenerateName = ""
	wf.Name = wfName(wfType, owner, repoName, headRefName)

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
			p.Name == "orgnNme" ||
			p.Name == "repoName" {
			continue
		}
		parms = append(parms, p)
	}

	parms = append(parms, []workflow.Parameter{
		{
			Name:  "repo",
			Value: workflow.Int64OrStringPtr(sshURL),
		},
		{
			Name:  "repo_git_url",
			Value: workflow.Int64OrStringPtr(gitURL),
		},
		{
			Name:  "repo_https_url",
			Value: workflow.Int64OrStringPtr(httpsURL),
		},
		{
			Name:  "repoName",
			Value: workflow.Int64OrStringPtr(repoName),
		},
		{
			Name:  "orgName",
			Value: workflow.Int64OrStringPtr(owner),
		},
		{
			Name:  "revision",
			Value: workflow.Int64OrStringPtr(headSHA),
		},
		{
			Name:  "refType",
			Value: workflow.Int64OrStringPtr(headRefType),
		},
		{
			Name:  "refName",
			Value: workflow.Int64OrStringPtr(headRefName),
		},
		{
			Name:  headRefType,
			Value: workflow.Int64OrStringPtr(headRefName),
		},
	}...)

	prIDArg := workflow.Int64OrStringPtr("")
	prBaseArg := workflow.Int64OrStringPtr("")

	if len(prs) != 0 {
		pr := prs[0]
		prid := strconv.Itoa(pr.GetNumber())
		prIDArg = workflow.Int64OrStringPtr(prid)
		prBaseArg = workflow.Int64OrStringPtr(*pr.Base.Ref)
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
	wf.Labels[labelWFType] = wfType
	wf.Labels[labelOrg] = labelSafe(owner)
	wf.Labels[labelRepo] = labelSafe(repoName)
	wf.Labels[labelBranch] = labelSafe(headRefName)

	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}

	wf.Annotations[annCommit] = headSHA
	wf.Annotations[annBranch] = headRefName
	wf.Annotations[annRepo] = repoName
	wf.Annotations[annOrg] = owner

	wf.Annotations[annInstID] = strconv.Itoa(int(instID))

	wf.Annotations[annCheckRunName] = *cr.Name
	wf.Annotations[annCheckRunID] = strconv.Itoa(int(*cr.ID))
}

// policy enforces the main build policy:
// - only build Draft PRs if the configured to do so.
// - only build PRs that are targeted at one of our valid base branches
func (ws *workflowSyncer) policy(
	ctx context.Context,
	updater ghCheckRunUpdater,
	repo *github.Repository,
	headBranch string,
	title string,
	prs []*github.PullRequest,
	crID int64,
) bool {

	if len(prs) > 0 {
		for _, pr := range prs {
			// TODO(tcolgate): I think this is never actually the case for web events. External PRs aren't
			// included
			if *pr.Head.Repo.URL != *pr.Base.Repo.URL {
				ghUpdateCheckRun(
					ctx,
					updater,
					crID,
					title,
					"refusing to build non-local PR, org members can run them manually using `/kube-ci run`",
					"completed",
					"failure",
				)
				log.Printf("not running %s %s, as it for a PR from a non-local branch", repo.GetFullName(), headBranch)
				return false
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
				ghUpdateCheckRun(
					ctx,
					updater,
					crID,
					title,
					fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
					"completed",
					"failure",
				)

				log.Printf("not running %s %s, base branch did not match %s", repo.GetFullName(), headBranch, ws.config.buildBranches.String())

				return false
			}
		}

		onlyDrafts := true
		for _, pr := range prs {
			onlyDrafts = onlyDrafts && pr.GetDraft()
		}

		if onlyDrafts && !ws.config.BuildDraftPRs {
			ghUpdateCheckRun(
				ctx,
				updater,
				crID,
				title,
				"auto checks Draft PRs are disabled, you can run manually using `/kube-ci run`",
				"completed",
				"failure",
			)
			log.Printf("not running %s %s, as it is only used in draft PRs", repo.GetFullName(), headBranch)
			return false
		}

		return true
	}

	// this commit was not for a PR, so we confirm we should build for the head branch it was targettted at
	if ws.config.buildBranches != nil && !ws.config.buildBranches.MatchString(headBranch) {
		ghUpdateCheckRun(
			ctx,
			updater,
			crID,
			title,
			fmt.Sprintf("checks are not automatically run for base branches that do not match `%s`, you can run manually using `/kube-ci run`", ws.config.buildBranches.String()),
			"completed",
			"failure",
		)
		log.Printf("not running %s %s, as it target unmatched base branches", repo.GetFullName(), headBranch)
		return false
	}

	return true
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

type StatusUpdater interface {
	StatusUpdate(
		ctx context.Context,
		updater ghCheckRunUpdater,
		crID int64,
		title string,
		msg string,
		status string,
		conclusion string,
	)
}

func (ws *workflowSyncer) runWorkflow(ctx context.Context, ghClient *repoClient, repo *github.Repository, headsha, headreftype, headbranch string, prs []*github.PullRequest, updater StatusUpdater) error {
	org := repo.GetOwner().GetLogin()
	name := repo.GetName()
	wf, err := ws.getWorkflow(
		ctx,
		ghClient,
		repo,
		headsha,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)",
			ws.config.CIFilePath,
			org,
			name,
			headsha,
		)
		return nil
	}

	if !runBranchOrTag(headreftype, wf) {
		log.Printf("not running %s/%s (%s) for reftype %s",
			org,
			name,
			headsha,
			headreftype,
		)
		return nil
	}

	title := "Workflow Setup"
	cr, _, crerr := ghClient.Checks.CreateCheckRun(ctx,
		org,
		name,
		github.CreateCheckRunOptions{
			Name:    checkRunName,
			HeadSHA: headsha,
			Status:  initialCheckRunStatus,
			Output: &github.CheckRunOutput{
				Title:   &title,
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if crerr != nil {
		log.Printf("Unable to create check run, %v", err)
		return fmt.Errorf("creating check run failed, %w", err)
	}

	updater.StatusUpdate(
		ctx,
		ghClient,
		*cr.ID,
		title,
		"Creating Workflow",
		"queued",
		"",
	)

	if err != nil {
		msg := fmt.Sprintf("unable to parse workflow, %v", err)
		updater.StatusUpdate(
			ctx,
			ghClient,
			*cr.ID,
			title,
			msg,
			"completed",
			"failure",
		)
		log.Printf("unable to parse workflow for %s (%s), %v", repo, headbranch, err)
		return nil
	}

	if !ws.policy(ctx, ghClient, repo, headbranch, title, prs, *cr.ID) {
		return nil
	}

	ws.cancelRunningWorkflows(
		labelSafe(org),
		labelSafe(name),
		labelSafe(headbranch),
	)

	wf = wf.DeepCopy()
	ws.updateWorkflow(
		wf,
		instID,
		repo,
		prs,
		headsha,
		headreftype,
		headbranch,
		cr,
	)

	err = ws.ensurePVC(
		wf,
		org,
		name,
		headbranch,
		ws.config.CacheDefaults,
	)
	if err != nil {
		updater.StatusUpdate(
			ctx,
			ghClient,
			*cr.ID,
			title,
			fmt.Sprintf("creation of cache volume failed, %v", err),
			"completed",
			"failure",
		)
		return err
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(wf)
	if err != nil {
		updater.StatusUpdate(
			ctx,
			ghClient,
			*cr.ID,
			title,
			fmt.Sprintf("argo workflow creation failed, %v", err),
			"completed",
			"failure",
		)

		return fmt.Errorf("workflow creation failed, %w", err)
	}

	return nil
}
