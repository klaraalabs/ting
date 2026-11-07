package apps

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/providers"
)

type APNsConfig struct {
	Environment, BundleID, TeamID, KeyID string
	Key                                  []byte
}

var appleID = regexp.MustCompile(`^[A-Z0-9]{10}$`)
var bundleID = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

func (s *Service) ConfigureAPNs(ctx context.Context, appID string, c APNsConfig) error {
	if len(c.Key) > 64<<10 {
		return apierror.Invalid("APNs private key must be at most 64 KiB.")
	}
	if (c.Environment != "sandbox" && c.Environment != "production" && c.Environment != "both") || !appleID.MatchString(c.TeamID) || !appleID.MatchString(c.KeyID) || !bundleID.MatchString(c.BundleID) || len(c.BundleID) > 255 {
		return apierror.Invalid("APNs requires an environment, bundle ID, and 10-character Team and Key IDs.")
	}
	block, rest := pem.Decode(c.Key)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return apierror.Invalid("Invalid APNs PKCS#8 private key.")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return apierror.Invalid("Invalid APNs PKCS#8 private key.")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return apierror.Invalid("APNs requires a P-256 private key.")
	}
	if _, err = s.Show(ctx, appID); err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	environments := []string{c.Environment}
	if c.Environment == "both" {
		environments = []string{"sandbox", "production"}
	}
	for _, env := range environments {
		version := uuid.NewString()
		encrypted := s.Vault.Seal(appID+":apns:"+env+":"+version, c.Key)
		_, err = tx.Exec(ctx, `INSERT INTO apns_credentials(app_id,environment,bundle_id,team_id,key_id,encrypted_key,version) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(app_id,environment) DO UPDATE SET bundle_id=EXCLUDED.bundle_id,team_id=EXCLUDED.team_id,key_id=EXCLUDED.key_id,encrypted_key=EXCLUDED.encrypted_key,version=EXCLUDED.version,updated_at=now()`, appID, env, c.BundleID, c.TeamID, c.KeyID, encrypted, version)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Service) ConfigureFCM(ctx context.Context, appID, project string, raw []byte) error {
	c, err := providers.ParseServiceAccount(raw, project)
	if err != nil {
		return apierror.Invalid("Use valid RSA service-account JSON for the same Firebase project with Google's OAuth token URI; google-services.json is not a server credential.")
	}
	if _, err = s.Show(ctx, appID); err != nil {
		return err
	}
	version := uuid.NewString()
	encrypted := s.Vault.Seal(appID+":fcm:"+version, raw)
	_, err = s.DB.Exec(ctx, `INSERT INTO fcm_credentials(app_id,project_id,client_email,encrypted_json,version) VALUES($1,$2,$3,$4,$5) ON CONFLICT(app_id) DO UPDATE SET project_id=EXCLUDED.project_id,client_email=EXCLUDED.client_email,encrypted_json=EXCLUDED.encrypted_json,version=EXCLUDED.version,updated_at=now()`, appID, project, c.Email, encrypted, version)
	return err
}
