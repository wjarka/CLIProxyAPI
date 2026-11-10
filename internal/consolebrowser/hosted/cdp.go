package hosted

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

func claudeCookie(domain, name, value string) string {
	domain = strings.TrimPrefix(domain, ".")
	if name == "sessionKey" && (domain == "claude.com" || domain == "platform.claude.com") && validSessionToken(value) {
		return value
	}
	return ""
}
func validSessionToken(s string) bool {
	return len(s) > 0 && len(s) <= 16384 && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
func dashboardURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && (u.Hostname() == "api.openai.com" || u.Hostname() == "platform.openai.com") && (strings.HasPrefix(u.Path, "/dashboard/") || strings.HasPrefix(u.Path, "/v1/dashboard/"))
}
func bearer(headers map[string]any) string {
	for key, value := range headers {
		if strings.EqualFold(key, "Authorization") {
			s, ok := value.(string)
			if !ok || !strings.HasPrefix(s, "Bearer ") {
				return ""
			}
			s = strings.TrimPrefix(s, "Bearer ")
			if validSessionToken(s) && !strings.HasPrefix(strings.ToLower(s), "sk-") {
				return s
			}
		}
	}
	return ""
}

type tokenRequest struct {
	url       string
	extra     string
	seen      bool
	forbidden bool
}
type tokenCollector struct{ requests map[string]*tokenRequest }

func newTokenCollector() *tokenCollector {
	return &tokenCollector{requests: make(map[string]*tokenRequest)}
}
func (c *tokenCollector) event(method string, raw json.RawMessage) string {
	if method != "Network.requestWillBeSent" && method != "Network.requestWillBeSentExtraInfo" {
		return ""
	}
	var p struct {
		RequestID string         `json:"requestId"`
		Headers   map[string]any `json:"headers"`
		Request   struct {
			URL     string         `json:"url"`
			Headers map[string]any `json:"headers"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &p) != nil || p.RequestID == "" || len(p.RequestID) > 256 {
		return ""
	}
	entry := c.requests[p.RequestID]
	if entry == nil {
		if len(c.requests) >= 2048 {
			return ""
		}
		entry = &tokenRequest{}
		c.requests[p.RequestID] = entry
	}
	if method == "Network.requestWillBeSent" {
		entry.seen = true
		entry.url = p.Request.URL
		if !dashboardURL(entry.url) {
			entry.forbidden = true
		}
		if !entry.forbidden {
			if token := bearer(p.Request.Headers); token != "" {
				return token
			}
		}
	} else {
		entry.extra = bearer(p.Headers)
	}
	if entry.seen && !entry.forbidden && dashboardURL(entry.url) {
		return entry.extra
	}
	return ""
}
func validCDPURL(raw string, port int) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "ws" && u.Host == "127.0.0.1:"+strconv.Itoa(port) && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.Path, "/devtools/page/") && len(strings.TrimPrefix(u.Path, "/devtools/page/")) > 0 && !strings.Contains(strings.TrimPrefix(u.Path, "/devtools/page/"), "/")
}
func discoverPage(ctx context.Context, base, provider string, port int) (string, error) {
	client := http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, "GET", base+"/json/list", nil)
	if e != nil {
		return "", ErrNotReady
	}
	res, e := client.Do(req)
	if e != nil {
		return "", ErrNotReady
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", ErrNotReady
	}
	var targets []struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
	if e != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &targets) != nil {
		return "", ErrNotReady
	}
	for _, t := range targets {
		u, e := url.Parse(t.URL)
		if e != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || t.Type != "page" {
			continue
		}
		allowed := provider == "claude" && (u.Hostname() == "claude.com" || u.Hostname() == "platform.claude.com") || provider == "openai" && u.Hostname() == "platform.openai.com"
		if allowed && validCDPURL(t.WebSocket, port) {
			return t.WebSocket, nil
		}
	}
	return "", ErrNotReady
}
func extractSession(ctx context.Context, port int, provider string) (string, error) {
	if port < 1 || port > 65535 {
		return "", ErrNotReady
	}
	endpoint, err := discoverPage(ctx, "http://127.0.0.1:"+strconv.Itoa(port), provider, port)
	if err != nil {
		return "", ErrNotReady
	}
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 3 * time.Second}
	ws, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return "", ErrNotReady
	}
	defer ws.Close()
	ws.SetReadLimit(2 * 1024 * 1024)
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.SetReadDeadline(deadline)
		_ = ws.SetWriteDeadline(deadline)
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-stopped:
		}
	}()
	send := func(id int, method string, params any) error {
		return ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	}
	if provider == "claude" {
		if send(1, "Network.getCookies", map[string]any{"urls": []string{"https://claude.com/", "https://platform.claude.com/"}}) != nil {
			return "", ErrNotReady
		}
	} else if provider == "openai" {
		if send(1, "Network.enable", map[string]any{}) != nil {
			return "", ErrNotReady
		}
	} else {
		return "", ErrNotReady
	}
	collector := newTokenCollector()
	for events := 0; events < 10000; events++ {
		var event struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if ws.ReadJSON(&event) != nil || len(event.Error) > 0 {
			return "", ErrNotReady
		}
		if provider == "claude" && event.ID == 1 {
			var result struct {
				Cookies []struct {
					Domain string `json:"domain"`
					Name   string `json:"name"`
					Value  string `json:"value"`
				} `json:"cookies"`
			}
			if json.Unmarshal(event.Result, &result) != nil {
				return "", ErrNotReady
			}
			for _, cookie := range result.Cookies {
				if token := claudeCookie(cookie.Domain, cookie.Name, cookie.Value); token != "" {
					return token, nil
				}
			}
			return "", ErrNotReady
		}
		if provider == "openai" {
			if event.ID == 1 {
				if send(2, "Page.reload", map[string]any{"ignoreCache": false}) != nil {
					return "", ErrNotReady
				}
			}
			if token := collector.event(event.Method, event.Params); token != "" {
				return token, nil
			}
		}
	}
	return "", ErrNotReady
}
