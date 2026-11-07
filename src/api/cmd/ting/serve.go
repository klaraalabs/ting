package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/config"
	"github.com/klaraalabs/ting/src/api/internal/delivery"
	"github.com/klaraalabs/ting/src/api/internal/providers"
	"github.com/klaraalabs/ting/src/api/internal/providers/apns"
	"github.com/klaraalabs/ting/src/api/internal/providers/fcm"
	"github.com/klaraalabs/ting/src/api/internal/retention"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
	"github.com/klaraalabs/ting/src/api/internal/server"
)

func serve(ctx context.Context, c config.Config, db *pgxpool.Pool, vault *secrets.Vault) error {
	credentials := &providers.Store{DB: db, Vault: vault}
	apple, google := apns.New(credentials), fcm.New(credentials)
	defer apple.Close()
	defer google.Close()
	workers, err := delivery.NewPool(&delivery.Store{DB: db, Vault: vault}, map[string]delivery.Sender{"ios": apple, "android": google}, c.Delivery)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Address)
	if err != nil {
		return errors.New("HTTP server failed to start")
	}
	srv := &http.Server{Handler: server.New(db, vault, c.TrustedProxies, c.AllowLocalHTTP, c.Delivery.TTL, c.Admin, c.HTTP, c.Notifications), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: c.HTTP.RequestTimeout + 5*time.Second, WriteTimeout: c.HTTP.RequestTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); workers.Run(workerCtx) }()
	retentionDone := make(chan struct{})
	go func() { defer close(retentionDone); (&retention.Service{DB: db, Config: c.Retention}).Run(workerCtx) }()
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Serve(listener) }()
	slog.Info("API and APNs/FCM delivery workers starting", "address", c.Address, "workerConcurrency", c.Delivery.Concurrency)
	var serveErr error
	select {
	case err := <-httpDone:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.New("HTTP server stopped unexpectedly")
		}
	case <-ctx.Done():
	}
	// Stop intake and leasing together. Worker attempts have their own grace
	// context; canceling the signal context does not abort them immediately.
	stopWorkers()
	shutdown, cancel := context.WithTimeout(context.Background(), c.Delivery.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
		serveErr = errors.New("HTTP shutdown deadline exceeded")
	}
	<-workerDone // Never close the shared database pool while workers still use it.
	<-retentionDone
	return serveErr
}
