package apps

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
)

type APNsMetadata struct {
	Environment string    `json:"environment"`
	BundleID    string    `json:"bundleId"`
	TeamID      string    `json:"teamId"`
	KeyID       string    `json:"keyId"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type APNsStatus struct {
	Status      string         `json:"status"`
	Credentials []APNsMetadata `json:"credentials"`
}

func (s *Service) APNsStatus(ctx context.Context, appID string) (APNsStatus, error) {
	out := APNsStatus{Status: "not_configured", Credentials: []APNsMetadata{}}
	if _, err := s.Show(ctx, appID); err != nil {
		return out, err
	}
	rows, err := s.DB.Query(ctx, `SELECT environment,bundle_id,team_id,key_id,updated_at FROM apns_credentials WHERE app_id=$1 ORDER BY environment`, appID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c APNsMetadata
		if err = rows.Scan(&c.Environment, &c.BundleID, &c.TeamID, &c.KeyID, &c.UpdatedAt); err != nil {
			return out, err
		}
		out.Credentials = append(out.Credentials, c)
	}
	if len(out.Credentials) > 0 {
		out.Status = "configured_untested"
	}
	return out, rows.Err()
}

type FCMMetadata struct {
	ProjectID   string    `json:"projectId"`
	ClientEmail string    `json:"clientEmail"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type FCMStatus struct {
	Status     string       `json:"status"`
	Credential *FCMMetadata `json:"credential"`
}

func (s *Service) FCMStatus(ctx context.Context, appID string) (FCMStatus, error) {
	out := FCMStatus{Status: "not_configured"}
	if _, err := s.Show(ctx, appID); err != nil {
		return out, err
	}
	var c FCMMetadata
	err := s.DB.QueryRow(ctx, `SELECT project_id,client_email,updated_at FROM fcm_credentials WHERE app_id=$1`, appID).Scan(&c.ProjectID, &c.ClientEmail, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Status = "configured_untested"
	out.Credential = &c
	return out, nil
}
func (s *Service) RemoveAPNs(ctx context.Context, appID, environment string) error {
	if environment != "sandbox" && environment != "production" && environment != "both" {
		return apierror.Invalid("environment must be sandbox, production, or both.")
	}
	if _, err := s.Show(ctx, appID); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM apns_credentials WHERE app_id=$1 AND ($2='both' OR environment=$2)`, appID, environment)
	return err
}
func (s *Service) RemoveFCM(ctx context.Context, appID string) error {
	if _, err := s.Show(ctx, appID); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM fcm_credentials WHERE app_id=$1`, appID)
	return err
}
