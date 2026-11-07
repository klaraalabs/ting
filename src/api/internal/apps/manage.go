package apps

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
)

type UpdateRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

func (s *Service) Update(ctx context.Context, id string, r UpdateRequest) (App, error) {
	var a App
	if r.Name == nil && r.Enabled == nil {
		return a, apierror.Invalid("Provide name or enabled.")
	}
	if r.Name != nil {
		name := strings.TrimSpace(*r.Name)
		r.Name = &name
		if name == "" || len(name) > 200 || strings.ContainsRune(name, 0) {
			return a, apierror.Invalid("Provide a name of 1 to 200 bytes without NUL.")
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `UPDATE apps SET name=COALESCE($2,name),enabled=COALESCE($3,enabled),updated_at=now() WHERE id=$1 RETURNING id,name,COALESCE(slug,''),enabled,created_at`, id, r.Name, r.Enabled).Scan(&a.ID, &a.Name, &a.Slug, &a.Enabled, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, apierror.NotFound
	}
	if err != nil {
		return a, err
	}
	if !a.Enabled {
		// Cancel pending work atomically. Re-enabling an App cannot resurrect it.
		_, err = tx.Exec(ctx, `UPDATE deliveries SET state='skipped',error_category='app_disabled',lease_id=NULL,locked_until=NULL,updated_at=now() WHERE app_id=$1 AND state='queued'`, id)
		if err != nil {
			return a, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return a, err
	}
	return a, nil
}
func (s *Service) ListPage(ctx context.Context, limit, offset int) ([]App, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,COALESCE(slug,''),enabled,created_at FROM apps ORDER BY created_at DESC,id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []App{}
	for rows.Next() {
		var a App
		if err = rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Enabled, &a.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

type TokenMetadata struct {
	ID         string     `json:"tokenId"`
	Label      string     `json:"label"`
	Suffix     string     `json:"suffix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

func (s *Service) ListTokens(ctx context.Context, appID string, limit, offset int) ([]TokenMetadata, error) {
	if _, err := s.Show(ctx, appID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id,label,suffix,created_at,last_used_at,revoked_at FROM app_api_tokens WHERE app_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, appID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TokenMetadata{}
	for rows.Next() {
		var r TokenMetadata
		if err = rows.Scan(&r.ID, &r.Label, &r.Suffix, &r.CreatedAt, &r.LastUsedAt, &r.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

type IdentityMetadata struct {
	KeyID     string     `json:"kid"`
	CreatedAt time.Time  `json:"createdAt"`
	RetiredAt *time.Time `json:"retiredAt"`
}

func (s *Service) ListIdentities(ctx context.Context, appID string, limit, offset int) ([]IdentityMetadata, error) {
	if _, err := s.Show(ctx, appID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT kid,created_at,retired_at FROM app_identity_secrets WHERE app_id=$1 ORDER BY created_at DESC,kid DESC LIMIT $2 OFFSET $3`, appID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []IdentityMetadata{}
	for rows.Next() {
		var r IdentityMetadata
		if err = rows.Scan(&r.KeyID, &r.CreatedAt, &r.RetiredAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
