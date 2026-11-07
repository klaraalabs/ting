package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/providers"
	"github.com/klaraalabs/ting/src/api/internal/providers/providertest"
)

type source struct {
	mu     sync.Mutex
	values map[string]providers.APNsCredential
}

func (s *source) APNs(_ context.Context, app, env string) (providers.APNsCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.values[app+":"+env]
	if !ok {
		return c, providers.ErrNotConfigured
	}
	return c, nil
}
func (s *source) set(app string, c providers.APNsCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[app+":"+c.Environment] = c
}
func (s *source) remove(app, env string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, app+":"+env)
}
func credential(t *testing.T, environment string) (providers.APNsCredential, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return providers.APNsCredential{Version: uuid.NewString(), Environment: environment, BundleID: "com.example.test", TeamID: "TEAM123456", KeyID: "KEY1234567", PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})}, key
}
func message() delivery.Message {
	return delivery.Message{AppID: "app_a", Platform: "ios", Environment: "production", NotificationID: uuid.NewString(), PushToken: strings.Repeat("a", 64), Title: "Hello", Body: "World", Data: map[string]string{"orderId": "789"}, RemainingTTL: time.Hour}
}
func adapter(t *testing.T, s Source, handler http.HandlerFunc) (*Adapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	a := New(s)
	a.newTransport = func() *http.Transport {
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		tr.TLSClientConfig.ServerName = srv.Certificate().DNSNames[0]
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}
		return tr
	}
	t.Cleanup(a.Close)
	return a, srv
}
func TestHTTP2PayloadJWTReuseAndIsolation(t *testing.T) {
	production, key := credential(t, "production")
	sandbox, sandboxKey := credential(t, "sandbox")
	other, otherKey := credential(t, "production")
	other.TeamID = "OTHER12345"
	other.KeyID = "OTHERKEY12"
	other.BundleID = "com.other.app"
	s := &source{values: map[string]providers.APNsCredential{}}
	s.set("app_a", production)
	s.set("app_a", sandbox)
	s.set("app_b", other)
	type seen struct{ host, remote, authorization string }
	observed := make(chan seen, 5)
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Method != "POST" || r.URL.Path != "/3/device/"+strings.Repeat("a", 64) {
			t.Error("wrong APNs protocol or path")
		}
		expected, public := production, &key.PublicKey
		if r.Host == "api.sandbox.push.apple.com" {
			expected, public = sandbox, &sandboxKey.PublicKey
		} else if r.Header.Get("apns-topic") == other.BundleID {
			expected, public = other, &otherKey.PublicKey
		}
		claims := &jwt.RegisteredClaims{}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "bearer ")
		token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(expected.TeamID), jwt.WithIssuedAt())
		if err != nil || token.Header["kid"] != expected.KeyID || claims.IssuedAt == nil {
			t.Errorf("invalid provider JWT: %v", err)
		}
		if r.Header.Get("apns-push-type") != "alert" || r.Header.Get("apns-priority") != "10" || r.Header.Get("apns-topic") != expected.BundleID {
			t.Error("missing APNs headers")
		}
		expiration, err := strconv.ParseInt(r.Header.Get("apns-expiration"), 10, 64)
		if err != nil || expiration < time.Now().Add(59*time.Minute).Unix() {
			t.Error("wrong expiration")
		}
		var payload map[string]any
		if err = json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["orderId"] != "789" || payload["aps"].(map[string]any)["alert"].(map[string]any)["title"] != "Hello" {
			t.Error("wrong APNs payload")
		}
		observed <- seen{r.Host, r.RemoteAddr, raw}
		w.WriteHeader(200)
	})
	m := message()
	for i := 0; i < 2; i++ {
		if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
			t.Fatalf("send: %+v", got)
		}
	}
	first, second := <-observed, <-observed
	if first.authorization != second.authorization || first.remote != second.remote {
		t.Fatal("did not reuse JWT and HTTP/2 connection")
	}
	m.Environment = "sandbox"
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	third := <-observed
	if third.host != "api.sandbox.push.apple.com" || third.remote == first.remote {
		t.Fatal("sandbox connection not isolated")
	}
	m.AppID = "app_b"
	m.Environment = "production"
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	fourth := <-observed
	if fourth.remote == first.remote {
		t.Fatal("developer teams shared a connection")
	}
}
func TestAPNsResponseClassification(t *testing.T) {
	for _, test := range []struct {
		status   int
		reason   string
		out      delivery.Outcome
		category string
	}{
		{200, "", delivery.Accepted, ""}, {410, "Unregistered", delivery.InvalidToken, "invalid_token"},
		{400, "BadDeviceToken", delivery.PermanentFailure, "invalid_payload"}, {400, "DeviceTokenNotForTopic", delivery.PermanentFailure, "invalid_payload"},
		{400, "BadPayload", delivery.PermanentFailure, "invalid_payload"}, {413, "PayloadTooLarge", delivery.PermanentFailure, "invalid_payload"},
		{403, "InvalidProviderToken", delivery.PermanentFailure, "provider_auth"}, {403, "ExpiredProviderToken", delivery.Retryable, "provider_auth"},
		{429, "TooManyRequests", delivery.Retryable, "provider_throttled"}, {500, "InternalServerError", delivery.Retryable, "provider_unavailable"},
		{503, "Shutdown", delivery.Retryable, "provider_unavailable"}, {410, "unknown", delivery.PermanentFailure, "provider_error"},
	} {
		t.Run(strconv.Itoa(test.status)+test.reason, func(t *testing.T) {
			c, _ := credential(t, "production")
			s := &source{values: map[string]providers.APNsCredential{}}
			s.set("app_a", c)
			a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"reason": test.reason, "timestamp": int64(1700000000000)})
			})
			result := a.Send(context.Background(), message())
			if result.Outcome != test.out || result.Category != test.category || result.RetryAfter != 2*time.Minute {
				t.Fatalf("got %+v", result)
			}
			if test.out == delivery.InvalidToken && !result.InvalidatedAt.Equal(time.UnixMilli(1700000000000)) {
				t.Fatal("lost APNs invalidation timestamp")
			}
		})
	}
}
func TestAPNsRotationRemovalAndTokenExpiry(t *testing.T) {
	c, _ := credential(t, "production")
	s := &source{values: map[string]providers.APNsCredential{}}
	s.set("app_a", c)
	var status int = 200
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == 403 {
			_, _ = io.WriteString(w, `{"reason":"ExpiredProviderToken"}`)
		}
	})
	m := message()
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	old := a.clients["app_a:production"]
	oldToken := old.token
	status = 403
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Retryable {
		t.Fatal(got)
	}
	if old.token != "" {
		t.Fatal("expired authorization retained")
	}
	status = 200
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	if old.token == oldToken {
		t.Fatal("provider JWT not refreshed")
	}
	replacement, _ := credential(t, "production")
	s.set("app_a", replacement)
	if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	if !old.retired.Load() || a.clients["app_a:production"].version != replacement.Version {
		t.Fatal("old credential client retained")
	}
	s.remove("app_a", "production")
	if got := a.Send(context.Background(), m); got.Outcome != delivery.PermanentFailure || got.Category != "credential_error" {
		t.Fatal(got)
	}
	if len(a.clients) != 0 {
		t.Fatal("removed credentials still cached")
	}
	s.set("app_a", replacement)
	m.Environment = "sandbox"
	if got := a.Send(context.Background(), m); got.Outcome != delivery.PermanentFailure {
		t.Fatal("production key used for sandbox")
	}
}
func TestAPNsCancellationAndResponseLimit(t *testing.T) {
	c, _ := credential(t, "production")
	s := &source{values: map[string]providers.APNsCredential{}}
	s.set("app_a", c)
	t.Run("cancellation", func(t *testing.T) {
		a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if result := a.Send(ctx, message()); result.Outcome != delivery.Retryable {
			t.Fatal(result)
		}
	})
	t.Run("bounded response", func(t *testing.T) {
		a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, strings.Repeat("x", 70<<10))
		})
		if got := a.Send(context.Background(), message()); got.Outcome != delivery.Retryable {
			t.Fatal(got)
		}
	})
}

