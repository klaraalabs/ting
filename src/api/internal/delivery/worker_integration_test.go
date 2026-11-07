package delivery_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/dbmigrations"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type fixture struct {
	db      *pgxpool.Pool
	store   *delivery.Store
	apps    *apps.Service
	devices *installations.Service
}

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
	schema := "worker_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	if err = dbmigrations.Migrate(ctx, parsed.String()); err != nil {
		t.Fatal(err)
	}
	c, err := pgxpool.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	c.MaxConns = 2
	db, err := pgxpool.NewWithConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	vault, _ := secrets.New(bytes.Repeat([]byte{9}, 32))
	return &fixture{db, &delivery.Store{DB: db, Vault: vault}, &apps.Service{DB: db, Vault: vault}, &installations.Service{DB: db, Vault: vault}}
}
func (f *fixture) app(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	a, err := f.apps.Create(ctx, "Worker test", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	err = f.apps.ConfigureAPNs(ctx, a.ID, apps.APNsConfig{Environment: "production", BundleID: "com.example.worker", TeamID: "TEAM123456", KeyID: "KEY1234567", Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}
func (f *fixture) device(t *testing.T, app string) installations.Registered {
	t.Helper()
	registered, _, err := f.devices.Register(context.Background(), app, uuid.NewString(), "user", "", installations.Registration{Platform: "ios", Environment: "production", PushToken: hex.EncodeToString(secrets.Hash(uuid.NewString())), Token: secrets.Token("ting_ik_"), Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	return registered
}
func (f *fixture) send(t *testing.T, app string) string {
	t.Helper()
	result, err := (&notifications.Service{DB: f.db}).Send(context.Background(), app, "", notifications.SendRequest{UserIDs: []string{"user"}, Notification: notifications.Alert{Title: "Title", Body: "Body"}, Data: map[string]string{"orderId": "789"}})
	if err != nil {
		t.Fatal(err)
	}
	return result.ID
}
func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) claim(t *testing.T, c delivery.Config) *delivery.Lease {
	t.Helper()
	l, err := f.store.Claim(context.Background(), []string{"ios"}, c)
	if err != nil || l == nil {
		t.Fatalf("claim: %v, %v", l, err)
	}
	return l
}
func (f *fixture) complete(t *testing.T, l *delivery.Lease, r delivery.Result, c delivery.Config) {
	t.Helper()
	ok, err := f.store.Complete(context.Background(), *l, r, "", c)
	if err != nil || !ok {
		t.Fatalf("complete: owned=%v error=%v", ok, err)
	}
}
func (f *fixture) state(t *testing.T, id int64) (state, category string, attempts int, leased bool) {
	t.Helper()
	err := f.db.QueryRow(context.Background(), `SELECT state,COALESCE(error_category,''),attempt_count,lease_id IS NOT NULL FROM deliveries WHERE id=$1`, id).Scan(&state, &category, &attempts, &leased)
	if err != nil {
		t.Fatal(err)
	}
	return
}
func (f *fixture) active(t *testing.T, app, id string) bool {
	t.Helper()
	var active bool
	if err := f.db.QueryRow(context.Background(), `SELECT active FROM installations WHERE app_id=$1 AND id=$2`, app, id).Scan(&active); err != nil {
		t.Fatal(err)
	}
	return active
}
func (f *fixture) counts(t *testing.T, app, notification string) notifications.Counts {
	t.Helper()
	s, err := (&notifications.Service{DB: f.db}).Status(context.Background(), app, notification)
	if err != nil {
		t.Fatal(err)
	}
	return s.Counts
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("condition did not become true")
		case <-ticker.C:
		}
	}
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}
func testConfig() delivery.Config {
	c := delivery.DefaultConfig()
	c.PollInterval = 10 * time.Millisecond
	c.DatabaseTimeout = 500 * time.Millisecond
	c.AttemptTimeout = time.Second
	c.LeaseDuration = 5 * time.Second
	c.ShutdownGrace = time.Second
	return c
}

type senderFunc func(context.Context, delivery.Message) delivery.Result

func (f senderFunc) Send(ctx context.Context, m delivery.Message) delivery.Result { return f(ctx, m) }
func start(t *testing.T, f *fixture, senders map[string]delivery.Sender, c delivery.Config) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	p, err := delivery.NewPool(f.store, senders, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	t.Cleanup(func() { cancel(); receive(t, done) })
	return cancel, done
}

func TestClaimConcurrencyAndLeaseRecovery(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	for i := 0; i < 12; i++ {
		f.device(t, a)
	}
	f.send(t, a)
	c := testConfig()
	// A transaction holding one row cannot block other consumers.
	tx, err := f.db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var held int64
	if err = tx.QueryRow(context.Background(), `SELECT id FROM deliveries ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	first := f.claim(t, c)
	if first.ID == held {
		t.Fatal("claimed locked row")
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	leases := make(chan *delivery.Lease, 11)
	failures := make(chan error, 11)
	var wg sync.WaitGroup
	for i := 0; i < 11; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := &delivery.Store{DB: f.db, Vault: f.store.Vault}
			l, err := other.Claim(context.Background(), []string{"ios"}, c)
			if err != nil || l == nil {
				failures <- errors.New("concurrent claim failed")
				return
			}
			leases <- l
		}()
	}
	wg.Wait()
	close(leases)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	seen := map[int64]bool{first.ID: true}
	for l := range leases {
		if seen[l.ID] {
			t.Fatal("duplicate active lease")
		}
		seen[l.ID] = true
	}
	if len(seen) != 12 {
		t.Fatal("missing claims")
	}
	if l, err := f.store.Claim(context.Background(), []string{"ios"}, c); err != nil || l != nil {
		t.Fatalf("stole active lease: %v %v", l, err)
	}
	f.exec(t, `UPDATE deliveries SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID)
	recovered := f.claim(t, c)
	if recovered.ID != first.ID || recovered.Token == first.Token || recovered.Attempt != 2 {
		t.Fatalf("bad recovery: %+v", recovered)
	}
	for _, result := range []delivery.Result{{Outcome: delivery.Accepted}, {Outcome: delivery.InvalidToken}} {
		ok, err := f.store.Complete(context.Background(), *first, result, "", c)
		if err != nil || ok {
			t.Fatalf("stale result persisted: %v %v", ok, err)
		}
	}
	if err = f.store.Release(context.Background(), *first); err != nil {
		t.Fatal(err)
	}
	if !f.active(t, a, first.InstallationID) {
		t.Fatal("stale result retired current installation")
	}
	f.complete(t, recovered, delivery.Result{Outcome: delivery.Accepted}, c)
	state, _, attempts, leased := f.state(t, first.ID)
	if state != "accepted" || attempts != 2 || leased {
		t.Fatalf("completion: %s %d %v", state, attempts, leased)
	}
}

func TestRetriesDeadlineAndFailureClassification(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	device := f.device(t, a)
	c := testConfig()
	c.MaxAttempts = 2
	f.send(t, a)
	l := f.claim(t, c)
	f.complete(t, l, delivery.Result{Outcome: delivery.Retryable, Category: "SECRET provider body", RetryAfter: 30 * time.Second}, c)
	state, category, attempts, leased := f.state(t, l.ID)
	if state != "queued" || category != "provider_error" || attempts != 1 || leased {
		t.Fatalf("retry: %s %s %d %v", state, category, attempts, leased)
	}
	var delay float64
	if err := f.db.QueryRow(context.Background(), `SELECT EXTRACT(EPOCH FROM(next_attempt_at-clock_timestamp()))::float8 FROM deliveries WHERE id=$1`, l.ID).Scan(&delay); err != nil {
		t.Fatal(err)
	}
	if delay < 28 || delay > 30 {
		t.Fatalf("Retry-After not honored: %f", delay)
	}
	if next, err := f.store.Claim(context.Background(), []string{"ios"}, c); err != nil || next != nil {
		t.Fatal("retried too early")
	}
	f.exec(t, `UPDATE deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, l.ID)
	l = f.claim(t, c)
	f.complete(t, l, delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}, c)
	state, category, _, _ = f.state(t, l.ID)
	if state != "failed" || category != "attempts_exhausted" {
		t.Fatalf("retry exhaustion: %s %s", state, category)
	}
	if !f.active(t, a, device.ID) {
		t.Fatal("transient failure retired installation")
	}
	f.send(t, a)
	l = f.claim(t, c)
	f.complete(t, l, delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_auth"}, c)
	state, category, _, _ = f.state(t, l.ID)
	if state != "failed" || category != "provider_auth" || !f.active(t, a, device.ID) {
		t.Fatal("auth failure misclassified")
	}
	f.send(t, a)
	l = f.claim(t, c)
	f.exec(t, `UPDATE deliveries SET expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, l.ID)
	f.complete(t, l, delivery.Result{Outcome: delivery.Retryable, RetryAfter: time.Hour}, c)
	state, category, _, _ = f.state(t, l.ID)
	if state != "failed" || category != "deadline_exceeded" {
		t.Fatal("retry scheduled past deadline")
	}
}

func TestInvalidTokenOnlyRetiresCurrentInstallation(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	one := f.device(t, a)
	two := f.device(t, a)
	first := f.send(t, a)
	second := f.send(t, a)
	c := testConfig()
	l := f.claim(t, c)
	f.complete(t, l, delivery.Result{Outcome: delivery.InvalidToken}, c)
	if f.active(t, a, l.InstallationID) {
		t.Fatal("invalid token remains active")
	}
	other := one.ID
	if l.InstallationID == one.ID {
		other = two.ID
	}
	if !f.active(t, a, other) {
		t.Fatal("invalidated other installation")
	}
	counts := f.counts(t, a, first)
	if counts.Failed != 1 || counts.Queued != 1 {
		t.Fatalf("first outcomes: %+v", counts)
	}
	counts = f.counts(t, a, second)
	if counts.Skipped != 1 || counts.Queued != 1 {
		t.Fatalf("queued work not canceled: %+v", counts)
	}
}

func TestReassociationFencesInFlightResults(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	device := f.device(t, a)
	f.send(t, a)
	c := testConfig()
	l := f.claim(t, c)
	_, skip, _, err := f.store.Prepare(context.Background(), *l, c.DatabaseTimeout)
	if err != nil || skip != "" {
		t.Fatal("valid lease failed preparation")
	}
	_, _, err = f.devices.Register(context.Background(), a, device.ID, "other-user", device.Token, installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("b", 64), Token: secrets.Token("ting_ik_"), Generation: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = f.store.Prepare(context.Background(), *l, c.DatabaseTimeout); !errors.Is(err, delivery.ErrLeaseLost) {
		t.Fatalf("stale association prepared: %v", err)
	}
	ok, err := f.store.Complete(context.Background(), *l, delivery.Result{Outcome: delivery.InvalidToken}, "", c)
	if err != nil || ok {
		t.Fatal("late result overwrote cancellation")
	}
	if !f.active(t, a, device.ID) {
		t.Fatal("late invalid-token result retired replacement token")
	}
	state, _, _, leased := f.state(t, l.ID)
	if state != "skipped" || leased {
		t.Fatal("reassociation did not clear lease")
	}
}

func TestSweepDeadlinesAndCrashedFinalAttempt(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	f.device(t, a)
	c := testConfig()
	c.MaxAttempts = 1
	f.send(t, a)
	l := f.claim(t, c)
	f.exec(t, `UPDATE deliveries SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, l.ID)
	if n, err := f.store.Sweep(context.Background(), c.MaxAttempts); err != nil || n != 0 {
		t.Fatal("sweep stole live lease")
	}
	_, skip, _, err := f.store.Prepare(context.Background(), *l, c.DatabaseTimeout)
	if err != nil || skip != "deadline_exceeded" {
		t.Fatalf("expired work prepared: %s %v", skip, err)
	}
	f.exec(t, `UPDATE deliveries SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, l.ID)
	if n, err := f.store.Sweep(context.Background(), c.MaxAttempts); err != nil || n != 1 {
		t.Fatalf("expiry: %d %v", n, err)
	}
	state, category, _, leased := f.state(t, l.ID)
	if state != "failed" || category != "deadline_exceeded" || leased {
		t.Fatal("deadline outcome incorrect")
	}
	f.send(t, a)
	l = f.claim(t, c)
	f.exec(t, `UPDATE deliveries SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, l.ID)
	if n, err := f.store.Sweep(context.Background(), c.MaxAttempts); err != nil || n != 1 {
		t.Fatalf("crashed final attempt: %d %v", n, err)
	}
	_, category, _, _ = f.state(t, l.ID)
	if category != "attempts_exhausted" {
		t.Fatal("crashed final attempt retried indefinitely")
	}
}

func TestPoolBoundsConcurrencyAndReleasesDatabase(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	for i := 0; i < 9; i++ {
		f.device(t, a)
	}
	notification := f.send(t, a)
	c := testConfig()
	c.Concurrency = 3
	started := make(chan delivery.Message, 9)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	sender := senderFunc(func(ctx context.Context, m delivery.Message) delivery.Result {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- m
		select {
		case <-release:
			return delivery.Result{Outcome: delivery.Accepted}
		case <-ctx.Done():
			return delivery.Result{Outcome: delivery.Retryable}
		}
	})
	cancel, done := start(t, f, map[string]delivery.Sender{"ios": sender}, c)
	for i := 0; i < 3; i++ {
		m := receive(t, started)
		if m.AppID != a || m.NotificationID != notification || m.Title != "Title" || m.Data["orderId"] != "789" || m.PushToken == "" || m.RemainingTTL <= 0 {
			t.Fatal("wrong provider message")
		}
	}
	// All pool connections remain available even with more sends than connections.
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	one, err := f.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.db.Acquire(ctx)
	if err != nil {
		one.Release()
		t.Fatal("provider calls hold database connections")
	}
	two.Release()
	one.Release()
	close(release)
	eventually(t, func() bool { return f.counts(t, a, notification).Accepted == 9 })
	cancel()
	receive(t, done)
	if maximum.Load() != 3 {
		t.Fatalf("concurrency bound: %d", maximum.Load())
	}
	var duplicate int
	if err = f.db.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE attempt_count<>1`).Scan(&duplicate); err != nil || duplicate != 0 {
		t.Fatal("duplicate attempts")
	}
}

func TestGracefulDrainStopsLeasing(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	for i := 0; i < 3; i++ {
		f.device(t, a)
	}
	notification := f.send(t, a)
	c := testConfig()
	c.Concurrency = 1
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var canceled atomic.Bool
	sender := senderFunc(func(ctx context.Context, _ delivery.Message) delivery.Result {
		started <- struct{}{}
		select {
		case <-release:
			return delivery.Result{Outcome: delivery.Accepted}
		case <-ctx.Done():
			canceled.Store(true)
			return delivery.Result{Outcome: delivery.Retryable}
		}
	})
	cancel, done := start(t, f, map[string]delivery.Sender{"ios": sender}, c)
	receive(t, started)
	cancel()
	close(release)
	receive(t, done)
	if canceled.Load() {
		t.Fatal("graceful stop canceled in-flight send")
	}
	counts := f.counts(t, a, notification)
	if counts.Accepted != 1 || counts.Queued != 2 {
		t.Fatalf("leased during drain: %+v", counts)
	}
	var attempts int
	if err := f.db.QueryRow(context.Background(), `SELECT sum(attempt_count) FROM deliveries`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("shutdown consumed pending work")
	}
}

func TestForcedShutdownLeavesRecoverableLease(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	f.device(t, a)
	notification := f.send(t, a)
	c := testConfig()
	c.Concurrency = 1
	c.ShutdownGrace = 30 * time.Millisecond
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	sender := senderFunc(func(ctx context.Context, _ delivery.Message) delivery.Result {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return delivery.Result{Outcome: delivery.InvalidToken}
	})
	cancel, done := start(t, f, map[string]delivery.Sender{"ios": sender}, c)
	receive(t, started)
	cancel()
	receive(t, canceled)
	receive(t, done)
	var id int64
	if err := f.db.QueryRow(context.Background(), `SELECT id FROM deliveries WHERE notification_id=$1`, notification).Scan(&id); err != nil {
		t.Fatal(err)
	}
	state, _, attempts, leased := f.state(t, id)
	if state != "queued" || attempts != 1 || !leased {
		t.Fatal("forced shutdown lost recoverable work")
	}
	f.exec(t, `UPDATE deliveries SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	replacement := senderFunc(func(context.Context, delivery.Message) delivery.Result {
		return delivery.Result{Outcome: delivery.Accepted}
	})
	stopReplacement, replacementDone := start(t, f, map[string]delivery.Sender{"ios": replacement}, c)
	eventually(t, func() bool { return f.counts(t, a, notification).Accepted == 1 })
	stopReplacement()
	receive(t, replacementDone)
	_, _, attempts, leased = f.state(t, id)
	if attempts != 2 || leased {
		t.Fatal("restarted worker did not finish recovered lease")
	}
}

func TestAttemptTimeoutAndNoAdapters(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	f.device(t, a)
	notification := f.send(t, a)
	c := testConfig()
	c.Concurrency = 1
	c.AttemptTimeout = 20 * time.Millisecond
	sender := senderFunc(func(ctx context.Context, _ delivery.Message) delivery.Result {
		<-ctx.Done()
		return delivery.Result{Outcome: delivery.InvalidToken}
	})
	cancel, done := start(t, f, map[string]delivery.Sender{"ios": sender}, c)
	eventually(t, func() bool {
		var count int
		err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE error_category='provider_timeout' AND attempt_count=1 AND lease_id IS NULL`).Scan(&count)
		return err == nil && count == 1
	})
	cancel()
	receive(t, done)
	c.AttemptTimeout = time.Second
	cancel, done = start(t, f, nil, c)
	// Expiration still runs without provider adapters; live work uses no attempts.
	f.exec(t, `UPDATE deliveries SET expires_at=clock_timestamp()-interval '1 second' WHERE notification_id=$1`, notification)
	eventually(t, func() bool { return f.counts(t, a, notification).Failed == 1 })
	cancel()
	receive(t, done)
	var attempts int
	if err := f.db.QueryRow(context.Background(), `SELECT attempt_count FROM deliveries WHERE notification_id=$1`, notification).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("unavailable adapter consumed attempts")
	}
}

