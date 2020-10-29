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
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v32/github"
	"github.com/mattn/go-shellwords"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/labels"
)

func (ws *workflowSyncer) webhookIssueComment(ctx context.Context, event *github.IssueCommentEvent) (int, string) {
	if *event.Action != "created" {
		return http.StatusOK, ""
	}

	cmdparts := strings.SplitN(strings.TrimSpace(*event.Comment.Body), " ", 3)

	rootCmd := "/kube-ci"
	if len(cmdparts) < 1 || cmdparts[0] != rootCmd {
		return http.StatusOK, ""
	}

	org := event.GetRepo().GetOwner().Login

	ghClient, err := ws.ghClientSrc.getClient(*org, int(*event.Installation.ID))
	if err != nil {
		log.Printf("error creating github client, %v", err)
		return http.StatusBadRequest, "failed to create github client"
	}

	user := event.Comment.GetUser()
	ok, _, err := ghClient.Organizations.IsMember(ctx, *org, user.GetLogin())
	if err != nil {
		log.Printf("error querying github membership, %v", err)
		return http.StatusBadRequest, "failed to check org membership"
	}
	if !ok {
		ws.slashComment(ctx, ghClient, event, "you must be an organisation member to execute commands")
		return http.StatusOK, "ok"
	}

	if len(cmdparts) == 1 {
		ws.slashUnknown(ctx, ghClient, event)
		return http.StatusOK, ""
	}

	cmd := cmdparts[1]

	var args []string
	if len(cmdparts) > 2 {
		args, err = shellwords.Parse(cmdparts[2])
		if err != nil {
			return http.StatusOK, ""
		}
	}

	type slashCommand func(ctx context.Context, ghc *github.Client, event *github.IssueCommentEvent, args ...string) error
	handlers := map[string]slashCommand{
		"run":    ws.slashRun,
		"deploy": ws.slashDeploy,
		"setup":  ws.slashSetup,
	}

	f, ok := handlers[cmd]

	if !ok {
		ws.slashUnknown(ctx, ghClient, event, cmd)
		return http.StatusOK, ""
	}

	err = f(ctx, ghClient, event, args...)
	if err != nil {
		log.Printf("slash command failed, %v", err)
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) slashComment(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent, body string) error {
	_, _, err := ghClient.Issues.CreateComment(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		int(*event.Issue.Number),
		&github.IssueComment{
			Body: &body,
		},
	)

	if err != nil {
		return errors.Wrap(err, "failed to create comment")
	}

	return nil
}

func (ws *workflowSyncer) slashUnknown(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent, args ...string) error {
	body := `
known command:
- *run*: run the workflow onthe current branch (only valid for PRs)
- *deploy* [environment (default: "staging")]: run the deploy workflow (not implemented yet)
- *setup* TEMPLATENAME: add/replace the current workflow with the specified template
`

	keys := []string{}
	for name := range ws.config.TemplateSet {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		t := ws.config.TemplateSet[name]
		body += fmt.Sprintf("  - *%s*: %s\n", name, t.Description)
	}
	body = strings.TrimSpace(body)

	return ws.slashComment(ctx, ghClient, event, body)
}

func (ws *workflowSyncer) slashRun(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent, args ...string) error {
	owner := event.Repo.GetOwner().GetLogin()
	repo := event.Repo.GetName()
	ghClient, err := ws.ghClientSrc.getClient(owner, int(*event.Installation.ID))
	if err != nil {
		return err
	}

	prlinks := event.GetIssue().GetPullRequestLinks()
	if prlinks == nil {
		body := strings.TrimSpace(`
    run is only valid on PR comments
	`)
		return ws.slashComment(ctx, ghClient, event, body)
	}

	parts := strings.Split(prlinks.GetURL(), "/")
	pridstr := parts[len(parts)-1]
	prid, _ := strconv.Atoi(pridstr)
	pr, _, err := ghClient.PullRequests.Get(ctx, owner, repo, prid)
	if err != nil {
		return errors.Wrap(err, "failed lookup up PR")
	}
	if pr == nil {
		return errors.New("failed lookup up PR")
	}

	headsha := pr.GetHead().GetSHA()
	wf, err := ws.getWorkflow(
		ctx,
		ghClient,
		owner,
		repo,
		headsha,
		ws.config.CIFilePath,
	)

	if os.IsNotExist(err) {
		log.Printf("no %s in %s/%s (%s)",
			ws.config.CIFilePath,
			owner,
			repo,
			headsha,
		)
		body := fmt.Sprintf("kube-ci config ```%s``` not found in this branch", ws.config.CIFilePath)
		return ws.slashComment(ctx, ghClient, event, body)
	}

	title := "Workflow Setup"
	cr, _, crerr := ghClient.Checks.CreateCheckRun(ctx,
		owner,
		repo,
		github.CreateCheckRunOptions{
			Name:    checkRunName,
			Status:  initialCheckRunStatus,
			HeadSHA: headsha,
			Output: &github.CheckRunOutput{
				Title:   github.String("Workflow Setup"),
				Summary: github.String("Creating workflow"),
			},
		},
	)
	if crerr != nil {
		log.Printf("Unable to create check run, %v", err)
		return fmt.Errorf("unable to create check run, %w", err)
	}
	ghUpdateCheckRun(
		ctx,
		ghClient,
		owner,
		repo,
		*cr.ID,
		"Workflow Setup",
		"Creating workflow",
		"in_progress",
		"",
	)

	if err != nil {
		msg := fmt.Sprintf("unable to parse workflow, %v", err)
		ghUpdateCheckRun(
			ctx,
			ghClient,
			owner,
			repo,
			*cr.ID,
			title,
			msg,
			"completed",
			"failure",
		)
		return ws.slashComment(ctx, ghClient, event, msg)
	}

	headref := pr.GetHead().GetRef()
	// We'll cancel all in-progress checks for this
	// repo/branch
	wfs, err := ws.lister.Workflows(ws.config.Namespace).List(labels.Set(
		map[string]string{
			labelOrg:         labelSafe(*event.Repo.Owner.Login),
			labelRepo:        labelSafe(*event.Repo.Name),
			labelBranch:      labelSafe(headref),
			labelDetailsHash: detailsHash(owner, repo, headref),
		}).AsSelector())

	for _, wf := range wfs {
		wf = wf.DeepCopy()
		ads := int64(0)
		wf.Spec.ActiveDeadlineSeconds = &ads
		ws.client.ArgoprojV1alpha1().Workflows(wf.Namespace).Update(wf)
	}

	wf = wf.DeepCopy()
	ws.updateWorkflow(wf, &github.CheckSuiteEvent{
		Repo:         event.GetRepo(),
		Installation: event.GetInstallation(),
		CheckSuite: &github.CheckSuite{
			PullRequests: []*github.PullRequest{pr},
			HeadBranch:   pr.Head.Ref,
			HeadSHA:      pr.Head.SHA,
		},
	}, cr)

	err = ws.ensurePVC(
		wf,
		owner,
		repo,
		headref,
		ws.config.CacheDefaults,
	)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			owner,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("creation of cache volume failed, %v", err),
			"completed",
			"failure",
		)
	}

	_, err = ws.client.ArgoprojV1alpha1().Workflows(ws.config.Namespace).Create(wf)
	if err != nil {
		ghUpdateCheckRun(
			ctx,
			ghClient,
			owner,
			repo,
			*cr.ID,
			title,
			fmt.Sprintf("argo workflow creation failed, %v", err),
			"completed",
			"failure",
		)

		return fmt.Errorf("argo workflow creation failed, %w", err)
	}

	return nil
}

