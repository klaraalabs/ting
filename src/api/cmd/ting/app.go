package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
)

type appServices struct {
	Apps          *apps.Service
	Installations *installations.Service
	Notifications *notifications.Service
}

// appValues holds every flag value; each action binds only the flags it lists.
type appValues map[string]*string

type appAction struct {
	name     string
	summary  string
	flags    []string // required flags first, then optional ones
	required int      // number of leading flags that are required
	example  string
	run      func(context.Context, appServices, appValues) (any, error)
	next     func(appValues, any) string
}

var appFlagUsage = map[string]string{
	"id":                   "public App ID (app_...)",
	"name":                 "App name",
	"slug":                 "lowercase, hyphenated App slug",
	"label":                "token label, e.g. the consuming backend's name",
	"token-id":             "token ID from token creation",
	"kid":                  "identity key ID",
	"environment":          "APNs environment: sandbox, production, or both",
	"bundle-id":            "APNs bundle ID",
	"team-id":              "Apple team ID",
	"key-id":               "APNs key ID",
	"p8-file":              "APNs .p8 private key file",
	"firebase-project-id":  "Firebase project ID",
	"service-account-file": "FCM service-account JSON file (not google-services.json)",
	"installation-id":      "registered installation UUID",
	"notification-id":      "notification UUID returned by a send",
	"output-file":          "create a new file with mode 0600 instead of writing to stdout",
	"format":               "output format: json or text",
}

const oneTimeWarning = "Store this credential now; Ting cannot show it again. If it is lost, issue a new one and revoke the old one."

func registerHint(id string) string {
	return "Register a device with a Ting SDK, then send it a test:\n  ting app installations list --id " + id + "\n  ting app test --id " + id + " --installation-id <INSTALLATION_ID>"
}

