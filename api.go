package kubeci

import (
	"fmt"
	"net/http"

	"encoding/json"
)

type APIHandler struct {
	Storage pvcManager
	Clients githubClientSource
	Runner  workflowRunner
	Slash   slashRunner

	UIBase       string
	GitHubSecret []byte
	AppID        int64
}

type RunResp struct {
}

type RunRequest struct {
	Org       string            `json:"org"`
	Repo      string            `json:"repo"`
	Ref       string            `json:"ref"`
	Hash      string            `json:"hash"`
	CIContext map[string]string `json:"ci_context"`
}

func (h *APIHandler) handleRun(w http.ResponseWriter, r *RunRequest) (RunResp, error) {
	return RunResp{}, nil
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

	resp, err := h.handleRun(w, &req)
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
