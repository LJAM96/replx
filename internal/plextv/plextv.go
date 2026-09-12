// Package plextv speaks to plex.tv for owner onboarding: PIN creation,
// claim polling, user validation and PMS resource discovery.
//
// Shapes follow the current plex.tv v2 API responses and are decoded
// tolerantly (unknown fields ignored): if Plex renames a field, the
// onboarding verify step fails closed with a clear error instead of
// silently binding the wrong server.
package plextv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to plex.tv (or a test double via BaseURL).
type Client struct {
	BaseURL          string
	ClientIdentifier string
	Product          string
	Version          string
	Platform         string
	HTTP             *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) newRequest(ctx context.Context, method, path string, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", c.ClientIdentifier)
	req.Header.Set("X-Plex-Product", c.Product)
	req.Header.Set("X-Plex-Version", c.Version)
	req.Header.Set("X-Plex-Platform", c.Platform)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	return req, nil
}

// PIN is a sign-in PIN awaiting administrator claim at plex.tv.
type PIN struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// AuthURL is the administrator-facing claim URL for a PIN.
func (c *Client) AuthURL(p PIN) string {
	q := url.Values{}
	q.Set("clientID", c.ClientIdentifier)
	q.Set("code", p.Code)
	q.Set("pinID", fmt.Sprint(p.ID))
	return "https://app.plex.tv/auth#?" + q.Encode()
}

// CreatePIN issues a strong PIN.
func (c *Client) CreatePIN(ctx context.Context) (PIN, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v2/pins?strong=true", "")
	if err != nil {
		return PIN{}, err
	}
	var pin PIN
	if err := c.do(req, &pin); err != nil {
		return PIN{}, err
	}
	if pin.ID == 0 || pin.Code == "" {
		return PIN{}, fmt.Errorf("plextv: malformed PIN response")
	}
	return pin, nil
}

// PollPIN returns the owner auth token once claimed, or "" while pending.
func (c *Client) PollPIN(ctx context.Context, id int64) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v2/pins/"+fmt.Sprint(id), "")
	if err != nil {
		return "", err
	}
	var body struct {
		AuthToken *string `json:"authToken"`
	}
	if err := c.do(req, &body); err != nil {
		return "", err
	}
	if body.AuthToken == nil {
		return "", nil
	}
	return *body.AuthToken, nil
}

// User is the owner account the token belongs to.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// GetUser validates a token and returns the account holder.
func (c *Client) GetUser(ctx context.Context, token string) (User, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v2/user", token)
	if err != nil {
		return User{}, err
	}
	var u User
	if err := c.do(req, &u); err != nil {
		return User{}, err
	}
	if u.ID == 0 {
		return User{}, fmt.Errorf("plextv: malformed user response")
	}
	return u, nil
}

// Connection is one published PMS connection URL.
type Connection struct {
	URI      string `json:"uri"`
	Protocol string `json:"protocol"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
	IPv6     bool   `json:"IPv6"`
}

// Resource is one plex.tv PMS resource.
type Resource struct {
	ClientIdentifier string       `json:"clientIdentifier"`
	Name             string       `json:"name"`
	Product          string       `json:"product"`
	Provides         string       `json:"provides"`
	AccessToken      string       `json:"accessToken"`
	Connections      []Connection `json:"connections"`
}

// ListServers returns PMS resources (provides contains "server") visible
// to the token holder, requesting HTTPS connection details.
func (c *Client) ListServers(ctx context.Context, token string) ([]Resource, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v2/resources?includeHttps=1&includeRelay=1&includeIPv6=1", token)
	if err != nil {
		return nil, err
	}
	var all []Resource
	if err := c.do(req, &all); err != nil {
		return nil, err
	}
	var servers []Resource
	for _, r := range all {
		if strings.Contains(r.Provides, "server") {
			servers = append(servers, r)
		}
	}
	return servers, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("plextv: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plextv: status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("plextv: decode: %w", err)
	}
	return nil
}
