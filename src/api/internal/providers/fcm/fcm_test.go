package fcm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/providers"
	"github.com/klaraalabs/ting/src/api/internal/providers/providertest"
)

type source struct {
	mu     sync.Mutex
	values map[string]providers.FCMCredential
}

func (s *source) FCM(_ context.Context, app string) (providers.FCMCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.values[app]
	if !ok {
		return c, providers.ErrNotConfigured
	}
	return c, nil
}
func (s *source) set(app string, c providers.FCMCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[app] = c
}
func (s *source) remove(app string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.values, app) }
func credential(t *testing.T, project string) (providers.FCMCredential, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"type": "service_account", "project_id": project, "client_email": "ting@" + project + ".iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "private_key_id": "key-" + project, "token_uri": "https://oauth2.googleapis.com/token", "universe_domain": "untrusted.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	return providers.FCMCredential{Version: uuid.NewString(), ProjectID: project, ServiceAccount: raw}, key
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func message() delivery.Message {
	return delivery.Message{AppID: "app_a", Platform: "android", NotificationID: uuid.NewString(), PushToken: "native-fcm-token", Title: "Hello", Body: "World", Data: map[string]string{"orderId": "789"}, RemainingTTL: time.Hour}
}
func adapter(t *testing.T, s Source, handler http.HandlerFunc) (*Adapter, *atomic.Int32) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	endpoint, _ := url.Parse(srv.URL)
	closed := &atomic.Int32{}
	a := New(s)
	a.factory = func(c providers.FCMCredential) (messagingClient, func(), error) {
		base := srv.Client().Transport.(*http.Transport).Clone()
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Scheme != "https" || (r.URL.Host != "oauth2.googleapis.com" && r.URL.Host != "fcm.googleapis.com") {
				t.Errorf("unexpected outbound endpoint: %s", r.URL.Host)
			}
			clone := r.Clone(r.Context())
			clone.Host = r.URL.Host
			clone.URL.Scheme = endpoint.Scheme
			clone.URL.Host = endpoint.Host
			return base.RoundTrip(clone)
		})
		client, err := newSDKClient(c, rt)
		return client, func() { closed.Add(1); base.CloseIdleConnections() }, err
	}
	t.Cleanup(a.Close)
	return a, closed
}
func oauth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"mock-access-token","expires_in":3600,"token_type":"Bearer"}`)
}
func TestFirebaseSDKAppIsolationAndPayload(t *testing.T) {
	aCredential, aKey := credential(t, "project-a")
	bCredential, bKey := credential(t, "project-b")
	s := &source{values: map[string]providers.FCMCredential{"app_a": aCredential, "app_b": bCredential}}
	var mu sync.Mutex
	tokens := map[string]int{}
	sends := map[string]int{}
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Host == "oauth2.googleapis.com" {
			if r.URL.Path != "/token" || r.Method != "POST" {
				t.Error("wrong OAuth request")
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			claims := jwt.MapClaims{}
			_, err := jwt.ParseWithClaims(r.Form.Get("assertion"), claims, func(token *jwt.Token) (any, error) {
				if token.Header["kid"] == "key-project-a" {
					return &aKey.PublicKey, nil
				}
				return &bKey.PublicKey, nil
			}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience("https://oauth2.googleapis.com/token"))
			if err != nil || claims["scope"] != "https://www.googleapis.com/auth/firebase.messaging" {
				t.Errorf("invalid scoped OAuth assertion: %v", err)
			}
			issuer, _ := claims["iss"].(string)
			mu.Lock()
			tokens[issuer]++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": issuer, "expires_in": 3600, "token_type": "Bearer"})
			return
		}
		project := "project-a"
		if strings.Contains(r.URL.Path, "project-b") {
			project = "project-b"
		}
		if r.URL.Path != "/v1/projects/"+project+"/messages:send" || r.Header.Get("Authorization") != "Bearer ting@"+project+".iam.gserviceaccount.com" {
			t.Error("FCM credentials crossed projects")
		}
		var request struct {
			Message struct {
				Token        string            `json:"token"`
				Notification map[string]string `json:"notification"`
				Data         map[string]string `json:"data"`
				Android      map[string]string `json:"android"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		m := request.Message
		if m.Token != "native-fcm-token" || m.Notification["title"] != "Hello" || m.Notification["body"] != "World" || m.Data["orderId"] != "789" || m.Android["priority"] != "high" {
			t.Error("wrong visible notification payload")
		}
		ttl, err := time.ParseDuration(m.Android["ttl"])
		if err != nil || ttl < 59*time.Minute || ttl > time.Hour {
			t.Errorf("invalid TTL: %q", m.Android["ttl"])
		}
		mu.Lock()
		sends[project]++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "projects/" + project + "/messages/accepted"})
	})
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/does-not-exist")
	t.Setenv("FIREBASE_CONFIG", `{"projectId":"wrong-project"}`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := message()
			if i%2 != 0 {
				m.AppID = "app_b"
			}
			if got := a.Send(context.Background(), m); got.Outcome != delivery.Accepted {
				t.Errorf("send: %+v", got)
			}
		}(i)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if tokens["ting@project-a.iam.gserviceaccount.com"] != 1 || tokens["ting@project-b.iam.gserviceaccount.com"] != 1 || sends["project-a"] != 4 || sends["project-b"] != 4 {
		t.Fatalf("unexpected auth/client reuse: %v %v", tokens, sends)
	}
}
func TestFCMResponseClassification(t *testing.T) {
	c, _ := credential(t, "project-test")
	for _, test := range []struct {
		status    int
		code, fcm string
		out       delivery.Outcome
		category  string
	}{
		{200, "", "", delivery.Accepted, ""}, {404, "NOT_FOUND", "UNREGISTERED", delivery.InvalidToken, "invalid_token"},
		{404, "NOT_FOUND", "", delivery.PermanentFailure, "provider_error"}, {400, "INVALID_ARGUMENT", "INVALID_ARGUMENT", delivery.PermanentFailure, "invalid_payload"},
		{403, "PERMISSION_DENIED", "SENDER_ID_MISMATCH", delivery.PermanentFailure, "provider_auth"}, {401, "UNAUTHENTICATED", "", delivery.PermanentFailure, "provider_auth"},
		{429, "RESOURCE_EXHAUSTED", "QUOTA_EXCEEDED", delivery.Retryable, "provider_throttled"},
		{500, "INTERNAL", "INTERNAL", delivery.Retryable, "provider_unavailable"}, {503, "UNAVAILABLE", "UNAVAILABLE", delivery.Retryable, "provider_unavailable"},
	} {
		t.Run(strconv.Itoa(test.status)+test.code, func(t *testing.T) {
			s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
			a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "oauth2.googleapis.com" {
					oauth(w)
					return
				}
				// Longer than the SDK's internal retry cap, so the queue receives it unchanged.
				w.Header().Set("Retry-After", "121")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if test.status == 200 {
					_, _ = io.WriteString(w, `{"name":"projects/project-test/messages/accepted"}`)
					return
				}
				details := []map[string]string{}
				if test.fcm != "" {
					details = append(details, map[string]string{"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError", "errorCode": test.fcm})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": test.status, "status": test.code, "message": "secret must not escape", "details": details}})
			})
			result := a.Send(context.Background(), message())
			if result.Outcome != test.out || result.Category != test.category || result.RetryAfter != 121*time.Second {
				t.Fatalf("got %+v", result)
			}
		})
	}
}
func TestFCMRotationRemovalAndCanceledCreationContext(t *testing.T) {
	c, _ := credential(t, "project-test")
	s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
	var tokenCalls atomic.Int32
	a, closed := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Host == "oauth2.googleapis.com" {
			tokenCalls.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"short-lived","expires_in":1,"token_type":"Bearer"}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"projects/project-test/messages/accepted"}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	if got := a.Send(ctx, message()); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	cancel()
	if got := a.Send(context.Background(), message()); got.Outcome != delivery.Accepted {
		t.Fatal("client retained canceled creation context", got)
	}
	if tokenCalls.Load() != 2 {
		t.Fatal("short-lived access token did not refresh")
	}
	old := a.clients["app_a"]
	c.Version = uuid.NewString()
	s.set("app_a", c)
	if got := a.Send(context.Background(), message()); got.Outcome != delivery.Accepted {
		t.Fatal(got)
	}
	if !old.retired.Load() || closed.Load() != 1 || a.clients["app_a"] == old {
		t.Fatal("client not replaced on rotation")
	}
	s.remove("app_a")
	if got := a.Send(context.Background(), message()); got.Category != "credential_error" || got.Outcome != delivery.PermanentFailure {
		t.Fatal(got)
	}
	if len(a.clients) != 0 || closed.Load() != 2 {
		t.Fatal("removed credential remained cached")
	}
}
func TestFCMCancellationRetainsRetryAfter(t *testing.T) {
	c, _ := credential(t, "project-test")
	s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "oauth2.googleapis.com" {
			oauth(w)
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"status":"UNAVAILABLE","code":503}}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := a.Send(ctx, message())
	if result.Outcome != delivery.Retryable || result.RetryAfter != 30*time.Second {
		t.Fatalf("lost Retry-After during SDK timeout: %+v", result)
	}
}
func TestFCMPayloadLimitAndTTLClamp(t *testing.T) {
	c, _ := credential(t, "project-test")
	s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
	var calls atomic.Int32
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "oauth2.googleapis.com" {
			oauth(w)
			return
		}
		calls.Add(1)
		var request struct {
			Message struct {
				Android map[string]string `json:"android"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Message.Android["ttl"] != "2419200s" {
			t.Errorf("TTL exceeds FCM limit: %v", request)
		}
		_, _ = io.WriteString(w, `{"name":"projects/project-test/messages/accepted"}`)
	})
	m := message()
	m.RemainingTTL = 30 * 24 * time.Hour
	if result := a.Send(context.Background(), m); result.Outcome != delivery.Accepted {
		t.Fatal(result)
	}
	m.Body = strings.Repeat("x", 4096)
	if result := a.Send(context.Background(), m); result.Outcome != delivery.PermanentFailure || result.Category != "invalid_payload" || calls.Load() != 1 {
		t.Fatal("oversized message reached provider")
	}
}

func TestFCMWorkersWithStoredCredentials(t *testing.T) {
	f := providertest.Database(t)
	ctx := context.Background()
	app := f.App(t)
	c, _ := credential(t, "project-worker")
	if err := f.Apps.ConfigureFCM(ctx, app, c.ProjectID, c.ServiceAccount); err != nil {
		t.Fatal(err)
	}
	f.Register(t, app, installations.Registration{Platform: "android", PushToken: "registered-token"})
	invalid := f.Register(t, app, installations.Registration{Platform: "android", PushToken: "unregistered-token"})
	a, closed := adapter(t, f.Credentials, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "oauth2.googleapis.com" {
			oauth(w)
			return
		}
		var payload struct {
			Message struct {
				Token string `json:"token"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Message.Token == "unregistered-token" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"status":"NOT_FOUND","code":404,"details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"projects/project-worker/messages/accepted"}`)
	})
	f.Start(t, "android", a)
	first := f.Send(t, app)
	f.Wait(t, app, first, notifications.Counts{Accepted: 1, Failed: 1})
	var active bool
	if err := f.DB.QueryRow(ctx, `SELECT active FROM installations WHERE app_id=$1 AND id=$2`, app, invalid.ID).Scan(&active); err != nil || active {
		t.Fatal("unregistered FCM token not retired")
	}
	if err := f.Apps.ConfigureFCM(ctx, app, c.ProjectID, c.ServiceAccount); err != nil {
		t.Fatal(err)
	}
	second := f.Send(t, app)
	f.Wait(t, app, second, notifications.Counts{Accepted: 1})
	if closed.Load() < 1 {
		t.Fatal("credential replacement did not close old FCM transport")
	}
	if _, err := f.DB.Exec(ctx, `DELETE FROM fcm_credentials WHERE app_id=$1`, app); err != nil {
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

func TestOAuthErrorsAreClassifiedWithoutSDKRetryStorm(t *testing.T) {
	c, _ := credential(t, "project-test")
	for _, status := range []int{400, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
			var calls atomic.Int32
			a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Host != "oauth2.googleapis.com" {
					t.Error("FCM called without OAuth authorization")
				}
				w.Header().Set("Content-Type", "application/json")
				if status == 429 {
					w.Header().Set("Retry-After", "120")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"private provider response"}`)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got := a.Send(ctx, message())
			if calls.Load() != 1 || ctx.Err() != nil {
				t.Fatal("OAuth failure retried inside SDK or canceled worker context")
			}
			if status == 400 && (got.Outcome != delivery.PermanentFailure || got.Category != "provider_auth") {
				t.Fatalf("bad grant: %+v", got)
			}
			if status == 429 && (got.Outcome != delivery.Retryable || got.Category != "provider_throttled" || got.RetryAfter != 2*time.Minute) {
				t.Fatalf("quota: %+v", got)
			}
		})
	}
}

func TestCancellationInterruptsOAuthExchange(t *testing.T) {
	c, _ := credential(t, "project-test")
	s := &source{values: map[string]providers.FCMCredential{"app_a": c}}
	started := make(chan struct{})
	release := make(chan struct{})
	a, _ := adapter(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "oauth2.googleapis.com" {
			t.Error("unexpected messaging request")
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan delivery.Result, 1)
	go func() { done <- a.Send(ctx, message()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("OAuth exchange did not start")
	}
	cancel()
	select {
	case result := <-done:
		if result.Outcome != delivery.Retryable {
			t.Fatalf("canceled exchange: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("OAuth exchange ignored cancellation")
	}
}