var appActions = []appAction{
	{name: "create", summary: "Create an App and print its public App ID", flags: []string{"name", "slug"}, required: 1,
		example: `ting app create --name "Example Production" --slug example-prod`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return s.Apps.Create(ctx, *v["name"], *v["slug"])
		},
		next: func(_ appValues, r any) string {
			id := r.(apps.App).ID
			return "Next, issue a sender token for your backend:\n  ting app token create --id " + id + ` --label "backend" --output-file ./sender.json`
		}},
	{name: "list", summary: "List Apps", example: "ting app list --format text",
		run: func(ctx context.Context, s appServices, _ appValues) (any, error) { return s.Apps.List(ctx) }},
	{name: "show", summary: "Show one App", flags: []string{"id"}, required: 1, example: `ting app show --id "$APP_ID"`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) { return s.Apps.Show(ctx, *v["id"]) }},
	{name: "token create", summary: "Issue a sender API token (shown once)", flags: []string{"id", "label"}, required: 2,
		example: `ting app token create --id "$APP_ID" --label "backend" --output-file ./sender.json`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return s.Apps.CreateToken(ctx, *v["id"], *v["label"])
		},
		next: func(v appValues, _ any) string {
			return oneTimeWarning + "\nNext, issue an identity-signing secret:\n  ting app identity-secret create --id " + *v["id"] + " --output-file ./identity.json"
		}},
	{name: "token revoke", summary: "Revoke a sender API token", flags: []string{"id", "token-id"}, required: 2,
		example: `ting app token revoke --id "$APP_ID" --token-id "$TOKEN_ID"`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return nil, s.Apps.RevokeToken(ctx, *v["id"], *v["token-id"])
		}},
	{name: "identity-secret create", summary: "Issue an identity-signing secret (shown once)", flags: []string{"id"}, required: 1,
		example: `ting app identity-secret create --id "$APP_ID" --output-file ./identity.json`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return s.Apps.CreateIdentity(ctx, *v["id"])
		},
		next: func(v appValues, _ any) string {
			return oneTimeWarning + "\nNext, configure a push provider:\n  ting app apns configure --help\n  ting app fcm configure --help"
		}},
	{name: "identity-secret retire", summary: "Retire an identity-signing secret", flags: []string{"id", "kid"}, required: 2,
		example: `ting app identity-secret retire --id "$APP_ID" --kid "$OLD_KID"`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return nil, s.Apps.RetireIdentity(ctx, *v["id"], *v["kid"])
		}},
	{name: "apns configure", summary: "Store APNs credentials", flags: []string{"id", "environment", "bundle-id", "team-id", "key-id", "p8-file"}, required: 6,
		example: "ting app apns configure --id \"$APP_ID\" --environment production \\\n    --bundle-id com.example.app --team-id ABCDE12345 --key-id FGHIJ67890 --p8-file ./AuthKey.p8",
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			raw, err := readCredential(*v["p8-file"])
			if err != nil {
				return nil, err
			}
			err = s.Apps.ConfigureAPNs(ctx, *v["id"], apps.APNsConfig{Environment: *v["environment"], BundleID: *v["bundle-id"], TeamID: *v["team-id"], KeyID: *v["key-id"], Key: raw})
			return map[string]string{"status": "configured_untested"}, err
		},
		next: func(v appValues, _ any) string {
			return "Stored, but Apple has not verified it yet. " + registerHint(*v["id"])
		}},
	{name: "fcm configure", summary: "Store FCM service-account credentials", flags: []string{"id", "firebase-project-id", "service-account-file"}, required: 3,
		example: "ting app fcm configure --id \"$APP_ID\" --firebase-project-id example-project \\\n    --service-account-file ./firebase-service-account.json",
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			raw, err := readCredential(*v["service-account-file"])
			if err != nil {
				return nil, err
			}
			err = s.Apps.ConfigureFCM(ctx, *v["id"], *v["firebase-project-id"], raw)
			return map[string]string{"status": "configured_untested"}, err
		},
		next: func(v appValues, _ any) string {
			return "Stored, but Google has not verified it yet. " + registerHint(*v["id"])
		}},
	{name: "installations list", summary: "List recently seen installations to choose a test target", flags: []string{"id"}, required: 1,
		example: `ting app installations list --id "$APP_ID" --format text`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			if _, err := s.Apps.Show(ctx, *v["id"]); err != nil {
				return nil, err
			}
			return s.Installations.List(ctx, *v["id"], installationListLimit, 0)
		}},
	{name: "test", summary: "Queue a test alert to one registered installation", flags: []string{"id", "installation-id"}, required: 2,
		example: `ting app test --id "$APP_ID" --installation-id "$INSTALLATION_ID"`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return s.Notifications.SendTest(ctx, *v["id"], "", *v["installation-id"])
		},
		next: func(v appValues, r any) string {
			return "Queued. A running `ting serve` delivers it. Check the provider's response with:\n  ting app notification status --id " + *v["id"] + " --notification-id " + r.(notifications.Accepted).ID
		}},
	{name: "notification status", summary: "Show the delivery outcome of a send", flags: []string{"id", "notification-id"}, required: 2,
		example: `ting app notification status --id "$APP_ID" --notification-id "$NOTIFICATION_ID"`,
		run: func(ctx context.Context, s appServices, v appValues) (any, error) {
			return s.Notifications.Status(ctx, *v["id"], *v["notification-id"])
		},
		next: func(_ appValues, r any) string {
			status := r.(notifications.Status)
			switch {
			case status.Counts.Queued > 0:
				return "Still queued. Check again shortly; delivery needs a running `ting serve`."
			case status.Counts.Accepted > 0:
				return "Accepted by the provider. Ting cannot confirm that the device displayed it."
			}
			return ""
		}},
}

const installationListLimit = 100

// appCommandLine is a parsed `ting app` invocation, validated before any
// configuration or database access so that help and typos work offline.
type appCommandLine struct {
	action *appAction
	values appValues
	output string
	format string
}

func appGroups() map[string]bool {
	groups := map[string]bool{}
	for _, a := range appActions {
		if group, _, ok := strings.Cut(a.name, " "); ok {
			groups[group] = true
		}
	}
	return groups
}

