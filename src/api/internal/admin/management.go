package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
)

type Management struct {
	Apps          *apps.Service
	Devices       *installations.Service
	Notifications *notifications.Service
}

func (m *Management) handler() http.Handler {
	mux := http.NewServeMux()
	type endpoint func(http.ResponseWriter, *http.Request) error
	methods := map[string]string{}
	handle := func(method, path string, fn endpoint) {
		if methods[path] != "" {
			methods[path] += ", "
		}
		methods[path] += method
		mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				apierror.Write(w, r, err)
			}
		})
	}
	const base = "/admin/api/v1/apps"
	const app = base + "/{appId}"
	handle("GET", base, func(w http.ResponseWriter, r *http.Request) error {
		limit, offset, err := pageParams(r)
		if err != nil {
			return err
		}
		result, err := m.Apps.ListPage(r.Context(), limit+1, offset)
		if err != nil {
			return err
		}
		writePage(w, result, limit, offset)
		return nil
	})
	handle("POST", base, func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		}
		if err := apierror.Decode(w, r, &input); err != nil {
			return err
		}
		result, err := m.Apps.Create(r.Context(), input.Name, input.Slug)
		if err != nil {
			return err
		}
		apierror.JSON(w, 201, result)
		return nil
	})
	handle("GET", app, func(w http.ResponseWriter, r *http.Request) error {
		result, err := m.Apps.Show(r.Context(), r.PathValue("appId"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	handle("PATCH", app, func(w http.ResponseWriter, r *http.Request) error {
		var input apps.UpdateRequest
		if err := apierror.Decode(w, r, &input); err != nil {
			return err
		}
		result, err := m.Apps.Update(r.Context(), r.PathValue("appId"), input)
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	handle("GET", app+"/tokens", func(w http.ResponseWriter, r *http.Request) error {
		limit, offset, err := pageParams(r)
		if err != nil {
			return err
		}
		result, err := m.Apps.ListTokens(r.Context(), r.PathValue("appId"), limit+1, offset)
		if err != nil {
			return err
		}
		writePage(w, result, limit, offset)
		return nil
	})
	handle("POST", app+"/tokens", func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			Label string `json:"label"`
		}
		if err := apierror.Decode(w, r, &input); err != nil {
			return err
		}
		result, err := m.Apps.CreateToken(r.Context(), r.PathValue("appId"), input.Label)
		if err != nil {
			return err
		}
		apierror.JSON(w, 201, result)
		return nil
	})
	handle("DELETE", app+"/tokens/{tokenId}", func(w http.ResponseWriter, r *http.Request) error {
		if err := m.Apps.RevokeToken(r.Context(), r.PathValue("appId"), r.PathValue("tokenId")); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET", app+"/identity-secrets", func(w http.ResponseWriter, r *http.Request) error {
		limit, offset, err := pageParams(r)
		if err != nil {
			return err
		}
		result, err := m.Apps.ListIdentities(r.Context(), r.PathValue("appId"), limit+1, offset)
		if err != nil {
			return err
		}
		writePage(w, result, limit, offset)
		return nil
	})
	handle("POST", app+"/identity-secrets", func(w http.ResponseWriter, r *http.Request) error {
		var input struct{}
		if err := apierror.Decode(w, r, &input); err != nil {
			return err
		}
		result, err := m.Apps.CreateIdentity(r.Context(), r.PathValue("appId"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 201, result)
		return nil
	})
	handle("DELETE", app+"/identity-secrets/{kid}", func(w http.ResponseWriter, r *http.Request) error {
		if err := m.Apps.RetireIdentity(r.Context(), r.PathValue("appId"), r.PathValue("kid")); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET", app+"/apns", func(w http.ResponseWriter, r *http.Request) error {
		result, err := m.Apps.APNsStatus(r.Context(), r.PathValue("appId"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	handle("PUT", app+"/apns", func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			Environment string `json:"environment"`
			BundleID    string `json:"bundleId"`
			TeamID      string `json:"teamId"`
			KeyID       string `json:"keyId"`
			PrivateKey  string `json:"privateKey"`
		}
		if err := decodeProvider(w, r, &input); err != nil {
			return err
		}
		if err := m.Apps.ConfigureAPNs(r.Context(), r.PathValue("appId"), apps.APNsConfig{Environment: input.Environment, BundleID: input.BundleID, TeamID: input.TeamID, KeyID: input.KeyID, Key: []byte(input.PrivateKey)}); err != nil {
			return err
		}
		apierror.JSON(w, 200, map[string]string{"status": "configured_untested"})
		return nil
	})
	handle("DELETE", app+"/apns", func(w http.ResponseWriter, r *http.Request) error {
		env := r.URL.Query()["environment"]
		if len(env) != 1 {
			return apierror.Invalid("Specify one environment: sandbox, production, or both.")
		}
		if err := m.Apps.RemoveAPNs(r.Context(), r.PathValue("appId"), env[0]); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET", app+"/fcm", func(w http.ResponseWriter, r *http.Request) error {
		result, err := m.Apps.FCMStatus(r.Context(), r.PathValue("appId"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	handle("PUT", app+"/fcm", func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			ProjectID      string          `json:"projectId"`
			ServiceAccount json.RawMessage `json:"serviceAccount"`
		}
		if err := decodeProvider(w, r, &input); err != nil {
			return err
		}
		if err := m.Apps.ConfigureFCM(r.Context(), r.PathValue("appId"), input.ProjectID, input.ServiceAccount); err != nil {
			return err
		}
		apierror.JSON(w, 200, map[string]string{"status": "configured_untested"})
		return nil
	})
	handle("DELETE", app+"/fcm", func(w http.ResponseWriter, r *http.Request) error {
		if err := m.Apps.RemoveFCM(r.Context(), r.PathValue("appId")); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET", app+"/installations", func(w http.ResponseWriter, r *http.Request) error {
		limit, offset, err := pageParams(r)
		if err != nil {
			return err
		}
		if _, err = m.Apps.Show(r.Context(), r.PathValue("appId")); err != nil {
			return err
		}
		result, err := m.Devices.List(r.Context(), r.PathValue("appId"), limit+1, offset)
		if err != nil {
			return err
		}
		writePage(w, result, limit, offset)
		return nil
	})
	handle("POST", app+"/test-notifications", func(w http.ResponseWriter, r *http.Request) error {
		var input struct {
			InstallationID string `json:"installationId"`
		}
		if err := apierror.Decode(w, r, &input); err != nil {
			return err
		}
		result, err := m.Notifications.SendTest(r.Context(), r.PathValue("appId"), r.Header.Get("Idempotency-Key"), input.InstallationID)
		if err != nil {
			return err
		}
		apierror.JSON(w, 202, result)
		return nil
	})
	handle("GET", app+"/notifications", func(w http.ResponseWriter, r *http.Request) error {
		limit, offset, err := pageParams(r)
		if err != nil {
			return err
		}
		if _, err = m.Apps.Show(r.Context(), r.PathValue("appId")); err != nil {
			return err
		}
		result, err := m.Notifications.Recent(r.Context(), r.PathValue("appId"), limit+1, offset)
		if err != nil {
			return err
		}
		writePage(w, result, limit, offset)
		return nil
	})
	handle("GET", app+"/notifications/{notificationId}", func(w http.ResponseWriter, r *http.Request) error {
		result, err := m.Notifications.Status(r.Context(), r.PathValue("appId"), r.PathValue("notificationId"))
		if err != nil {
			return err
		}
		apierror.JSON(w, 200, result)
		return nil
	})
	for path, allowed := range methods {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allowed)
			apierror.Write(w, r, apierror.New(405, "method_not_allowed", "Method not allowed."))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apierror.Write(w, r, apierror.NotFound) })
	return mux
}

func pageParams(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	for _, param := range []struct {
		name     string
		target   *int
		min, max int
	}{{"limit", &limit, 1, 100}, {"offset", &offset, 0, 100000}} {
		if values, exists := r.URL.Query()[param.name]; exists {
			if len(values) != 1 {
				return 0, 0, apierror.Invalid("Provide one limit and offset.")
			}
			value, err := strconv.Atoi(values[0])
			if err != nil || value < param.min || value > param.max {
				return 0, 0, apierror.Invalid("limit must be 1–100 and offset 0–100000.")
			}
			*param.target = value
		}
	}
	return limit, offset, nil
}
func writePage[T any](w http.ResponseWriter, items []T, limit, offset int) {
	var next *int
	if len(items) > limit {
		items = items[:limit]
		n := offset + limit
		if n <= 100000 {
			next = &n
		}
	}
	apierror.JSON(w, 200, struct {
		Items      []T  `json:"items"`
		NextOffset *int `json:"nextOffset"`
	}{items, next})
}
func decodeProvider(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	return apierror.Decode(w, r, out)
}
