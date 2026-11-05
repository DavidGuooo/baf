package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/daemon"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func TestRunFlagsRejectBeforeContactingDaemon(t *testing.T) {
	t.Setenv("BAF_HOME", t.TempDir()) // no daemon exists here
	for _, tc := range []struct {
		kind envelope.Kind
		args []string
		want string
	}{
		{envelope.Request, []string{"claude", "--desktop", "work"}, "notice"},
		{envelope.Notice, []string{"claude", "--dir", "repo", "--to-thread", "old", "work"}, "to_thread"},
		{envelope.Notice, []string{"claude", "--desktop", "--to-thread", "old", "work"}, "to_thread"},
		{envelope.Notice, []string{"claude", "--dir=", "--desktop", "work"}, "must not be empty"},
		{envelope.Notice, []string{"claude", "--dir", strings.Repeat("x", 1025), "work"}, "1024 bytes"},
		{envelope.Notice, []string{"claude", "--dir", "bad\x00path", "work"}, "NUL"},
		{envelope.Notice, []string{"claude", "--kind", "result", "--dir", "repo", "work"}, "notice or request"},
	} {
		err := send(tc.args, tc.kind)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v error=%v want=%q", tc.args, err, tc.want)
		}
	}
}

func TestRunFlagsSentAsWritten(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "baf-run-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("BAF_HOME", home)
	t.Setenv("BAF_AGENT", "me")
	received := make(chan envelope.Envelope, 3)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		var env envelope.Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Error(err)
			return
		}
		received <- env
		json.NewEncoder(w).Encode(mailbox.Receipt{ID: env.ID})
	})
	ln, err := net.Listen("unix", daemon.SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	for _, tc := range []struct {
		kind    envelope.Kind
		args    []string
		dir     string
		desktop bool
	}{
		{envelope.Notice, []string{"box/claude", "--dir", "~/repo with spaces", "work", "--desktop"}, "~/repo with spaces", true},
		{envelope.Request, []string{"--dir", "Documents/GitHub/baf", "box/codex", "work"}, "Documents/GitHub/baf", false},
		{envelope.Notice, []string{"box/claude", "work", "--desktop"}, "", true},
	} {
		if err := send(tc.args, tc.kind); err != nil {
			t.Fatal(err)
		}
		env := <-received
		if env.Run == nil || env.Run.Dir != tc.dir || env.Run.Desktop != tc.desktop || env.Kind != tc.kind {
			t.Fatalf("env=%+v run=%+v", env, env.Run)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "repo with spaces")); !os.IsNotExist(err) {
		t.Fatalf("sender created receiver folder: %v", err)
	}
}
