package admin

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

const CookieName = "__Host-ting_session"

type Session struct {
	Operator  Operator  `json:"operator"`
	ExpiresAt time.Time `json:"expiresAt"`
	CSRFToken string    `json:"csrfToken"`
	Token     string    `json:"-"`
}

func csrfToken(token string) string {
	return base64.RawURLEncoding.EncodeToString(secrets.Hash("ting:admin:csrf:" + token))
}
func validToken(token string) bool {
	if !strings.HasPrefix(token, "ting_admin_") || len(token) != 54 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "ting_admin_"))
	return err == nil && len(raw) == 32
}
func (s *Service) Login(ctx context.Context, username, password, peer, previous string) (Session, error) {
	var out Session
	name, err := NormalizeUsername(username)
	if err != nil || ValidatePassword(password) != nil {
		return out, apierror.Unauthorized
	}
	if err = s.allowLogin(ctx, name, peer); err != nil {
		return out, err
	}
	var hash string
	err = s.DB.QueryRow(ctx, `SELECT id,username,password_hash FROM admin_users WHERE username=$1`, name).Scan(&out.Operator.ID, &out.Operator.Username, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Session{}, err
	}
	match, err := checkPassword(ctx, password, hash)
	if err != nil {
		return Session{}, err
	}
	if !match {
		return Session{}, apierror.Unauthorized
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	var current string
	// Switching operators can touch both users' sessions. Lock both users in
	// deterministic order so opposite switches cannot deadlock with cleanup.
	rows, err := tx.Query(ctx, `SELECT id,password_hash FROM admin_users WHERE id=$1 OR id=(SELECT user_id FROM admin_sessions WHERE token_hash=$2) ORDER BY id FOR UPDATE`, out.Operator.ID, secrets.Hash(previous))
	if err != nil {
		return Session{}, err
	}
	for rows.Next() {
		var id, passwordHash string
		if err = rows.Scan(&id, &passwordHash); err != nil {
			rows.Close()
			return Session{}, err
		}
		if id == out.Operator.ID {
			current = passwordHash
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Session{}, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(current)) != 1 {
		return Session{}, apierror.Unauthorized
	}
	if validToken(previous) {
		_, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, secrets.Hash(previous))
		if err != nil {
			return Session{}, err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id=$1 AND (expires_at<=now() OR revoked_at IS NOT NULL)`, out.Operator.ID)
	if err != nil {
		return Session{}, err
	}
	// Retain at most ten active sessions per operator, oldest first.
	_, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash IN (SELECT token_hash FROM admin_sessions WHERE user_id=$1 ORDER BY created_at DESC,token_hash OFFSET 9)`, out.Operator.ID)
	if err != nil {
		return Session{}, err
	}
	out.Token = secrets.Token("ting_admin_")
	err = tx.QueryRow(ctx, `INSERT INTO admin_sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+($3 * interval '1 second')) RETURNING expires_at`, secrets.Hash(out.Token), out.Operator.ID, s.Config.SessionTTL.Seconds()).Scan(&out.ExpiresAt)
	if err != nil {
		return Session{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	out.CSRFToken = csrfToken(out.Token)
	return out, nil
}
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	var out Session
	if !validToken(token) {
		return out, apierror.Unauthorized
	}
	err := s.DB.QueryRow(ctx, `SELECT u.id,u.username,s.expires_at FROM admin_sessions s JOIN admin_users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now()`, secrets.Hash(token)).Scan(&out.Operator.ID, &out.Operator.Username, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, apierror.Unauthorized
	}
	if err != nil {
		return out, err
	}
	out.Token = token
	out.CSRFToken = csrfToken(token)
	return out, nil
}
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.DB.Exec(ctx, `UPDATE admin_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, secrets.Hash(token))
	return err
}