func TestPersistedDeadlineAndIdempotency(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	f.device(t, a)
	ctx := context.Background()
	request := notifications.SendRequest{UserIDs: []string{"user"}, Notification: notifications.Alert{Title: "T", Body: "B"}}
	service := &notifications.Service{DB: f.db, DeliveryTTL: 2 * time.Hour}
	sent, err := service.Send(ctx, a, "deadline", request)
	if err != nil {
		t.Fatal(err)
	}
	var before time.Time
	var ttl float64
	if err = f.db.QueryRow(ctx, `SELECT expires_at,EXTRACT(EPOCH FROM (expires_at-created_at))::float8 FROM deliveries WHERE notification_id=$1`, sent.ID).Scan(&before, &ttl); err != nil {
		t.Fatal(err)
	}
	if ttl < 7200 || ttl > 7202 {
		t.Fatalf("TTL not persisted: %f", ttl)
	}
	service.DeliveryTTL = time.Minute
	replay, err := service.Send(ctx, a, "deadline", request)
	if err != nil || replay.ID != sent.ID {
		t.Fatal("idempotent replay failed")
	}
	var after time.Time
	if err = f.db.QueryRow(ctx, `SELECT expires_at FROM deliveries WHERE notification_id=$1`, sent.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatal("retry changed accepted delivery deadline")
	}
}

