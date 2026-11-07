package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type Service struct {
	DB          *pgxpool.Pool
	DeliveryTTL time.Duration
	Limits      Limits
}
type Alert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
type SendRequest struct {
	UserIDs      []string   `json:"userIds"`
	Notification Alert      `json:"notification"`
	Data         StringData `json:"data,omitempty"`
}

// StringData rejects JSON null values, which encoding/json otherwise converts
// silently to empty strings in a map[string]string.
type StringData map[string]string

func (d *StringData) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("data must be an object of strings")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	values := make(StringData, len(fields))
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("data values must be strings")
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
		values[key] = text
	}
	*d = values
	return nil
}

type Accepted struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (r *SendRequest) Validate() error {
	if len(r.UserIDs) == 0 || len(r.UserIDs) > 1000 {
		return apierror.Invalid("userIds must contain 1 to 1000 unique IDs.")
	}
	seen := make(map[string]bool, len(r.UserIDs))
	for _, id := range r.UserIDs {
		if strings.TrimSpace(id) == "" || strings.ContainsRune(id, 0) || len(id) > 256 || seen[id] {
			return apierror.Invalid("User IDs must be unique, non-empty, and at most 256 bytes.")
		}
		seen[id] = true
	}
	return r.validatePayload()
}
func (r *SendRequest) validatePayload() error {
	if strings.TrimSpace(r.Notification.Title) == "" || strings.TrimSpace(r.Notification.Body) == "" {
		return apierror.Invalid("notification.title and notification.body are required.")
	}
	if strings.ContainsRune(r.Notification.Title, 0) || strings.ContainsRune(r.Notification.Body, 0) {
		return apierror.Invalid("Notification text cannot contain NUL characters.")
	}
	for key, value := range r.Data {
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return apierror.Invalid("data cannot contain NUL characters.")
		}
		if key == "" || key == "aps" || key == "from" || key == "gcm" || key == "google" || key == "message_type" || key == "collapse_key" || strings.HasPrefix(key, "google.") || strings.HasPrefix(key, "gcm.") {
			return apierror.Invalid("data contains a reserved key.")
		}
	}
	// APNs merges custom data into the root, FCM puts it beside notification.
	apns := map[string]any{"aps": map[string]any{"alert": r.Notification}}
	for k, v := range r.Data {
		apns[k] = v
	}
	payload, _ := json.Marshal(apns)
	if len(payload) > 4096 {
		return apierror.New(413, "payload_too_large", "APNs payload exceeds 4096 bytes.")
	}
	fcm, _ := json.Marshal(struct {
		Notification Alert             `json:"notification"`
		Data         map[string]string `json:"data,omitempty"`
		Android      map[string]string `json:"android"`
	}{r.Notification, r.Data, map[string]string{"priority": "high", "ttl": "2419200s"}})
	if len(fcm) > 4096 {
		return apierror.New(413, "payload_too_large", "FCM payload exceeds 4096 bytes.")
	}
	return nil
}
func (s *Service) Send(ctx context.Context, appID, key string, r SendRequest) (Accepted, error) {
	return s.send(ctx, appID, key, r, "")
}

