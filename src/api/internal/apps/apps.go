package apps

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type Service struct {
	DB    *pgxpool.Pool
	Vault *secrets.Vault
}
type App struct {
	ID        string    `json:"appId"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *Service) Create(ctx context.Context, name, slug string) (App, error) {
	a := App{ID: secrets.AppID(), Name: strings.TrimSpace(name), Slug: slug}
	if a.Name == "" || len(a.Name) > 200 || strings.ContainsRune(a.Name, 0) || len(slug) > 100 || (slug != "" && !slugPattern.MatchString(slug)) {
		return a, apierror.Invalid("Provide a name up to 200 bytes and an optional lowercase, hyphenated slug.")
	}
	err := s.DB.QueryRow(ctx, `INSERT INTO apps(id,name,slug) VALUES($1,$2,NULLIF($3,'')) RETURNING enabled,created_at`, a.ID, a.Name, a.Slug).Scan(&a.Enabled, &a.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		err = apierror.New(409, "slug_conflict", "An App already uses that slug.")
	}
	return a, err
}
func (s *Service) List(ctx context.Context) ([]App, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,COALESCE(slug,''),enabled,created_at FROM apps ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []App{}
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Enabled, &a.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Service) Show(ctx context.Context, id string) (App, error) {
	var a App
	err := s.DB.QueryRow(ctx, `SELECT id,name,COALESCE(slug,''),enabled,created_at FROM apps WHERE id=$1`, id).Scan(&a.ID, &a.Name, &a.Slug, &a.Enabled, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = apierror.NotFound
	}
	return a, err
}
