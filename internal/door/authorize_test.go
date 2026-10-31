package door

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func (h *harness) decide(csrf, decision string) (*http.Response, string) {
	h.t.Helper()
	form := url.Values{"csrf": {csrf}, "decision": {decision}}
	return h.request(h.browser, "POST", h.signin.URL+"/authorize", form.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func TestAllowRepeat(t *testing.T) {
	for _, decisions := range [][2]string{{"allow", "allow"}, {"allow", "deny"}, {"deny", "deny"}, {"deny", "allow"}} {
		t.Run(strings.Join(decisions[:], " then "), func(t *testing.T) {
			h := newHarness(t)
			_, csrf, _ := h.page(h.query("openai/dot"))
			first, body := h.decide(csrf, decisions[0])
			if first.StatusCode != 302 {
				t.Fatalf("first submit: %d %s", first.StatusCode, body)
			}
			if len(first.Cookies()) != 0 {
				t.Fatal("submit changed the guard cookie")
			}
			h.clock.Add(1)
			second, body := h.decide(csrf, decisions[1])
			if second.StatusCode != 302 || second.Header.Get("Location") != first.Header.Get("Location") {
				t.Fatalf("repeat: %d %s, redirects differ: %t", second.StatusCode, body,
					second.Header.Get("Location") != first.Header.Get("Location"))
			}
			back, err := first.Location()
			if err != nil {
				t.Fatal(err)
			}
			var rows int
			if err := h.s.db.QueryRow(`SELECT count(*) FROM connectors`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if decisions[0] == "allow" {
				code := back.Query().Get("code")
				if rows != 1 || code == "" || back.Query().Get("error") != "" {
					t.Fatal("Allow did not preserve one connector and code")
				}
				h.token(codeForm(code), 200)
				h.token(codeForm(code), 400)
			} else if rows != 0 || back.Query().Get("error") != "access_denied" || back.Query().Get("code") != "" {
				t.Fatal("Deny created a connector or changed its redirect")
			}
		})
	}
}

func TestAllowConcurrentRepeat(t *testing.T) {
	h := newHarness(t)
	_, csrf, _ := h.page(h.query("openai/dot"))
	var wg sync.WaitGroup
	responses := make(chan *http.Response, 8)
	for range cap(responses) {
		wg.Go(func() {
			resp, _ := h.allow(csrf)
			responses <- resp
		})
	}
	wg.Wait()
	close(responses)
	location := ""
	for resp := range responses {
		if resp.StatusCode != 302 {
			t.Fatalf("concurrent submit: %s", resp.Status)
		}
		if location == "" {
			location = resp.Header.Get("Location")
		} else if location != resp.Header.Get("Location") {
			t.Fatal("concurrent submits issued different redirects")
		}
	}
	back, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	h.token(codeForm(back.Query().Get("code")), 200)
	h.token(codeForm(back.Query().Get("code")), 400)
}

func TestAllowRepeatGuard(t *testing.T) {
	for _, kind := range []string{"cookie missing", "guard mismatch", "other login", "other node", "tagged"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			_, csrf, _ := h.page(h.query("openai/dot"))
			first, _ := h.allow(csrf)
			if first.StatusCode != 302 {
				t.Fatal(first.Status)
			}
			h.identity.mu.Lock()
			switch kind {
			case "cookie missing":
				h.browser.Jar = nil
			case "guard mismatch":
				csrf = randomToken()
			case "other login":
				h.identity.login = "another@example.com"
			case "other node":
				h.identity.node = "another-phone"
			case "tagged":
				h.identity.tagged = true
			}
			h.identity.mu.Unlock()
			for _, expired := range []bool{false, true} {
				if expired {
					h.clock.Add(600)
				}
				resp, body := h.allow(csrf)
				if resp.StatusCode != 403 || resp.Header.Get("Location") != "" {
					t.Fatalf("repeat (expired=%t): %d %s", expired, resp.StatusCode, body)
				}
			}
		})
	}
}

func TestAllowRepeatFinished(t *testing.T) {
	for _, kind := range []string{"exchanged", "allow expired", "deny expired", "expired after another page"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			_, csrf, _ := h.page(h.query("openai/dot"))
			decision := "allow"
			if kind == "deny expired" {
				decision = "deny"
			}
			first, _ := h.decide(csrf, decision)
			if first.StatusCode != 302 {
				t.Fatal(first.Status)
			}
			back, err := first.Location()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "exchanged" {
				h.token(codeForm(back.Query().Get("code")), 200)
			} else {
				h.clock.Add(600)
			}
			if kind == "expired after another page" {
				// A different browser opens a new form, running expired-form cleanup.
				jar := h.browser.Jar
				h.browser.Jar = nil
				h.page(h.query("openai/other"))
				h.browser.Jar = jar
			}
			for range 2 {
				resp, body := h.allow(csrf)
				if resp.StatusCode != 200 || !strings.Contains(body, "sign-in already finished") ||
					!strings.Contains(body, "Return to ChatGPT") || resp.Header.Get("Location") != "" {
					t.Fatalf("finished repeat: %d %s", resp.StatusCode, body)
				}
			}
			h.token(codeForm(back.Query().Get("code")), 400)
		})
	}
}

func TestAllowRepeatLog(t *testing.T) {
	h := newHarness(t)
	_, csrf, _ := h.page(h.query("openai/dot"))
	first, _ := h.allow(csrf)
	back, err := first.Location()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h.allow(csrf)
	line := logs.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "level=INFO") ||
		!strings.Contains(line, "door sign-in repeated") || !strings.Contains(line, "owner@example.com") {
		t.Fatalf("repeat log: %s", line)
	}
	if strings.Contains(line, csrf) || strings.Contains(line, back.Query().Get("code")) {
		t.Fatal("repeat log leaked the guard or code")
	}
}
