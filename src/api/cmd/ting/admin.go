package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klaraalabs/ting/src/api/internal/admin"
	"golang.org/x/term"
)

func adminCommand(ctx context.Context, s *admin.Service, args []string) error {
	return runAdminCommand(ctx, s, args, readOperatorPassword, os.Stdout)
}
func runAdminCommand(ctx context.Context, s *admin.Service, args []string, readPassword func() (string, error), out io.Writer) error {
	if len(args) == 0 || (args[0] != "create" && args[0] != "reset-password") {
		return commandError("usage: ting admin create/reset-password --username <name>")
	}
	f := flag.NewFlagSet("admin "+args[0], flag.ContinueOnError)
	// Do not echo unknown flags, which could accidentally contain a password.
	f.SetOutput(io.Discard)
	username := f.String("username", "", "operator username")
	if err := f.Parse(args[1:]); err != nil {
		return commandError("usage: ting admin create/reset-password --username <name>; passwords are prompted, never passed as flags")
	}
	if f.NArg() != 0 {
		return commandError("unexpected positional arguments")
	}
	name, err := admin.NormalizeUsername(*username)
	if err != nil {
		return err
	}
	password, err := readPassword()
	if err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result any = map[string]string{"status": "ok"}
	if args[0] == "create" {
		result, err = s.Create(operation, name, password)
	} else {
		err = s.ResetPassword(operation, name, password)
	}
	if err != nil {
		return err
	}
	if err = json.NewEncoder(out).Encode(result); err != nil {
		return commandError("operator operation completed but output failed")
	}
	return nil
}
func readOperatorPassword() (string, error) {
	// A controlling terminal avoids accepting passwords in command arguments,
	// environment variables, shell pipelines, or redirected input.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", commandError("an interactive terminal is required to enter the operator password")
	}
	defer tty.Close()
	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return "", commandError("an interactive terminal is required")
	}
	fmt.Fprint(tty, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(tty)
	if err != nil {
		return "", commandError("could not read password")
	}
	defer clear(first)
	if err = admin.ValidatePassword(string(first)); err != nil {
		return "", err
	}
	fmt.Fprint(tty, "Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(tty)
	if err != nil {
		return "", commandError("could not read password confirmation")
	}
	defer clear(second)
	if string(first) != string(second) {
		return "", commandError("passwords do not match")
	}
	return string(first), nil
}
