package playback

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/LJAM96/replx/internal/trace"
)

// decisionRequest is the parsed universal negotiation request.
type decisionRequest struct {
	RatingKey      string
	RequestedIndex int
	SessionID      string
	ExistingMaxBR  *int
	Query          url.Values
}

// parseDecision extracts rating key, requested media index and session.
// The rating key travels URL-encoded in the path (or key) param; the media
// index defaults to 0 when the client omits it.
func parseDecision(r *http.Request) decisionRequest {
	q := r.URL.Query()
	d := decisionRequest{Query: q, SessionID: trace.ExtractSession(r)}
	// Rating key travels URL-encoded in the path (or key) param.
	d.RatingKey = trace.ExtractRatingKey(r)
	if mi := q.Get("mediaIndex"); mi != "" {
		if n, err := strconv.Atoi(mi); err == nil && n >= 0 {
			d.RequestedIndex = n
		}
	}
	if br := q.Get("maxVideoBitrate"); br != "" {
		if n, err := strconv.Atoi(br); err == nil && n > 0 {
			d.ExistingMaxBR = &n
		}
	}
	return d
}

// rewriteQuery returns the origin-bound query with the selected media
// index and output bitrate cap applied. Existing tighter client caps are
// preserved: the minimum of client and policy always wins.
func rewriteQuery(q url.Values, selected int, outputCap *int) url.Values {
	out := url.Values{}
	for k, vs := range q {
		out[k] = append([]string(nil), vs...)
	}
	out.Set("mediaIndex", strconv.Itoa(selected))
	if outputCap != nil {
		if cur := out.Get("maxVideoBitrate"); cur != "" {
			if n, err := strconv.Atoi(cur); err == nil && n < *outputCap {
				return out
			}
		}
		out.Set("maxVideoBitrate", strconv.Itoa(*outputCap))
	}
	return out
}

// decisionResponse is the validated PMS negotiation answer.
type decisionResponse struct {
	Code         int
	DirectPlay   bool
	DirectStream bool
	Transcode    bool
	MediaIndex   int
	HasIndex     bool
}

// parseDecisionResponse decodes the PMS JSON answer. Booleans decide the
// mode; the numeric code is recorded for the audit trail.
func parseDecisionResponse(body []byte) (decisionResponse, error) {
	var env struct {
		MediaContainer struct {
			Decision *struct {
				Code int    `json:"code"`
				Text string `json:"text"`
			} `json:"decision"`
			DirectPlay   bool `json:"directPlay"`
			DirectStream bool `json:"directStream"`
			Transcode    bool `json:"transcode"`
			MediaIndex   *int `json:"mediaIndex"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return decisionResponse{}, fmt.Errorf("playback: decision parse: %w", err)
	}
	mc := env.MediaContainer
	out := decisionResponse{DirectPlay: mc.DirectPlay, DirectStream: mc.DirectStream, Transcode: mc.Transcode}
	if mc.Decision != nil {
		out.Code = mc.Decision.Code
	}
	if mc.MediaIndex != nil {
		out.MediaIndex, out.HasIndex = *mc.MediaIndex, true
	}
	return out, nil
}

// modeOf names the playback mode for session persistence.
func modeOf(d decisionResponse) string {
	switch {
	case d.DirectPlay:
		return "directPlay"
	case d.DirectStream:
		return "directStream"
	default:
		return "transcode"
	}
}

// liveMedia mirrors the origin Media array for the no-index fallback path.
type liveMedia struct {
	ID              *int64 `json:"id"`
	Container       string `json:"container"`
	VideoCodec      string `json:"videoCodec"`
	VideoProfile    string `json:"videoProfile"`
	Width           *int   `json:"width"`
	Height          *int   `json:"height"`
	Bitrate         *int   `json:"bitrate"`
	VideoResolution string `json:"videoResolution"`
	DynamicRange    string `json:"dynamicRange"`
	AudioCodec      string `json:"audioCodec"`
	AudioChannels   *int   `json:"audioChannels"`
	Part            []struct {
		ID  *int64 `json:"id"`
		Key string `json:"key"`
	} `json:"Part"`
}

// parseLiveVariants decodes variants from user-scoped origin metadata.
// Playability stays unknown: without a validated capability profile the
// engine refuses to invent direct-play certainty.
func parseLiveVariants(raw []byte) ([]variantSource, error) {
	var env struct {
		MediaContainer struct {
			Metadata []struct {
				Media []liveMedia `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("playback: metadata parse: %w", err)
	}
	if len(env.MediaContainer.Metadata) == 0 {
		return nil, fmt.Errorf("playback: no metadata in origin response")
	}
	var out []variantSource
	for idx, m := range env.MediaContainer.Metadata[0].Media {
		partOK, partKey, partPlexID := false, "", ""
		for _, p := range m.Part {
			if p.Key != "" {
				partOK, partKey = true, p.Key
				if p.ID != nil {
					partPlexID = strconv.FormatInt(*p.ID, 10)
				}
				break
			}
		}
		id := ""
		if m.ID != nil {
			id = strconv.FormatInt(*m.ID, 10)
		}
		out = append(out, variantSource{
			MediaIndex: idx, PlexMediaID: id,
			Width: m.Width, Height: m.Height, BitrateKbps: m.Bitrate,
			DynamicRange: orUnknown(m.DynamicRange), VideoCodec: m.VideoCodec,
			AudioChannels: m.AudioChannels, PartAvailable: partOK,
			PartKey: partKey, PartPlexID: partPlexID,
		})
	}
	return out, nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "UNKNOWN"
	}
	return s
}
