package door

import (
	"errors"
	"net/url"
	"time"
)

// Refusal details stay in the log. Callers still see only invalid_grant.
type grantRefusal string

func (e grantRefusal) Error() string { return errGrant.Error() }
func (e grantRefusal) Unwrap() error { return errGrant }

func refusalReason(err error) string {
	var refusal grantRefusal
	if errors.As(err, &refusal) {
		return string(refusal)
	}
	return "store unavailable"
}

func policyRefusal(err error, missing string) grantRefusal {
	if errors.Is(err, errDoorNotListed) {
		return grantRefusal(missing)
	}
	return grantRefusal("policy unavailable")
}

func exchangeRefusal(g grant, form url.Values, codeExpires int64, now time.Time) string {
	if form.Get("client_id") != g.ClientID {
		return "client_id mismatch"
	}
	if form.Get("resource") != "" && form.Get("resource") != g.Resource {
		return "resource mismatch"
	}
	if form.Get("grant_type") == "authorization_code" {
		if now.Unix() >= codeExpires {
			return "code expired"
		}
		if form.Get("redirect_uri") != g.Redirect {
			return "redirect_uri mismatch"
		}
		if !validVerifier(form.Get("code_verifier")) || challenge(form.Get("code_verifier")) != g.Challenge {
			return "bad PKCE verifier"
		}
	}
	return ""
}

// URLs identify the client or resource, but their query and user info can hold
// credentials. Never log those, fragments, or the original request query.
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid URL"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	return u.String()
}

func (s *Server) shouldLogBearerRefusal(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for agent, until := range s.bearerRefusals {
		if !now.Before(until) {
			delete(s.bearerRefusals, agent)
		}
	}
	if _, seen := s.bearerRefusals[name]; seen {
		return false
	}
	// Unknown names arrive from the internet too. Bound their bookkeeping
	// without evicting a name that must stay quiet for the rest of its minute.
	if len(s.bearerRefusals) >= 1024 {
		return false
	}
	s.bearerRefusals[name] = now.Add(time.Minute)
	return true
}
