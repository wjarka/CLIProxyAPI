// Package consolebrowser is the private, fixed-endpoint browser broker client.
package consolebrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Session struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func unavailable() error       { return &Error{Code: "browser_unavailable"} }

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

func ValidID(id string) bool { return opaqueID.MatchString(id) }

type Client struct {
	base   *url.URL
	token  string
	http   *http.Client
	dialer *websocket.Dialer
}

func NewClient(raw, token string) (*Client, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, unavailable()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{base: u, token: token, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, dialer: &websocket.Dialer{HandshakeTimeout: 15 * time.Second, Subprotocols: []string{"binary"}}}, nil
}

// Missing or unreadable configuration disables hosted login without changing manual import.
func NewClientFromEnv() *Client {
	raw, file := strings.TrimSpace(os.Getenv("CLIPROXY_CONSOLE_BROWSER_URL")), strings.TrimSpace(os.Getenv("CLIPROXY_CONSOLE_BROWSER_TOKEN_FILE"))
	if raw == "" || file == "" {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(data) > 8192 {
		return nil
	}
	c, _ := NewClient(raw, strings.TrimSpace(string(data)))
	return c
}
func (c *Client) endpoint(path string) string {
	u := *c.base
	u.Path = path
	u.RawPath = ""
	return u.String()
}
func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return unavailable()
		}
		body = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return unavailable()
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		return unavailable()
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 && response.StatusCode != 201 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
		expected := map[string]int{"browser_unavailable": 503, "browser_busy": 429, "browser_expired": 410, "login_not_ready": 409, "invalid_request": 400, "not_found": 404}
		if expected[failure.Error] == response.StatusCode {
			return &Error{Code: failure.Error}
		}
		return unavailable()
	}
	if output != nil {
		data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil || len(data) > 65536 || json.Unmarshal(data, output) != nil {
			return unavailable()
		}
	}
	return nil
}
func (c *Client) Create(ctx context.Context, provider string) (Session, error) {
	if provider != "claude" && provider != "openai" {
		return Session{}, &Error{Code: "invalid_request"}
	}
	var s Session
	err := c.request(ctx, "POST", "/sessions", map[string]string{"provider": provider}, &s)
	if err == nil && (!validSession(s) || s.Provider != provider) {
		err = unavailable()
	}
	return s, err
}
func validSession(s Session) bool {
	return ValidID(s.ID) && (s.Provider == "claude" || s.Provider == "openai") && s.Status == "ready" && !s.ExpiresAt.IsZero()
}
func sessionPath(id string) (string, error) {
	if !ValidID(id) {
		return "", &Error{Code: "invalid_request"}
	}
	return "/sessions/" + id, nil
}
func (c *Client) Get(ctx context.Context, id string) (Session, error) {
	path, err := sessionPath(id)
	if err != nil {
		return Session{}, err
	}
	var s Session
	err = c.request(ctx, "GET", path, nil, &s)
	if err == nil && (!validSession(s) || s.ID != id) {
		err = unavailable()
	}
	return s, err
}
func (c *Client) Delete(ctx context.Context, id string) error {
	path, err := sessionPath(id)
	if err != nil {
		return err
	}
	err = c.request(ctx, "DELETE", path, nil, nil)
	var safe *Error
	if errors.As(err, &safe) && (safe.Code == "not_found" || safe.Code == "browser_expired") {
		return nil
	}
	return err
}
func (c *Client) Finish(ctx context.Context, id string) (string, error) {
	path, err := sessionPath(id)
	if err != nil {
		return "", err
	}
	var result struct {
		Session string `json:"session"`
	}
	err = c.request(ctx, "POST", path+"/finish", nil, &result)
	if err == nil && (strings.TrimSpace(result.Session) == "" || len(result.Session) > 16384) {
		err = &Error{Code: "login_not_ready"}
	}
	if err != nil {
		return "", err
	}
	return result.Session, nil
}
func (c *Client) View(ctx context.Context, id string) (*websocket.Conn, error) {
	path, err := sessionPath(id)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(c.endpoint(path + "/view"))
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	headers := http.Header{"Authorization": []string{"Bearer " + c.token}}
	conn, response, err := c.dialer.DialContext(ctx, u.String(), headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, unavailable()
	}
	if conn.Subprotocol() != "binary" {
		_ = conn.Close()
		return nil, unavailable()
	}
	return conn, nil
}