func TestSkipDisabledAppsAndUnreadableTokens(t *testing.T) {
	f := setup(t)
	disabled := f.app(t)
	f.device(t, disabled)
	first := f.send(t, disabled)
	broken := f.app(t)
	device := f.device(t, broken)
	second := f.send(t, broken)
	f.exec(t, `UPDATE apps SET enabled=false WHERE id=$1`, disabled)
	f.exec(t, `UPDATE installations SET encrypted_push_token=$2 WHERE app_id=$1`, broken, []byte("corrupt"))
	var calls atomic.Int32
	sender := senderFunc(func(context.Context, delivery.Message) delivery.Result {
		calls.Add(1)
		return delivery.Result{Outcome: delivery.Accepted}
	})
	cancel, done := start(t, f, map[string]delivery.Sender{"ios": sender}, testConfig())
	eventually(t, func() bool {
		return f.counts(t, disabled, first).Skipped == 1 && f.counts(t, broken, second).Failed == 1
	})
	cancel()
	receive(t, done)
	if calls.Load() != 0 {
		t.Fatal("ineligible jobs reached provider")
	}
	if !f.active(t, broken, device.ID) {
		t.Fatal("decryption failure invalidated device")
	}
}

func TestReleaseAndExpiredOwnership(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	device := f.device(t, a)
	f.send(t, a)
	c := testConfig()
	l := f.claim(t, c)
	if err := f.store.Release(context.Background(), *l); err != nil {
		t.Fatal(err)
	}
	state, _, attempts, leased := f.state(t, l.ID)
	if state != "queued" || attempts != 0 || leased {
		t.Fatal("unstarted release consumed an attempt")
	}
	l = f.claim(t, c)
	f.exec(t, `UPDATE deliveries SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, l.ID)
	ok, err := f.store.Complete(context.Background(), *l, delivery.Result{Outcome: delivery.InvalidToken}, "", c)
	if err != nil || ok || !f.active(t, a, device.ID) {
		t.Fatal("expired owner mutated installation")
	}
}

func TestSweepIsBounded(t *testing.T) {
	f := setup(t)
	a := f.app(t)
	for i := 0; i < 105; i++ {
		f.device(t, a)
	}
	notification := f.send(t, a)
	f.exec(t, `UPDATE deliveries SET expires_at=clock_timestamp()-interval '1 second' WHERE notification_id=$1`, notification)
	for _, want := range []int64{100, 5, 0} {
		got, err := f.store.Sweep(context.Background(), 8)
		if err != nil || got != want {
			t.Fatalf("batch size: %d, want %d, error %v", got, want, err)
		}
	}
}

func TestHistoricalAPNsInvalidationPreservesRecentRegistration(t *testing.T) {
	f := setup(t)
	app := f.app(t)
	device := f.device(t, app)
	f.send(t, app)
	c := testConfig()
	lease := f.claim(t, c)
	f.complete(t, lease, delivery.Result{Outcome: delivery.InvalidToken, InvalidatedAt: time.Now().Add(-time.Hour)}, c)
	if !f.active(t, app, device.ID) {
		t.Fatal("historical APNs invalidation retired newer registration")
	}
	f.send(t, app)
	lease = f.claim(t, c)
	f.complete(t, lease, delivery.Result{Outcome: delivery.InvalidToken, InvalidatedAt: time.Now().Add(time.Second)}, c)
	if f.active(t, app, device.ID) {
		t.Fatal("current APNs invalidation not applied")
	}
}
