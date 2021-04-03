package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v32/github"
)

type workflowRunner interface {
	runWorkflow(ctx context.Context, ghClient *repoClient, repo *github.Repository, headsha, headreftype, headbranch, entrypoint string, prs []*github.PullRequest, updater StatusUpdater) error
}

type pvcManager interface {
	deletePVC(org, repo, branch, action string) error
}

type slashRunner interface {
	slashCommand(ctx context.Context, client *repoClient, event *github.IssueCommentEvent) error
}

type hookHandler struct {
	pvcs    pvcManager
	clients githubClientSource
	runner  workflowRunner
	slash   slashRunner

	uiBase   string
	ghSecret []byte
	appID    int64
}

func (h *hookHandler) webhookIssueComment(ctx context.Context, event *github.IssueCommentEvent) (int, string) {
	if *event.Action != "created" {
		return http.StatusOK, ""
	}

	cmdparts := strings.SplitN(strings.TrimSpace(*event.Comment.Body), " ", 3)

	rootCmd := "/kube-ci"
	if len(cmdparts) < 1 || cmdparts[0] != rootCmd {
		return http.StatusOK, ""
	}

	org := event.GetRepo().GetOwner().Login
	repo := event.GetRepo().GetName()

	client, err := h.clients.getClient(*org, int(*event.Installation.ID), repo)
	if err != nil {
		log.Printf("error creating github client, %v", err)
		return http.StatusBadRequest, "failed to create github client"
	}

	err = h.slash.slashCommand(ctx, client, event)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, "ok"
}

func (h *hookHandler) webhookDeleteBranchEvent(ctx context.Context, event *github.DeleteEvent) (int, string) {
	org := *event.GetRepo().GetOwner().Login
	repo := *event.Repo.Name
	branch := event.GetRef()
	err := h.pvcs.deletePVC(
		org,
		repo,
		branch,
		"deleted branch "+branch,
	)
	if err != nil {
		log.Printf("failed to delete pvcs for delete branch %s in %s/%s, %v", org, repo, branch, err)
	}
	return http.StatusOK, "OK"
}

func (h *hookHandler) webhookRepositoryDeleteEvent(ctx context.Context, event *github.RepositoryEvent) (int, string) {
	org := *event.GetRepo().GetOwner().Login
	repo := *event.Repo.Name
	err := h.pvcs.deletePVC(
		org,
		repo,
		"",
		event.GetAction()+" repository",
	)

	if err != nil {
		log.Printf("failed to delete pvcs for %s repo %s/%s, %v", event.GetAction(), org, repo, err)
	}

	return http.StatusOK, "OK"
}

func (h *hookHandler) webhookDeployment(ctx context.Context, event *github.DeploymentEvent) (int, string) {
	org := event.GetRepo().GetOwner().Login
	repo := event.GetRepo().GetName()

	ghClient, err := h.clients.getClient(*org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, "failed to create github client"
	}

	user := event.Deployment.Creator.Name
	ok, err := ghClient.IsMember(ctx, *user)
	if err != nil {
		return http.StatusBadRequest, "failed to check org membership"
	}

	if !ok {
		return http.StatusBadRequest, "deployment user not from our orgs"
	}

	logURL := fmt.Sprintf(
		"%s/workflows/%s/%s",
		h.uiBase,
		"blah",
		"blah")

	pending := "pending"
	_, err = ghClient.CreateDeploymentStatus(
		ctx,
		*event.Deployment.ID,
		&github.DeploymentStatusRequest{
			State:  &pending,
			LogURL: &logURL,
		},
	)

	if err != nil {
		log.Printf("create deployment state failed, %v", err)
		return http.StatusInternalServerError, ""
	}

	go func() {
		time.Sleep(10 * time.Second)
		success := "success"
		_, err := ghClient.CreateDeploymentStatus(
			context.Background(),
			*event.Deployment.ID,
			&github.DeploymentStatusRequest{
				State:  &success,
				LogURL: &logURL,
			},
		)

		if err != nil {
			log.Printf("create deployment state failed, %v", err)
		}
	}()

	return http.StatusOK, ""
}

func (h *hookHandler) webhookDeploymentStatus(ctx context.Context, event *github.DeploymentStatusEvent) (int, string) {
	log.Printf("status: %v is %v", *event.DeploymentStatus.ID, *event.DeploymentStatus.State)
	return http.StatusOK, ""
}