func (ws *workflowSyncer) slashDeploy(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent, args ...string) error {
	if len(args) > 1 {
		return ws.slashComment(ctx, ghClient, event, "please specify one environment")
	}

	env := "staging"
	if len(args) == 1 {
		env = args[0]
	}

	_, sha, err := ws.issueHead(ctx, ghClient, event)
	if err != nil {
		return errors.Wrap(err, "cannot setup ci for repository")
	}

	msg := fmt.Sprintf("deploying the thing to %v", env)
	dep, _, err := ghClient.Repositories.CreateDeployment(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		&github.DeploymentRequest{
			Ref:         &sha,
			Description: &msg,
			Environment: &env,
		},
	)

	if err != nil {
		return errors.Wrap(err, "failed to create deploy")
	}

	log.Printf("Deployment created, %v", *dep.ID)

	return nil
}

func (ws *workflowSyncer) slashSetup(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent, args ...string) error {
	if len(args) != 1 {
		ws.slashComment(ctx, ghClient, event, "you mest specify a template")
		return nil
	}

	tmpl, ok := ws.config.TemplateSet[args[0]]
	if !ok {
		ws.slashComment(ctx, ghClient, event, "unknown template "+args[0])
		return nil
	}

	branch, sha, err := ws.issueHead(ctx, ghClient, event)
	if err != nil {
		ws.slashComment(ctx, ghClient, event, "blah")
		return errors.Wrap(err, "cannot setup ci for repository")
	}

	fileName := ws.config.CIFilePath

	path := filepath.Dir(fileName)
	_, files, _, err := ghClient.Repositories.GetContents(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		path,
		&github.RepositoryContentGetOptions{
			Ref: branch,
		},
	)

	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode != http.StatusNotFound {
				return errors.Wrap(ghErr, "couldn't get directory listing")
			}
			log.Printf("couldn't get %s for %s", path, sha)
		} else {
			return errors.Wrap(err, "couldn't get directory listing")
		}
	}

	var existingSHA *string
	for _, f := range files {
		log.Printf("checking %s %s", *f.Path, *f.SHA)

		if *f.Path == fileName {
			existingSHA = f.SHA
			log.Printf("found existing %v (%v)", *f.Path, *existingSHA)
			break
		}
	}

	bs, err := ioutil.ReadFile(tmpl.CI)
	if err != nil {
		ws.slashComment(ctx, ghClient, event, fmt.Sprintf("couldn't read template file %s, ci server config is broken!", fileName))
		return nil
	}

	body := fmt.Sprintf(`kube-ci configured by %s via %s`, *event.Comment.User.Login, *event.Comment.HTMLURL)

	opts := &github.RepositoryContentFileOptions{
		Message: &body,
		Content: bs,
		Branch:  &branch,
		SHA:     existingSHA,
	}
	_, _, err = ghClient.Repositories.CreateFile(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		fileName,
		opts)
	if err != nil {
		return err
	}

	return nil
}

