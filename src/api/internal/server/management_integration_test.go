package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

type managementFixture struct {
	*fixture
	cookie *http.Cookie
	csrf   string
}
type page[T any] struct {
	Items      []T  `json:"items"`
	NextOffset *int `json:"nextOffset"`
}

func managementSetup(t *testing.T) *managementFixture {
	t.Helper()
	f := setup(t)
	if _, err := operatorService(f).Create(context.Background(), "admin", operatorPassword); err != nil {
		t.Fatal(err)
	}
	cookie, session := operatorLogin(t, f, nil)
	return &managementFixture{f, cookie, session.CSRFToken}
}
func (m *managementFixture) request(method, path string, body any, change func(*http.Request)) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, adminOrigin+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12000"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", adminOrigin)
	r.Header.Set("X-Ting-CSRF", m.csrf)
	r.AddCookie(m.cookie)
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	m.handler.ServeHTTP(w, r)
	return w
}
func (m *managementFixture) createApp(t *testing.T, name string) apps.App {
	t.Helper()
	w := m.request("POST", "/admin/api/v1/apps", map[string]string{"name": name}, nil)
	expect(t, w, 201)
	return decode[apps.App](t, w)
}
func appPath(id string) string { return "/admin/api/v1/apps/" + id }
func apnsInput(t *testing.T) map[string]string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"environment": "both", "bundleId": "com.example.ting", "teamId": "TEAM123456", "keyId": "KEY1234567", "privateKey": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
}
func fcmInput(t *testing.T) map[string]any {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"projectId": "example-project", "serviceAccount": map[string]string{"type": "service_account", "project_id": "example-project", "client_email": "ting@example-project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": "https://oauth2.googleapis.com/token"}}
}
func assertNoSecrets(t *testing.T, w *httptest.ResponseRecorder, values ...string) {
	t.Helper()
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("management response cacheable")
	}
	for _, value := range append(values, "PRIVATE KEY", "encrypted_key", "encrypted_json", "password_hash", "token_hash", "credential_hash", "encrypted_secret", "encrypted_push_token") {
		if value != "" && strings.Contains(w.Body.String(), value) {
			t.Fatalf("management response contains sensitive data: %s", value[:min(len(value), 12)])
		}
	}
}
func TestManagementAppAndCredentialLifecycle(t *testing.T) {
	m := managementSetup(t)
	ctx := context.Background()
	a := m.createApp(t, "First")
	b := m.createApp(t, "Second")
	path := appPath(a.ID)
	w := m.request("GET", "/admin/api/v1/apps?limit=1", nil, nil)
	expect(t, w, 200)
	first := decode[page[apps.App]](t, w)
	if len(first.Items) != 1 || first.NextOffset == nil || *first.NextOffset != 1 {
		t.Fatal("first App page missing continuation")
	}
	w = m.request("GET", "/admin/api/v1/apps?limit=1&offset=1", nil, nil)
	expect(t, w, 200)
	second := decode[page[apps.App]](t, w)
	if len(second.Items) != 1 || second.NextOffset != nil || first.Items[0].ID == second.Items[0].ID {
		t.Fatal("App pagination duplicated or omitted a row")
	}
	w = m.request("PATCH", path, map[string]any{"name": "Renamed"}, nil)
	expect(t, w, 200)
	if decode[apps.App](t, w).Name != "Renamed" {
		t.Fatal("rename not applied")
	}
	for _, body := range []any{map[string]any{}, map[string]string{"name": " "}, map[string]string{"name": "a\x00b"}, map[string]any{"enabled": "false"}, map[string]any{"unknown": true}} {
		expect(t, m.request("PATCH", path, body, nil), 400)
	}
	expect(t, m.request("POST", "/admin/api/v1/apps", map[string]string{"name": "slug", "slug": "same-slug"}, nil), 201)
	expect(t, m.request("POST", "/admin/api/v1/apps", map[string]string{"name": "slug", "slug": "same-slug"}, nil), 409)
	for _, query := range []string{"?limit=0", "?limit=101", "?offset=-1", "?offset=100001", "?limit=1&limit=2"} {
		expect(t, m.request("GET", "/admin/api/v1/apps"+query, nil, nil), 400)
	}
	expect(t, m.request("GET", appPath("missing"), nil, nil), 404)
	w = m.request("POST", path+"/tokens", map[string]string{"label": "backend"}, nil)
	expect(t, w, 201)
	token := decode[apps.IssuedToken](t, w)
	w = m.request("POST", path+"/identity-secrets", map[string]any{}, nil)
	expect(t, w, 201)
	identityKey := decode[apps.IssuedIdentity](t, w)
	w = m.request("GET", path+"/tokens", nil, nil)
	expect(t, w, 200)
	assertNoSecrets(t, w, token.Token, identityKey.Secret)
	if list := decode[page[apps.TokenMetadata]](t, w); len(list.Items) != 1 || list.Items[0].Suffix != token.Suffix || list.Items[0].RevokedAt != nil {
		t.Fatal("token metadata incorrect")
	}
	w = m.request("GET", path+"/identity-secrets", nil, nil)
	expect(t, w, 200)
	assertNoSecrets(t, w, token.Token, identityKey.Secret)
	expect(t, m.request("DELETE", appPath(b.ID)+"/tokens/"+token.ID, nil, nil), 404)
	expect(t, m.request("DELETE", appPath(b.ID)+"/identity-secrets/"+identityKey.KeyID, nil, nil), 404)
	for i := 0; i < 2; i++ {
		expect(t, m.request("DELETE", path+"/tokens/"+token.ID, nil, nil), 204)
		expect(t, m.request("DELETE", path+"/identity-secrets/"+identityKey.KeyID, nil, nil), 204)
	}
	w = m.request("GET", path+"/tokens", nil, nil)
	if decode[page[apps.TokenMetadata]](t, w).Items[0].RevokedAt == nil {
		t.Fatal("revoked token metadata missing")
	}
	w = m.request("GET", path+"/identity-secrets", nil, nil)
	if decode[page[apps.IdentityMetadata]](t, w).Items[0].RetiredAt == nil {
		t.Fatal("retired identity metadata missing")
	}
	expect(t, m.fixture.request("POST", "/v1/notifications", token.Token, "", "", "", notifications.SendRequest{UserIDs: []string{"user"}, Notification: notifications.Alert{Title: "Test", Body: "Body"}}), 401)
	for _, suffix := range []string{"/apns", "/fcm"} {
		w = m.request("GET", path+suffix, nil, nil)
		expect(t, w, 200)
		if !strings.Contains(w.Body.String(), "not_configured") {
			t.Fatal("unconfigured provider misreported")
		}
	}
	apple := apnsInput(t)
	expect(t, m.request("PUT", path+"/apns", apple, nil), 200)
	var previousVersion string
	if err := m.db.QueryRow(ctx, `SELECT version FROM apns_credentials WHERE app_id=$1 AND environment='production'`, a.ID).Scan(&previousVersion); err != nil {
		t.Fatal(err)
	}
	apple["keyId"] = "NEWKEY1234"
	expect(t, m.request("PUT", path+"/apns", apple, nil), 200)
	var version string
	if err := m.db.QueryRow(ctx, `SELECT version FROM apns_credentials WHERE app_id=$1 AND environment='production'`, a.ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version == previousVersion {
		t.Fatal("credential replacement did not rotate version")
	}
	w = m.request("GET", path+"/apns", nil, nil)
	expect(t, w, 200)
	assertNoSecrets(t, w, apple["privateKey"])
	status := decode[apps.APNsStatus](t, w)
	if status.Status != "configured_untested" || len(status.Credentials) != 2 || status.Credentials[0].KeyID != "NEWKEY1234" {
		t.Fatal("APNs metadata incorrect")
	}
	google := fcmInput(t)
	expect(t, m.request("PUT", path+"/fcm", google, nil), 200)
	w = m.request("GET", path+"/fcm", nil, nil)
	expect(t, w, 200)
	assertNoSecrets(t, w, google["serviceAccount"].(map[string]string)["private_key"])
	if status := decode[apps.FCMStatus](t, w); status.Status != "configured_untested" || status.Credential.ProjectID != "example-project" {
		t.Fatal("FCM metadata incorrect")
	}
	// Provider material remains encrypted after the HTTP upload path.
	var encrypted []byte
	if err := m.db.QueryRow(ctx, `SELECT encrypted_json FROM fcm_credentials WHERE app_id=$1`, a.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("PRIVATE KEY")) || bytes.Contains(encrypted, []byte("private_key")) {
		t.Fatal("provider material stored as plaintext")
	}
	expect(t, m.request("DELETE", path+"/apns", nil, nil), 400)
	expect(t, m.request("DELETE", path+"/apns?environment=sandbox", nil, nil), 204)
	w = m.request("GET", path+"/apns", nil, nil)
	if len(decode[apps.APNsStatus](t, w).Credentials) != 1 {
		t.Fatal("APNs delete removed wrong environments")
	}
	for i := 0; i < 2; i++ {
		expect(t, m.request("DELETE", path+"/apns?environment=both", nil, nil), 204)
		expect(t, m.request("DELETE", path+"/fcm", nil, nil), 204)
	}
	for _, suffix := range []string{"/apns", "/fcm"} {
		w = m.request("GET", appPath(b.ID)+suffix, nil, nil)
		if !strings.Contains(w.Body.String(), "not_configured") {
			t.Fatal("credential crossed App boundary")
		}
	}
	apple["privateKey"] = strings.Repeat("x", 65537)
	expect(t, m.request("PUT", path+"/apns", apple, nil), 400)
	google["serviceAccount"].(map[string]string)["token_uri"] = "https://attacker.invalid/token"
	expect(t, m.request("PUT", path+"/fcm", google, nil), 400)
	expect(t, m.request("PUT", path+"/fcm", map[string]string{"projectId": "example-project", "serviceAccount": strings.Repeat("x", 600000)}, nil), 413)
}

type managementSender struct {
	mu       sync.Mutex
	messages []delivery.Message
}

func (s *managementSender) Send(_ context.Context, m delivery.Message) delivery.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return delivery.Result{Outcome: delivery.Accepted}
}
func TestManagementSingleInstallationSendAndDisable(t *testing.T) {
	m := managementSetup(t)
	ctx := context.Background()
	a := m.app(t, true)
	b := m.app(t, true)
	path := appPath(a.id)
	one := m.register(t, a, "private-user", uuid.NewString(), "", strings.Repeat("a", 64))
	two := m.register(t, a, "private-user", uuid.NewString(), "", strings.Repeat("b", 64))
	other := m.register(t, b, "private-user", uuid.NewString(), "", strings.Repeat("c", 64))
	w := m.request("GET", path+"/installations?limit=1", nil, nil)
	expect(t, w, 200)
	devices := decode[page[installations.Metadata]](t, w)
	if len(devices.Items) != 1 || devices.NextOffset == nil {
		t.Fatal("installation pagination failed")
	}
	assertNoSecrets(t, w, one.Token, two.Token, "private-user", strings.Repeat("a", 64))
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": other.ID}, nil), 404)
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": uuid.NewString()}, nil), 404)
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": one.ID, "pushToken": "unverified"}, nil), 400)
	var before int
	if err := m.db.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatal("rejected tests persisted notification rows")
	}
	key := func(r *http.Request) { r.Header.Set("Idempotency-Key", "operator-test-key") }
	w = m.request("POST", path+"/test-notifications", map[string]string{"installationId": one.ID}, key)
	expect(t, w, 202)
	sent := decode[notifications.Accepted](t, w)
	w = m.request("POST", path+"/test-notifications", map[string]string{"installationId": one.ID}, key)
	expect(t, w, 202)
	if decode[notifications.Accepted](t, w).ID != sent.ID {
		t.Fatal("test retry duplicated notification")
	}
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": two.ID}, key), 409)
	expect(t, m.fixture.request("POST", "/v1/notifications", a.token, "", "", "operator-test-key", notifications.SendRequest{UserIDs: []string{"private-user"}, Notification: notifications.Alert{Title: "Ting test", Body: "Test notification from Ting."}}), 409)
	w = m.request("GET", path+"/notifications/"+sent.ID, nil, nil)
	expect(t, w, 200)
	if decode[notifications.Status](t, w).Counts.Queued != 1 {
		t.Fatal("test send targeted more than one device")
	}
	expect(t, m.request("GET", appPath(b.id)+"/notifications/"+sent.ID, nil, nil), 404)
	w = m.request("GET", path+"/notifications", nil, nil)
	expect(t, w, 200)
	recent := decode[page[notifications.Recent]](t, w)
	if len(recent.Items) != 1 || recent.Items[0].Source != "operator_test" || recent.Items[0].Counts.Queued != 1 {
		t.Fatal("recent test-send summary incorrect")
	}
	assertNoSecrets(t, w, one.Token, "private-user")
	c := delivery.DefaultConfig()
	c.Concurrency = 1
	c.PollInterval = 10 * time.Millisecond
	sender := &managementSender{}
	pool, err := delivery.NewPool(&delivery.Store{DB: m.db, Vault: m.service.Vault}, map[string]delivery.Sender{"ios": sender}, c)
	if err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); pool.Run(run) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := (&notifications.Service{DB: m.db}).Status(ctx, a.id, sent.ID)
		if err == nil && status.Counts.Accepted == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("test delivery not accepted: %+v %v", status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	sender.mu.Lock()
	if len(sender.messages) != 1 || sender.messages[0].InstallationID != one.ID || sender.messages[0].PushToken != strings.Repeat("a", 64) {
		t.Fatal("worker sent test to wrong installation")
	}
	sender.mu.Unlock()
	w = m.request("GET", path+"/notifications", nil, nil)
	expect(t, w, 200)
	if decode[page[notifications.Recent]](t, w).Items[0].Counts.Accepted != 1 {
		t.Fatal("recent outcomes did not update")
	}
	// Disabling an App cancels queued work and fences an already claimed attempt.
	w = m.request("POST", path+"/test-notifications", map[string]string{"installationId": two.ID}, nil)
	expect(t, w, 202)
	pending := decode[notifications.Accepted](t, w)
	store := &delivery.Store{DB: m.db, Vault: m.service.Vault}
	lease, err := store.Claim(ctx, []string{"ios"}, c)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	expect(t, m.request("PATCH", path, map[string]bool{"enabled": false}, nil), 200)
	if committed, err := store.Complete(ctx, *lease, delivery.Result{Outcome: delivery.Accepted}, "", c); err != nil || committed {
		t.Fatal("disabled App accepted stale worker result")
	}
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": one.ID}, nil), 409)
	expect(t, m.fixture.request("GET", "/v1/notifications/"+pending.ID, a.token, "", "", "", nil), 401)
	w = m.request("GET", path+"/notifications/"+pending.ID, nil, nil)
	expect(t, w, 200)
	status := decode[notifications.Status](t, w)
	if status.Counts.Skipped != 1 || status.Errors["app_disabled"] != 1 {
		t.Fatal("disable did not record skipped delivery")
	}
	expect(t, m.request("PATCH", path, map[string]bool{"enabled": true}, nil), 200)
	if l, err := store.Claim(ctx, []string{"ios"}, c); err != nil || l != nil {
		t.Fatal("re-enabling resurrected canceled delivery")
	}
	expect(t, m.fixture.request("DELETE", "/v1/installations/"+two.ID, two.Token, a.id, "", "", nil), 204)
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": two.ID}, nil), 409)
	expect(t, m.request("DELETE", path+"/apns?environment=production", nil, nil), 204)
	expect(t, m.request("POST", path+"/test-notifications", map[string]string{"installationId": one.ID}, nil), 409)
}

