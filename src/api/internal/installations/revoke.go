package installations

import (
	"context"
	"crypto/subtle"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

func (s *Service) Revoke(ctx context.Context, appID, id, token string, generation int64) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return apierror.Invalid("installationId must be a UUID.")
	}
	id = parsed.String()
	if !validCredential(token) {
		return credentialInvalid
	}
	if generation <= 0 || generation > MaxGeneration {
		return apierror.Invalid("generation must be an integer between 1 and 9007199254740991.")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Logout remains available even if the operator disables the App.
	if _, err = tx.Exec(ctx, `SELECT id FROM apps WHERE id=$1 FOR UPDATE`, appID); err != nil {
		return err
	}
	var storedCredential []byte
	var currentGeneration int64
	var active bool
	err = tx.QueryRow(ctx, `SELECT credential_hash,mutation_generation,active FROM installations WHERE app_id=$1 AND id=$2 FOR UPDATE`, appID, id).Scan(&storedCredential, &currentGeneration, &active)
	if err != nil {
		if err == pgx.ErrNoRows {
			return credentialInvalid
		}
		return err
	}
	if subtle.ConstantTimeCompare(storedCredential, secrets.Hash(token)) != 1 {
		return credentialInvalid
	}
	if generation < currentGeneration {
		return superseded
	}
	if generation == currentGeneration {
		if active {
			return generationConflict
		}
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE installations SET active=false,mutation_generation=$3,revision=revision+CASE WHEN active THEN 1 ELSE 0 END,invalidated_at=COALESCE(invalidated_at,now()) WHERE app_id=$1 AND id=$2`, appID, id, generation)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE deliveries SET state='skipped',lease_id=NULL,locked_until=NULL,error_category='installation_revoked',updated_at=now() WHERE app_id=$1 AND installation_id=$2 AND state='queued'`, appID, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
