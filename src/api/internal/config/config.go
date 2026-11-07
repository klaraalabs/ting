package config

import (
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/retention"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

type Config struct {
	DatabaseURL    string
	Address        string
	EncryptionKey  []byte
	TrustedProxies []netip.Prefix
	AllowLocalHTTP bool
	Delivery       delivery.Config
	Admin          admin.Config
	HTTP           server.Limits
	Notifications  notifications.Limits
	Retention      retention.Config
}

func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), Address: os.Getenv("TING_BIND_ADDRESS"), Delivery: delivery.DefaultConfig(), Admin: admin.DefaultConfig(), HTTP: server.DefaultLimits(), Notifications: notifications.DefaultLimits(), Retention: retention.DefaultConfig()}
	if value := os.Getenv("TING_ADMIN_ENABLED"); value != "" {
		var err error
		c.Admin.Enabled, err = strconv.ParseBool(value)
		if err != nil {
			return c, errors.New("TING_ADMIN_ENABLED must be a boolean")
		}
	}
	if value := os.Getenv("TING_ADMIN_ORIGIN"); value != "" {
		c.Admin.Origin = value
	}
	if value := os.Getenv("TING_ADMIN_SESSION_TTL"); value != "" {
		var err error
		c.Admin.SessionTTL, err = time.ParseDuration(value)
		if err != nil {
			return c, errors.New("TING_ADMIN_SESSION_TTL must be a duration")
		}
	}
	if err := c.Admin.Validate(); err != nil {
		return c, err
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.Address == "" {
		c.Address = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, errors.New("TING_BIND_ADDRESS must be host:port")
	}
	if value := os.Getenv("TING_ALLOW_LOCAL_HTTP"); value != "" {
		var err error
		c.AllowLocalHTTP, err = strconv.ParseBool(value)
		if err != nil {
			return c, errors.New("TING_ALLOW_LOCAL_HTTP must be a boolean")
		}
	}
	key, file := os.Getenv("TING_CREDENTIALS_ENCRYPTION_KEY"), os.Getenv("TING_CREDENTIALS_ENCRYPTION_KEY_FILE")
	if key != "" && file != "" {
		return c, errors.New("set only one encryption key source")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return c, errors.New("cannot read encryption key file")
		}
		key = strings.TrimSpace(string(data))
	}
	var err error
	c.EncryptionKey, err = base64.StdEncoding.DecodeString(key)
	if err != nil || len(c.EncryptionKey) != 32 {
		return c, errors.New("TING_CREDENTIALS_ENCRYPTION_KEY must be base64-encoded 32 bytes")
	}
	for _, cidr := range strings.Split(os.Getenv("TING_TRUSTED_PROXIES"), ",") {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return c, errors.New("TING_TRUSTED_PROXIES must contain comma-separated IP CIDRs")
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	for name, target := range map[string]*int{
		"TING_WORKER_CONCURRENCY":        &c.Delivery.Concurrency,
		"TING_DELIVERY_MAX_ATTEMPTS":     &c.Delivery.MaxAttempts,
		"TING_QUEUE_MAX_PENDING":         &c.Notifications.MaxPending,
		"TING_QUEUE_MAX_PENDING_PER_APP": &c.Notifications.MaxPendingPerApp,
		"TING_MAX_TARGETS_PER_SEND":      &c.Notifications.MaxTargets,
		"TING_APP_SENDS_PER_MINUTE":      &c.Notifications.SendsPerMinute,
		"TING_REQUESTS_PER_MINUTE":       &c.HTTP.RequestsPerMinute,
		"TING_RATE_LIMIT_MAX_PEERS":      &c.HTTP.MaxPeers,
		"TING_MAX_CONCURRENT_REQUESTS":   &c.HTTP.MaxConcurrent,
		"TING_MAX_REQUEST_BYTES":         &c.HTTP.MaxBodyBytes,
		"TING_RETENTION_BATCH_SIZE":      &c.Retention.BatchSize,
	} {
		if value := os.Getenv(name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return c, errors.New(name + " must be an integer")
			}
			*target = parsed
		}
	}
	for name, target := range map[string]*time.Duration{
		"TING_WORKER_POLL_INTERVAL":     &c.Delivery.PollInterval,
		"TING_DELIVERY_LEASE_DURATION":  &c.Delivery.LeaseDuration,
		"TING_DELIVERY_ATTEMPT_TIMEOUT": &c.Delivery.AttemptTimeout,
		"TING_DELIVERY_RETRY_BASE":      &c.Delivery.RetryBase,
		"TING_DELIVERY_RETRY_MAX":       &c.Delivery.RetryMax,
		"TING_DELIVERY_TTL":             &c.Delivery.TTL,
		"TING_SHUTDOWN_GRACE":           &c.Delivery.ShutdownGrace,
		"TING_REQUEST_TIMEOUT":          &c.HTTP.RequestTimeout,
		"TING_RETENTION":                &c.Retention.Age,
		"TING_RETENTION_INTERVAL":       &c.Retention.Interval,
	} {
		if value := os.Getenv(name); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return c, errors.New(name + " must be a duration such as 15s or 24h")
			}
			*target = parsed
		}
	}
	for _, validate := range []func() error{c.Delivery.Validate, c.HTTP.Validate, c.Notifications.Validate, c.Retention.Validate} {
		if err := validate(); err != nil {
			return c, err
		}
	}
	return c, nil
}
