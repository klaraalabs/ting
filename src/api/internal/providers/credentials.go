// Package providers contains the credential boundary shared by push adapters.
package providers

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

var ErrNotConfigured = errors.New("provider not configured")
var ErrCredentials = errors.New("provider credentials unavailable")

type APNsCredential struct {
	Version, Environment, BundleID, TeamID, KeyID string
	PrivateKey                                    []byte
}
type FCMCredential struct {
	Version, ProjectID string
	ServiceAccount     []byte
}
type Store struct {
	DB    *pgxpool.Pool
	Vault *secrets.Vault
}

// Read on every send, including cache hits, so another process's credential
// replacement or removal takes effect without a server restart.
func (s *Store) APNs(ctx context.Context, appID, environment string) (APNsCredential, error) {
	var c APNsCredential
	var encrypted []byte
	c.Environment = environment
	err := s.DB.QueryRow(ctx, `SELECT version,bundle_id,team_id,key_id,encrypted_key FROM apns_credentials WHERE app_id=$1 AND environment=$2`, appID, environment).Scan(&c.Version, &c.BundleID, &c.TeamID, &c.KeyID, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotConfigured
	}
	if err != nil {
		return c, err
	}
	c.PrivateKey, err = s.Vault.Open(appID+":apns:"+environment+":"+c.Version, encrypted)
	if err != nil {
		return c, ErrCredentials
	}
	return c, nil
}
func (s *Store) FCM(ctx context.Context, appID string) (FCMCredential, error) {
	var c FCMCredential
	var encrypted []byte
	err := s.DB.QueryRow(ctx, `SELECT version,project_id,encrypted_json FROM fcm_credentials WHERE app_id=$1`, appID).Scan(&c.Version, &c.ProjectID, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotConfigured
	}
	if err != nil {
		return c, err
	}
	c.ServiceAccount, err = s.Vault.Open(appID+":fcm:"+c.Version, encrypted)
	if err != nil {
		return c, ErrCredentials
	}
	return c, nil
}
func CredentialFailure(err error) delivery.Result {
	if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrCredentials) {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "credential_error"}
	}
	return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
}
