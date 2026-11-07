package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
)

func TestAppCLIHelpNeedsNoDatabase(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"token"}, {"token", "--help"}, {"token", "create", "--help"}, {"test", "-h"}} {
		var out bytes.Buffer
		cmd, err := parseAppCommand(args, &out)
		if err != nil || cmd != nil || !strings.Contains(out.String(), "Usage: ting app") {
			t.Fatalf("%v: cmd=%v err=%v out=%q", args, cmd, err, out.String())
		}
	}
	var out bytes.Buffer
	parseAppCommand([]string{"apns", "configure", "--help"}, &out)
	help := out.String()
	if !strings.Contains(help, "--p8-file <value>") || !strings.Contains(help, "Example:") || strings.Contains(help, "-label") {
		t.Fatalf("action help should list only its own flags: %s", help)
	}
}

func TestAppCLIRejectsFlagsOfOtherActionsAndMissingRequiredFlags(t *testing.T) {
	cases := map[string][]string{
		"not defined: -p8-file":                    {"token", "create", "--id", "app_x", "--label", "x", "--p8-file", "key.p8"},
		"missing required --label":                 {"token", "create", "--id", "app_x"},
		"missing required --id, --installation-id": {"test"},
		`unknown App action "tokn"`:                {"tokn", "create"},
		`unexpected argument "extra"`:              {"show", "--id", "app_x", "extra"},
		`unknown --format "yaml"`:                  {"show", "--id", "app_x", "--format", "yaml"},
	}
	for want, args := range cases {
		_, err := parseAppCommand(args, &bytes.Buffer{})
		var safe commandError
		if !errors.As(err, &safe) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v: got %v, want %q", args, err, want)
		}
	}
	cmd, err := parseAppCommand([]string{"installations", "list", "--id", "app_x", "--format", "text"}, &bytes.Buffer{})
	if err != nil || cmd.action.name != "installations list" || *cmd.values["id"] != "app_x" || cmd.format != "text" {
		t.Fatalf("parse: %+v %v", cmd, err)
	}
	if cmd, err = parseAppCommand([]string{"show", "--id", "app_x"}, &bytes.Buffer{}); err != nil || cmd.format != "json" {
		t.Fatalf("JSON should be the default format: %+v %v", cmd, err)
	}
}

func TestAppCLIOutputFileErrorsNameTheCause(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.json")
	if err := os.WriteFile(existing, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{existing: "already exists", filepath.Join(dir, "missing", "out.json"): "directory does not exist"} {
		cmd, err := parseAppCommand([]string{"list", "--output-file", path}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		err = cmd.execute(context.Background(), appServices{}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", path, err, want)
		}
	}
}

func TestAppCLITextFormat(t *testing.T) {
	var out bytes.Buffer
	created := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, result := range []any{
		[]apps.App{{ID: "app_1", Name: "Example", Enabled: true, CreatedAt: created}},
		[]installations.Metadata{{ID: "inst-1", Platform: "ios", Environment: "sandbox", Active: true, LastSeenAt: created}},
		[]installations.Metadata{},
		apps.IssuedToken{ID: "tok-1", Token: "ting_sk_secret", Suffix: "cret"},
		notifications.Status{ID: "n-1", Status: "complete", Counts: notifications.Counts{Accepted: 1, Failed: 2}, Errors: map[string]int{"unregistered": 2}},
		map[string]string{"status": "configured_untested"},
	} {
		if err := writeText(&out, result); err != nil {
			t.Fatal(err)
		}
	}
	text := out.String()
	for _, want := range []string{"APP ID", "app_1", "Example", "yes", "INSTALLATION ID", "inst-1", "sandbox", "No installations yet",
		"Token:", "ting_sk_secret", "Accepted:", "unregistered (2)", "Status:", "configured_untested"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "{") {
		t.Fatalf("text output should not contain JSON: %s", text)
	}
}
