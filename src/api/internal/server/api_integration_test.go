package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/dbmigrations"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

type fixture struct {
	db      *pgxpool.Pool
	service *apps.Service
	handler http.Handler
}
type appFixture struct{ id, token, kid, secret string }

func setup(t *testing.T) *fixture {
	t.Helper()
	databaseURL := os.Getenv("TING_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TING_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
	})
	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsedURL.Query()
	query.Set("search_path", schema)
	parsedURL.RawQuery = query.Encode()
	cfg, err := pgxpool.ParseConfig(parsedURL.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 10
	// Running twice also verifies restart migrations are harmless.
	for i := 0; i < 2; i++ {
		if err = dbmigrations.Migrate(ctx, cfg.ConnString()); err != nil {
			t.Fatal(err)
		}
	}
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	service := &apps.Service{DB: db, Vault: vault}
	return &fixture{db, service, server.New(db, vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())}
}
func (f *fixture) app(t *testing.T, configured bool) appFixture {
	t.Helper()
	ctx := context.Background()
	a, err := f.service.Create(ctx, "Test App", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.service.CreateToken(ctx, a.ID, "backend")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.service.CreateIdentity(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		err = f.service.ConfigureAPNs(ctx, a.ID, apps.APNsConfig{Environment: "production", BundleID: "com.example.ting", TeamID: "TEAM123456", KeyID: "KEY1234567", Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})})
		if err != nil {
			t.Fatal(err)
		}
	}
	return appFixture{a.ID, token.Token, identity.KeyID, identity.Secret}
}
func identity(a appFixture, user string, change func(*jwt.RegisteredClaims)) string {
	now := time.Now()
	claims := jwt.RegisteredClaims{Issuer: a.id, Subject: user, Audience: jwt.ClaimStrings{"ting:" + a.id}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute))}
	if change != nil {
		change(&claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = a.kid
	raw, _ := token.SignedString([]byte(a.secret))
	return raw
}
func (f *fixture) request(method, path, token, appID, installationToken, key string, body any) *httptest.ResponseRecorder {
	// Existing scenarios use generation 1 for direct registrations and 100 for logout.
	// Recovery/ordering tests below construct explicit generations without this helper.
	if registration, ok := body.(installations.Registration); ok {
		if registration.Token == "" {
			registration.Token = secrets.Token("ting_ik_")
		}
		if registration.Generation == 0 {
			registration.Generation = 1
		}
		body = registration
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "https://ting.test"+path, bytes.NewReader(raw))
	req.TLS = &tls.ConnectionState{}
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Ting-App-Id", appID)
	req.Header.Set("X-Ting-Installation-Token", installationToken)
	if method == "DELETE" {
		req.Header.Set("X-Ting-Installation-Generation", "100")
	}
	req.Header.Set("Idempotency-Key", key)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	return recorder
}
func expect(t *testing.T, r *httptest.ResponseRecorder, code int) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("got HTTP %d, want %d: %s", r.Code, code, r.Body.String())
	}
}
func decode[T any](t *testing.T, r *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *fixture) register(t *testing.T, a appFixture, user, id, credential, push string) installations.Registered {
	t.Helper()
	proposed, generation := secrets.Token("ting_ik_"), int64(1)
	if credential != "" {
		var oldUser string
		var active bool
		if err := f.db.QueryRow(context.Background(), `SELECT external_user_id,active,mutation_generation+1 FROM installations WHERE app_id=$1 AND id=$2`, a.id, id).Scan(&oldUser, &active, &generation); err != nil {
			t.Fatal(err)
		}
		if oldUser == user && active {
			proposed = credential
		}
	}
	r := f.request("PUT", "/v1/installations/"+id, identity(a, user, nil), a.id, credential, "", installations.Registration{Platform: "ios", PushToken: push, Environment: "production", Token: proposed, Generation: generation})
	code := 201
	if credential != "" {
		code = 200
	}
	expect(t, r, code)
	return decode[installations.Registered](t, r)
}
func (f *fixture) send(t *testing.T, a appFixture, key string, users ...string) notifications.Accepted {
	t.Helper()
	r := f.request("POST", "/v1/notifications", a.token, "", "", key, notifications.SendRequest{UserIDs: users, Notification: notifications.Alert{Title: "Test", Body: "Hello"}})
	expect(t, r, 202)
	return decode[notifications.Accepted](t, r)
}
func (f *fixture) status(t *testing.T, a appFixture, id string) notifications.Status {
	t.Helper()
	r := f.request("GET", "/v1/notifications/"+id, a.token, "", "", "", nil)
	expect(t, r, 200)
	return decode[notifications.Status](t, r)
}

