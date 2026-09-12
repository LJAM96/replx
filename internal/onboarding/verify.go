package onboarding

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/LJAM96/replx-edge/internal/crypto"
	"github.com/LJAM96/replx-edge/internal/plextv"
	"github.com/LJAM96/replx-edge/internal/pms"
)

// SelectMediaOrigin picks the client reachable HTTPS origin connection.
// Preference: https, non-relay, non-local, and never the replx-edge public
// hostname itself. A LAN-only https connection is accepted as degraded
// (documented in the report); with no https candidate at all it fails
// closed and points at the media gateway profile.
func SelectMediaOrigin(conns []plextv.Connection, publicHost string) (string, error) {
	type candidate struct {
		uri   string
		local bool
	}
	var direct []candidate
	for _, c := range conns {
		if !strings.EqualFold(c.Protocol, "https") || c.Relay || c.URI == "" {
			continue
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.Host == "" {
			continue
		}
		if publicHost != "" && strings.EqualFold(u.Hostname(), publicHost) {
			continue
		}
		direct = append(direct, candidate{uri: strings.TrimSuffix(c.URI, "/"), local: c.Local})
	}
	for _, c := range direct {
		if !c.local {
			return c.uri, nil
		}
	}
	if len(direct) > 0 {
		return direct[0].uri, nil
	}
	return "", fmt.Errorf("onboarding: no client reachable https origin connection: fix origin TLS (plex.direct or public hostname) or enable the DNS-only media gateway profile")
}

// TripleMatch is the identity invariant: origin root, plex.tv resource and
// replx-edge proxied root must all name the same non-empty machine.
func TripleMatch(origin, resource, proxied string) bool {
	return origin != "" && origin == resource && resource == proxied
}

// CustomURLPresent reports whether the selected resource publishes a
// connection for the replx-edge public hostname.
func CustomURLPresent(resources []plextv.Resource, selectedID, publicHost string) bool {
	if publicHost == "" {
		return false
	}
	for _, r := range resources {
		if r.ClientIdentifier != selectedID {
			continue
		}
		for _, c := range r.Connections {
			u, err := url.Parse(c.URI)
			if err != nil {
				continue
			}
			if strings.EqualFold(u.Hostname(), publicHost) {
				return true
			}
		}
	}
	return false
}

// VerifyReport is the browser-safe verification result (no tokens).
type VerifyReport struct {
	Stage             string   `json:"stage"`
	OriginID          string   `json:"originMachineIdentifier"`
	ResourceID        string   `json:"resourceMachineIdentifier"`
	ProxiedID         string   `json:"proxiedMachineIdentifier"`
	Match             bool     `json:"match"`
	CustomURLPresent  bool     `json:"customUrlPresent"`
	MediaOrigin       string   `json:"mediaOrigin"`
	Checks            []string `json:"checks"`
	MediaFallbackNote string   `json:"mediaFallbackNote,omitempty"`
}

// Verify performs the triple-check and the Custom Server Access URL check.
// It fetches the proxied root through the local control listener so the
// real proxy path is exercised, not assumed.
func (s *Service) Verify(ctx context.Context) (VerifyReport, error) {
	var serverID, internalURL, mediaOrigin, machineID, resourceID string
	err := s.DB.QueryRow(ctx, `SELECT id, internal_origin_url, client_media_origin_url, machine_identifier FROM plex_servers
		WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID, &internalURL, &mediaOrigin, &machineID)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: no selected server")
	}
	var pmsCipher, ownerCipher []byte
	if err := s.DB.QueryRow(ctx, `SELECT pms_access_token_ciphertext, owner_token_ciphertext, selected_resource_id
		FROM plex_owner_credentials WHERE server_id=$1`, serverID).Scan(&pmsCipher, &ownerCipher, &resourceID); err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: no owner credentials")
	}
	pmsToken, err := crypto.Decrypt(s.Secret, PurposePMSToken, pmsCipher)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: credential decrypt failed")
	}
	defer zeroBytes(pmsToken)
	ownerToken, err := crypto.Decrypt(s.Secret, PurposeOwnerJWT, ownerCipher)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: credential decrypt failed")
	}
	defer zeroBytes(ownerToken)

	originID, err := pms.FetchIdentity(internalURL, string(pmsToken))
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: origin identity: %w", err)
	}
	id, err := s.EnsureIdentity(ctx)
	if err != nil {
		return VerifyReport{}, err
	}
	resources, err := s.NewTV(id.ClientID).ListServers(ctx, string(ownerToken))
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: resources: %w", err)
	}
	control := s.ControlBase
	if control == "" {
		control = "http://127.0.0.1:32400"
	}
	proxiedID, err := pms.FetchIdentity(control, string(pmsToken))
	if err != nil {
		return VerifyReport{}, fmt.Errorf("onboarding: proxied identity (is the control listener proxying?): %w", err)
	}
	report := VerifyReport{
		Stage:            StageVerified,
		OriginID:         originID.MachineIdentifier,
		ResourceID:       resourceID,
		ProxiedID:        proxiedID.MachineIdentifier,
		MediaOrigin:      mediaOrigin,
		CustomURLPresent: CustomURLPresent(resources, resourceID, publicHost(s.PublicURL)),
	}
	report.Match = TripleMatch(originID.MachineIdentifier, resourceID, proxiedID.MachineIdentifier) &&
		originID.MachineIdentifier == machineID
	report.Checks = []string{
		"origin root machineIdentifier read",
		"plex.tv resource resolved for the same PMS",
		"replx-edge proxied root read through the control listener",
	}
	if !report.Match {
		report.Stage = StageSelected
		return report, fmt.Errorf("onboarding: identity mismatch: origin=%q resource=%q proxied=%q",
			report.OriginID, report.ResourceID, report.ProxiedID)
	}
	if !report.CustomURLPresent {
		report.Stage = StageSelected
		report.Checks = append(report.Checks, "Custom Server Access URL NOT published: add "+s.PublicURL+" on the PMS and re-verify")
		return report, fmt.Errorf("onboarding: Custom Server Access URL %s not published for this PMS", s.PublicURL)
	}
	if _, err := s.DB.Exec(ctx, "UPDATE plex_owner_credentials SET status='verified' WHERE server_id=$1", serverID); err != nil {
		return VerifyReport{}, err
	}
	if err := setSetting(ctx, s.DB, setVerified, "true"); err != nil {
		return VerifyReport{}, err
	}
	return report, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
