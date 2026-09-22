// Package origin is the single hardened HTTP transport for trusted PMS
// communication. Every request carrying a Plex credential must go through
// it.
//
// Background: the ordinary Go client follows redirects, and Go strips
// only Authorization/Cookie-style headers on cross-host hops. X-Plex-Token
// is custom and receives no such protection, so a PMS endpoint answering
// with a redirect to another hostname would previously cause Replx to
// forward a Plex credential there. This package makes that impossible:
//
//   - APIClient refuses any redirect leaving the exact configured origin
//     tuple (scheme plus host). Same-origin redirects are followed with
//     headers intact; anything else fails closed.
//   - TransparentClient never follows redirects at all: origin 3xx
//     responses are returned to the caller untouched, which is the only
//     correct behaviour for the pass-through proxy.
//
// Anything sending a PMS credential goes through this package.
package origin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIClient returns a client for server-to-server PMS API operations
// against baseURL. Redirects stay within the exact origin tuple; a hop
// anywhere else aborts the request instead of forwarding credentials.
func APIClient(baseURL string, timeout time.Duration) (*http.Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("origin: invalid base URL")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			if !strings.EqualFold(req.URL.Scheme, base.Scheme) ||
				!strings.EqualFold(req.URL.Host, base.Host) {
				return fmt.Errorf("origin: refusing cross-origin redirect to %s",
					redactHost(req.URL))
			}
			if len(via) >= 5 {
				return fmt.Errorf("origin: too many redirects")
			}
			return nil
		},
	}, nil
}

// MustAPIClient is APIClient that panics on misconfiguration, for static
// wiring where the base URL is already validated.
func MustAPIClient(baseURL string, timeout time.Duration) *http.Client {
	c, err := APIClient(baseURL, timeout)
	if err != nil {
		panic(err.Error())
	}
	return c
}

// TransparentClient returns a client that never follows redirects: 3xx
// responses are delivered to the caller as-is. The pass-through proxy uses
// it so origin redirects remain origin responses rather than being
// followed (and re-credentialed) by Replx. A non-positive timeout means
// no timeout (for indefinite streams such as SSE).
func TransparentClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func redactHost(u *url.URL) string {
	if u == nil {
		return "<nil>"
	}
	return u.Scheme + "://" + u.Host + "/..."
}