func TestAPIIntegration(t *testing.T) {
	f := setup(t)
	t.Run("fanout isolation lifecycle and encrypted storage", func(t *testing.T) {
		a := f.app(t, true)
		b := f.app(t, true)
		one := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("a", 64))
		two := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("b", 64))
		f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("c", 64))
		f.register(t, b, "alice", uuid.NewString(), "", strings.Repeat("d", 64))
		send := f.send(t, a, "first", "alice")
		if status := f.status(t, a, send.ID); status.Counts.Queued != 3 {
			t.Fatalf("fanout: %+v", status)
		}
		expect(t, f.request("GET", "/v1/notifications/"+send.ID, b.token, "", "", "", nil), 404)
		expect(t, f.request("POST", "/v1/notifications", one.Token, "", "", "", nil), 401)
		management := f.request("GET", "/admin/api/v1/apps", a.token, "", "", "", nil)
		if management.Code != 401 && management.Code != 403 && management.Code != 404 {
			t.Fatalf("sender token was not rejected by management: %d", management.Code)
		}
		expect(t, f.request("DELETE", "/v1/installations/"+two.ID, one.Token, a.id, "", "", nil), 401)
		expect(t, f.request("DELETE", "/v1/installations/"+one.ID, one.Token, b.id, "", "", nil), 401)
		stolen := f.request("PUT", "/v1/installations/"+one.ID, identity(a, "mallory", nil), a.id, "", "", installations.Registration{Platform: "ios", PushToken: strings.Repeat("a", 64), Environment: "production"})
		expect(t, stolen, 401)
		updated := f.register(t, a, "alice", one.ID, one.Token, strings.Repeat("e", 64))
		if updated.Token != one.Token {
			t.Fatal("same-user refresh rotated credential")
		}
		switched := f.register(t, a, "bob", one.ID, one.Token, strings.Repeat("e", 64))
		if switched.Token == one.Token {
			t.Fatal("account switch did not rotate credential")
		}
		expect(t, f.request("DELETE", "/v1/installations/"+one.ID, one.Token, a.id, "", "", nil), 401)
		if status := f.status(t, a, send.ID); status.Counts.Skipped != 1 || status.Counts.Queued != 2 {
			t.Fatalf("stale work not cancelled: %+v", status)
		}
		if status := f.status(t, a, f.send(t, a, "second", "alice").ID); status.Counts.Queued != 2 {
			t.Fatalf("old user still receives: %+v", status)
		}
		expect(t, f.request("DELETE", "/v1/installations/"+two.ID, two.Token, a.id, "", "", nil), 204)
		expect(t, f.request("DELETE", "/v1/installations/"+two.ID, two.Token, a.id, "", "", nil), 204)
		if status := f.status(t, a, f.send(t, a, "third", "alice").ID); status.Counts.Queued != 1 {
			t.Fatalf("logout affected wrong devices: %+v", status)
		}
		// A reinstall owning the same native token must not create duplicate fanout.
		f.register(t, a, "bob", uuid.NewString(), "", strings.Repeat("e", 64))
		if status := f.status(t, a, f.send(t, a, "bob", "bob").ID); status.Counts.Queued != 1 {
			t.Fatalf("duplicate native token: %+v", status)
		}
		var raw, hash []byte
		if err := f.db.QueryRow(context.Background(), `SELECT encrypted_push_token,credential_hash FROM installations WHERE app_id=$1 AND id=$2`, a.id, one.ID).Scan(&raw, &hash); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(strings.Repeat("e", 64))) || bytes.Equal(hash, []byte(switched.Token)) {
			t.Fatal("plaintext credentials persisted")
		}
	})
	t.Run("idempotency concurrency and fixed recipients", func(t *testing.T) {
		a := f.app(t, true)
		f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("f", 64))
		const count = 16
		responses := make(chan *httptest.ResponseRecorder, count)
		var wg sync.WaitGroup
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				responses <- f.request("POST", "/v1/notifications", a.token, "", "", "same", notifications.SendRequest{UserIDs: []string{"alice", "unknown"}, Notification: notifications.Alert{Title: "Test", Body: "Hello"}})
			}()
		}
		wg.Wait()
		close(responses)
		id := ""
		for r := range responses {
			expect(t, r, 202)
			got := decode[notifications.Accepted](t, r)
			if id != "" && got.ID != id {
				t.Fatal("concurrent duplicate notification")
			}
			id = got.ID
		}
		reordered := f.send(t, a, "same", "unknown", "alice")
		if reordered.ID != id {
			t.Fatal("order changed fingerprint")
		}
		expect(t, f.request("POST", "/v1/notifications", a.token, "", "", "same", notifications.SendRequest{UserIDs: []string{"alice"}, Notification: notifications.Alert{Title: "changed", Body: "Hello"}}), 409)
		f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("1", 64))
		f.send(t, a, "same", "alice", "unknown")
		if status := f.status(t, a, id); status.Counts.Queued != 1 {
			t.Fatalf("retry retargeted new installation: %+v", status)
		}
		empty := f.status(t, a, f.send(t, a, "unknown", "nobody").ID)
		if empty.Status != "complete" || empty.Counts != (notifications.Counts{}) {
			t.Fatalf("unknown recipient: %+v", empty)
		}
		b := f.app(t, true)
		if other := f.send(t, b, "same", "alice", "unknown"); other.ID == id {
			t.Fatal("cross-App idempotency")
		}
	})
	t.Run("identity verification and key overlap", func(t *testing.T) {
		a := f.app(t, true)
		path := "/v1/installations/" + uuid.NewString()
		body := installations.Registration{Platform: "ios", PushToken: strings.Repeat("2", 64), Environment: "production"}
		tests := []struct {
			name   string
			change func(*jwt.RegisteredClaims)
		}{
			{"expired", func(c *jwt.RegisteredClaims) {
				c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-6 * time.Minute))
			}},
			{"wrong issuer", func(c *jwt.RegisteredClaims) { c.Issuer = "other" }},
			{"wrong audience", func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"ting:other"} }},
			{"missing issued at", func(c *jwt.RegisteredClaims) { c.IssuedAt = nil }},
			{"missing expiry", func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }},
			{"future issued at", func(c *jwt.RegisteredClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Minute)) }},
			{"long lifetime", func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(6 * time.Minute)) }},
			{"empty subject", func(c *jwt.RegisteredClaims) { c.Subject = "" }},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				expect(t, f.request("PUT", path, identity(a, "alice", test.change), a.id, "", "", body), 401)
			})
		}
		bad := a
		bad.secret = "wrong"
		expect(t, f.request("PUT", path, identity(bad, "alice", nil), a.id, "", "", body), 401)
		bad = a
		bad.kid = uuid.NewString()
		expect(t, f.request("PUT", path, identity(bad, "alice", nil), a.id, "", "", body), 401)
		expect(t, f.request("PUT", path, a.token, a.id, "", "", body), 401)
		unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": a.id, "sub": "alice"})
		unsigned.Header["kid"] = a.kid
		raw, _ := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
		expect(t, f.request("PUT", path, raw, a.id, "", "", body), 401)
		replacement, err := f.service.CreateIdentity(context.Background(), a.id)
		if err != nil {
			t.Fatal(err)
		}
		f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("3", 64))
		newer := a
		newer.kid = replacement.KeyID
		newer.secret = replacement.Secret
		f.register(t, newer, "alice", uuid.NewString(), "", strings.Repeat("4", 64))
		if err = f.service.RetireIdentity(context.Background(), a.id, a.kid); err != nil {
			t.Fatal(err)
		}
		expect(t, f.request("PUT", path, identity(a, "alice", nil), a.id, "", "", body), 401)
	})
	t.Run("provider gating validation and disabled App", func(t *testing.T) {
		a := f.app(t, false)
		expect(t, f.request("PUT", "/v1/installations/"+uuid.NewString(), identity(a, "alice", nil), a.id, "", "", installations.Registration{Platform: "ios", PushToken: strings.Repeat("5", 64), Environment: "production"}), 409)
		a = f.app(t, true)
		expect(t, f.request("PUT", "/v1/installations/"+uuid.NewString(), identity(a, "alice", nil), a.id, "", "", installations.Registration{Platform: "ios", PushToken: strings.Repeat("5", 64), Environment: "sandbox"}), 409)
		err := f.service.ConfigureFCM(context.Background(), a.id, "test", []byte(`{"project_info":{"project_id":"test"}}`))
		if err == nil {
			t.Fatal("accepted google-services.json")
		}
		for _, body := range []any{map[string]any{"userIds": []string{"a"}, "notification": map[string]string{"title": "Title", "body": "Body"}, "extra": true}, map[string]any{"userIds": []string{"a"}, "notification": map[string]string{"title": "Title", "body": "Body"}, "data": map[string]any{"x": 1}}} {
			expect(t, f.request("POST", "/v1/notifications", a.token, "", "", "", body), 400)
		}
		if _, err = f.db.Exec(context.Background(), `UPDATE apps SET enabled=false WHERE id=$1`, a.id); err != nil {
			t.Fatal(err)
		}
		expect(t, f.request("POST", "/v1/notifications", a.token, "", "", "", nil), 401)
	})
}

