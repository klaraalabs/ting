package admin

import (
	"context"

	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

var errThrottled = apierror.New(429, "login_throttled", "Too many login attempts. Retry in one minute.")

func (s *Service) allowLogin(ctx context.Context, username, peer string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// A shared global budget bounds work and key growth, including across processes.
	_, err = tx.Exec(ctx, `DELETE FROM admin_login_limits WHERE expires_at<=now()`)
	if err != nil {
		return err
	}
	allowed := true
	for _, limit := range []struct {
		key     string
		maximum int
	}{{"global", 60}, {"peer:" + peer, 10}, {"username:" + username, 5}} {
		var count int
		err = tx.QueryRow(ctx, `INSERT INTO admin_login_limits(key_hash,attempts,expires_at) VALUES($1,1,now()+interval '1 minute')
   ON CONFLICT(key_hash) DO UPDATE SET attempts=CASE WHEN admin_login_limits.expires_at<=now() THEN 1 ELSE LEAST(admin_login_limits.attempts+1,61) END,
   expires_at=CASE WHEN admin_login_limits.expires_at<=now() THEN now()+interval '1 minute' ELSE admin_login_limits.expires_at END RETURNING attempts`, secrets.Hash(limit.key)).Scan(&count)
		if err != nil {
			return err
		}
		if count > limit.maximum {
			allowed = false
			break
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if !allowed {
		return errThrottled
	}
	return nil
}
