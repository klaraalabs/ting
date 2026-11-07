package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/admin"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"github.com/klaraalabs/ting/src/api/internal/apps"
	"github.com/klaraalabs/ting/src/api/internal/config"
	"github.com/klaraalabs/ting/src/api/internal/dbmigrations"
	"github.com/klaraalabs/ting/src/api/internal/installations"
	"github.com/klaraalabs/ting/src/api/internal/notifications"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const usage = `Usage: ting <command>

Commands:
  serve                 Run the HTTP API, Management API, and APNs/FCM delivery workers
  db migrate            Apply database migrations
  admin create          Create an operator account (password is prompted)
  admin reset-password  Reset an operator password (password is prompted)
  app <action>          Manage Apps, credentials, push providers, and test sends

Run "ting app help" to list App actions. Configuration is read from the
environment; see docs/configuration.md.
`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		fmt.Print(usage)
		return nil
	}
	if args[0] != "serve" && args[0] != "db" && args[0] != "app" && args[0] != "admin" {
		return fmt.Errorf("unknown command %q; run ting help", args[0])
	}
	var appCmd *appCommandLine
	if args[0] == "app" {
		var err error
		if appCmd, err = parseAppCommand(args[1:], os.Stdout); err != nil {
			return fmt.Errorf("App command failed: %w", err)
		}
		if appCmd == nil {
			return nil
		}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, time.Minute)
	if err = dbmigrations.Migrate(startup, c.DatabaseURL); err != nil {
		cancel()
		return errors.New("database migration failed; check connectivity and database permissions")
	}
	cancel()
	if args[0] == "db" {
		if len(args) != 2 || args[1] != "migrate" {
			return errors.New("usage: ting db migrate")
		}
		return nil
	}
	poolConfig, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	poolConfig.MaxConns = 10
	poolConfig.ConnConfig.ConnectTimeout = 5 * time.Second
	db, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return errors.New("cannot open database")
	}
	defer db.Close()
	if args[0] == "admin" {
		err = adminCommand(ctx, &admin.Service{DB: db, Config: c.Admin}, args[1:])
		var apiErr *apierror.Error
		if err != nil && !errors.As(err, &apiErr) {
			return fmt.Errorf("Operator command failed: %w", safeCLIError(err))
		}
		return err
	}
	vault, err := secrets.New(c.EncryptionKey)
	if err != nil {
		return err
	}
	if args[0] == "app" {
		operation, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		services := appServices{
			Apps:          &apps.Service{DB: db, Vault: vault},
			Installations: &installations.Service{DB: db, Vault: vault},
			Notifications: &notifications.Service{DB: db, DeliveryTTL: c.Delivery.TTL, Limits: c.Notifications},
		}
		err = appCmd.execute(operation, services, os.Stdout, os.Stderr)
		var apiErr *apierror.Error
		if err != nil && !errors.As(err, &apiErr) {
			return fmt.Errorf("App command failed: %w", safeCLIError(err))
		}
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: ting serve")
	}
	return serve(ctx, c, db, vault)
}

// SQL errors can include row data. Only explicitly safe command errors reach stderr.
type commandError string

func (e commandError) Error() string { return string(e) }
func safeCLIError(err error) error {
	var e commandError
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errors.New("operation could not be completed")
}
