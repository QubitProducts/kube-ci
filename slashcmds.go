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
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v22/github"
	"github.com/mattn/go-shellwords"
	"github.com/pkg/errors"
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

	if len(cmdparts) == 1 {
		ws.slashUnknown(ctx, event)
		return http.StatusOK, ""
	}

	cmd := cmdparts[1]

	var err error
	var args []string
	if len(cmdparts) > 2 {
		args, err = shellwords.Parse(cmdparts[2])
		if err != nil {
			return http.StatusOK, ""
		}
	}

	type slashCommand func(ctx context.Context, event *github.IssueCommentEvent, args ...string) error
	handlers := map[string]slashCommand{
		"deploy": ws.slashDeploy,
		"setup":  ws.slashSetup,
	}

	f, ok := handlers[cmd]

	if !ok {
		ws.slashUnknown(ctx, event, cmd)
		return http.StatusOK, ""
	}

	err = f(ctx, event, args...)
	if err != nil {
		log.Printf("slash command failed, %v", err)
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (ws *workflowSyncer) slashComment(ctx context.Context, event *github.IssueCommentEvent, body string) error {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return errors.Wrap(err, "failed to create github client")
	}

	_, _, err = ghClient.Issues.CreateComment(
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

func (ws *workflowSyncer) slashUnknown(ctx context.Context, event *github.IssueCommentEvent, args ...string) error {
	body := strings.TrimSpace(`
Please issue a know command:
- deploy [environment]
- setup [template-name]
`)

	return ws.slashComment(ctx, event, body)
}

func (ws *workflowSyncer) slashDeploy(ctx context.Context, event *github.IssueCommentEvent, args ...string) error {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return errors.Wrap(err, "failed to create github client")
	}

	if len(args) > 1 {
		return errors.Wrap(err, "please specify one environment")
	}

	env := "staging"
	if len(args) == 1 {
		env = args[0]
	}

	_, sha, err := ws.issueHead(ctx, event)
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

func (ws *workflowSyncer) slashSetup(ctx context.Context, event *github.IssueCommentEvent, args ...string) error {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return errors.Wrap(err, "failed to create github client")
	}

	if len(args) != 1 {
		ws.slashComment(ctx, event, "blah")
		return nil
	}

	if len(args) != 1 {
		// should check the template requested exists
		ws.slashComment(ctx, event, "blah")
		return nil
	}

	branch, sha, err := ws.issueHead(ctx, event)
	if err != nil {
		ws.slashComment(ctx, event, "blah")
		return errors.Wrap(err, "cannot setup ci for repository")
	}

	fileName := ".kube-ci/myfile"

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

	body := strings.TrimSpace(`no, you set it up!!`)
	content := bytes.TrimSpace([]byte(fmt.Sprintf(`some junk %s`, time.Now())))

	opts := &github.RepositoryContentFileOptions{
		Message: &body,
		Content: content,
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

func (ws *workflowSyncer) issueHead(ctx context.Context, event *github.IssueCommentEvent) (string, string, error) {
	ghClient, err := ws.ghClientSrc.getClient(int(*event.Installation.ID))
	if err != nil {
		return "", "", errors.Wrap(err, "failed to create github client")
	}

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