func isHelpArg(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "-help" || arg == "--help"
}

// parseAppCommand returns a nil command after printing requested help.
func parseAppCommand(args []string, stdout io.Writer) (*appCommandLine, error) {
	if len(args) == 0 || isHelpArg(args[0]) {
		printAppHelp(stdout)
		return nil, nil
	}
	name := args[0]
	args = args[1:]
	if appGroups()[name] {
		if len(args) == 0 || isHelpArg(args[0]) {
			printAppHelp(stdout)
			return nil, nil
		}
		name += " " + args[0]
		args = args[1:]
	}
	var action *appAction
	for i := range appActions {
		if appActions[i].name == name {
			action = &appActions[i]
		}
	}
	if action == nil {
		return nil, commandError(fmt.Sprintf("unknown App action %q; run ting app help", name))
	}
	f := flag.NewFlagSet("app "+name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	cmd := &appCommandLine{action: action, values: appValues{}}
	for _, flagName := range action.flags {
		cmd.values[flagName] = f.String(flagName, "", appFlagUsage[flagName])
	}
	f.StringVar(&cmd.output, "output-file", "", appFlagUsage["output-file"])
	f.StringVar(&cmd.format, "format", "json", appFlagUsage["format"])
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printActionHelp(stdout, action, f)
			return nil, nil
		}
		return nil, commandError(fmt.Sprintf("%v; run ting app %s --help", err, name))
	}
	if f.NArg() != 0 {
		return nil, commandError(fmt.Sprintf("unexpected argument %q; pass values as --flag value; run ting app %s --help", f.Arg(0), name))
	}
	if cmd.format != "json" && cmd.format != "text" {
		return nil, commandError(fmt.Sprintf("unknown --format %q; use json or text", cmd.format))
	}
	var missing []string
	for _, flagName := range action.flags[:action.required] {
		if strings.TrimSpace(*cmd.values[flagName]) == "" {
			missing = append(missing, "--"+flagName)
		}
	}
	if len(missing) > 0 {
		return nil, commandError(fmt.Sprintf("missing required %s; run ting app %s --help", strings.Join(missing, ", "), name))
	}
	return cmd, nil
}

func printAppHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage: ting app <action> [flags]\n\nActions:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, a := range appActions {
		fmt.Fprintf(tw, "  %s\t%s\n", a.name, a.summary)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nSetup order: create, token create, identity-secret create, apns/fcm configure,\nregister a device with an SDK, installations list, test, notification status.\n\nRun \"ting app <action> --help\" for its flags and an example.")
}

func printActionHelp(w io.Writer, a *appAction, f *flag.FlagSet) {
	usage := "Usage: ting app " + a.name
	for i, name := range a.flags {
		if i < a.required {
			usage += " --" + name + " <value>"
		} else {
			usage += " [--" + name + " <value>]"
		}
	}
	fmt.Fprintf(w, "%s [--format json|text] [--output-file <path>]\n\n%s.\n\nFlags:\n", usage, a.summary)
	f.SetOutput(w)
	f.PrintDefaults()
	f.SetOutput(io.Discard)
	fmt.Fprintf(w, "\nExample:\n  %s\n", a.example)
}

func outputFileError(err error) error {
	switch {
	case errors.Is(err, os.ErrExist):
		return commandError("output file already exists; choose a new path, existing files are never overwritten")
	case errors.Is(err, os.ErrNotExist):
		return commandError("cannot create output file; its directory does not exist")
	case errors.Is(err, os.ErrPermission):
		return commandError("cannot create output file; permission denied")
	}
	return commandError("cannot create output file")
}

