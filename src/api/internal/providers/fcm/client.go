package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
	"google.golang.org/api/option"

	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/providers"
)

type messagingClient interface {
	Send(context.Context, *messaging.Message) (string, error)
}
type observation struct {
	retryAfter  time.Duration
	authFailure *delivery.Result
	cancelSDK   context.CancelFunc
}
type observationKey struct{}

// Auth is scoped to this credential version, with a context-aware token refresh.
// A cached client never retains the context of the request that first created it.
type authenticatedTransport struct {
	base        http.RoundTripper
	config      *jwt.Config
	tokenClient *http.Client
	gate        chan struct{}
	token       *oauth2.Token
}

func (t *authenticatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "fcm.googleapis.com" {
		return nil, errors.New("unexpected messaging endpoint")
	}
	token, err := t.accessToken(req.Context())
	if err != nil {
		if observed, _ := req.Context().Value(observationKey{}).(*observation); observed != nil {
			result := delivery.Result{Outcome: delivery.Retryable, Category: "provider_unavailable"}
			var authErr *oauth2.RetrieveError
			if errors.As(err, &authErr) && authErr.Response != nil {
				status := authErr.Response.StatusCode
				if status == 400 || status == 401 || status == 403 {
					result = delivery.Result{Outcome: delivery.PermanentFailure, Category: "provider_auth"}
				}
				if status == 429 {
					result = delivery.Result{Outcome: delivery.Retryable, Category: "provider_throttled", RetryAfter: time.Minute}
				}
			}
			observed.authFailure = &result
			// The SDK treats token-exchange failures as network errors. Stop its
			// inner retries for permanent auth errors or a provider Retry-After;
			// the worker receives the classified result using its uncanceled context.
			if observed.cancelSDK != nil && (result.Outcome == delivery.PermanentFailure || result.RetryAfter > 0 || observed.retryAfter > 0) {
				observed.cancelSDK()
			}
		}
		return nil, err
	}
	clone := req.Clone(req.Context())
	if observed, _ := req.Context().Value(observationKey{}).(*observation); observed != nil {
		observed.authFailure = nil
	}
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(clone)
}
func (t *authenticatedTransport) accessToken(ctx context.Context) (string, error) {
	select {
	case t.gate <- struct{}{}:
		defer func() { <-t.gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.token.Valid() {
		return t.token.AccessToken, nil
	}
	// jwt.TokenSource uses Client.PostForm without attaching its context to the
	// request. Bind it explicitly so cancellation and Retry-After observation
	// cover the token exchange as well as the messaging request.
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tokenClient := *t.tokenClient
	tokenClient.Transport = contextTransport{ctx: refreshCtx, base: tokenClient.Transport}
	tokenCtx := context.WithValue(refreshCtx, oauth2.HTTPClient, &tokenClient)
	token, err := t.config.TokenSource(tokenCtx).Token()
	if err != nil {
		return "", err
	}
	t.token = token
	return token.AccessToken, nil
}

type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.Clone(t.ctx))
}

type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if response != nil {
		if observed, _ := req.Context().Value(observationKey{}).(*observation); observed != nil {
			delay := providers.RetryAfter(response.Header.Get("Retry-After"), time.Now())
			if delay > observed.retryAfter {
				observed.retryAfter = delay
			}
		}
		response.Body = providers.LimitedBody(response.Body)
	}
	return response, err
}

func newSDKClient(c providers.FCMCredential, base http.RoundTripper) (messagingClient, error) {
	account, err := providers.ParseServiceAccount(c.ServiceAccount, c.ProjectID)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(account)
	if err != nil {
		return nil, providers.ErrCredentials
	}
	config, err := google.JWTConfigFromJSON(canonical, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, providers.ErrCredentials
	}
	bounded := boundedTransport{base: base}
	auth := &authenticatedTransport{base: bounded, config: config, gate: make(chan struct{}, 1), tokenClient: &http.Client{Transport: bounded, Timeout: 5 * time.Second, CheckRedirect: providers.NoRedirect}}
	httpClient := &http.Client{Transport: auth, Timeout: 15 * time.Second, CheckRedirect: providers.NoRedirect}
	// Explicit configuration and credentials prevent any default Firebase app,
	// FIREBASE_CONFIG, or GOOGLE_APPLICATION_CREDENTIALS fallback.
	app, err := firebase.NewApp(context.Background(), &firebase.Config{ProjectID: account.ProjectID}, option.WithCredentialsJSON(canonical), option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, providers.ErrCredentials
	}
	client, err := app.Messaging(context.Background())
	if err != nil {
		return nil, providers.ErrCredentials
	}
	return client, nil
}
