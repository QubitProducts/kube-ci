package kubeci

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"encoding/json"

	workflow "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
)

type APIHandler struct {
	Storage pvcManager
	Clients githubClientSource
	Runner  workflowRunner
	Slash   slashRunner

	UIBase       string
	GitHubSecret []byte
	AppID        int64
	InstallID    int
}

type RunResp struct {
	Workflow *workflow.Workflow `json:"workflow,omitempty"`
	Debug    string             `json:"debug,omitempty"`
}

type RunRequest struct {
	Org        string            `json:"org"`
	Repo       string            `json:"repo"`
	Ref        string            `json:"ref"`
	RefType    string            `json:"ref_type,omitempty"`
	Hash       string            `json:"hash"`
	CIContext  map[string]string `json:"ci_context"`
	Entrypoint string            `json:"entrypoint"`
	Run        bool              `json:"run"`
}

func (h *APIHandler) handleRun(ctx context.Context, w http.ResponseWriter, r *RunRequest) (RunResp, error) {
	ghClient, err := h.Clients.getClient(r.Org, h.InstallID, r.Repo)
	if err != nil {
		return RunResp{}, fmt.Errorf("could not get github client, %w", err)
	}

	repo, err := ghClient.GetRepo(ctx)
	if err != nil {
		return RunResp{}, fmt.Errorf("could not get github repo, %w", err)
	}

	refType := "branch"
	if r.RefType != "" {
		refType = r.RefType
	}

	// We'll tidy up the filenames
	var ciContext map[string]string
	if r.CIContext != nil {
		ciContext = make(map[string]string, len(r.CIContext))
		for k, v := range r.CIContext {
			ciContext[strings.TrimPrefix(k, "/")] = v
		}
	}

	wctx := WorkflowContext{
		Repo:        repo,
		Ref:         r.Ref,
		RefType:     refType,
		SHA:         r.Hash,
		Entrypoint:  r.Entrypoint,
		ContextData: ciContext,

		Event: nil,
		PRs:   nil,
	}

	wf, debug, err := h.Runner.getWorkflow(ctx, ghClient, &wctx)
	if err != nil {
		return RunResp{Debug: debug}, fmt.Errorf("could not get workflow, %w", err)
	}

	if !r.Run {
		return RunResp{
			Workflow: wf,
			Debug:    debug,
		}, nil
	}

	return RunResp{}, fmt.Errorf("only dry run is supported at present")
}

func (h *APIHandler) HandleRun(w http.ResponseWriter, r *http.Request) {
	req := RunRequest{}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot decode input, %v", err), http.StatusBadRequest)
		return
	}

	switch {
	case req.Org == "":
		err = fmt.Errorf("org must be specified")
	case req.Repo == "":
		err = fmt.Errorf("repo must be specified")
	case req.Ref == "":
		err = fmt.Errorf("ref must be specified")
	case req.Hash == "":
		err = fmt.Errorf("hash must be specified")
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("cannot decode input, %v", err), http.StatusBadRequest)
		return
	}

	resp, err := h.handleRun(r.Context(), w, &req)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot decode input, %v", err), http.StatusBadRequest)
	}

	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot encode response, %v", err), http.StatusInternalServerError)
	}
}

func NewAPIHandler(h APIHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/run", h.HandleRun)
	return mux
}
