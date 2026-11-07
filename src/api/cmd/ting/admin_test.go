package main

import (
	"context"
	"io"
	"testing"
)

func TestOperatorCLIRejectsPasswordFlagsAndInvalidCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"delete"}, {"create"}, {"create", "--username", "admin", "--password", "secret"}, {"reset-password", "--username", "admin", "secret"}} {
		prompted := false
		err := runAdminCommand(context.Background(), nil, args, func() (string, error) { prompted = true; return "", nil }, io.Discard)
		if err == nil || prompted {
			t.Fatalf("unsafe arguments accepted: %v", args)
		}
	}
}
