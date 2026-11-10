package hosted

import (
	"encoding/json"
	"testing"
)

func TestClaudeCookieFilter(t *testing.T) {
	for _, tt := range []struct{ domain, name, value, want string }{{".claude.com", "sessionKey", "good", "good"}, {"platform.claude.com", "sessionKey", "good", "good"}, {"evilclaude.com", "sessionKey", "bad", ""}, {"accounts.google.com", "sessionKey", "bad", ""}, {"claude.com.evil.test", "sessionKey", "bad", ""}, {"claude.com", "other", "bad", ""}, {"claude.com", "sessionKey", "", ""}} {
		if got := claudeCookie(tt.domain, tt.name, tt.value); got != tt.want {
			t.Fatalf("%#v: %q", tt, got)
		}
	}
}
func TestOpenAIFilterAndExtraInfoOrdering(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		c := newTokenCollector()
		request := json.RawMessage(`{"requestId":"one","request":{"url":"https://api.openai.com/dashboard/billing/credit_grants","headers":{}}}`)
		extra := json.RawMessage(`{"requestId":"one","headers":{"authorization":"Bearer session-token"}}`)
		var got string
		if reverse {
			got = c.event("Network.requestWillBeSentExtraInfo", extra)
			if got != "" {
				t.Fatal("accepted headers without URL")
			}
			got = c.event("Network.requestWillBeSent", request)
		} else {
			got = c.event("Network.requestWillBeSent", request)
			if got != "" {
				t.Fatal("accepted without token")
			}
			got = c.event("Network.requestWillBeSentExtraInfo", extra)
		}
		if got != "session-token" {
			t.Fatal(reverse, got)
		}
	}
	for _, u := range []string{"http://api.openai.com/dashboard/a", "https://api.openai.com.evil.test/dashboard/a", "https://platform.openai.com/v1/chat/completions", "https://api.openai.com:444/dashboard/a", "https://user@api.openai.com/dashboard/a"} {
		c := newTokenCollector()
		p, _ := json.Marshal(map[string]any{"requestId": "x", "request": map[string]any{"url": u, "headers": map[string]string{"Authorization": "Bearer session-token"}}})
		if got := c.event("Network.requestWillBeSent", p); got != "" {
			t.Fatal(u, got)
		}
	}
	for _, token := range []string{"sk-secret", "sk-proj-secret", "", "has spaces"} {
		c := newTokenCollector()
		p, _ := json.Marshal(map[string]any{"requestId": "x", "request": map[string]any{"url": "https://platform.openai.com/v1/dashboard/a", "headers": map[string]string{"Authorization": "Bearer " + token}}})
		if got := c.event("Network.requestWillBeSent", p); got != "" {
			t.Fatal(token)
		}
	}
}
