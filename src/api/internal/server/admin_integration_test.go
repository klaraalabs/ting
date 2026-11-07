package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

const operatorPassword = "test-only operator passphrase"
const adminOrigin = "https://localhost:8080"

func operatorService(f *fixture) *admin.Service {
	return &admin.Service{DB: f.db, Config: admin.DefaultConfig()}
}
func adminRequest(h http.Handler, method string, body any, cookie *http.Cookie, csrf string, change func(*http.Request)) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, adminOrigin+"/admin/api/v1/session", bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:9876"
	r.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		r.Header.Set("Origin", adminOrigin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-Ting-CSRF", csrf)
	}
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func operatorLogin(t *testing.T, f *fixture, previous *http.Cookie) (*http.Cookie, admin.Session) {
	t.Helper()
	w := adminRequest(f.handler, "POST", map[string]string{"username": "ADMIN", "password": operatorPassword}, previous, "login", nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	c := cookies[0]
	if c.Name != admin.CookieName || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 28800 {
		t.Fatalf("unsafe cookie: %+v", c)
	}
	var session admin.Session
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Operator.Username != "admin" || len(session.CSRFToken) != 43 || session.Token != "" || strings.Contains(w.Body.String(), c.Value) {
		t.Fatal("session response leaked bearer or omitted CSRF")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("session response cacheable")
	}
	return c, session
}
func TestOperatorSessionLifecycle(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	ctx := context.Background()
	user, err := s.Create(ctx, " Admin ", operatorPassword)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "admin" {
		t.Fatal("username not canonicalized")
	}
	if _, err = s.Create(ctx, "ADMIN", operatorPassword); err == nil {
		t.Fatal("duplicate operator accepted")
	}
	var hash string
	if err = f.db.QueryRow(ctx, `SELECT password_hash FROM admin_users WHERE id=$1`, user.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, operatorPassword) {
		t.Fatal("password persisted unsafely")
	}
	first, session := operatorLogin(t, f, nil)
	var stored []byte
	if err = f.db.QueryRow(ctx, `SELECT token_hash FROM admin_sessions WHERE user_id=$1`, user.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, secrets.Hash(first.Value)) {
		t.Fatal("session is not stored hashed")
	}
	// Sessions and their CSRF token survive constructing another HTTP server.
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	restarted := server.New(f.db, vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())
	w := adminRequest(restarted, "GET", nil, first, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), session.CSRFToken) {
		t.Fatalf("session reload: %d", w.Code)
	}
	second, session2 := operatorLogin(t, f, first)
	if first.Value == second.Value || session.CSRFToken == session2.CSRFToken {
		t.Fatal("session not rotated")
	}
	if w = adminRequest(f.handler, "GET", nil, first, "", nil); w.Code != 401 {
		t.Fatal("old session survived rotation")
	}
	if w = adminRequest(f.handler, "DELETE", nil, second, session.CSRFToken, nil); w.Code != 403 {
		t.Fatal("old CSRF token accepted")
	}
	if w = adminRequest(f.handler, "DELETE", nil, second, session2.CSRFToken, nil); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge != -1 || !c[0].Secure {
		t.Fatal("logout did not expire cookie")
	}
	if w = adminRequest(f.handler, "GET", nil, second, "", nil); w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
	third, _ := operatorLogin(t, f, nil)
	if _, err = f.db.Exec(ctx, `UPDATE admin_sessions SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE token_hash=$1`, secrets.Hash(third.Value)); err != nil {
		t.Fatal(err)
	}
	if w = adminRequest(f.handler, "GET", nil, third, "", nil); w.Code != 401 {
		t.Fatal("expired session accepted")
	}
}
func TestOperatorHTTPBoundaries(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	ctx := context.Background()
	if _, err := s.Create(ctx, "admin", operatorPassword); err != nil {
		t.Fatal(err)
	}
	cookie, session := operatorLogin(t, f, nil)
	for _, tc := range []struct {
		name, method, csrf string
		cookie             *http.Cookie
		change             func(*http.Request)
		status             int
	}{
		{"no session", "GET", "", nil, nil, 401},
		{"fake session", "GET", "", &http.Cookie{Name: admin.CookieName, Value: secrets.Token("ting_admin_")}, nil, 401},
		{"duplicate cookie", "GET", "", cookie, func(r *http.Request) { r.AddCookie(cookie) }, 401},
		{"missing csrf", "DELETE", "", cookie, nil, 403},
		{"wrong csrf", "DELETE", "wrong", cookie, nil, 403},
		{"missing origin", "DELETE", session.CSRFToken, cookie, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"evil origin", "DELETE", session.CSRFToken, cookie, func(r *http.Request) { r.Header.Set("Origin", "https://localhost:8080.evil.test") }, 403},
		{"null origin", "DELETE", session.CSRFToken, cookie, func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"duplicate origin", "DELETE", session.CSRFToken, cookie, func(r *http.Request) { r.Header.Add("Origin", adminOrigin) }, 403},
		{"cross-site fetch", "GET", "", cookie, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"wrong host", "GET", "", cookie, func(r *http.Request) { r.Host = "attacker.test" }, 403},
		{"cleartext localhost", "GET", "", cookie, func(r *http.Request) { r.TLS = nil }, 400},
		{"spoofed forwarding", "GET", "", cookie, func(r *http.Request) {
			r.TLS = nil
			r.RemoteAddr = "203.0.113.1:9000"
			r.Header.Set("X-Forwarded-Proto", "https")
		}, 400},
		{"login missing csrf", "POST", "", nil, nil, 403},
		{"login missing origin", "POST", "login", nil, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"login cross origin", "POST", "login", nil, func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"login form", "POST", "login", nil, func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		{"method", "PATCH", session.CSRFToken, cookie, nil, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := adminRequest(f.handler, tc.method, map[string]string{"username": "admin", "password": operatorPassword}, tc.cookie, tc.csrf, tc.change)
			if w.Code != tc.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	app := f.app(t, false)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		w := adminRequest(f.handler, method, map[string]string{"username": "admin", "password": operatorPassword}, nil, "login", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+app.token) })
		if w.Code != 401 {
			t.Fatal("App token accepted for operator authentication")
		}
	}
	r := httptest.NewRequest("POST", "https://localhost:8080/v1/notifications", strings.NewReader(`{"userIds":["u"],"notification":{"title":"t","body":"b"}}`))
	r.AddCookie(cookie)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+cookie.Value)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("operator session accepted for public sending")
	}
	cfg := admin.DefaultConfig()
	cfg.Enabled = false
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	disabled := server.New(f.db, vault, nil, false, 24*time.Hour, cfg, server.DefaultLimits(), notifications.DefaultLimits())
	if w = adminRequest(disabled, "GET", nil, cookie, "", nil); w.Code != 404 {
		t.Fatal("disabled admin still served")
	}
	proxy := server.New(f.db, vault, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())
	w = adminRequest(proxy, "GET", nil, cookie, "", func(r *http.Request) { r.TLS = nil; r.Header.Set("X-Forwarded-Proto", "https") })
	if w.Code != 200 {
		t.Fatalf("trusted TLS proxy rejected: %d", w.Code)
	}
	w = adminRequest(f.handler, "POST", map[string]string{"username": "admin", "password": strings.Repeat("x", 5000)}, nil, "login", nil)
	if w.Code != 413 {
		t.Fatalf("oversized login body: %d", w.Code)
	}
}
func TestOperatorResetAndConcurrentLogin(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	ctx := context.Background()
	if _, err := s.Create(ctx, "admin", operatorPassword); err != nil {
		t.Fatal(err)
	}
	first, _ := operatorLogin(t, f, nil)
	start := make(chan struct{})
	tokens := make(chan string, 4)
	errs := make(chan error, 5)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			session, err := s.Login(ctx, "admin", operatorPassword, "parallel", "")
			if err == nil {
				tokens <- session.Token
			} else {
				var e *apierror.Error
				if !errors.As(err, &e) || (e.Status != 401 && e.Status != 429) {
					errs <- err
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := s.ResetPassword(ctx, "admin", "a new operator passphrase"); err != nil {
			errs <- err
		}
	}()
	close(start)
	wg.Wait()
	close(tokens)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for token := range tokens {
		if _, err := s.Authenticate(ctx, token); err == nil {
			t.Fatal("concurrent login bypassed password reset")
		}
	}
	if _, err := s.Authenticate(ctx, first.Value); err == nil {
		t.Fatal("password reset left old session valid")
	}
	if _, err := s.Login(ctx, "admin", operatorPassword, "after-reset", ""); err == nil {
		t.Fatal("old password accepted")
	}
	session, err := s.Login(ctx, "admin", "a new operator passphrase", "after-reset", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
}
func TestOperatorThrottlePersistsAndDoesNotEnumerate(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	ctx := context.Background()
	if _, err := s.Create(ctx, "admin", operatorPassword); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{"username": "admin", "password": "a wrong operator password"}
	missing := map[string]string{"username": "missing", "password": "a wrong operator password"}
	a := adminRequest(f.handler, "POST", bad, nil, "login", nil)
	b := adminRequest(f.handler, "POST", missing, nil, "login", nil)
	if a.Code != 401 || b.Code != 401 || a.Body.String() != b.Body.String() {
		t.Fatal("login reveals account existence")
	}
	for i := 0; i < 4; i++ {
		w := adminRequest(f.handler, "POST", bad, nil, "login", nil)
		if w.Code != 401 {
			t.Fatalf("early throttle: %d", w.Code)
		}
	}
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	restarted := server.New(f.db, vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())
	w := adminRequest(restarted, "POST", map[string]string{"username": "ADMIN", "password": operatorPassword}, nil, "login", func(r *http.Request) { r.RemoteAddr = "127.0.0.2:9000" })
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("username throttle did not persist: %d", w.Code)
	}
	if _, err := f.db.Exec(ctx, `UPDATE admin_login_limits SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	operatorLogin(t, f, nil)
	// Different usernames still share the peer budget.
	for i := 0; i < 9; i++ {
		w = adminRequest(f.handler, "POST", map[string]string{"username": fmt.Sprintf("missing%d", i), "password": operatorPassword}, nil, "login", nil)
		if w.Code != 401 {
			t.Fatalf("early peer throttle: %d", w.Code)
		}
	}
	w = adminRequest(f.handler, "POST", missing, nil, "login", nil)
	if w.Code != 429 {
		t.Fatal("peer budget bypassed")
	}
}

func TestOperatorConcurrentThrottleAndGlobalBudget(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := adminRequest(f.handler, "POST", map[string]string{"username": "unknown", "password": operatorPassword}, nil, "login", func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("127.0.0.%d:9000", i+1) })
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[401] != 5 || counts[429] != 7 {
		t.Fatalf("concurrent username limit: %v", counts)
	}
	if _, err := f.db.Exec(ctx, `UPDATE admin_login_limits SET attempts=60 WHERE key_hash=$1`, secrets.Hash("global")); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(f.handler, "POST", map[string]string{"username": "another", "password": operatorPassword}, nil, "login", func(r *http.Request) { r.RemoteAddr = "127.0.0.99:9000" })
	if w.Code != 429 {
		t.Fatalf("global login budget bypassed: %d", w.Code)
	}
}

func TestOperatorSecureCookieRoundTrip(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	if _, err := s.Create(context.Background(), "admin", operatorPassword); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	cfg := admin.DefaultConfig()
	cfg.Origin = "https://" + srv.Listener.Addr().String()
	vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
	srv.Config.Handler = server.New(f.db, vault, nil, false, 24*time.Hour, cfg, server.DefaultLimits(), notifications.DefaultLimits())
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	raw, _ := json.Marshal(map[string]string{"username": "admin", "password": operatorPassword})
	req, _ := http.NewRequest("POST", srv.URL+"/admin/api/v1/session", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("X-Ting-CSRF", "login")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var session admin.Session
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("TLS login: %d %v", response.StatusCode, err)
	}
	response, err = client.Get(srv.URL + "/admin/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("cookie jar did not authenticate TLS request")
	}
	req, _ = http.NewRequest("DELETE", srv.URL+"/admin/api/v1/session", nil)
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("X-Ting-CSRF", session.CSRFToken)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal("TLS logout failed")
	}
	response, err = client.Get(srv.URL + "/admin/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("cookie jar retained authenticated session after logout")
	}
}

func TestOperatorSwitchAndSessionBound(t *testing.T) {
	f := setup(t)
	s := operatorService(f)
	ctx := context.Background()
	first, err := s.Create(ctx, "first", operatorPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, "second", operatorPassword)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Login(ctx, first.Username, operatorPassword, "switch", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Login(ctx, second.Username, operatorPassword, "switch", a.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, a.Token); err == nil {
		t.Fatal("switching operators retained the previous session")
	}
	if err = s.ResetPassword(ctx, first.Username, "another operator password"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, b.Token); err != nil {
		t.Fatal("reset revoked another operator's session")
	}
	for i := 0; i < 12; i++ {
		_, err = f.db.Exec(ctx, `INSERT INTO admin_sessions(token_hash,user_id,created_at,expires_at) VALUES($1,$2,now()-interval '1 hour',now()+interval '1 hour')`, secrets.Hash(fmt.Sprintf("seed-%d", i)), second.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Login(ctx, second.Username, operatorPassword, "switch", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.db.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE user_id=$1`, second.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 10 {
		t.Fatalf("session count not bounded: %d", count)
	}
	a, err = s.Login(ctx, first.Username, "another operator password", "switch", "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := s.Login(ctx, first.Username, "another operator password", "switch", b.Token)
		results <- err
	}()
	go func() {
		<-start
		_, err := s.Login(ctx, second.Username, operatorPassword, "switch", a.Token)
		results <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("opposite operator switches: %v", err)
		}
	}
	for _, token := range []string{a.Token, b.Token} {
		if _, err := s.Authenticate(ctx, token); err == nil {
			t.Fatal("opposite switches retained an old session")
		}
	}
}
