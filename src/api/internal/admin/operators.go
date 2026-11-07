package admin

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type Service struct {
	DB     *pgxpool.Pool
	Config Config
}
type Operator struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func NormalizeUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !usernamePattern.MatchString(value) {
		return "", apierror.Invalid("Username must be 3 to 64 ASCII letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
	}
	return value, nil
}
func (s *Service) Create(ctx context.Context, username, password string) (Operator, error) {
	var out Operator
	name, err := NormalizeUsername(username)
	if err != nil {
		return out, err
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return out, err
	}
	out = Operator{ID: uuid.NewString(), Username: name}
	_, err = s.DB.Exec(ctx, `INSERT INTO admin_users(id,username,password_hash) VALUES($1,$2,$3)`, out.ID, name, hash)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return Operator{}, apierror.New(409, "operator_exists", "An operator with that username already exists.")
	}
	return out, err
}
func (s *Service) ResetPassword(ctx context.Context, username, password string) error {
	name, err := NormalizeUsername(username)
	if err != nil {
		return err
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Lock order matches Login: user first, then sessions. A concurrent password
	// verification must recheck the hash under this lock before issuing a session.
	tag, err := tx.Exec(ctx, `UPDATE admin_users SET password_hash=$2,updated_at=now() WHERE username=$1`, name, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apierror.NotFound
	}
	_, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=now() WHERE user_id=(SELECT id FROM admin_users WHERE username=$1) AND revoked_at IS NULL`, name)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM admin_login_limits WHERE key_hash=$1`, secrets.Hash("username:"+name))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
