package delivery

import (
	"errors"
	"time"
)

const DefaultTTL = 24 * time.Hour

type Config struct {
	Concurrency     int
	PollInterval    time.Duration
	LeaseDuration   time.Duration
	AttemptTimeout  time.Duration
	DatabaseTimeout time.Duration
	MaxAttempts     int
	RetryBase       time.Duration
	RetryMax        time.Duration
	TTL             time.Duration
	ShutdownGrace   time.Duration
}

func DefaultConfig() Config {
	return Config{Concurrency: 4, PollInterval: time.Second, LeaseDuration: time.Minute,
		AttemptTimeout: 15 * time.Second, DatabaseTimeout: 5 * time.Second, MaxAttempts: 8,
		RetryBase: time.Second, RetryMax: 5 * time.Minute, TTL: DefaultTTL, ShutdownGrace: 15 * time.Second}
}

func (c Config) Validate() error {
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return errors.New("delivery concurrency must be between 1 and 64")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 100 {
		return errors.New("delivery max attempts must be between 1 and 100")
	}
	if c.PollInterval <= 0 || c.PollInterval > time.Minute || c.AttemptTimeout <= 0 || c.AttemptTimeout > 5*time.Minute || c.DatabaseTimeout <= 0 || c.DatabaseTimeout > time.Minute {
		return errors.New("invalid delivery polling, attempt, or database timeout")
	}
	if c.LeaseDuration <= c.AttemptTimeout+2*c.DatabaseTimeout || c.LeaseDuration > time.Hour {
		return errors.New("delivery lease must exceed attempt timeout plus two database timeouts and be at most one hour")
	}
	if c.RetryBase <= 0 || c.RetryMax < c.RetryBase || c.RetryMax > 24*time.Hour {
		return errors.New("delivery retry delays must be positive, ordered, and at most 24 hours")
	}
	if c.TTL <= 0 || c.TTL > 30*24*time.Hour {
		return errors.New("delivery TTL must be positive and at most 30 days")
	}
	if c.ShutdownGrace <= 0 || c.ShutdownGrace > time.Minute {
		return errors.New("delivery shutdown grace must be positive and at most one minute")
	}
	return nil
}
