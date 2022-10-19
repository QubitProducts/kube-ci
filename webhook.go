package kubeci

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-github/v45/github"
)

type workflowRunner interface {
	runWorkflow(ctx context.Context, ghClient wfGHClient, wctx *WorkflowContext) (*workflow.Workflow, error)
}

type pvcManager interface {
	deletePVC(org, repo, branch, action string) error
}

type slashRunner interface {
	slashCommand(ctx context.Context, client GithubClientInterface, event *github.IssueCommentEvent) error
}

type HookHandler struct {
	Storage pvcManager
	Clients githubClientSource
	Runner  workflowRunner
	Slash   slashRunner

	UIBase       string
	GitHubSecret []byte
	AppID        int64
}

func (h *HookHandler) webhookIssueComment(ctx context.Context, event *github.IssueCommentEvent) (int, string) {
	if *event.Action != "created" {
		return http.StatusOK, ""
	}

	cmdparts := strings.SplitN(strings.TrimSpace(*event.Comment.Body), " ", 3)

	rootCmd := "/kube-ci"
	if len(cmdparts) < 1 || cmdparts[0] != rootCmd {
		return http.StatusOK, ""
	}

	org := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()

	client, err := h.Clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		log.Printf("error creating github client, %v", err)
		return http.StatusBadRequest, "failed to create github client"
	}

	err = h.Slash.slashCommand(ctx, client, event)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, "ok"
}