func (h *hookHandler) webhookCreateTag(ctx context.Context, event *github.CreateEvent) (int, string) {
	owner := event.Repo.Owner.GetLogin()
	repo := event.Repo.GetName()
	ghClient, err := h.clients.getClient(owner, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	ref, err := ghClient.GetRef(
		ctx,
		"tags/"+event.GetRef(),
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	headSHA := ref.Object.GetSHA()

	err = h.runner.runWorkflow(
		ctx,
		ghClient,
		event.Repo,
		headSHA,
		"tag",
		event.GetRef(),
		"",
		nil,
		ghClient,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (h *hookHandler) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	org := *event.Org.Login
	repo := event.Repo.GetName()
	ghClient, err := h.clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	err = h.runner.runWorkflow(
		ctx,
		ghClient,
		event.Repo,
		*event.CheckSuite.HeadSHA,
		"branch",
		*event.CheckSuite.HeadBranch,
		"",
		event.CheckSuite.PullRequests,
		ghClient,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (h *hookHandler) webhookCheckRunRequestAction(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	repo := *event.Repo.Name
	org := event.Repo.Owner.GetName()
	ghClient, err := h.clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	/*
			// All is good (return an error to fail)
			ciFile := ".kube-ci/deploy.yaml"

			wf, err := ws.getWorkflow(
				ctx,
				ghClient,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
				ciFile,
			)

		if os.IsNotExist(err) {
			log.Printf("no %s in %s/%s (%s)",
				ciFile,
				*event.Org.Login,
				*event.Repo.Name,
				*event.CheckRun.HeadSHA,
			)
			return http.StatusOK, ""
		}
	*/

	env := "staging"
	msg := fmt.Sprintf("deploying the thing to %v", env)
	dep, err := ghClient.CreateDeployment(
		ctx,
		&github.DeploymentRequest{
			Ref:         event.CheckRun.HeadSHA,
			Description: &msg,
			Environment: &env,
		},
	)

	if err != nil {
		log.Printf("create deployment ailed, %v", err)
		return http.StatusInternalServerError, ""
	}

	log.Printf("Deployment created, %v", *dep.ID)

	return http.StatusOK, "blah"
}

func (h *hookHandler) loggingWebhook(w http.ResponseWriter, r *http.Request) (int, string) {
	status, msg := h.webhook(w, r)
	if status != 0 && status != http.StatusOK {
		log.Printf("error returned from webhook, %d: %s", status, msg)
	}
	return status, msg
}

func (h *hookHandler) webhook(w http.ResponseWriter, r *http.Request) (int, string) {
	payload, err := github.ValidatePayload(r, h.ghSecret)
	if err != nil {
		return http.StatusBadRequest, "request did not validate"
	}

	eventType := github.WebHookType(r)
	rawEvent, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return http.StatusBadRequest, "could not parse request"
	}

	type repoGetter interface {
		GetRepo() *github.Repository
	}
	type pushRepoGetter interface {
		GetRepo() *github.PushEventRepository
	}

	switch rev := rawEvent.(type) {
	case repoGetter:
		r := rev.GetRepo()
		log.Printf("webhook event of type %s for %s/%s", eventType, r.GetOwner().GetLogin(), r.GetName())
	case pushRepoGetter:
		r := rev.GetRepo()
		log.Printf("webhook event of type %s for %s/%s", eventType, r.GetOwner().GetLogin(), r.GetName())
	default:
		log.Printf("webhook event of type %s", eventType)
	}

	ctx := r.Context()

	switch event := rawEvent.(type) {

	case *github.CheckSuiteEvent:
		if event.GetCheckSuite().GetApp().GetID() != h.appID {
			return http.StatusOK, "ignoring, wrong appID"
		}
		// TODO: HeadBranch is not set for all events, need to understand why
		// log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "requested", "rerequested":
			return h.webhookCheckSuite(ctx, event)
		case "completed":
			return http.StatusOK, "OK"
		default:
			log.Printf("unknown checksuite action %q ignored", *event.Action)
			return http.StatusOK, "unknown checksuite action ignored"
		}

	case *github.CreateEvent:
		switch event.GetRefType() {
		case "tag":
			return h.webhookCreateTag(ctx, event)
		default:
			return http.StatusOK, "OK"
		}

	case *github.CheckRunEvent:
		if event.GetCheckRun().GetCheckSuite().GetApp().GetID() != h.appID {
			return http.StatusOK, "ignoring, wrong appID"
		}
		// TODO: HeadBranch is not set for all events, need to understand why
		//log.Printf("%s event (%s) for %s(%s), by %s", eventType, *event.Action, *event.Repo.FullName, *event.CheckRun.CheckSuite.HeadBranch, event.Sender.GetLogin())
		switch *event.Action {
		case "rerequested":
			ev := &github.CheckSuiteEvent{
				Org:          event.Org,
				Repo:         event.Repo,
				CheckSuite:   event.CheckRun.GetCheckSuite(),
				Installation: event.Installation,
				Action:       event.Action,
			}
			return h.webhookCheckSuite(ctx, ev)
		case "requested_action":
			return h.webhookCheckRunRequestAction(ctx, event)
		case "created", "completed":
			return http.StatusOK, "OK"
		default:
			log.Printf("unknown checkrun action %q ignored", *event.Action)
			return http.StatusOK, "unknown checkrun action ignored"
		}

	case *github.DeploymentEvent:
		return h.webhookDeployment(ctx, event)

	case *github.DeploymentStatusEvent:
		return h.webhookDeploymentStatus(ctx, event)

	case *github.IssueCommentEvent:
		return h.webhookIssueComment(ctx, event)

	case *github.DeleteEvent:
		if event.GetRefType() != "branch" {
			return http.StatusOK, fmt.Sprintf("ignore %s delete event", event.GetRefType())
		}
		return h.webhookDeleteBranchEvent(ctx, event)

	case *github.RepositoryEvent:
		switch event.GetAction() {
		case "archived", "deleted":
			return h.webhookRepositoryDeleteEvent(ctx, event)
		default:
			return http.StatusOK, fmt.Sprintf("ignore repo %s event", event.GetAction())
		}

	default:
		return http.StatusOK, fmt.Sprintf("unknown event type %T", event)
	}
}