// SendTest selects a persisted installation, never an arbitrary native token or
// every installation belonging to its user. Workers apply their normal fencing.
func (s *Service) SendTest(ctx context.Context, appID, key, installationID string) (Accepted, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return Accepted{}, apierror.Invalid("installationId must be a UUID.")
	}
	return s.send(ctx, appID, key, SendRequest{Notification: Alert{Title: "Ting test", Body: "Test notification from Ting."}}, id.String())
}
func (s *Service) send(ctx context.Context, appID, key string, r SendRequest, target string) (Accepted, error) {
	var result Accepted
	limits := s.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if len(key) > 200 || strings.ContainsFunc(key, func(r rune) bool { return r < 32 || r > 126 }) || (key != "" && strings.TrimSpace(key) == "") {
		return result, apierror.Invalid("Idempotency-Key must contain 1 to 200 printable bytes.")
	}
	validate := r.Validate
	if target != "" {
		validate = r.validatePayload
	}
	if err := validate(); err != nil {
		return result, err
	}
	// Recipient order and map insertion order do not change the send's meaning.
	r.UserIDs = append([]string(nil), r.UserIDs...)
	sort.Strings(r.UserIDs)
	canonical, _ := json.Marshal(r)
	source := "api"
	if target != "" {
		source = "operator_test"
		canonical, _ = json.Marshal(struct {
			Source         string `json:"source"`
			InstallationID string `json:"installationId"`
			Notification   Alert  `json:"notification"`
		}{source, target, r.Notification})
	}
	fingerprint := secrets.Hash(string(canonical))
	data := r.Data
	if data == nil {
		data = map[string]string{}
	}
	dataJSON, _ := json.Marshal(data)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	// Serialize admission across Apps and server processes. Workers only release
	// capacity, so they do not need this lock. Always lock admission before App.
	var admission bool
	if err = tx.QueryRow(ctx, `SELECT singleton FROM notification_admission WHERE singleton FOR UPDATE`).Scan(&admission); err != nil {
		return result, err
	}
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&enabled)
	if target != "" {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, apierror.NotFound
		}
		if err == nil && !enabled {
			return result, apierror.New(409, "app_disabled", "Enable the App before sending a test notification.")
		}
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
		return result, apierror.Unauthorized
	}
	if err != nil {
		return result, err
	}
	id := uuid.NewString()
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO notifications(id,app_id,title,body,data,idempotency_key,request_hash,source) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8) ON CONFLICT(app_id,idempotency_key) DO NOTHING RETURNING id`, id, appID, r.Notification.Title, r.Notification.Body, dataJSON, key, fingerprint, source).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var previous []byte
		err = tx.QueryRow(ctx, `SELECT id,request_hash FROM notifications WHERE app_id=$1 AND idempotency_key=$2`, appID, key).Scan(&id, &previous)
		if err != nil {
			return result, err
		}
		if !bytes.Equal(previous, fingerprint) {
			return result, apierror.New(409, "idempotency_conflict", "This idempotency key was used with a different request.")
		}
	} else if err != nil {
		return result, err
	} else {
		ttl := s.DeliveryTTL
		if ttl == 0 {
			ttl = delivery.DefaultTTL
		}
		if target == "" {
			var targets int
			err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM installations WHERE app_id=$1 AND external_user_id=ANY($2::text[]) AND active LIMIT $3) i`, appID, r.UserIDs, limits.MaxTargets+1).Scan(&targets)
			if err != nil {
				return result, err
			}
			if err = limits.admit(ctx, tx, appID, targets); err != nil {
				return result, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO deliveries(app_id,notification_id,installation_id,installation_revision,expires_at) SELECT app_id,$2,id,revision,clock_timestamp()+$4*interval '1 microsecond' FROM installations WHERE app_id=$1 AND external_user_id=ANY($3::text[]) AND active`, appID, id, r.UserIDs, ttl.Microseconds())
		} else {
			var active, configured bool
			var revision int64
			err = tx.QueryRow(ctx, `SELECT active,revision,CASE WHEN platform='ios' THEN EXISTS(SELECT 1 FROM apns_credentials WHERE app_id=i.app_id AND environment=i.apns_environment) ELSE EXISTS(SELECT 1 FROM fcm_credentials WHERE app_id=i.app_id) END FROM installations i WHERE app_id=$1 AND id=$2 FOR SHARE`, appID, target).Scan(&active, &revision, &configured)
			if errors.Is(err, pgx.ErrNoRows) {
				return result, apierror.NotFound
			}
			if err != nil {
				return result, err
			}
			if !active {
				return result, apierror.New(409, "installation_inactive", "Choose an active registered installation.")
			}
			if !configured {
				return result, apierror.New(409, "provider_not_configured", "Configure this installation's provider before sending a test.")
			}
			if err = limits.admit(ctx, tx, appID, 1); err != nil {
				return result, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO deliveries(app_id,notification_id,installation_id,installation_revision,expires_at) VALUES($1,$2,$3,$4,clock_timestamp()+$5*interval '1 microsecond')`, appID, id, target, revision, ttl.Microseconds())
		}
		if err != nil {
			return result, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return Accepted{id, "queued"}, nil
}
