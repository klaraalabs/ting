package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/retention"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

func limitRequest(user string) notifications.SendRequest {
	return notifications.SendRequest{UserIDs: []string{user}, Notification: notifications.Alert{Title: "Test", Body: "Hello"}}
}
func TestQueueAdmissionAcrossServiceInstances(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, b := f.app(t, true), f.app(t, true)
	f.register(t, a, "user", uuid.NewString(), "", strings.Repeat("a", 64))
	f.register(t, b, "user", uuid.NewString(), "", strings.Repeat("b", 64))
	limits := notifications.Limits{MaxPending: 3, MaxPendingPerApp: 2, MaxTargets: 2, SendsPerMinute: 100}
	var wg sync.WaitGroup
	accepted := make(chan string, 20)
	failures := make(chan error, 20)
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			app := a.id
			if i%2 == 0 {
				app = b.id
			}
			s := &notifications.Service{DB: f.db, Limits: limits}
			_, err := s.Send(ctx, app, fmt.Sprintf("parallel-%d", i), limitRequest("user"))
			if err == nil {
				accepted <- app
			} else {
				failures <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(accepted)
	close(failures)
	counts := map[string]int{}
	for app := range accepted {
		counts[app]++
	}
	if counts[a.id]+counts[b.id] != 3 || counts[a.id] > 2 || counts[b.id] > 2 {
		t.Fatalf("queue limits exceeded: %v", counts)
	}
	for err := range failures {
		var e *apierror.Error
		if !errors.As(err, &e) || (e.Code != "app_queue_full" && e.Code != "queue_full") {
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	var pending, rows, budget int
	if err := f.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM deliveries WHERE state='queued'),(SELECT count(*) FROM notifications),(SELECT sum(sends) FROM notification_send_limits)`).Scan(&pending, &rows, &budget); err != nil {
		t.Fatal(err)
	}
	if pending != 3 || rows != 3 || budget != 3 {
		t.Fatalf("rejected sends committed data or quota: %d %d %d", pending, rows, budget)
	}
}

func TestFanoutLimitsAndCapacityRelease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := f.app(t, true)
	for i := 0; i < 2; i++ {
		f.register(t, a, "user", uuid.NewString(), "", strings.Repeat(fmt.Sprintf("%x", i+1), 64))
	}
	limits := notifications.Limits{MaxPending: 3, MaxPendingPerApp: 3, MaxTargets: 1, SendsPerMinute: 10}
	s := &notifications.Service{DB: f.db, Limits: limits}
	_, err := s.Send(ctx, a.id, "fanout", limitRequest("user"))
	var e *apierror.Error
	if !errors.As(err, &e) || e.Code != "fanout_too_large" {
		t.Fatalf("fanout accepted: %v", err)
	}
	var n int
	if err = f.db.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&n); err != nil || n != 0 {
		t.Fatal("fanout rejection left a notification")
	}
	limits.MaxTargets = 3
	s.Limits = limits
	first, err := s.Send(ctx, a.id, "first", limitRequest("user"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(ctx, a.id, "second", limitRequest("user")); !errors.As(err, &e) || e.Code != "app_queue_full" {
		t.Fatalf("over-capacity fanout accepted: %v", err)
	}
	store := &delivery.Store{DB: f.db, Vault: f.service.Vault}
	c := delivery.DefaultConfig()
	lease, err := store.Claim(ctx, []string{"ios"}, c)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	// An in-flight lease still occupies capacity.
	if _, err = s.Send(ctx, a.id, "second", limitRequest("user")); !errors.As(err, &e) || e.Code != "app_queue_full" {
		t.Fatal("leased work stopped counting toward capacity")
	}
	if ok, err := store.Complete(ctx, *lease, delivery.Result{Outcome: delivery.Accepted}, "", c); err != nil || !ok {
		t.Fatalf("complete: %v", err)
	}
	if _, err = s.Send(ctx, a.id, "second", limitRequest("user")); err != nil {
		t.Fatal("terminal delivery did not free capacity", err)
	}
	if replay, err := s.Send(ctx, a.id, "first", limitRequest("user")); err != nil || replay.ID != first.ID {
		t.Fatal("full queue rejected accepted replay")
	}
	if _, err = s.Send(ctx, a.id, "empty", limitRequest("absent")); err != nil {
		t.Fatal("zero-target notification consumed delivery capacity")
	}
}

func TestPersistentSendBudgetAndManagementLimits(t *testing.T) {
	m := managementSetup(t)
	ctx := context.Background()
	a, b := m.app(t, true), m.app(t, true)
	one := m.register(t, a, "user", uuid.NewString(), "", strings.Repeat("a", 64))
	m.register(t, b, "user", uuid.NewString(), "", strings.Repeat("b", 64))
	limits := notifications.Limits{MaxPending: 1, MaxPendingPerApp: 1, MaxTargets: 1, SendsPerMinute: 2}
	m.handler = server.New(m.db, m.service.Vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), limits)
	key := func(r *http.Request) { r.Header.Set("Idempotency-Key", "test-key") }
	w := m.request("POST", appPath(a.id)+"/test-notifications", map[string]string{"installationId": one.ID}, key)
	expect(t, w, 202)
	first := decode[notifications.Accepted](t, w)
	w = m.fixture.request("POST", "/v1/notifications", a.token, "", "", "over-capacity", limitRequest("user"))
	expect(t, w, 429)
	if w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "app_queue_full") {
		t.Fatal("missing queue retry semantics")
	}
	w = m.fixture.request("POST", "/v1/notifications", b.token, "", "", "global", limitRequest("user"))
	expect(t, w, 503)
	if w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "queue_full") {
		t.Fatal("global limit not shared across Apps")
	}
	// Zero-target sends consume the App budget despite adding no delivery jobs.
	w = m.fixture.request("POST", "/v1/notifications", a.token, "", "", "empty", limitRequest("absent"))
	expect(t, w, 202)
	m.handler = server.New(m.db, m.service.Vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), limits)
	w = m.fixture.request("POST", "/v1/notifications", a.token, "", "", "empty-2", limitRequest("absent"))
	expect(t, w, 429)
	if !strings.Contains(w.Body.String(), "app_send_rate_limited") {
		t.Fatal("send budget did not survive server recreation")
	}
	w = m.request("POST", appPath(a.id)+"/test-notifications", map[string]string{"installationId": one.ID}, key)
	expect(t, w, 202)
	if decode[notifications.Accepted](t, w).ID != first.ID {
		t.Fatal("budget-limited replay duplicated test")
	}
	if _, err := m.db.Exec(ctx, `UPDATE notification_send_limits SET expires_at=now()-interval '1 second' WHERE app_id=$1`, a.id); err != nil {
		t.Fatal(err)
	}
	w = m.fixture.request("POST", "/v1/notifications", a.token, "", "", "empty-2", limitRequest("absent"))
	expect(t, w, 202)
}

func TestRetentionPreservesActiveWorkAndInstallationFences(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := f.app(t, true)
	f.register(t, a, "user", uuid.NewString(), "", strings.Repeat("a", 64))
	inactive := f.register(t, a, "inactive", uuid.NewString(), "", strings.Repeat("d", 64))
	expect(t, f.request("DELETE", "/v1/installations/"+inactive.ID, inactive.Token, a.id, "", "", nil), 204)
	ids := map[string]string{}
	for _, name := range []string{"old-terminal", "recent-completion", "queued", "leased", "locked-terminal", "old-empty", "recent-empty"} {
		user := "user"
		if strings.Contains(name, "empty") {
			user = "absent"
		}
		ids[name] = f.send(t, a, name, user).ID
	}
	if _, err := f.db.Exec(ctx, `UPDATE notifications SET created_at=now()-interval '10 days'; UPDATE deliveries SET state='accepted',updated_at=now()-interval '9 days'`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"queued", "leased"} {
		if _, err := f.db.Exec(ctx, `UPDATE deliveries SET state='queued',expires_at=now()-interval '1 day' WHERE notification_id=$1`, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(ctx, `UPDATE deliveries SET updated_at=now() WHERE notification_id=$1`, ids["recent-completion"]); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"leased", "locked-terminal"} {
		if _, err := f.db.Exec(ctx, `UPDATE deliveries SET lease_id=$2,locked_until=now()+interval '1 hour' WHERE notification_id=$1`, ids[name], uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(ctx, `UPDATE notifications SET created_at=now() WHERE id=$1`, ids["recent-empty"]); err != nil {
		t.Fatal(err)
	}
	cfg := retention.DefaultConfig()
	cfg.BatchSize = 1
	cleaner := &retention.Service{DB: f.db, Config: cfg}
	for i := 0; i < 2; i++ {
		counts, err := cleaner.Cleanup(ctx)
		if err != nil || counts.Notifications != 1 {
			t.Fatalf("bounded retention: %+v %v", counts, err)
		}
	}
	for name, id := range ids {
		_, err := (&notifications.Service{DB: f.db}).Status(ctx, a.id, id)
		if name == "old-terminal" || name == "old-empty" {
			if !errors.Is(err, apierror.NotFound) {
				t.Fatalf("old record retained: %s %v", name, err)
			}
		} else if err != nil {
			t.Fatalf("live/recent record removed: %s %v", name, err)
		}
	}
	var generation int64
	if err := f.db.QueryRow(ctx, `SELECT mutation_generation FROM installations WHERE app_id=$1 AND id=$2 AND NOT active`, a.id, inactive.ID).Scan(&generation); err != nil || generation != 100 {
		t.Fatal("cleanup removed installation generation fence")
	}
	var count int
	if err := f.db.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE notification_id=$1`, ids["old-terminal"]).Scan(&count); err != nil || count != 0 {
		t.Fatal("notification deletion did not cascade to deliveries")
	}
	// Cleanup shares the admission lock; a competing send's idempotency lookup
	// cannot lose its row between conflict detection and reading the acknowledgment.
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT singleton FROM notification_admission FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if counts, err := cleaner.Cleanup(ctx); err != nil || counts.Notifications != 0 {
		t.Fatal("cleanup ignored busy admission")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	resent, err := (&notifications.Service{DB: f.db}).Send(ctx, a.id, "old-terminal", limitRequest("user"))
	if err != nil || resent.ID == ids["old-terminal"] {
		t.Fatal("expired idempotency record prevented a new send")
	}
}

func TestRetentionAuthRecordsAndBackgroundCancellation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	operator, err := operatorService(f).Create(ctx, "admin", operatorPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"active", "expired", "revoked"} {
		_, err = f.db.Exec(ctx, `INSERT INTO admin_sessions(token_hash,user_id,created_at,expires_at,revoked_at) VALUES($1,$2,now()-interval '2 hours',CASE WHEN $3='expired' THEN now()-interval '1 hour' ELSE now()+interval '1 hour' END,CASE WHEN $3='revoked' THEN now() ELSE NULL END)`, secrets.Hash(state), operator.ID, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err = f.db.Exec(ctx, `INSERT INTO admin_login_limits(key_hash,attempts,expires_at) VALUES($1,1,now()+$2*interval '1 hour')`, secrets.Hash(fmt.Sprint(i)), 2*i-1); err != nil {
			t.Fatal(err)
		}
	}
	cfg := retention.DefaultConfig()
	cleaner := &retention.Service{DB: f.db, Config: cfg}
	counts, err := cleaner.Cleanup(ctx)
	if err != nil || counts.Sessions != 2 || counts.LoginLimits != 1 {
		t.Fatalf("auth cleanup: %+v %v", counts, err)
	}
	var n int
	if err = f.db.QueryRow(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatal("live session removed")
	}
	a := f.app(t, false)
	sent := f.send(t, a, "background", "absent")
	f.send(t, a, "background-2", "absent")
	f.send(t, a, "background-3", "absent")
	cleaner.Config.BatchSize = 1
	if _, err = f.db.Exec(ctx, `UPDATE notifications SET created_at=now()-interval '8 days' WHERE app_id=$1`, a.id); err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); cleaner.Run(run) }()
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err = f.db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE app_id=$1`, a.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background cleanup did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = (&notifications.Service{DB: f.db}).Status(ctx, a.id, sent.ID); !errors.Is(err, apierror.NotFound) {
		t.Fatal("background record survived cleanup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention shutdown did not cancel its interval wait")
	}
}

func TestHTTPBodyReadDeadline(t *testing.T) {
	f := setup(t)
	a := f.app(t, false)
	limits := server.DefaultLimits()
	limits.RequestTimeout = time.Second
	srv := httptest.NewTLSServer(server.New(f.db, f.service.Vault, nil, false, 24*time.Hour, admin.DefaultConfig(), limits, notifications.DefaultLimits()))
	defer srv.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r, err := http.NewRequest("POST", srv.URL+"/v1/notifications", reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+a.token)
	go func() { _, _ = writer.Write([]byte(`{"userIds":`)) }()
	client := srv.Client()
	client.Timeout = 4 * time.Second
	began := time.Now()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 504 || time.Since(began) > 3*time.Second {
		t.Fatalf("slow body not timed out: %d", response.StatusCode)
	}
}

type heldBody struct {
	started, release chan struct{}
	reader           io.Reader
	once             sync.Once
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.release })
	return b.reader.Read(p)
}
func (b *heldBody) Close() error { return nil }
func TestHTTPConcurrencyBodyLimitsAndDeadline(t *testing.T) {
	f := setup(t)
	a := f.app(t, false)
	limits := server.DefaultLimits()
	limits.MaxConcurrent = 1
	limits.MaxBodyBytes = 4096
	limits.RequestTimeout = time.Second
	f.handler = server.New(f.db, f.service.Vault, nil, false, 24*time.Hour, admin.DefaultConfig(), limits, notifications.DefaultLimits())
	raw, _ := json.Marshal(limitRequest("absent"))
	body := &heldBody{started: make(chan struct{}), release: make(chan struct{}), reader: bytes.NewReader(raw)}
	r := httptest.NewRequest("POST", "https://ting.test/v1/notifications", body)
	r.Header.Set("Authorization", "Bearer "+a.token)
	r.Header.Set("Content-Type", "application/json")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); f.handler.ServeHTTP(w, r); done <- w }()
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		close(body.release)
		t.Fatal("first request did not occupy a slot")
	}
	w := f.request("GET", "/v1/notifications/"+uuid.NewString(), a.token, "", "", "", nil)
	expect(t, w, 503)
	if w.Header().Get("Retry-After") != "1" {
		t.Fatal("server_busy missing retry hint")
	}
	expect(t, f.request("GET", "/healthz", "", "", "", "", nil), 200)
	close(body.release)
	expect(t, <-done, 202)
	for _, knownLength := range []bool{true, false} {
		r = httptest.NewRequest("POST", "https://ting.test/v1/notifications", strings.NewReader(string(raw)+strings.Repeat(" ", 5000)))
		if !knownLength {
			r.ContentLength = -1
		}
		r.Header.Set("Authorization", "Bearer "+a.token)
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		expect(t, w, 413)
	}
	tx, err := f.db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `SELECT singleton FROM notification_admission FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	w = f.request("POST", "/v1/notifications", a.token, "", "", "deadline", limitRequest("absent"))
	expect(t, w, 504)
	if time.Since(began) > 3*time.Second {
		t.Fatal("request deadline not enforced")
	}
}
