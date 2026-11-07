package apps

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type IssuedToken struct {
	ID     string `json:"tokenId"`
	Token  string `json:"token"`
	Suffix string `json:"suffix"`
}

func (s *Service) CreateToken(ctx context.Context, appID, label string) (IssuedToken, error) {
	var result IssuedToken
	if strings.TrimSpace(label) == "" || len(label) > 200 || strings.ContainsRune(label, 0) {
		return result, apierror.Invalid("A token label of 1 to 200 bytes is required.")
	}
	if _, err := s.Show(ctx, appID); err != nil {
		return result, err
	}
	result = IssuedToken{ID: uuid.NewString(), Token: secrets.Token("ting_sk_")}
	result.Suffix = result.Token[len(result.Token)-8:]
	_, err := s.DB.Exec(ctx, `INSERT INTO app_api_tokens(id,app_id,token_hash,suffix,label) VALUES($1,$2,$3,$4,$5)`, result.ID, appID, secrets.Hash(result.Token), result.Suffix, label)
	return result, err
}
func (s *Service) RevokeToken(ctx context.Context, appID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return apierror.Invalid("Invalid token ID.")
	}
	tag, err := s.DB.Exec(ctx, `UPDATE app_api_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE app_id=$1 AND id=$2`, appID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return apierror.NotFound
	}
	return err
}

type IssuedIdentity struct {
	KeyID  string `json:"kid"`
	Secret string `json:"secret"`
}

// Existing keys remain valid until explicitly retired, allowing consumer deployments to overlap.
func (s *Service) CreateIdentity(ctx context.Context, appID string) (IssuedIdentity, error) {
	var result IssuedIdentity
	if _, err := s.Show(ctx, appID); err != nil {
		return result, err
	}
	result = IssuedIdentity{KeyID: uuid.NewString(), Secret: secrets.Token("")}
	encrypted := s.Vault.Seal(appID+":identity:"+result.KeyID, []byte(result.Secret))
	_, err := s.DB.Exec(ctx, `INSERT INTO app_identity_secrets(app_id,kid,encrypted_secret) VALUES($1,$2,$3)`, appID, result.KeyID, encrypted)
	return result, err
}
func (s *Service) RetireIdentity(ctx context.Context, appID, kid string) error {
	if _, err := uuid.Parse(kid); err != nil {
		return apierror.Invalid("Invalid identity key ID.")
	}
	tag, err := s.DB.Exec(ctx, `UPDATE app_identity_secrets SET retired_at=COALESCE(retired_at,now()) WHERE app_id=$1 AND kid=$2`, appID, kid)
	if err == nil && tag.RowsAffected() == 0 {
		return apierror.NotFound
	}
	return err
}