func (ws *workflowSyncer) issueHead(ctx context.Context, ghClient *github.Client, event *github.IssueCommentEvent) (string, string, error) {
	if event.Issue.PullRequestLinks == nil {
		branch, _, err := ghClient.Repositories.GetBranch(
			ctx,
			*event.Repo.Owner.Login,
			*event.Repo.Name,
			*event.Repo.DefaultBranch,
		)
		if err != nil {
			return "", "", err
		}
		return *event.Repo.DefaultBranch, *branch.Commit.SHA, nil
	}

	link := event.Issue.PullRequestLinks.URL
	parts := strings.Split(*link, "/")
	prid, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return "", "", errors.Wrapf(err, "couldn't parse pull-request ID from %s", *link)
	}

	pr, _, err := ghClient.PullRequests.Get(
		ctx,
		*event.Repo.Owner.Login,
		*event.Repo.Name,
		prid,
	)
	if err != nil {
		return "", "", errors.Wrap(err, "couldn't get PR")
	}

	if *pr.Head.Repo.URL != *pr.Base.Repo.URL ||
		*pr.Head.Repo.URL != *event.Repo.URL {
		return "", "", fmt.Errorf("refusing to update cross-repo PR")
	}

	return *pr.Head.Ref, *pr.Head.SHA, nil
}
