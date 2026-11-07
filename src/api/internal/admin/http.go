package admin

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/apierror"
)

// Session and management requests share the same HTTPS, origin and cookie boundary.
func NewHandler(s *Service, m *Management, isHTTPS func(*http.Request) bool) http.Handler {
	origin, _ := url.Parse(s.Config.Origin)
	management := m.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		err := func() error {
			if !s.Config.Enabled {
				return apierror.NotFound
			}
			if r.URL.Path != "/admin/api/v1/session" && r.URL.Path != "/admin/api/v1/apps" && !strings.HasPrefix(r.URL.Path, "/admin/api/v1/apps/") {
				return apierror.NotFound
			}
			if !isHTTPS(r) {
				return apierror.New(400, "https_required", "Operator authentication requires HTTPS.")
			}
			if origin == nil || r.Host != origin.Host {
				return apierror.New(403, "origin_rejected", "Use the configured admin origin.")
			}
			if r.Header.Get("Authorization") != "" {
				return apierror.Unauthorized
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				return apierror.New(403, "origin_rejected", "Cross-origin admin requests are not allowed.")
			}
			if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || origins[0] != s.Config.Origin) {
				return apierror.New(403, "origin_rejected", "Use the configured admin origin.")
			}
			if r.URL.Path != "/admin/api/v1/session" {
				token, err := sessionCookie(r)
				if err != nil {
					return err
				}
				session, err := s.Authenticate(ctx, token)
				if err != nil {
					return err
				}
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					if err = checkMutation(r, s.Config.Origin, session.CSRFToken); err != nil {
						return err
					}
				}
				management.ServeHTTP(w, r)
				return nil
			}
			switch r.Method {
			case http.MethodPost:
				if err := checkMutation(r, s.Config.Origin, "login"); err != nil {
					return err
				}
				var request struct {
					Username string `json:"username"`
					Password string `json:"password"`
				}
				r.Body = http.MaxBytesReader(w, r.Body, 4096)
				if err := apierror.Decode(w, r, &request); err != nil {
					var e *apierror.Error
					if errors.As(err, &e) && e.Status == 413 {
						return apierror.New(413, "payload_too_large", "Login body exceeds 4 KiB.")
					}
					return err
				}
				peer, _, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil {
					peer = r.RemoteAddr
				}
				previous, _ := sessionCookie(r)
				session, err := s.Login(ctx, request.Username, request.Password, peer, previous)
				if err != nil {
					return err
				}
				http.SetCookie(w, sessionSetCookie(session.Token, session.ExpiresAt, int(s.Config.SessionTTL.Seconds())))
				apierror.JSON(w, 200, session)
				return nil
			case http.MethodGet, http.MethodDelete:
				token, err := sessionCookie(r)
				if err != nil {
					return err
				}
				session, err := s.Authenticate(ctx, token)
				if err != nil {
					return err
				}
				if r.Method == http.MethodGet {
					apierror.JSON(w, 200, session)
					return nil
				}
				if err = checkMutation(r, s.Config.Origin, session.CSRFToken); err != nil {
					return err
				}
				if err = s.Logout(ctx, token); err != nil {
					return err
				}
				http.SetCookie(w, sessionSetCookie("", time.Unix(1, 0), -1))
				w.WriteHeader(http.StatusNoContent)
				return nil
			default:
				w.Header().Set("Allow", "GET, POST, DELETE")
				return apierror.New(405, "method_not_allowed", "Method not allowed.")
			}
		}()
		if err != nil {
			var e *apierror.Error
			if errors.As(err, &e) && e.Status == 429 {
				w.Header().Set("Retry-After", "60")
			}
			apierror.Write(w, r, err)
		}
	})
}
func checkMutation(r *http.Request, origin, csrf string) error {
	origins := r.Header.Values("Origin")
	tokens := r.Header.Values("X-Ting-CSRF")
	if len(origins) != 1 || origins[0] != origin || len(tokens) != 1 || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(csrf)) != 1 {
		return apierror.New(403, "csrf_rejected", "A matching Origin and X-Ting-CSRF header are required.")
	}
	return nil
}
func sessionCookie(r *http.Request) (string, error) {
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) != 1 || !validToken(cookies[0].Value) {
		return "", apierror.Unauthorized
	}
	return cookies[0].Value, nil
}
func sessionSetCookie(token string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge}
}
