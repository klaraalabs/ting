package notifications

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
)

type Counts struct {
	Queued   int `json:"queued"`
	Accepted int `json:"accepted"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}
type Status struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Counts Counts         `json:"counts"`
	Errors map[string]int `json:"errors"`
}

func (s *Service) Status(ctx context.Context, appID, id string) (Status, error) {
	result := Status{ID: id, Status: "complete", Errors: map[string]int{}}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return result, apierror.NotFound
	}
	id = parsed.String()
	result.ID = id
	// One statement gives counts and error categories from the same snapshot.
	var categories []byte
	err = s.DB.QueryRow(ctx, `SELECT
 count(d.id) FILTER(WHERE d.state='queued'),count(d.id) FILTER(WHERE d.state='accepted'),count(d.id) FILTER(WHERE d.state='failed'),count(d.id) FILTER(WHERE d.state='skipped'),
 COALESCE((SELECT jsonb_object_agg(category,total) FROM (SELECT error_category AS category,count(*) AS total FROM deliveries WHERE app_id=$1 AND notification_id=$2 AND error_category IS NOT NULL GROUP BY error_category) e),'{}'::jsonb)
 FROM notifications n LEFT JOIN deliveries d ON d.notification_id=n.id AND d.app_id=n.app_id
 WHERE n.app_id=$1 AND n.id=$2 GROUP BY n.id`, appID, id).Scan(&result.Counts.Queued, &result.Counts.Accepted, &result.Counts.Failed, &result.Counts.Skipped, &categories)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, apierror.NotFound
	}
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(categories, &result.Errors); err != nil {
		return result, err
	}
	if result.Counts.Queued > 0 {
		result.Status = "queued"
	}
	return result, nil
}
