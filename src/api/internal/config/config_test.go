package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("DATABASE_URL", "postgres://localhost/ting")
	t.Setenv("TING_BIND_ADDRESS", "")
	t.Setenv("TING_ALLOW_LOCAL_HTTP", "")
	t.Setenv("TING_CREDENTIALS_ENCRYPTION_KEY", key)
	t.Setenv("TING_CREDENTIALS_ENCRYPTION_KEY_FILE", "")
	t.Setenv("TING_TRUSTED_PROXIES", "")
	t.Setenv("TING_ADMIN_ENABLED", "")
	t.Setenv("TING_ADMIN_ORIGIN", "")
	t.Setenv("TING_ADMIN_SESSION_TTL", "")
	c, err := Load()
	if err != nil || c.Address != "127.0.0.1:8080" {
		t.Fatalf("valid config: %v", err)
	}
	t.Run("admin settings", func(t *testing.T) {
		t.Setenv("TING_ADMIN_ENABLED", "false")
		t.Setenv("TING_ADMIN_ORIGIN", "https://ting.example")
		t.Setenv("TING_ADMIN_SESSION_TTL", "1h")
		c, err := Load()
		if err != nil || c.Admin.Enabled || c.Admin.Origin != "https://ting.example" || c.Admin.SessionTTL != time.Hour {
			t.Fatalf("admin settings: %+v %v", c.Admin, err)
		}
	})
	t.Run("worker settings", func(t *testing.T) {
		t.Setenv("TING_WORKER_CONCURRENCY", "6")
		t.Setenv("TING_DELIVERY_TTL", "2h")
		t.Setenv("TING_DELIVERY_MAX_ATTEMPTS", "3")
		c, err := Load()
		if err != nil || c.Delivery.Concurrency != 6 || c.Delivery.TTL != 2*time.Hour || c.Delivery.MaxAttempts != 3 {
			t.Fatalf("worker settings: %+v %v", c.Delivery, err)
		}
	})
	t.Run("operational settings", func(t *testing.T) {
		for key, value := range map[string]string{"TING_QUEUE_MAX_PENDING": "20", "TING_QUEUE_MAX_PENDING_PER_APP": "10", "TING_MAX_TARGETS_PER_SEND": "3", "TING_APP_SENDS_PER_MINUTE": "5", "TING_REQUESTS_PER_MINUTE": "30", "TING_RATE_LIMIT_MAX_PEERS": "100", "TING_MAX_CONCURRENT_REQUESTS": "4", "TING_MAX_REQUEST_BYTES": "8192", "TING_REQUEST_TIMEOUT": "2s", "TING_RETENTION": "48h", "TING_RETENTION_INTERVAL": "30s", "TING_RETENTION_BATCH_SIZE": "12"} {
			t.Setenv(key, value)
		}
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Notifications.MaxPending != 20 || c.Notifications.MaxPendingPerApp != 10 || c.Notifications.MaxTargets != 3 || c.Notifications.SendsPerMinute != 5 || c.HTTP.RequestsPerMinute != 30 || c.HTTP.MaxPeers != 100 || c.HTTP.MaxConcurrent != 4 || c.HTTP.MaxBodyBytes != 8192 || c.HTTP.RequestTimeout != 2*time.Second || c.Retention.Age != 48*time.Hour || c.Retention.Interval != 30*time.Second || c.Retention.BatchSize != 12 {
			t.Fatal("operational configuration not applied")
		}
	})
	for _, test := range []struct{ name, value string }{
		{"TING_WORKER_CONCURRENCY", "0"}, {"TING_DELIVERY_TTL", "forever"},
		{"TING_DELIVERY_LEASE_DURATION", "10s"}, {"TING_DELIVERY_MAX_ATTEMPTS", "no"},
		{"TING_DELIVERY_RETRY_MAX", "0s"}, {"TING_SHUTDOWN_GRACE", "0s"},
		{"TING_ADMIN_ENABLED", "maybe"}, {"TING_ADMIN_ORIGIN", "http://ting.example"},
		{"TING_ADMIN_SESSION_TTL", "0s"}, {"TING_ADMIN_SESSION_TTL", "25h"}, {"TING_ADMIN_SESSION_TTL", "invalid"},
		{"TING_QUEUE_MAX_PENDING", "0"}, {"TING_QUEUE_MAX_PENDING", "1000001"}, {"TING_QUEUE_MAX_PENDING", "1"},
		{"TING_QUEUE_MAX_PENDING_PER_APP", "100001"}, {"TING_QUEUE_MAX_PENDING_PER_APP", "1"},
		{"TING_MAX_TARGETS_PER_SEND", "0"}, {"TING_MAX_TARGETS_PER_SEND", "10001"}, {"TING_APP_SENDS_PER_MINUTE", "0"},
		{"TING_REQUESTS_PER_MINUTE", "0"}, {"TING_RATE_LIMIT_MAX_PEERS", "0"}, {"TING_RATE_LIMIT_MAX_PEERS", "100001"},
		{"TING_MAX_CONCURRENT_REQUESTS", "0"}, {"TING_MAX_CONCURRENT_REQUESTS", "1001"}, {"TING_MAX_REQUEST_BYTES", "4095"}, {"TING_MAX_REQUEST_BYTES", "1048577"},
		{"TING_REQUEST_TIMEOUT", "0s"}, {"TING_REQUEST_TIMEOUT", "31s"}, {"TING_RETENTION", "0h"}, {"TING_RETENTION", "8761h"},
		{"TING_RETENTION_INTERVAL", "0s"}, {"TING_RETENTION_INTERVAL", "2h"}, {"TING_RETENTION_BATCH_SIZE", "0"}, {"TING_RETENTION_BATCH_SIZE", "1001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid worker configuration accepted")
			}
		})
	}
	t.Run("missing database", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		if _, err := Load(); err == nil {
			t.Fatal("accepted missing database")
		}
	})
	t.Run("bad key", func(t *testing.T) {
		t.Setenv("TING_CREDENTIALS_ENCRYPTION_KEY", "bad-secret")
		if _, err := Load(); err == nil {
			t.Fatal("accepted short key")
		}
	})
	t.Run("bad proxy", func(t *testing.T) {
		t.Setenv("TING_TRUSTED_PROXIES", "*")
		if _, err := Load(); err == nil {
			t.Fatal("accepted wildcard proxy")
		}
	})
	t.Run("mounted key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TING_CREDENTIALS_ENCRYPTION_KEY_FILE", path)
		if _, err := Load(); err == nil {
			t.Fatal("accepted ambiguous key sources")
		}
		t.Setenv("TING_CREDENTIALS_ENCRYPTION_KEY", "")
		if _, err := Load(); err != nil {
			t.Fatal(err)
		}
	})
}