func (h *HookHandler) webhookDeleteBranchEvent(ctx context.Context, event *github.DeleteEvent) (int, string) {
	org := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()
	branch := event.GetRef()
	err := h.Storage.deletePVC(
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

func (h *HookHandler) webhookRepositoryDeleteEvent(ctx context.Context, event *github.RepositoryEvent) (int, string) {
	org := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()
	err := h.Storage.deletePVC(
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

// DeploymentPayload will do something useful eventually
type KubeCIPayload struct {
	Run              bool `json:"run"`               // should we run a workflow
	CreateDeployment bool `json:"create_deployment"` // if true, don't create a deployment

	// RefType and RefName avoid going to the github API if we already know what type of
	// ref the user requested
	RefType string `json:"ref_type"`
	RefName string `json:"ref_name"`
	SHA     string `json:"sha"`
}

type DeploymentPayload struct {
	KubeCI KubeCIPayload `json:"kube_ci"`
}

func (h *HookHandler) webhookDeployment(ctx context.Context, event *github.DeploymentEvent) (int, string) {
	org := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()

	ghClient, err := h.Clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, "failed to create github client"
	}

	payload := DeploymentPayload{}
	// don't care about an error here.
	_ = json.Unmarshal(event.Deployment.Payload, &payload)

	if !payload.KubeCI.Run {
		log.Printf("ignoring deployment event for %s/%s, CI run not requested", org, repo)
		return http.StatusOK, "Ignored, set kube-ci.run to launch a CI task"
	}

	refType := payload.KubeCI.RefType
	switch refType {
	case "branch", "tag":
	case "":
		refType = "branch"
	default:
		return http.StatusBadRequest, "ignored, ref_type must be branch or tag"
	}

	wctx := WorkflowContext{
		Repo:        event.GetRepo(),
		SHA:         event.GetDeployment().GetSHA(),
		Ref:         event.GetDeployment().GetRef(),
		RefType:     refType, // TODO - the ref could be a tag or a branch
		Entrypoint:  event.GetDeployment().GetTask(),
		PRs:         nil,
		DeployEvent: event,
	}

	// Run a workflow to perform the deploy
	_, err = h.Runner.runWorkflow(ctx,
		ghClient,
		&wctx,
	)
	if err != nil {
		return http.StatusBadRequest, fmt.Sprintf("error when running workflow, %v", err)
	}

	return http.StatusOK, ""
}

func (h *HookHandler) webhookDeploymentStatus(ctx context.Context, event *github.DeploymentStatusEvent) (int, string) {
	log.Printf("deploy status: %v is %v", *event.DeploymentStatus.ID, *event.DeploymentStatus.State)
	return http.StatusOK, ""
}

func (h *HookHandler) webhookCreateTag(ctx context.Context, event *github.CreateEvent) (int, string) {
	owner := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()
	ghClient, err := h.Clients.getClient(owner, int(*event.Installation.ID), repo)
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

	wctx := WorkflowContext{
		Repo:        event.GetRepo(),
		SHA:         headSHA,
		Ref:         event.GetRef(),
		RefType:     "tag",
		Entrypoint:  "",
		PRs:         nil,
		DeployEvent: nil,
	}

	_, err = h.Runner.runWorkflow(
		ctx,
		ghClient,
		&wctx,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (h *HookHandler) webhookCheckSuite(ctx context.Context, event *github.CheckSuiteEvent) (int, string) {
	org := event.GetOrg().GetLogin()
	repo := event.GetRepo().GetName()
	ghClient, err := h.Clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	wctx := WorkflowContext{
		Repo:        event.GetRepo(),
		SHA:         event.GetCheckSuite().GetHeadSHA(),
		Ref:         *event.GetCheckSuite().HeadBranch,
		RefType:     "branch",
		Entrypoint:  "",
		PRs:         event.GetCheckSuite().PullRequests,
		DeployEvent: nil,
	}

	_, err = h.Runner.runWorkflow(
		ctx,
		ghClient,
		&wctx,
	)

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, ""
}

func (h *HookHandler) webhookCheckRunRequestActionClearCache(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	id := event.RequestedAction.Identifier
	org := event.GetRepo().GetOwner().GetLogin()
	repo := *event.Repo.Name

	branch := ""
	if id == "clearCacheBranch" {
		branch = *event.CheckRun.CheckSuite.HeadBranch
	}

	// TODO(tcm): update the check-run, or create a new one, to indicate the
	// cache is being cleared.

	err := h.Storage.deletePVC(
		org,
		repo,
		branch,
		"cache clear requested by "+*event.Sender.Login,
	)
	if err != nil {
		log.Printf("error while deleting cache, %v", err)
	}

	return 200, "OK"
}

func (h *HookHandler) webhookCheckRunRequestAction(ctx context.Context, event *github.CheckRunEvent) (int, string) {
	repo := event.GetRepo().GetName()
	org := event.GetRepo().GetOwner().GetLogin()
	ghClient, err := h.Clients.getClient(org, int(*event.Installation.ID), repo)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	id := event.RequestedAction.Identifier
	switch id {
	case "clearCacheBranch", "clearCache":
		return h.webhookCheckRunRequestActionClearCache(ctx, event)
	case "run":
		wctx := WorkflowContext{
			Repo:        event.GetRepo(),
			SHA:         event.GetCheckRun().GetHeadSHA(),
			Ref:         event.GetCheckRun().GetCheckSuite().GetHeadBranch(),
			RefType:     "branch",
			Entrypoint:  event.GetCheckRun().GetExternalID(),
			PRs:         event.GetCheckRun().GetCheckSuite().PullRequests,
			DeployEvent: nil,
		}
		_, err = h.Runner.runWorkflow(ctx, ghClient, &wctx)
	case "skip":
		user := event.GetSender().GetLogin()
		summary := fmt.Sprintf("User %s skipped this manual action", user)
		ghClient.UpdateCheckRun(
			ctx,
			event.GetCheckRun().GetID(),
			github.UpdateCheckRunOptions{
				Name:       event.GetCheckRun().GetName(),
				Conclusion: github.String("neutral"),
				Output: &github.CheckRunOutput{
					Title:   github.String("Manual Step - Skipped"),
					Summary: github.String(summary),
				},
			},
		)
	default:
		return http.StatusBadRequest, "unrecognized task"
	}

	if err != nil {
		return http.StatusBadRequest, err.Error()
	}

	return http.StatusOK, "action initiated"
}

func (h *HookHandler) LoggingWebhook(w http.ResponseWriter, r *http.Request) (int, string) {
	status, msg := h.webhook(w, r)
	if status != 0 && status != http.StatusOK {
		log.Printf("error returned from webhook, %d: %s", status, msg)
	}
	return status, msg
}

func (h *HookHandler) webhookPayload(ctx context.Context, rawEvent interface{}) (int, string) {
	switch event := rawEvent.(type) {

	case *github.CheckSuiteEvent:
		if event.GetCheckSuite().GetApp().GetID() != h.AppID {
			return http.StatusOK, "ignoring, wrong appID"
		}
		// TODO: HeadBranch is not set for all events, need to understand why
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
		if event.GetCheckRun().GetCheckSuite().GetApp().GetID() != h.AppID {
			return http.StatusOK, "ignoring, wrong appID"
		}

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

	case *github.IssuesEvent:
		if *event.Action != "opened" {
			return http.StatusOK, fmt.Sprintf("ignoring event type %T", event)
		}
		icEvent := &github.IssueCommentEvent{
			Action: github.String("created"),
			Repo:   event.Repo,
			Comment: &github.IssueComment{
				Body: event.Issue.Body,
				User: event.Issue.User,
			},
			Issue: event.Issue,
		}
		return h.webhookIssueComment(ctx, icEvent)

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

func (h *HookHandler) webhook(w http.ResponseWriter, r *http.Request) (int, string) {
	payload, err := github.ValidatePayload(r, h.GitHubSecret)
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

	return h.webhookPayload(r.Context(), rawEvent)
}
