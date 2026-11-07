package notifications

import (
	"context"
	"encoding/json"
	"time"
)

type Recent struct {
	Status
	CreatedAt time.Time `json:"createdAt"`
	Source    string    `json:"source"`
}

func (s *Service) Recent(ctx context.Context, appID string, limit, offset int) ([]Recent, error) {
	// Limit notifications before aggregating targets; one statement gives a
	// consistent snapshot without one status query per result.
	rows, err := s.DB.Query(ctx, `WITH recent AS (SELECT id,created_at,source FROM notifications WHERE app_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3)
 SELECT n.id,n.created_at,n.source,c.queued,c.accepted,c.failed,c.skipped,c.errors FROM recent n CROSS JOIN LATERAL (
 SELECT COALESCE(sum(total) FILTER(WHERE state='queued'),0)::bigint AS queued,
 COALESCE(sum(total) FILTER(WHERE state='accepted'),0)::bigint AS accepted,
 COALESCE(sum(total) FILTER(WHERE state='failed'),0)::bigint AS failed,
 COALESCE(sum(total) FILTER(WHERE state='skipped'),0)::bigint AS skipped,
 COALESCE((SELECT jsonb_object_agg(category,total) FROM (SELECT error_category AS category,count(*) AS total FROM deliveries WHERE app_id=$1 AND notification_id=n.id AND error_category IS NOT NULL GROUP BY error_category) e),'{}'::jsonb) AS errors
 FROM (SELECT state,count(*) AS total FROM deliveries WHERE app_id=$1 AND notification_id=n.id GROUP BY state) counts
 ) c ORDER BY n.created_at DESC,n.id DESC`, appID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recent{}
	for rows.Next() {
		var r Recent
		var categories []byte
		if err = rows.Scan(&r.ID, &r.CreatedAt, &r.Source, &r.Counts.Queued, &r.Counts.Accepted, &r.Counts.Failed, &r.Counts.Skipped, &categories); err != nil {
			return nil, err
		}
		r.Status.Status = "complete"
		if r.Counts.Queued > 0 {
			r.Status.Status = "queued"
		}
		if err = json.Unmarshal(categories, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
