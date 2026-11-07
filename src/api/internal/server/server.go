package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/auth"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

func New(db *pgxpool.Pool, vault *secrets.Vault, proxies []netip.Prefix, allowLocalHTTP bool, deliveryTTL time.Duration, adminConfig admin.Config, limits Limits, sendLimits notifications.Limits) http.Handler {
	authentication := &auth.Service{DB: db, Vault: vault}
	devices := &installations.Service{DB: db, Vault: vault}
	sends := &notifications.Service{DB: db, DeliveryTTL: deliveryTTL, Limits: sendLimits}
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin.NewHandler(&admin.Service{DB: db, Config: adminConfig}, &admin.Management{Apps: &apps.Service{DB: db, Vault: vault}, Devices: devices, Notifications: sends}, func(r *http.Request) bool { return httpsRequest(r, proxies) }))
	handle := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				apierror.Write(w, r, err)
			}
		})
	}
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) error {
		apierror.JSON(w, 200, map[string]string{"status": "ok"})
		return nil
	})
	handle("GET /readyz", func(w http.ResponseWriter, r *http.Request) error {
		if err := db.Ping(r.Context()); err != nil {
			return apierror.New(503, "not_ready", "Database is unavailable.")
		}
		apierror.JSON(w, 200, map[string]string{"status": "ready"})
		return nil
	})
	handle("POST /v1/notifications", func(w http.ResponseWriter, r *http.Request) error {
		appID, err := authentication.Sender(r.Context(), auth.Bearer(r))
		if err != nil {
			return err
		}
		var request notifications.SendRequest
		if err = apierror.Decode(w, r, &request); err != nil {
			return err
		}
		result, err := sends.Send(r.Context(), appID, r.Header.Get("Idempotency-Key"), request)
		if err != nil {
			return err
		}
		apierror.JSON(w, 202, result)
		return nil
	})
	handle("GET /v1/notifications/{id}", func(w http.ResponseWriter, r *http.Request) error {
		appID, err := authentication.Sender(r.Context(), auth.Bearer(r))
		if err != nil {
			return err
		}
		result, err := sends.Status(r.Context(), appID, r.PathValue("id"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	handle("PUT /v1/installations/{id}", func(w http.ResponseWriter, r *http.Request) error {
		appID := r.Header.Get("X-Ting-App-Id")
		user, err := authentication.Identity(r.Context(), appID, auth.Bearer(r))
		if err != nil {
			return err
		}
		var request installations.Registration
		if err = apierror.Decode(w, r, &request); err != nil {
			return err
		}
		result, created, err := devices.Register(r.Context(), appID, r.PathValue("id"), user, r.Header.Get("X-Ting-Installation-Token"), request)
		if err != nil {
			return err
		}
		status := 200
		if created {
			status = 201
		}
		apierror.JSON(w, status, result)
		return nil
	})
	handle("DELETE /v1/installations/{id}", func(w http.ResponseWriter, r *http.Request) error {
		generation, err := strconv.ParseInt(r.Header.Get("X-Ting-Installation-Generation"), 10, 64)
		if err != nil {
			return apierror.Invalid("X-Ting-Installation-Generation must be a positive integer.")
		}
		if err := devices.Revoke(r.Context(), r.Header.Get("X-Ting-App-Id"), r.PathValue("id"), auth.Bearer(r), generation); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	for _, path := range []string{"/healthz", "/readyz", "/v1/notifications", "/v1/notifications/{id}", "/v1/installations/{id}"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			apierror.Write(w, r, apierror.New(405, "method_not_allowed", "Method not allowed."))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apierror.Write(w, r, apierror.NotFound) })
	limiter := newLimiter(limits)
	slots := make(chan struct{}, limits.MaxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generate locally so untrusted request headers never enter diagnostic IDs.
		requestID := uuid.NewString()
		w.Header().Set("X-Request-Id", requestID)
		ctx, cancel := context.WithTimeout(r.Context(), limits.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			if recover() != nil {
				slog.ErrorContext(r.Context(), "request panic recovered", "requestId", requestID, "route", r.Pattern)
				apierror.Write(w, r, apierror.New(500, "internal_error", "The request could not be completed."))
			}
		}()
		health := r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
		if !health && !secureRequest(r, proxies, allowLocalHTTP) {
			apierror.Write(w, r, apierror.New(400, "https_required", "Use HTTPS for non-local requests."))
			return
		}
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && !limiter.allow(peerIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			apierror.Write(w, r, apierror.New(429, "rate_limited", "Too many requests. Retry later."))
			return
		}
		if !health {
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(limits.RequestTimeout))
			defer controller.SetReadDeadline(time.Time{})
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				apierror.Write(w, r, apierror.Retry(503, "server_busy", "Too many active requests. Retry later.", 1))
				return
			}
			if r.ContentLength > int64(limits.MaxBodyBytes) {
				apierror.Write(w, r, apierror.New(413, "payload_too_large", "Request body exceeds the configured limit."))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, int64(limits.MaxBodyBytes))
		}
		mux.ServeHTTP(w, r)
	})
}
