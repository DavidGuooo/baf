package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidGuooo/baf/internal/agent"
	"github.com/DavidGuooo/baf/internal/envelope"
	"github.com/DavidGuooo/baf/internal/mailbox"
)

func fileAPI(t *testing.T) http.Handler {
	t.Helper()
	mb, err := mailbox.Open(mailbox.Options{Path: filepath.Join(t.TempDir(), "mailbox.db"), Node: "box", Agents: map[string]agent.Adapter{"me": nil}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	return Handler(mb, []string{"me"})
}

func TestAddFileRejectedName(t *testing.T) {
	h := fileAPI(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/files?"+url.Values{"name": {"../x"}}.Encode(), bytes.NewBufferString("bytes")))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422; body=%s", w.Code, w.Body)
	}
	var e apiError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !e.Rejected || e.Error == "" {
		t.Fatalf("error=%+v", e)
	}
}

func TestFileRoundTrip(t *testing.T) {
	h := fileAPI(t)
	// More than the JSON endpoint's 1 MB limit, including non-text bytes.
	data := bytes.Repeat([]byte{0, 1, 255, 42}, (1<<20)/4+1)
	call := func(method, path string, body io.Reader, out any) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, body))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d; body=%s", method, path, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	var f envelope.File
	call("POST", "/v1/files?name=report.pdf", bytes.NewReader(data), &f)
	if f.Name != "report.pdf" || f.Size != int64(len(data)) || f.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatalf("file=%+v", f)
	}
	env := envelope.Envelope{V: envelope.Version, ID: envelope.NewID(), Kind: envelope.Notice, To: "box/me", FromAgent: "me", Body: envelope.Body{Files: []envelope.File{f}}}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var receipt mailbox.Receipt
	call("POST", "/v1/send", bytes.NewReader(raw), &receipt)
	if receipt.ID != env.ID || receipt.Queued {
		t.Fatalf("receipt=%+v", receipt)
	}
	var it mailbox.Item
	call("GET", "/v1/messages/"+env.ID+"?agent=me", nil, &it)
	if len(it.Files) != 1 || it.Files[0].File != f {
		t.Fatalf("files=%+v", it.Files)
	}
	got, err := os.ReadFile(it.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("stored file bytes differ")
	}
}

type fileRoundTripper func(*http.Request) (*http.Response, error)

func (f fileRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientAddFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		body     string
		want     string
		rejected bool
	}{
		{"success", 200, `{"name":"a & b.pdf","sha256":"hash","size":3}`, "", false},
		{"rejection", 422, `{"error":"bad file","rejected":true}`, "rejected: bad file", true},
		{"failure", 500, `{"error":"disk full"}`, "disk full", false},
		{"plain response", 404, `not found`, "404 Not Found", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{http: &http.Client{Transport: fileRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Path != "/v1/files" || r.URL.Query().Get("name") != "a & b.pdf" {
					t.Errorf("request=%s %s", r.Method, r.URL)
				}
				b, err := io.ReadAll(r.Body)
				if err != nil || string(b) != "raw" {
					t.Errorf("body=%q err=%v", b, err)
				}
				return &http.Response{StatusCode: tc.code, Status: fmt.Sprintf("%d %s", tc.code, http.StatusText(tc.code)), Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}}
			f, err := c.AddFile("a & b.pdf", strings.NewReader("raw"))
			if tc.want == "" {
				if err != nil || f.Name != "a & b.pdf" || f.Size != 3 || f.SHA256 != "hash" {
					t.Fatalf("file=%+v err=%v", f, err)
				}
			} else {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("err=%v, want %q", err, tc.want)
				}
				var rej *RejectedError
				if errors.As(err, &rej) != tc.rejected {
					t.Fatalf("rejection type: %T", err)
				}
			}
		})
	}
	c := &Client{http: &http.Client{Transport: fileRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: errors.New("not listening")}
	})}}
	if _, err := c.AddFile("x", strings.NewReader("raw")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err=%v, want ErrNotRunning", err)
	}
	if _, err := c.Status(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("status err=%v, want ErrNotRunning", err)
	}
}
