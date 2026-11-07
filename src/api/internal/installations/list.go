package installations

import (
	"context"
	"time"
)

type Metadata struct {
	ID             string     `json:"installationId"`
	Platform       string     `json:"platform"`
	Environment    string     `json:"apnsEnvironment,omitempty"`
	Active         bool       `json:"active"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	TokenUpdatedAt time.Time  `json:"tokenUpdatedAt"`
	InvalidatedAt  *time.Time `json:"invalidatedAt"`
}

func (s *Service) List(ctx context.Context, appID string, limit, offset int) ([]Metadata, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,platform,apns_environment,active,created_at,last_seen_at,token_updated_at,invalidated_at FROM installations WHERE app_id=$1 ORDER BY last_seen_at DESC,id DESC LIMIT $2 OFFSET $3`, appID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Metadata{}
	for rows.Next() {
		var r Metadata
		if err = rows.Scan(&r.ID, &r.Platform, &r.Environment, &r.Active, &r.CreatedAt, &r.LastSeenAt, &r.TokenUpdatedAt, &r.InvalidatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