// execute runs the action. Results go to stdout or the output file; guidance
// goes to hints (stderr) so that stdout holds only the result.
func (c *appCommandLine) execute(ctx context.Context, s appServices, stdout io.Writer, hints io.Writer) error {
	// Reserve the output destination before issuing a secret. Never overwrite existing files.
	var dest *os.File
	if c.output != "" {
		var err error
		dest, err = os.OpenFile(c.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return outputFileError(err)
		}
		defer dest.Close()
	}
	result, err := c.action.run(ctx, s, c.values)
	if err != nil {
		if dest != nil {
			_ = os.Remove(c.output)
		}
		return err
	}
	if result == nil {
		result = map[string]string{"status": "ok"}
	}
	writer := stdout
	if dest != nil {
		writer = dest
	}
	if c.format == "text" {
		err = writeText(writer, result)
	} else {
		err = json.NewEncoder(writer).Encode(result)
	}
	if err != nil {
		return commandError("operation completed but output failed; revoke and reissue any lost credential")
	}
	if dest != nil {
		if err = dest.Sync(); err != nil {
			return commandError("operation completed but output sync failed; revoke and reissue any lost credential")
		}
		fmt.Fprintf(hints, "Wrote %s (mode 0600).\n", c.output)
	}
	if c.action.next != nil {
		if hint := c.action.next(c.values, result); hint != "" {
			fmt.Fprintln(hints, hint)
		}
	}
	return nil
}

// writeText renders results for people: tables for lists, aligned
// "field: value" lines for single records.
func writeText(w io.Writer, result any) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	when := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	switch r := result.(type) {
	case []apps.App:
		if len(r) == 0 {
			fmt.Fprintln(tw, "No Apps yet. Create one with: ting app create --name <name>")
			break
		}
		fmt.Fprintln(tw, "APP ID\tNAME\tSLUG\tENABLED\tCREATED")
		for _, a := range r {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, dash(a.Slug), yesNo(a.Enabled), when(a.CreatedAt))
		}
	case []installations.Metadata:
		if len(r) == 0 {
			fmt.Fprintln(tw, "No installations yet. Register a device with a Ting SDK first.")
			break
		}
		fmt.Fprintln(tw, "INSTALLATION ID\tPLATFORM\tAPNS ENV\tACTIVE\tLAST SEEN")
		for _, i := range r {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.ID, i.Platform, dash(i.Environment), yesNo(i.Active), when(i.LastSeenAt))
		}
		if len(r) == installationListLimit {
			fmt.Fprintf(tw, "Showing the %d most recently seen installations.\n", installationListLimit)
		}
	case apps.App:
		writeFields(tw, "App ID", r.ID, "Name", r.Name, "Slug", dash(r.Slug), "Enabled", yesNo(r.Enabled), "Created", when(r.CreatedAt))
	case apps.IssuedToken:
		writeFields(tw, "Token ID", r.ID, "Token", r.Token, "Suffix", r.Suffix)
	case apps.IssuedIdentity:
		writeFields(tw, "Key ID", r.KeyID, "Secret", r.Secret)
	case notifications.Accepted:
		writeFields(tw, "Notification ID", r.ID, "Status", r.Status)
	case notifications.Status:
		categories := make([]string, 0, len(r.Errors))
		for category, total := range r.Errors {
			categories = append(categories, fmt.Sprintf("%s (%d)", category, total))
		}
		sort.Strings(categories)
		writeFields(tw, "Notification ID", r.ID, "Status", r.Status,
			"Queued", strconv.Itoa(r.Counts.Queued), "Accepted", strconv.Itoa(r.Counts.Accepted),
			"Failed", strconv.Itoa(r.Counts.Failed), "Skipped", strconv.Itoa(r.Counts.Skipped),
			"Errors", dash(strings.Join(categories, ", ")))
	case map[string]string:
		writeFields(tw, "Status", r["status"])
	default:
		return json.NewEncoder(w).Encode(result)
	}
	return tw.Flush()
}

func writeFields(w io.Writer, pairs ...string) {
	for i := 0; i < len(pairs); i += 2 {
		fmt.Fprintf(w, "%s:\t%s\n", pairs[i], pairs[i+1])
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func readCredential(path string) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, commandError("provider credential file not found")
	}
	if err != nil {
		return nil, commandError("cannot read provider credential file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, commandError("provider credential file must be at most 64 KiB")
	}
	return data, nil
}
