// Package providertest supplies isolated PostgreSQL fixtures for provider tests.
package providertest

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"strings"
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
	"github.com/klaraalabs/ting/src/api/internal/providers"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

type Fixture struct {
	DB          *pgxpool.Pool
	Vault       *secrets.Vault
	Apps        *apps.Service
	Credentials *providers.Store
	Devices     *installations.Service
}

func Database(t *testing.T) *Fixture {
	t.Helper()
	databaseURL := os.Getenv("TING_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TING_TEST_DATABASE_URL for provider integration tests")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "provider_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	db, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	vault, _ := secrets.New(bytes.Repeat([]byte{13}, 32))
	return &Fixture{db, vault, &apps.Service{DB: db, Vault: vault}, &providers.Store{DB: db, Vault: vault}, &installations.Service{DB: db, Vault: vault}}
}
func (f *Fixture) App(t *testing.T) string {
	t.Helper()
	a, err := f.Apps.Create(context.Background(), "Provider test", "")
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}
func (f *Fixture) Register(t *testing.T, app string, r installations.Registration) installations.Registered {
	t.Helper()
	r.Token = secrets.Token("ting_ik_")
	r.Generation = 1
	device, _, err := f.Devices.Register(context.Background(), app, uuid.NewString(), "user", "", r)
	if err != nil {
		t.Fatal(err)
	}
	return device
}
func (f *Fixture) Send(t *testing.T, app string) string {
	t.Helper()
	n, err := (&notifications.Service{DB: f.DB}).Send(context.Background(), app, "", notifications.SendRequest{UserIDs: []string{"user"}, Notification: notifications.Alert{Title: "Hello", Body: "Worker integration"}})
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}
func (f *Fixture) Start(t *testing.T, platform string, sender delivery.Sender) {
	t.Helper()
	c := delivery.DefaultConfig()
	c.Concurrency = 2
	c.PollInterval = 10 * time.Millisecond
	p, err := delivery.NewPool(&delivery.Store{DB: f.DB, Vault: f.Vault}, map[string]delivery.Sender{platform: sender}, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("workers failed to stop")
		}
	})
}
func (f *Fixture) Wait(t *testing.T, app, id string, want notifications.Counts) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		s, err := (&notifications.Service{DB: f.DB}).Status(ctx, app, id)
		if err == nil && s.Counts == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("notification did not reach %+v; got %+v, error %v", want, s, err)
		case <-timer.C:
		}
	}
}
