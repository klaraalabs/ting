package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

// The server has already committed when encoding the response fails.
type lostRegistrationResponse struct{ http.ResponseWriter }

func (lostRegistrationResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (f *fixture) installationRequest(method, id, jwt, appID, credential string, generation int64, body any, discard bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "https://ting.test/v1/installations/"+id, bytes.NewReader(raw))
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ting-App-Id", appID)
	if method == "DELETE" {
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("X-Ting-Installation-Generation", strconv.FormatInt(generation, 10))
	} else {
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("X-Ting-Installation-Token", credential)
	}
	recorder := httptest.NewRecorder()
	var writer http.ResponseWriter = recorder
	if discard {
		writer = lostRegistrationResponse{recorder}
	}
	f.handler.ServeHTTP(writer, req)
	return recorder
}

func TestInstallationLifecycleContract(t *testing.T) {
	var contract struct {
		Version   int `json:"version"`
		Scenarios []struct {
			Name  string `json:"name"`
			Steps []struct {
				Method          string `json:"method"`
				User            string `json:"user"`
				Generation      int64  `json:"generation"`
				Credential      string `json:"credential"`
				Proposed        string `json:"proposed"`
				Push            string `json:"push"`
				Status          int    `json:"status"`
				Error           string `json:"error"`
				DiscardResponse bool   `json:"discardResponse"`
				Restart         bool   `json:"restart"`
			} `json:"steps"`
			FinalUser       string `json:"finalUser"`
			FinalCredential string `json:"finalCredential"`
			FinalGeneration int64  `json:"finalGeneration"`
			FinalActive     bool   `json:"finalActive"`
		} `json:"scenarios"`
	}
	raw, err := os.ReadFile("../../../../tests/contracts/installation-lifecycle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Scenarios) == 0 {
		t.Fatal("unsupported or empty lifecycle contract")
	}
	for _, scenario := range contract.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			f := setup(t)
			a := f.app(t, true)
			id := uuid.NewString()
			credentials := map[string]string{"": "", "first": secrets.Token("ting_ik_"), "second": secrets.Token("ting_ik_"), "third": secrets.Token("ting_ik_")}
			for i, step := range scenario.Steps {
				if step.Restart {
					vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
					f.handler = server.New(f.db, vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())
				}
				registration := installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat(step.Push, 64), Token: credentials[step.Proposed], Generation: step.Generation}
				jwt := ""
				if step.Method == "PUT" {
					jwt = identity(a, step.User, nil)
				}
				r := f.installationRequest(step.Method, id, jwt, a.id, credentials[step.Credential], step.Generation, registration, step.DiscardResponse)
				if r.Code != step.Status {
					t.Fatalf("step %d: got %d, want %d: %s", i, r.Code, step.Status, r.Body.String())
				}
				if step.Error != "" {
					got := decode[struct{ Error struct{ Code string } }](t, r)
					if got.Error.Code != step.Error {
						t.Fatalf("step %d: got error %s, want %s", i, got.Error.Code, step.Error)
					}
				} else if step.Method == "PUT" && !step.DiscardResponse {
					got := decode[installations.Registered](t, r)
					if got.ID != id || got.Token != registration.Token || got.Generation != step.Generation {
						t.Fatalf("step %d: unexpected registration response", i)
					}
				}
			}
			var user string
			var hash []byte
			var generation int64
			var active bool
			err := f.db.QueryRow(context.Background(), `SELECT external_user_id,credential_hash,mutation_generation,active FROM installations WHERE app_id=$1 AND id=$2`, a.id, id).Scan(&user, &hash, &generation, &active)
			if err != nil {
				t.Fatal(err)
			}
			if user != scenario.FinalUser || !bytes.Equal(hash, secrets.Hash(credentials[scenario.FinalCredential])) || generation != scenario.FinalGeneration || active != scenario.FinalActive {
				t.Fatalf("unexpected final association: user=%s generation=%d active=%v", user, generation, active)
			}
			var count int
			if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM installations WHERE app_id=$1`, a.id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("recovery changed installation identity: count=%d err=%v", count, err)
			}
		})
	}
}

func TestLogoutFencesConcurrentRefresh(t *testing.T) {
	f := setup(t)
	a := f.app(t, true)
	one := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("a", 64))
	two := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("b", 64))
	queued := f.send(t, a, "before-logout", "alice")
	jwt := identity(a, "alice", nil)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			if index == 0 {
				r := f.installationRequest("PUT", one.ID, jwt, a.id, one.Token, 2, installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("a", 64), Token: one.Token, Generation: 2}, false)
				if r.Code != 200 && r.Code != 409 {
					t.Errorf("refresh: %d %s", r.Code, r.Body.String())
				}
			} else {
				r := f.installationRequest("DELETE", one.ID, "", a.id, one.Token, 3, nil, false)
				if r.Code != 204 {
					t.Errorf("logout: %d %s", r.Code, r.Body.String())
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	var active bool
	var generation int64
	if err := f.db.QueryRow(context.Background(), `SELECT active,mutation_generation FROM installations WHERE app_id=$1 AND id=$2`, a.id, one.ID).Scan(&active, &generation); err != nil {
		t.Fatal(err)
	}
	if active || generation != 3 {
		t.Fatalf("refresh resurrected logout: active=%v generation=%d", active, generation)
	}
	if status := f.status(t, a, queued.ID); status.Counts.Skipped != 1 || status.Counts.Queued != 1 {
		t.Fatalf("logout failed to cancel only its own deliveries: %+v", status)
	}
	if status := f.status(t, a, f.send(t, a, "after-logout", "alice").ID); status.Counts.Queued != 1 {
		t.Fatalf("logout affected other installation %s: %+v", two.ID, status)
	}
}

func TestRegistrationRecoveryStillRequiresFreshIdentity(t *testing.T) {
	f := setup(t)
	a := f.app(t, true)
	one := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("a", 64))
	body := installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("a", 64), Token: one.Token, Generation: 1}
	for _, jwt := range []string{"", one.Token, a.token} {
		r := f.installationRequest("PUT", one.ID, jwt, a.id, "", 1, body, false)
		expect(t, r, 401)
	}
	other := f.app(t, true)
	r := f.installationRequest("PUT", one.ID, identity(other, "alice", nil), a.id, "", 1, body, false)
	expect(t, r, 401)
	for _, generation := range []int64{0, -1, installations.MaxGeneration + 1} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			body.Generation = generation
			expect(t, f.installationRequest("PUT", one.ID, identity(a, "alice", nil), a.id, one.Token, generation, body, false), 400)
			expect(t, f.installationRequest("DELETE", one.ID, "", a.id, one.Token, generation, nil, false), 400)
		})
	}
}

func TestRegistrationReplayIsReadOnly(t *testing.T) {
	f := setup(t)
	a := f.app(t, true)
	one := f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("a", 64))
	snapshot := func() string {
		t.Helper()
		var row string
		if err := f.db.QueryRow(context.Background(), `SELECT to_jsonb(i)::text FROM installations i WHERE app_id=$1 AND id=$2`, a.id, one.ID).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := snapshot()
	body := installations.Registration{Platform: "ios", Environment: "production", PushToken: strings.Repeat("a", 64), Token: one.Token, Generation: 1}
	expect(t, f.installationRequest("PUT", one.ID, identity(a, "alice", nil), a.id, "", 1, body, false), 200)
	if snapshot() != before {
		t.Fatal("exact replay mutated the registration")
	}
	// A genuine reinstall can transfer the token, but replay of the old registration cannot transfer it back.
	f.register(t, a, "alice", uuid.NewString(), "", strings.Repeat("a", 64))
	expect(t, f.installationRequest("PUT", one.ID, identity(a, "alice", nil), a.id, "", 1, body, false), 409)
	if status := f.status(t, a, f.send(t, a, "after-replay", "alice").ID); status.Counts.Queued != 1 {
		t.Fatalf("old replay retired the replacement installation: %+v", status)
	}
}
