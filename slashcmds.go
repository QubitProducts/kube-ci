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

package kubeci

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-github/v45/github"
	"github.com/mattn/go-shellwords"
)

type SlashHandler struct {
	Runner         workflowRunner
	CIContextPath  string
	CIYAMLFile     string
	CIStarlarkFile string
	Templates      TemplateSet
}

func (s *SlashHandler) slashComment(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent, body string) error {
	err := ghClient.CreateIssueComment(
		ctx,
		int(*event.Issue.Number),
		&github.IssueComment{
			Body: &body,
		},
	)

	if err != nil {
		return fmt.Errorf("failed to create comment, %w", err)
	}

	return nil
}

func (s *SlashHandler) slashUnknown(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent, args ...string) error {
	body := `
known command:
- *run*: run the workflow onthe current branch (only valid for PRs)
- *deploy* [environment (default: "staging")]: run the deploy workflow (not implemented yet)
- *setup* TEMPLATENAME: add/replace the current workflow with the specified template
`

	tsHelp := strings.Split(s.Templates.Help(), "\n")
	for _, line := range tsHelp {
		body += fmt.Sprintf("  %s\n", line)
	}
	body = strings.TrimSpace(body)

	return s.slashComment(ctx, ghClient, event, body)
}

func (s *SlashHandler) slashRun(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent, args ...string) error {
	prlinks := event.GetIssue().GetPullRequestLinks()
	if prlinks == nil {
		body := strings.TrimSpace(`
    run is only valid on PR comments
	`)
		return s.slashComment(ctx, ghClient, event, body)
	}

	parts := strings.Split(prlinks.GetURL(), "/")
	pridstr := parts[len(parts)-1]
	prid, _ := strconv.Atoi(pridstr)
	pr, err := ghClient.GetPullRequest(ctx, prid)
	if err != nil {
		return fmt.Errorf("failed lookup up PR, %w", err)
	}
	if pr == nil {
		return errors.New("failed lookup up PR")
	}

	headsha := pr.GetHead().GetSHA()
	headref := pr.GetHead().GetRef()
	wctx := WorkflowContext{
		Repo:       event.Repo,
		SHA:        headsha,
		RefType:    "branch",
		Ref:        headref,
		Entrypoint: "",
		PRs:        []*github.PullRequest{pr},
		Event:      event,
	}
	_, err = s.Runner.runWorkflow(
		ctx,
		ghClient,
		&wctx,
	)
	return err
}

func (s *SlashHandler) slashDeploy(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent, args ...string) error {
	if len(args) > 1 {
		return s.slashComment(ctx, ghClient, event, "please specify one environment")
	}

	env := "staging"
	if len(args) == 1 {
		env = args[0]
	}

	_, sha, err := s.issueHead(ctx, ghClient, event)
	if err != nil {
		return fmt.Errorf("cannot setup ci for repository, %w", err)
	}

	msg := fmt.Sprintf("deploying the thing to %v", env)
	dep, err := ghClient.CreateDeployment(
		ctx,
		&github.DeploymentRequest{
			Ref:         &sha,
			Description: &msg,
			Environment: &env,
		},
	)

	if err != nil {
		return fmt.Errorf("failed to create deploy, %w", err)
	}

	log.Printf("Deployment created, %v", *dep.ID)

	return nil
}

func (s *SlashHandler) slashSetup(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent, args ...string) error {
	if len(args) != 1 {
		s.slashComment(ctx, ghClient, event, "you mest specify a template")
		return nil
	}

	tmpl, ok := s.Templates[args[0]]
	if !ok {
		s.slashComment(ctx, ghClient, event, "unknown template "+args[0])
		return nil
	}

	branch, sha, err := s.issueHead(ctx, ghClient, event)
	if err != nil {
		s.slashComment(ctx, ghClient, event, "blah")
		return fmt.Errorf("cannot setup ci for repository, %w", err)
	}

	path := s.CIContextPath
	fileName := filepath.Join(path, s.CIYAMLFile)

	files, err := ghClient.GetContents(
		ctx,
		path,
		&github.RepositoryContentGetOptions{
			Ref: branch,
		},
	)

	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok {
			if ghErr.Response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("couldn't get directory listing, %w", ghErr)
			}
			log.Printf("couldn't get %s for %s", path, sha)
		} else {
			return fmt.Errorf("couldn't get directory listing, %w", err)
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

	bs, err := os.ReadFile(tmpl.CI)
	if err != nil {
		s.slashComment(ctx, ghClient, event, fmt.Sprintf("couldn't read template file %s, ci server config is broken!", fileName))
		return nil
	}

	body := fmt.Sprintf(`kube-ci configured by %s via %s`, *event.Comment.User.Login, *event.Comment.HTMLURL)

	opts := &github.RepositoryContentFileOptions{
		Message: &body,
		Content: bs,
		Branch:  &branch,
		SHA:     existingSHA,
	}
	err = ghClient.CreateFile(
		ctx,
		fileName,
		opts)
	if err != nil {
		return err
	}

	return nil
}

func (s *SlashHandler) issueHead(ctx context.Context, ghClient GithubClientInterface, event *github.IssueCommentEvent) (string, string, error) {
	if event.Issue.PullRequestLinks == nil {
		branch, err := ghClient.GetBranch(
			ctx,
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
		return "", "", fmt.Errorf("couldn't parse pull-request ID from %s, %w", *link, err)
	}

	pr, err := ghClient.GetPullRequest(ctx, prid)
	if err != nil {
		return "", "", fmt.Errorf("couldn't get PR, %w", err)
	}

	if *pr.Head.Repo.URL != *pr.Base.Repo.URL ||
		*pr.Head.Repo.URL != *event.Repo.URL {
		return "", "", fmt.Errorf("refusing to update cross-repo PR")
	}

	return *pr.Head.Ref, *pr.Head.SHA, nil
}

func (s *SlashHandler) slashCommand(ctx context.Context, client GithubClientInterface, event *github.IssueCommentEvent) error {
	cmdparts := strings.SplitN(strings.TrimSpace(*event.Comment.Body), " ", 3)
	user := event.Comment.GetUser()

	ok, err := client.IsMember(ctx, user.GetLogin())
	if err != nil {
		log.Printf("error querying github membership, %v", err)
		return fmt.Errorf("failed to check org membership")
	}
	if !ok {
		s.slashComment(ctx, client, event, "you must be an organisation member to execute commands")
		return nil
	}

	if len(cmdparts) == 1 {
		s.slashUnknown(ctx, client, event)
		return nil
	}

	cmd := cmdparts[1]

	var args []string
	if len(cmdparts) > 2 {
		args, err = shellwords.Parse(cmdparts[2])
		if err != nil {
			return nil
		}
	}

	type slashCommand func(ctx context.Context, ghc GithubClientInterface, event *github.IssueCommentEvent, args ...string) error
	handlers := map[string]slashCommand{
		"run":    s.slashRun,
		"deploy": s.slashDeploy,
		"setup":  s.slashSetup,
	}

	f, ok := handlers[cmd]

	if !ok {
		s.slashUnknown(ctx, client, event, cmd)
		return nil
	}

	err = f(ctx, client, event, args...)
	if err != nil {
		log.Printf("slash command failed, %v", err)
		return err
	}

	return nil
}
