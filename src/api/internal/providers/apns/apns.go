package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/providers"
)

type Source interface {
	APNs(context.Context, string, string) (providers.APNsCredential, error)
}
type Adapter struct {
	source       Source
	mu           sync.Mutex
	clients      map[string]*client
	newTransport func() *http.Transport
	now          func() time.Time
}
type client struct {
	version, environment, bundle, team, keyID string
	key                                       *ecdsa.PrivateKey
	http                                      *http.Client
	transport                                 *http.Transport
	retired                                   atomic.Bool
	tokenMu                                   sync.Mutex
	token                                     string
	issued                                    time.Time
}

func New(source Source) *Adapter {
	return &Adapter{source: source, clients: map[string]*client{}, newTransport: providers.Transport, now: time.Now}
}
func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, c := range a.clients {
		c.close()
		delete(a.clients, key)
	}
}
func (c *client) close() { c.retired.Store(true); c.transport.CloseIdleConnections() }

func (a *Adapter) get(ctx context.Context, app, environment string) (*client, error) {
	// Serialize version lookup and cache replacement so a delayed old lookup cannot
	// roll a cache back after another request has observed a replacement.
	a.mu.Lock()
	defer a.mu.Unlock()
	key := app + ":" + environment
	credential, err := a.source.APNs(ctx, app, environment)
	old := a.clients[key]
	if err != nil {
		if old != nil && (errors.Is(err, providers.ErrNotConfigured) || errors.Is(err, providers.ErrCredentials)) {
			old.close()
			delete(a.clients, key)
		}
		return nil, err
	}
	if old != nil && old.version == credential.Version {
		return old, nil
	}
	if old != nil {
		old.close()
		delete(a.clients, key)
	}
	if credential.Environment != environment || credential.Version == "" || credential.TeamID == "" || credential.KeyID == "" || credential.BundleID == "" {
		return nil, providers.ErrCredentials
	}
	block, _ := pem.Decode(credential.PrivateKey)
	if block == nil {
		return nil, providers.ErrCredentials
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, providers.ErrCredentials
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || private.Curve != elliptic.P256() {
		return nil, providers.ErrCredentials
	}
	if len(a.clients) >= 128 {
		for key, c := range a.clients {
			c.close()
			delete(a.clients, key)
			break
		}
	}
	tr := a.newTransport()
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP2(true)
	c := &client{version: credential.Version, environment: environment, bundle: credential.BundleID, team: credential.TeamID, keyID: credential.KeyID, key: private, transport: tr, http: &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: providers.NoRedirect}}
	a.clients[key] = c
	return c, nil
}
func (c *client) authorization(now time.Time) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && !now.Before(c.issued) && now.Sub(c.issued) < 50*time.Minute {
		return c.token, nil
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{Issuer: c.team, IssuedAt: jwt.NewNumericDate(now)})
	token.Header["kid"] = c.keyID
	signed, err := token.SignedString(c.key)
	if err != nil {
		return "", err
	}
	c.token = signed
	c.issued = now
	return signed, nil
}
func (c *client) expireAuthorization(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

func (a *Adapter) Send(ctx context.Context, m delivery.Message) delivery.Result {
	if m.Platform != "ios" || (m.Environment != "sandbox" && m.Environment != "production") || m.RemainingTTL <= 0 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	if raw, err := hex.DecodeString(m.PushToken); err != nil || len(raw) == 0 || len(raw) > 256 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	payload := map[string]any{"aps": map[string]any{"alert": map[string]string{"title": m.Title, "body": m.Body}}}
	for key, value := range m.Data {
		if key == "aps" {
			return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
		}
		payload[key] = value
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > 4096 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	deadline := a.now().Add(m.RemainingTTL)
	c, err := a.get(ctx, m.AppID, m.Environment)
	if err != nil {
		return providers.CredentialFailure(err)
	}
	defer func() {
		if c.retired.Load() {
			c.transport.CloseIdleConnections()
		}
	}()
	now := a.now()
	if !now.Before(deadline) {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	token, err := c.authorization(now)
	if err != nil {
		return providers.CredentialFailure(providers.ErrCredentials)
	}
	host := "https://api.push.apple.com"
	if m.Environment == "sandbox" {
		host = "https://api.sandbox.push.apple.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+m.PushToken, bytes.NewReader(body))
	if err != nil {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apns-topic", c.bundle)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", strconv.FormatInt(deadline.Unix(), 10))
	if m.NotificationID != "" {
		req.Header.Set("apns-id", m.NotificationID)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_error"}
	}
	raw, err := io.ReadAll(providers.LimitedBody(response.Body))
	if err != nil {
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
	}
	var failure struct {
		Reason    string `json:"reason"`
		Timestamp int64  `json:"timestamp"`
	}
	_ = json.Unmarshal(raw, &failure)
	if failure.Reason == "ExpiredProviderToken" && response.StatusCode == 403 {
		c.expireAuthorization(token)
	}
	result := classify(response.StatusCode, failure.Reason)
	result.RetryAfter = providers.RetryAfter(response.Header.Get("Retry-After"), a.now())
	if failure.Reason == "TooManyProviderTokenUpdates" && result.RetryAfter < 20*time.Minute {
		result.RetryAfter = 20 * time.Minute
	}
	if result.Outcome == delivery.InvalidToken && failure.Timestamp > 0 {
		result.InvalidatedAt = time.UnixMilli(failure.Timestamp)
	}
	return result
}
func classify(status int, reason string) delivery.Result {
	switch {
	case status == 200:
		return delivery.Result{Outcome: delivery.Accepted}
	case status == 410 && reason == "Unregistered":
		return delivery.Result{Outcome: delivery.InvalidToken, Category: "invalid_token"}
	case status == 429:
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_throttled"}
	case status >= 500 || status == 408:
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
	case status == 403 && reason == "ExpiredProviderToken":
		return delivery.Result{Outcome: delivery.Retryable, Category: "provider_auth"}
	case status == 401 || status == 403:
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_auth"}
	case status == 400 || status == 413:
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "invalid_payload"}
	default:
		return delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_error"}
	}
}
