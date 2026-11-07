package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type Service struct {
	DB    *pgxpool.Pool
	Vault *secrets.Vault
}

func Bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}
func (s *Service) Sender(ctx context.Context, token string) (string, error) {
	if !strings.HasPrefix(token, "ting_sk_") || len(token) != 51 {
		return "", apierror.Unauthorized
	}
	var appID string
	err := s.DB.QueryRow(ctx, `UPDATE app_api_tokens t SET last_used_at=now() FROM apps a WHERE t.app_id=a.id AND a.enabled AND t.token_hash=$1 AND t.revoked_at IS NULL RETURNING t.app_id`, secrets.Hash(token)).Scan(&appID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = apierror.Unauthorized
	}
	return appID, err
}
func (s *Service) Identity(ctx context.Context, appID, raw string) (string, error) {
	if len(raw) > 8192 || appID == "" {
		return "", apierror.Unauthorized
	}
	claims := &jwt.RegisteredClaims{}
	var lookupErr error
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok {
			return nil, apierror.Unauthorized
		}
		parsed, err := uuid.Parse(kid)
		if err != nil || parsed.String() != kid {
			return nil, apierror.Unauthorized
		}
		var encrypted []byte
		err = s.DB.QueryRow(ctx, `SELECT s.encrypted_secret FROM app_identity_secrets s JOIN apps a ON a.id=s.app_id WHERE s.app_id=$1 AND s.kid=$2 AND s.retired_at IS NULL AND a.enabled`, appID, kid).Scan(&encrypted)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				lookupErr = err
			}
			return nil, apierror.Unauthorized
		}
		key, err := s.Vault.Open(appID+":identity:"+kid, encrypted)
		if err != nil {
			lookupErr = err
		}
		return key, err
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(appID), jwt.WithAudience("ting:"+appID), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if lookupErr != nil {
		return "", lookupErr
	}
	if err != nil || !token.Valid || !ValidIdentityClaims(claims) {
		return "", apierror.Unauthorized
	}
	return claims.Subject, nil
}
func ValidIdentityClaims(c *jwt.RegisteredClaims) bool {
	return strings.TrimSpace(c.Subject) != "" && !strings.ContainsRune(c.Subject, 0) && len(c.Subject) <= 256 && c.IssuedAt != nil && c.ExpiresAt != nil && c.ExpiresAt.After(c.IssuedAt.Time) && c.ExpiresAt.Sub(c.IssuedAt.Time) <= 5*time.Minute
}
