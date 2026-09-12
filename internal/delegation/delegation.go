// Package delegation exchanges a caller token for a PMS transient
// delegation token (GET /security/token?type=delegation&scope=all).
//
// The transient token carries the caller's access, lives at most 48 hours
// and dies on PMS restart. It is the only token ever placed in a client
// redirect URL: persistent user tokens must never appear in a Location,
// log line, or trace. Owner tokens never enter this path at all —
// delegation always runs under the requesting user's own token.
package delegation

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fetch mints a transient token from originBase for userToken.
func Fetch(ctx context.Context, originBase, userToken string) (string, error) {
	return FetchWithClient(ctx, http.DefaultClient, originBase, userToken)
}

// FetchWithClient is Fetch with an injectable client (tests).
func FetchWithClient(ctx context.Context, client *http.Client, originBase, userToken string) (string, error) {
	if userToken == "" {
		return "", fmt.Errorf("delegation: no caller token")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(originBase, "/")+"/security/token?type=delegation&scope=all", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("X-Plex-Token", userToken)
	resp, err := client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return "", fmt.Errorf("delegation: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("delegation: status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	var container struct {
		Token string `xml:"token,attr"`
	}
	if err := xml.Unmarshal(body, &container); err != nil || container.Token == "" {
		return "", fmt.Errorf("delegation: no transient token in response")
	}
	return container.Token, nil
}