func TestAPNsWorkersWithStoredCredentials(t *testing.T) {
	f := providertest.Database(t)
	ctx := context.Background()
	app := f.App(t)
	c, _ := credential(t, "production")
	configure := func(c providers.APNsCredential) {
		t.Helper()
		if err := f.Apps.ConfigureAPNs(ctx, app, apps.APNsConfig{Environment: c.Environment, BundleID: c.BundleID, TeamID: c.TeamID, KeyID: c.KeyID, Key: c.PrivateKey}); err != nil {
			t.Fatal(err)
		}
	}
	configure(c)
	good := f.Register(t, app, installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("a", 64)})
	invalid := f.Register(t, app, installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("b", 64)})
	a, _ := adapter(t, f.Credentials, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, strings.Repeat("b", 64)) {
			w.WriteHeader(410)
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": "Unregistered", "timestamp": time.Now().Add(time.Second).UnixMilli()})
			return
		}
		w.WriteHeader(200)
	})
	f.Start(t, "ios", a)
	first := f.Send(t, app)
	f.Wait(t, app, first, notifications.Counts{Accepted: 1, Failed: 1})
	var active bool
	if err := f.DB.QueryRow(ctx, `SELECT active FROM installations WHERE app_id=$1 AND id=$2`, app, invalid.ID).Scan(&active); err != nil || active {
		t.Fatal("unregistered APNs token not retired")
	}
	if err := f.DB.QueryRow(ctx, `SELECT active FROM installations WHERE app_id=$1 AND id=$2`, app, good.ID).Scan(&active); err != nil || !active {
		t.Fatal("valid token retired")
	}
	a.mu.Lock()
	old := a.clients[app+":production"]
	a.mu.Unlock()
	replacement, _ := credential(t, "production")
	configure(replacement)
	second := f.Send(t, app)
	f.Wait(t, app, second, notifications.Counts{Accepted: 1})
	if !old.retired.Load() {
		t.Fatal("CLI credential replacement did not invalidate APNs client")
	}
	if _, err := f.DB.Exec(ctx, `DELETE FROM apns_credentials WHERE app_id=$1`, app); err != nil {
		t.Fatal(err)
	}
	third := f.Send(t, app)
	f.Wait(t, app, third, notifications.Counts{Failed: 1})
	a.mu.Lock()
	remaining := len(a.clients)
	a.mu.Unlock()
	if remaining != 0 {
		t.Fatal("deleted credential still cached")
	}
}

func TestProviderJWTRefreshesBeforeOneHour(t *testing.T) {
	c, key := credential(t, "production")
	client := &client{key: key, team: c.TeamID, keyID: c.KeyID}
	now := time.Now()
	first, err := client.authorization(now)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := client.authorization(now.Add(49 * time.Minute))
	if err != nil || reused != first {
		t.Fatal("JWT refreshed too frequently")
	}
	refreshed, err := client.authorization(now.Add(51 * time.Minute))
	if err != nil || refreshed == first {
		t.Fatal("JWT did not refresh before expiry")
	}
}
