package delivery

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type Pool struct {
	store     *Store
	senders   map[string]Sender
	platforms []string
	config    Config
}

func NewPool(store *Store, senders map[string]Sender, c Config) (*Pool, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if store == nil || store.DB == nil || store.Vault == nil {
		return nil, errors.New("delivery store and vault are required")
	}
	p := &Pool{store: store, senders: make(map[string]Sender), config: c}
	for platform, sender := range senders {
		if (platform != "ios" && platform != "android") || sender == nil {
			return nil, errors.New("invalid delivery sender")
		}
		p.senders[platform] = sender
		p.platforms = append(p.platforms, platform)
	}
	sort.Strings(p.platforms)
	return p, nil
}

// Run stops leasing when ctx is canceled, but lets in-flight attempts finish for
// ShutdownGrace. Afterwards it cancels their contexts and waits for cleanup.
// Senders must respect their contexts; no per-delivery goroutines are spawned.
func (p *Pool) Run(ctx context.Context) {
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); p.maintain(ctx) }()
	for i := 0; i < p.config.Concurrency && len(p.platforms) > 0; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.work(ctx, workCtx) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	<-ctx.Done()
	timer := time.NewTimer(p.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		cancelWork()
		<-done
	}
}

func (p *Pool) work(intakeCtx, workCtx context.Context) {
	for intakeCtx.Err() == nil {
		claimCtx, cancel := context.WithTimeout(intakeCtx, p.config.DatabaseTimeout)
		lease, err := p.store.Claim(claimCtx, p.platforms, p.config)
		cancel()
		if err != nil {
			if intakeCtx.Err() != nil {
				return
			}
			slog.Error("delivery lease failed")
			if !wait(intakeCtx, p.config.PollInterval) {
				return
			}
			continue
		}
		if lease == nil {
			if !wait(intakeCtx, p.config.PollInterval) {
				return
			}
			continue
		}
		if intakeCtx.Err() != nil {
			releaseCtx, cancel := context.WithTimeout(workCtx, p.config.DatabaseTimeout)
			if err := p.store.Release(releaseCtx, *lease); err != nil {
				slog.Error("delivery lease release failed", "deliveryId", lease.ID)
			}
			cancel()
			return
		}
		p.attempt(workCtx, *lease)
	}
}

func (p *Pool) attempt(workCtx context.Context, l Lease) {
	prepareCtx, cancel := context.WithTimeout(workCtx, p.config.DatabaseTimeout)
	message, skip, budget, err := p.store.Prepare(prepareCtx, l, p.config.DatabaseTimeout)
	cancel()
	if err != nil {
		if !errors.Is(err, ErrLeaseLost) && workCtx.Err() == nil {
			slog.Error("delivery preparation failed", "deliveryId", l.ID, "appId", l.AppID)
		}
		return // The lease will expire, including after database outages.
	}
	result := Result{}
	if skip == "" {
		if budget > p.config.AttemptTimeout {
			budget = p.config.AttemptTimeout
		}
		attemptCtx, cancel := context.WithTimeout(workCtx, budget)
		result = callSender(attemptCtx, p.senders[message.Platform], message)
		if attemptCtx.Err() != nil && result.Outcome != Accepted {
			result = Result{Outcome: Retryable, Category: "provider_timeout", RetryAfter: result.RetryAfter}
		}
		cancel()
	}
	// A forced shutdown leaves ownership intact for crash-safe recovery. Do not
	// record an ambiguous canceled call as a provider rejection or invalid token.
	if workCtx.Err() != nil {
		return
	}
	finishCtx, cancel := context.WithTimeout(workCtx, p.config.DatabaseTimeout)
	defer cancel()
	if _, err := p.store.Complete(finishCtx, l, result, skip, p.config); err != nil {
		slog.Error("delivery result persistence failed", "deliveryId", l.ID, "appId", l.AppID, "notificationId", l.NotificationID)
	}
}

func callSender(ctx context.Context, sender Sender, m Message) (result Result) {
	defer func() {
		if recover() != nil {
			result = Result{Outcome: Retryable, Category: "internal_error"}
		}
	}()
	return sender.Send(ctx, m)
}

func (p *Pool) maintain(ctx context.Context) {
	for ctx.Err() == nil {
		operation, cancel := context.WithTimeout(ctx, p.config.DatabaseTimeout)
		_, err := p.store.Sweep(operation, p.config.MaxAttempts)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("delivery expiration failed")
		}
		if !wait(ctx, p.config.PollInterval) {
			return
		}
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
