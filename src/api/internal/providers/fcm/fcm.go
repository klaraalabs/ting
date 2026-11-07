package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"firebase.google.com/go/v4/errorutils"
	"firebase.google.com/go/v4/messaging"

	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/providers"
)

type Source interface {
	FCM(context.Context, string) (providers.FCMCredential, error)
}
type entry struct {
	version string
	client  messagingClient
	close   func()
	retired atomic.Bool
}

func (e *entry) retire() { e.retired.Store(true); e.close() }

type Adapter struct {
	source  Source
	mu      sync.Mutex
	clients map[string]*entry
	factory func(providers.FCMCredential) (messagingClient, func(), error)
}

func New(source Source) *Adapter {
	return &Adapter{source: source, clients: map[string]*entry{}, factory: func(c providers.FCMCredential) (messagingClient, func(), error) {
		tr := providers.Transport()
		client, err := newSDKClient(c, tr)
		if err != nil {
			tr.CloseIdleConnections()
		}
		return client, tr.CloseIdleConnections, err
	}}
}
func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for app, c := range a.clients {
		c.retire()
		delete(a.clients, app)
	}
}
func (a *Adapter) get(ctx context.Context, app string) (*entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, err := a.source.FCM(ctx, app)
	old := a.clients[app]
	if err != nil {
		if old != nil && (errors.Is(err, providers.ErrNotConfigured) || errors.Is(err, providers.ErrCredentials)) {
			old.retire()
			delete(a.clients, app)
		}
		return nil, err
	}
	if old != nil && old.version == credential.Version {
		return old, nil
	}
	if old != nil {
		old.retire()
		delete(a.clients, app)
	}
	if credential.Version == "" {
		return nil, providers.ErrCredentials
	}
	client, close, err := a.factory(credential)
	if err != nil {
		return nil, err
	}
	if len(a.clients) >= 128 {
		for key, c := range a.clients {
			c.retire()
			delete(a.clients, key)
			break
		}
	}
	e := &entry{version: credential.Version, client: client, close: close}
	a.clients[app] = e
	return e, nil
}
func (a *Adapter) Send(ctx context.Context, m delivery.Message) delivery.Result {
	if m.Platform != "android" || m.Environment != "" || m.PushToken == "" || m.RemainingTTL <= 0 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	deadline := time.Now().Add(m.RemainingTTL)
	client, err := a.get(ctx, m.AppID)
	if err != nil {
		return providers.CredentialFailure(err)
	}
	defer func() {
		if client.retired.Load() {
			client.close()
		}
	}()
	ttl := time.Until(deadline)
	if ttl <= 0 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	if ttl > 28*24*time.Hour {
		ttl = 28 * 24 * time.Hour
	}
	ttl = ttl.Truncate(time.Second)
	message := &messaging.Message{Notification: &messaging.Notification{Title: m.Title, Body: m.Body}, Data: m.Data, Android: &messaging.AndroidConfig{Priority: "high", TTL: &ttl}}
	// Check actual SDK encoding, excluding the routing token and request envelope.
	payload, err := json.Marshal(message)
	if err != nil || len(payload) > 4096 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	message.Token = m.PushToken
	sdkCtx, cancelSDK := context.WithCancel(ctx)
	defer cancelSDK()
	observed := &observation{cancelSDK: cancelSDK}
	name, err := client.client.Send(context.WithValue(sdkCtx, observationKey{}, observed), message)
	result := classify(err)
	if err == nil && name == "" {
		result = delivery.Result{Outcome: delivery.Retryable, Category: "provider_error"}
	}
	if err != nil && observed.authFailure != nil {
		result = *observed.authFailure
	}
	if observed.retryAfter > result.RetryAfter {
		result.RetryAfter = observed.retryAfter
	}
	return result
}
func classify(err error) delivery.Result {
	switch {
	case err == nil:
		return delivery.Result{Outcome: delivery.Accepted}
	case messaging.IsUnregistered(err):
		return delivery.Result{Outcome: delivery.InvalidToken, Category: "invalid_token"}
	case messaging.IsSenderIDMismatch(err) || messaging.IsThirdPartyAuthError(err) || errorutils.IsUnauthenticated(err) || errorutils.IsPermissionDenied(err):
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_auth"}
	case messaging.IsQuotaExceeded(err) || errorutils.IsResourceExhausted(err):
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_throttled", RetryAfter: time.Minute}
	case messaging.IsInvalidArgument(err) || errorutils.IsInvalidArgument(err):
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	case messaging.IsUnavailable(err) || messaging.IsInternal(err) || errorutils.IsUnavailable(err) || errorutils.IsInternal(err):
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
	default:
		if response := errorutils.HTTPResponse(err); response != nil && response.StatusCode >= 400 && response.StatusCode < 500 {
			return delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_error"}
		}
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
	}
}