func TestConcurrentMigrations(t *testing.T) {
	// setup applies migrations twice; this checks independent server startup calls too.
	f := setup(t)
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			failures <- dbmigrations.Migrate(ctx, f.db.Config().ConnString())
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(fmt.Errorf("concurrent migration: %w", err))
		}
	}
}

func TestFCMConfigurationAndSenderRotation(t *testing.T) {
	f := setup(t)
	a := f.app(t, false)
	b := f.app(t, false)
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "example-project", "client_email": "ting@example-project.iam.gserviceaccount.com", "private_key": privateKey, "token_uri": "https://oauth2.googleapis.com/token"})
	if err = f.service.ConfigureFCM(ctx, a.id, "different-project", raw); err == nil {
		t.Fatal("accepted mismatched project")
	}
	if err = f.service.ConfigureFCM(ctx, a.id, "example-project", raw); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	var before string
	if err = f.db.QueryRow(ctx, `SELECT encrypted_json,version FROM fcm_credentials WHERE app_id=$1`, a.id).Scan(&encrypted, &before); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("PRIVATE KEY")) || bytes.Equal(encrypted, raw) {
		t.Fatal("plaintext FCM credentials stored")
	}
	decrypted, err := f.service.Vault.Open(a.id+":fcm:"+before, encrypted)
	if err != nil || !bytes.Equal(decrypted, raw) {
		t.Fatalf("stored key cannot decrypt: %v", err)
	}
	if _, err = f.service.Vault.Open(b.id+":fcm:"+before, encrypted); err == nil {
		t.Fatal("FCM key not bound to App")
	}
	if err = f.service.ConfigureFCM(ctx, a.id, "example-project", raw); err != nil {
		t.Fatal(err)
	}
	var after string
	if err = f.db.QueryRow(ctx, `SELECT version FROM fcm_credentials WHERE app_id=$1`, a.id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("replacement did not change credential version")
	}
	registration := installations.Registration{Platform: "android", PushToken: "native-fcm-token"}
	expect(t, f.request("PUT", "/v1/installations/"+uuid.NewString(), identity(a, "alice", nil), a.id, "", "", registration), 201)
	expect(t, f.request("PUT", "/v1/installations/"+uuid.NewString(), identity(b, "alice", nil), b.id, "", "", registration), 409)
	replacement, err := f.service.CreateToken(ctx, a.id, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	second := a
	second.token = replacement.Token
	f.send(t, a, "old", "alice")
	f.send(t, second, "new", "alice")
	var originalID string
	if err = f.db.QueryRow(ctx, `SELECT id FROM app_api_tokens WHERE token_hash=$1`, secrets.Hash(a.token)).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	if err = f.service.RevokeToken(ctx, a.id, originalID); err != nil {
		t.Fatal(err)
	}
	expect(t, f.request("POST", "/v1/notifications", a.token, "", "", "", nil), 401)
	f.send(t, second, "still-valid", "alice")
}
