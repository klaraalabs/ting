package installations

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

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
type Registration struct {
	Platform    string `json:"platform"`
	PushToken   string `json:"pushToken"`
	Environment string `json:"apnsEnvironment,omitempty"`
	Token       string `json:"installationToken"`
	Generation  int64  `json:"generation"`
}
type Registered struct {
	ID         string `json:"installationId"`
	Token      string `json:"installationToken"`
	Status     string `json:"status"`
	Generation int64  `json:"generation"`
}

// JSON numbers must round-trip through JavaScript SDKs without losing order.
const MaxGeneration int64 = 1<<53 - 1

func (r *Registration) Validate() error {
	if r.Generation <= 0 || r.Generation > MaxGeneration {
		return apierror.Invalid("generation must be an integer between 1 and 9007199254740991.")
	}
	if !validCredential(r.Token) {
		return apierror.Invalid("installationToken must be ting_ik_ followed by 32 bytes in canonical unpadded base64url.")
	}
	if r.PushToken == "" || len(r.PushToken) > 4096 || strings.ContainsAny(r.PushToken, " \t\r\n\x00") {
		return apierror.Invalid("A native push token is required.")
	}
	switch r.Platform {
	case "ios":
		if r.Environment != "sandbox" && r.Environment != "production" {
			return apierror.Invalid("iOS requires apnsEnvironment sandbox or production.")
		}
		if _, err := hex.DecodeString(r.PushToken); err != nil || len(r.PushToken) > 512 {
			return apierror.Invalid("APNs tokens must be hexadecimal.")
		}
		r.PushToken = strings.ToLower(r.PushToken)
	case "android":
		if r.Environment != "" {
			return apierror.Invalid("Android must omit apnsEnvironment.")
		}
	default:
		return apierror.Invalid("platform must be ios or android.")
	}
	return nil
}

func validCredential(token string) bool {
	if !strings.HasPrefix(token, "ting_ik_") || len(token) != 51 {
		return false
	}
	raw := strings.TrimPrefix(token, "ting_ik_")
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}

var credentialInvalid = apierror.New(401, "installation_credential_invalid", "A valid credential for this installation is required.")
var superseded = apierror.New(409, "installation_superseded", "This installation mutation has been superseded.")
var generationConflict = apierror.New(409, "installation_generation_conflict", "This generation was already used for a different installation mutation.")