func TestManagementAuthorizationEveryRoute(t *testing.T) {
	m := managementSetup(t)
	a := m.app(t, false)
	path := appPath(a.id)
	for _, route := range []struct{ method, path string }{
		{"GET", "/admin/api/v1/apps"}, {"POST", "/admin/api/v1/apps"}, {"GET", path}, {"PATCH", path},
		{"GET", path + "/tokens"}, {"POST", path + "/tokens"}, {"DELETE", path + "/tokens/" + uuid.NewString()},
		{"GET", path + "/identity-secrets"}, {"POST", path + "/identity-secrets"}, {"DELETE", path + "/identity-secrets/" + uuid.NewString()},
		{"GET", path + "/apns"}, {"PUT", path + "/apns"}, {"DELETE", path + "/apns?environment=both"},
		{"GET", path + "/fcm"}, {"PUT", path + "/fcm"}, {"DELETE", path + "/fcm"},
		{"GET", path + "/installations"}, {"POST", path + "/test-notifications"}, {"GET", path + "/notifications"}, {"GET", path + "/notifications/" + uuid.NewString()},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			// Vary the peer to keep this security matrix separate from the public rate limiter.
			change := func(r *http.Request) { r.RemoteAddr = "127.0.0.2:1234"; r.Header.Del("Cookie") }
			expect(t, m.request(route.method, route.path, map[string]any{}, change), 401)
			expect(t, m.request(route.method, route.path, map[string]any{}, func(r *http.Request) {
				r.RemoteAddr = "127.0.0.3:1234"
				r.Header.Set("Authorization", "Bearer "+a.token)
			}), 401)
			expect(t, m.request(route.method, route.path, map[string]any{}, func(r *http.Request) { r.RemoteAddr = "127.0.0.4:1234"; r.Header.Set("Origin", "https://evil.test") }), 403)
			if route.method != "GET" {
				expect(t, m.request(route.method, route.path, map[string]any{}, func(r *http.Request) { r.RemoteAddr = "127.0.0.5:1234"; r.Header.Del("X-Ting-CSRF") }), 403)
			}
		})
	}
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	cfg := admin.DefaultConfig()
	cfg.Enabled = false
	m.handler = server.New(m.db, vault, nil, false, 24*time.Hour, cfg, server.DefaultLimits(), notifications.DefaultLimits())
	expect(t, m.request("GET", "/admin/api/v1/apps", nil, nil), 404)
}
