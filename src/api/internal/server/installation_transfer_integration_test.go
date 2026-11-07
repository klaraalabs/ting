package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

func TestInstallationTransferContract(t *testing.T) {
	var contract struct {
		Version   int `json:"version"`
		Scenarios []struct {
			Name          string `json:"name"`
			OldGeneration int64  `json:"oldGeneration"`
			Steps         []struct {
				Installation string `json:"installation"`
				Method       string `json:"method"`
				User         string `json:"user"`
				Generation   int64  `json:"generation"`
				Credential   string `json:"credential"`
				Proposed     string `json:"proposed"`
				Push         string `json:"push"`
				Status       int    `json:"status"`
				Error        string `json:"error"`
				Restart      bool   `json:"restart"`
			} `json:"steps"`
		} `json:"scenarios"`
	}
	raw, err := os.ReadFile("../../../../tests/contracts/installation-transfer.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Version != 1 || len(contract.Scenarios) == 0 {
		t.Fatal("unsupported or empty transfer contract")
	}
	for _, platform := range []string{"ios", "android"} {
		for _, scenario := range contract.Scenarios {
			t.Run(platform+"/"+scenario.Name, func(t *testing.T) {
				f := setup(t)
				a := f.app(t, true)
				if platform == "android" {
					input := fcmInput(t)
					account, err := json.Marshal(input["serviceAccount"])
					if err != nil {
						t.Fatal(err)
					}
					if err := f.service.ConfigureFCM(context.Background(), a.id, "example-project", account); err != nil {
						t.Fatal(err)
					}
				}
				ids := map[string]string{"old": uuid.NewString(), "replacement": uuid.NewString()}
				credentials := map[string]string{"": "", "first": secrets.Token("ting_ik_"), "second": secrets.Token("ting_ik_"), "third": secrets.Token("ting_ik_")}
				jwt := map[string]string{"": "", "alice": identity(a, "alice", nil), "bob": identity(a, "bob", nil)}
				var replacementSend string
				for i, step := range scenario.Steps {
					if step.Restart {
						vault, _ := secrets.New(bytes.Repeat([]byte{7}, 32))
						f.handler = server.New(f.db, vault, nil, false, 24*time.Hour, admin.DefaultConfig(), server.DefaultLimits(), notifications.DefaultLimits())
					}
					registration := installations.Registration{Platform: platform, PushToken: strings.Repeat(step.Push, 64), Token: credentials[step.Proposed], Generation: step.Generation}
					if platform == "ios" {
						registration.Environment = "production"
					}
					response := f.installationRequest(step.Method, ids[step.Installation], jwt[step.User], a.id, credentials[step.Credential], step.Generation, registration, false)
					if response.Code != step.Status {
						t.Fatalf("step %d: got %d, want %d: %s", i, response.Code, step.Status, response.Body.String())
					}
					if step.Error != "" {
						got := decode[struct{ Error struct{ Code string } }](t, response)
						if got.Error.Code != step.Error {
							t.Fatalf("step %d: got error %s, want %s", i, got.Error.Code, step.Error)
						}
					}
					if step.Installation == "replacement" && step.Status == 201 {
						replacementSend = f.send(t, a, "before-stale-request", "bob").ID
					}
					if step.Error == "installation_superseded" && step.Generation < 4 {
						if status := f.status(t, a, replacementSend); status.Counts.Queued != 1 || status.Counts.Skipped != 0 {
							t.Fatalf("stale PUT canceled replacement delivery: %+v", status)
						}
					}
				}
				var active, transferred bool
				var generation int64
				var credential []byte
				if err := f.db.QueryRow(context.Background(), `SELECT active,superseded_at IS NOT NULL,mutation_generation,credential_hash FROM installations WHERE app_id=$1 AND id=$2`, a.id, ids["old"]).Scan(&active, &transferred, &generation, &credential); err != nil {
					t.Fatal(err)
				}
				if active || !transferred || generation != scenario.OldGeneration || !bytes.Equal(credential, secrets.Hash(credentials["first"])) {
					t.Fatal("transfer fence or old ownership changed")
				}
				for _, user := range []string{"alice", "bob"} {
					want := 0
					if user == "bob" {
						want = 1
					}
					if status := f.status(t, a, f.send(t, a, "after-transfer-"+user, user).ID); status.Counts.Queued != want {
						t.Fatalf("wrong fan-out for %s: %+v", user, status)
					}
				}
			})
		}
	}
}
