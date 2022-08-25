package kubeci

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
)

type StatusHandler struct {
	Status *int32
}

func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status := atomic.LoadInt32(h.Status)
	w.WriteHeader(int(status))

	// We'll be kind and do a JSON blob here, as the python
	// version does.
	resp := struct {
		Service string `json:"service"`
		Status  int32  `json:"status"`
	}{
		Service: "kube-ci",
		Status:  status,
	}

	bs, _ := json.Marshal(resp)
	buf := bytes.NewBuffer(bs)

	io.Copy(w, buf)
}

func (h *StatusHandler) Shutdown() {
	atomic.StoreInt32(h.Status, 500)
}
