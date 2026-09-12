package spike

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Valid observation states for the compatibility matrix.
var validStates = map[string]bool{
	"UNKNOWN": true, "SUPPORTED": true, "UNSUPPORTED": true, "DEGRADED": true, "ADMIN_FORCED": true,
}

// Observation is one operator-recorded client result.
type Observation struct {
	Platform       string `json:"platform"`
	Product        string `json:"product"`
	ProductVersion string `json:"productVersion"`
	PlaybackType   string `json:"playbackType"`
	Status         string `json:"status"`
	Notes          string `json:"notes"`
}

// ObservationRow is a stored observation with aggregate counts.
type ObservationRow struct {
	Observation
	Observations int    `json:"observations"`
	UpdatedAt    string `json:"updatedAt"`
}

// Observations persists the spike matrix in compatibility_profiles.
type Observations struct {
	DB *pgxpool.Pool
}

// Record upserts one observation. Repeated records increment the counter;
// a product version change is noted but never silently inherits old
// certainty (confidence is left for the operator to raise deliberately).
func (o *Observations) Record(ctx context.Context, in Observation) error {
	in.Platform = strings.TrimSpace(in.Platform)
	in.Product = strings.TrimSpace(in.Product)
	in.PlaybackType = strings.TrimSpace(in.PlaybackType)
	in.Status = strings.ToUpper(strings.TrimSpace(in.Status))
	if in.Platform == "" || in.Product == "" || in.PlaybackType == "" {
		return fmt.Errorf("spike: platform, product and playbackType are required")
	}
	if !validStates[in.Status] {
		return fmt.Errorf("spike: invalid status %q", in.Status)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id string
	var lastVer *string
	err := o.DB.QueryRow(ctx, `SELECT id, last_observed_product_version FROM compatibility_profiles
		WHERE platform=$1 AND product=$2 AND playback_type=$3
		AND COALESCE(device_model_pattern,'')='' AND COALESCE(product_version_pattern,'')=''`,
		in.Platform, in.Product, in.PlaybackType).Scan(&id, &lastVer)
	if err != nil {
		_, err = o.DB.Exec(ctx, `INSERT INTO compatibility_profiles(platform, product, playback_type, direct_origin_status, observations, last_observed_product_version, notes)
			VALUES($1,$2,$3,$4,1,$5,$6)`, in.Platform, in.Product, in.PlaybackType, in.Status, nullIfEmpty(in.ProductVersion), nullIfEmpty(in.Notes))
		return err
	}
	note := in.Notes
	if lastVer != nil && *lastVer != "" && *lastVer != in.ProductVersion {
		note = fmt.Sprintf("version changed %s -> %s; certainty reset by operator review. %s", *lastVer, in.ProductVersion, in.Notes)
	}
	_, err = o.DB.Exec(ctx, `UPDATE compatibility_profiles SET direct_origin_status=$1, observations=observations+1,
		last_observed_product_version=$2, notes=$3, updated_at=now() WHERE id=$4`,
		in.Status, nullIfEmpty(in.ProductVersion), nullIfEmpty(note), id)
	return err
}

// Matrix returns all spike observations newest first.
func (o *Observations) Matrix(ctx context.Context) ([]ObservationRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := o.DB.Query(ctx, `SELECT platform, product, COALESCE(last_observed_product_version,''),
		playback_type, direct_origin_status, COALESCE(notes,''), observations, updated_at::text
		FROM compatibility_profiles ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObservationRow
	for rows.Next() {
		var r ObservationRow
		if err := rows.Scan(&r.Platform, &r.Product, &r.ProductVersion, &r.PlaybackType, &r.Status, &r.Notes, &r.Observations, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
