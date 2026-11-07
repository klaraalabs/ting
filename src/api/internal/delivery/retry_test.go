package delivery

import (
	"context"
	"testing"
	"time"
)

func TestRetryBoundsAndRetryAfter(t *testing.T) {
	c := DefaultConfig()
	c.RetryBase = time.Second
	c.RetryMax = 8 * time.Second
	for _, test := range []struct {
		attempt int
		upper   time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {4, 8 * time.Second}, {100, 8 * time.Second}} {
		for i := 0; i < 100; i++ {
			d := retryDelay(c, test.attempt, 0)
			if d < test.upper/2 || d > test.upper {
				t.Fatalf("attempt %d delay %s", test.attempt, d)
			}
		}
	}
	if got := retryDelay(c, 100, time.Hour); got != time.Hour {
		t.Fatalf("Retry-After was capped: %s", got)
	}
}

type panickingSender struct{}

func (panickingSender) Send(context.Context, Message) Result { panic("secret provider material") }
func TestProviderPanicDoesNotEscape(t *testing.T) {
	r := callSender(context.Background(), panickingSender{}, Message{})
	if r.Outcome != Retryable || r.Category != "internal_error" {
		t.Fatalf("unexpected result: %+v", r)
	}
}
func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Concurrency = 0 }, func(c *Config) { c.Concurrency = 65 },
		func(c *Config) { c.LeaseDuration = c.AttemptTimeout + 2*c.DatabaseTimeout },
		func(c *Config) { c.RetryMax = c.RetryBase - 1 }, func(c *Config) { c.MaxAttempts = 0 },
		func(c *Config) { c.TTL = -1 }, func(c *Config) { c.ShutdownGrace = 0 },
	} {
		c := DefaultConfig()
		change(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
}
