// Package plextv speaks to plex.tv for owner onboarding: PIN creation,
// claim polling, user validation and PMS resource discovery.
//
// Shapes follow the current plex.tv v2 API responses and are decoded
// tolerantly (unknown fields ignored): if Plex renames a field, the
// onboarding verify step fails closed with a clear error instead of
// silently binding the wrong server.
package plextv

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SharedUser is a Plex account with accepted access to the selected server.
type SharedUser struct {
	AccountID    int64
	Username     string
	FriendlyName string
	Restricted   bool
}

// ListUsersWithServerAccess reads the owner's Plex sharing list, including
// friends outside Plex Home. Matching the machine identifier prevents users
// shared only with another server from appearing in this server's dashboard.
func (c *Client) ListUsersWithServerAccess(ctx context.Context, token, machineID string) ([]SharedUser, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/users", token)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex.tv users: HTTP %d", resp.StatusCode)
	}
	var document struct {
		Users []struct {
			ID           int64  `xml:"id,attr"`
			Username     string `xml:"username,attr"`
			Title        string `xml:"title,attr"`
			FriendlyName string `xml:"friendlyName,attr"`
			Restricted   bool   `xml:"restricted,attr"`
			Servers      []struct {
				MachineID    string `xml:"machineIdentifier,attr"`
				NumLibraries int    `xml:"numLibraries,attr"`
				Pending      bool   `xml:"pending,attr"`
			} `xml:"Server"`
		} `xml:"User"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&document); err != nil {
		return nil, fmt.Errorf("plex.tv users: decode: %w", err)
	}
	users := make([]SharedUser, 0, len(document.Users))
	for _, user := range document.Users {
		if user.ID <= 0 {
			continue
		}
		for _, server := range user.Servers {
			if server.MachineID == machineID && server.NumLibraries > 0 && !server.Pending {
				name := user.FriendlyName
				if name == "" {
					name = user.Title
				}
				users = append(users, SharedUser{AccountID: user.ID, Username: user.Username, FriendlyName: name, Restricted: user.Restricted})
				break
			}
		}
	}
	return users, nil
}

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
	return c.createPIN(ctx, nil)
}

// CreatePINJWT issues a strong PIN bound to the device JWK. The JWK
// registers the installation public key with plex.tv as part of the claim.
func (c *Client) CreatePINJWT(ctx context.Context, jwk json.RawMessage) (PIN, error) {
	body, err := json.Marshal(map[string]any{"jwk": json.RawMessage(jwk), "strong": true})
	if err != nil {
		return PIN{}, err
	}
	return c.createPIN(ctx, body)
}

func (c *Client) createPIN(ctx context.Context, body []byte) (PIN, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v2/pins?strong=true", "")
	if err != nil {
		return PIN{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
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

// Nonce fetches a 5-minute auth nonce for JWT refresh.
func (c *Client) Nonce(ctx context.Context, token string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v2/auth/nonce", token)
	if err != nil {
		return "", err
	}
	var body struct {
		Nonce string `json:"nonce"`
		Code  string `json:"code"`
	}
	if err := c.do(req, &body); err != nil {
		return "", err
	}
	if body.Nonce == "" {
		body.Nonce = body.Code
	}
	if body.Nonce == "" {
		return "", fmt.Errorf("plextv: malformed nonce response")
	}
	return body.Nonce, nil
}

// RefreshToken exchanges a signed device JWT (carrying the nonce) for a
// fresh 7-day Plex JWT.
func (c *Client) RefreshToken(ctx context.Context, deviceJWT string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v2/auth/token", "")
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"deviceJWT": deviceJWT})
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	var out struct {
		AuthToken *string `json:"authToken"`
		Token     *string `json:"token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if out.AuthToken != nil && *out.AuthToken != "" {
		return *out.AuthToken, nil
	}
	if out.Token != nil && *out.Token != "" {
		return *out.Token, nil
	}
	return "", fmt.Errorf("plextv: malformed token response")
}

// PollPIN returns the owner auth token once claimed, or "" while pending.
func (c *Client) PollPIN(ctx context.Context, id int64) (string, error) {
	return c.pollPIN(ctx, id, "")
}

// PollPINJWT polls with a signed device JWT (aud=plex.tv). The Plex JWT
// arrives in authToken exactly as in the legacy flow.
func (c *Client) PollPINJWT(ctx context.Context, id int64, deviceJWT string) (string, error) {
	return c.pollPIN(ctx, id, deviceJWT)
}

func (c *Client) pollPIN(ctx context.Context, id int64, deviceJWT string) (string, error) {
	path := "/api/v2/pins/" + fmt.Sprint(id)
	if deviceJWT != "" {
		path += "?deviceJWT=" + url.QueryEscape(deviceJWT)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, "")
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
		return &StatusError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("plextv: decode: %w", err)
	}
	return nil
}

// StatusError is a non-2xx plex.tv response. Callers use errors.As to
// decide fallback (e.g. JWT shape rejected -> legacy PIN).
type StatusError struct {
	StatusCode int
	Status     string
}

func (e *StatusError) Error() string { return fmt.Sprintf("plextv: status %s", e.Status) }

// IsClientError reports 4xx responses (caller-shaped requests).
func IsClientError(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500
}
