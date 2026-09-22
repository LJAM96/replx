package pms

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/origin"
)

// Identity is the origin PMS server identity Replx Edge must preserve.
type Identity struct {
	MachineIdentifier string
	FriendlyName      string
	Version           string
}

// FetchIdentity reads the PMS root (XML by default, JSON on request) with
// the owner PMS access token and returns the server identity. It fails
// closed when machineIdentifier is absent: Replx Edge must never invent
// a server identity.
func FetchIdentity(base, token string) (Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if id, err := fetchXML(ctx, base, token); err == nil {
		return id, nil
	} else {
		lastErr := err
		if id, err2 := fetchJSON(ctx, base, token); err2 == nil {
			return id, nil
		} else {
			return Identity{}, fmt.Errorf("pms: identity: xml: %v; json: %v", lastErr, err2)
		}
	}
}

type xmlRoot struct {
	MachineIdentifier string `xml:"machineIdentifier,attr"`
	FriendlyName      string `xml:"friendlyName,attr"`
	Version           string `xml:"version,attr"`
}

func fetchXML(ctx context.Context, base, token string) (Identity, error) {
	body, err := get(ctx, base, "/identity", token, "application/xml")
	if err != nil {
		// Fall back to the root document, which carries the same attrs.
		body, err = get(ctx, base, "/", token, "application/xml")
		if err != nil {
			return Identity{}, err
		}
	}
	var root xmlRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return Identity{}, err
	}
	if root.MachineIdentifier == "" {
		return Identity{}, fmt.Errorf("pms: missing machineIdentifier")
	}
	return Identity{MachineIdentifier: root.MachineIdentifier, FriendlyName: root.FriendlyName, Version: root.Version}, nil
}

type jsonRoot struct {
	MediaContainer struct {
		MachineIdentifier string `json:"machineIdentifier"`
		FriendlyName      string `json:"friendlyName"`
		Version           string `json:"version"`
	} `json:"MediaContainer"`
}

func fetchJSON(ctx context.Context, base, token string) (Identity, error) {
	body, err := get(ctx, base, "/identity", token, "application/json")
	if err != nil {
		return Identity{}, err
	}
	var root jsonRoot
	if err := json.Unmarshal(body, &root); err != nil {
		return Identity{}, err
	}
	if root.MediaContainer.MachineIdentifier == "" {
		return Identity{}, fmt.Errorf("pms: missing machineIdentifier")
	}
	return Identity{
		MachineIdentifier: root.MediaContainer.MachineIdentifier,
		FriendlyName:      root.MediaContainer.FriendlyName,
		Version:           root.MediaContainer.Version,
	}, nil
}

func get(ctx context.Context, base, path, token, accept string) ([]byte, error) {
	client, err := origin.APIClient(base, 0)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pms: status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
