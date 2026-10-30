// Package daemon runs the mailbox as a long-lived process and serves the
// local API that the `baf` command uses. The API listens on a Unix socket in
// baf's home directory, readable only by its owner, so only this machine's
// own user (and the agents it runs) can use it.
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

// Status describes the running daemon.
type Status struct {
	Node   string   `json:"node"`
	Agents []string `json:"agents"`
}

// apiError is the body of every non-2xx response.
type apiError struct {
	Error    string `json:"error"`
	Rejected bool   `json:"rejected,omitempty"` // the mailbox refused the message
}

// Handler serves the local API for mb.
func Handler(mb *mailbox.Mailbox, agents []string) http.Handler {
	sorted := append([]string(nil), agents...)
	sort.Strings(sorted)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{Node: mb.Node(), Agents: sorted})
	})

	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		var env envelope.Envelope
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&env); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "bad envelope: " + err.Error()})
			return
		}
		receipt, err := mb.Send(env)
		var rej *mailbox.Rejection
		switch {
		case errors.As(err, &rej):
			writeJSON(w, http.StatusUnprocessableEntity, apiError{Error: rej.Reason, Rejected: true})
		case errors.Is(err, mailbox.ErrRemoteNotSupported):
			writeJSON(w, http.StatusNotImplemented, apiError{Error: err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		default:
			writeJSON(w, http.StatusOK, receipt)
		}
	})

	mux.HandleFunc("GET /v1/inbox", func(w http.ResponseWriter, r *http.Request) {
		items, err := mb.Inbox(r.URL.Query().Get("agent"), r.URL.Query().Get("all") == "1")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, nonNil(items))
	})

	mux.HandleFunc("GET /v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, err := mb.Read(r.URL.Query().Get("agent"), r.PathValue("id"))
		switch {
		case errors.Is(err, mailbox.ErrNotFound):
			writeJSON(w, http.StatusNotFound, apiError{Error: err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
		default:
			writeJSON(w, http.StatusOK, item)
		}
	})

	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			limit = 20
		}
		runs, err := mb.Runs(limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, nonNil(runs))
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