func (s *Service) Register(ctx context.Context, appID, id, user, credential string, r Registration) (Registered, bool, error) {
	var result Registered
	parsed, err := uuid.Parse(id)
	if err != nil {
		return result, false, apierror.Invalid("installationId must be a UUID.")
	}
	id = parsed.String()
	if err = r.Validate(); err != nil {
		return result, false, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, false, err
	}
	defer tx.Rollback(ctx)
	// Serialize registrations and token reassignment per App. Send takes a shared lock.
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
		return result, false, apierror.Unauthorized
	}
	if err != nil {
		return result, false, err
	}
	var configured bool
	err = tx.QueryRow(ctx, `SELECT CASE WHEN $2='ios' THEN EXISTS(SELECT 1 FROM apns_credentials WHERE app_id=$1 AND environment=$3) ELSE EXISTS(SELECT 1 FROM fcm_credentials WHERE app_id=$1) END`, appID, r.Platform, r.Environment).Scan(&configured)
	if err != nil {
		return result, false, err
	}
	if !configured {
		return result, false, apierror.New(409, "provider_not_configured", "The App has no credentials for this platform and environment.")
	}
	var oldUser, platform, environment string
	var storedCredential, oldHash []byte
	var active bool
	var transferred bool
	var generation int64
	err = tx.QueryRow(ctx, `SELECT external_user_id,credential_hash,platform,apns_environment,push_token_hash,active,mutation_generation,superseded_at IS NOT NULL FROM installations WHERE app_id=$1 AND id=$2 FOR UPDATE`, appID, id).Scan(&oldUser, &storedCredential, &platform, &environment, &oldHash, &active, &generation, &transferred)
	created := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !created {
		return result, false, err
	}
	hash := secrets.Hash(r.PushToken)
	proposedHash := secrets.Hash(r.Token)
	if !created {
		currentProof := credential != "" && subtle.ConstantTimeCompare(storedCredential, secrets.Hash(credential)) == 1
		proposedProof := subtle.ConstantTimeCompare(storedCredential, proposedHash) == 1
		// The pre-saved proposed credential recovers a lost creation/rotation response.
		// It authorizes only an exact replay, never a new mutation without currentProof.
		if !currentProof && !proposedProof {
			return result, false, credentialInvalid
		}
		// Transfer permanently retires the old UUID. Its generation counter cannot
		// order mutations against a replacement UUID, even after that owner logs out.
		if transferred {
			return result, false, superseded
		}
		if r.Generation < generation {
			return result, false, superseded
		}
		if r.Generation == generation {
			if !active {
				return result, false, superseded
			}
			if !proposedProof || oldUser != user || platform != r.Platform || environment != r.Environment || !bytes.Equal(oldHash, hash) {
				return result, false, generationConflict
			}
			return Registered{ID: id, Token: r.Token, Status: "registered", Generation: generation}, false, nil
		}
		if !currentProof {
			return result, false, credentialInvalid
		}
		if (oldUser != user || !active) && proposedProof {
			return result, false, apierror.Invalid("Reassociation and reactivation require a new installationToken.")
		}
	}
	if !created && platform != r.Platform {
		return result, false, apierror.Invalid("An installation cannot change platforms.")
	}
	// A reinstall can transfer a native token. Fence inactive associations too:
	// a delayed reactivation must not reclaim a token after logout or invalidation.
	_, err = tx.Exec(ctx, `WITH retired AS (
 UPDATE installations SET superseded_at=now(),active=false,invalidated_at=COALESCE(invalidated_at,now()),revision=revision+CASE WHEN active THEN 1 ELSE 0 END
 WHERE app_id=$1 AND id<>$2 AND platform=$3 AND apns_environment=$4 AND push_token_hash=$5 AND superseded_at IS NULL RETURNING id
 ) UPDATE deliveries SET state='skipped',lease_id=NULL,locked_until=NULL,error_category='installation_changed',updated_at=now()
 WHERE app_id=$1 AND installation_id IN (SELECT id FROM retired) AND state='queued'`, appID, id, r.Platform, r.Environment, hash)
	if err != nil {
		return result, false, err
	}
	changed := !created && (oldUser != user || !bytes.Equal(oldHash, hash) || environment != r.Environment || !active)
	if changed {
		_, err = tx.Exec(ctx, `UPDATE deliveries SET state='skipped',lease_id=NULL,locked_until=NULL,error_category='installation_changed',updated_at=now() WHERE app_id=$1 AND installation_id=$2 AND state='queued'`, appID, id)
		if err != nil {
			return result, false, err
		}
	}
	encrypted := s.Vault.Seal(appID+":installation:"+id, []byte(r.PushToken))
	_, err = tx.Exec(ctx, `INSERT INTO installations(app_id,id,external_user_id,platform,apns_environment,encrypted_push_token,push_token_hash,credential_hash,mutation_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$10)
 ON CONFLICT(app_id,id) DO UPDATE SET external_user_id=EXCLUDED.external_user_id,apns_environment=EXCLUDED.apns_environment,encrypted_push_token=EXCLUDED.encrypted_push_token,push_token_hash=EXCLUDED.push_token_hash,credential_hash=EXCLUDED.credential_hash,mutation_generation=EXCLUDED.mutation_generation,active=true,invalidated_at=NULL,last_seen_at=now(),revision=installations.revision+CASE WHEN $9 THEN 1 ELSE 0 END,token_updated_at=CASE WHEN installations.push_token_hash<>EXCLUDED.push_token_hash THEN now() ELSE installations.token_updated_at END`, appID, id, user, r.Platform, r.Environment, encrypted, hash, proposedHash, changed, r.Generation)
	if err != nil {
		return result, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, false, err
	}
	return Registered{ID: id, Token: r.Token, Status: "registered", Generation: r.Generation}, created, nil
}
