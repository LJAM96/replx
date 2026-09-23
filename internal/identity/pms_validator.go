package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/origin"
)

// PMSValidator checks a token against the configured Plex server. It only
// reports validity; it never infers a Plex account or shares cache scope.
type PMSValidator struct {
	url    string
	client *http.Client
}

func NewPMSValidator(base string) (*PMSValidator, error) {
	client, err := origin.APIClient(base, 8*time.Second)
	if err != nil {
		return nil, err
	}
	return &PMSValidator{url: strings.TrimSuffix(base, "/") + "/library/sections", client: client}, nil
}

func (v *PMSValidator) ValidateToken(ctx context.Context, token string) (bool, error) {
	if v == nil || token == "" {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Plex-Token", token)
	resp, err := v.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("PMS token validation status %d", resp.StatusCode)
	}
}
