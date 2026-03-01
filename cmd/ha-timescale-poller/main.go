package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	hasspoller "hass-poller"
	"hass-poller/internal/config"
	"hass-poller/internal/engine"
	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/httpserver"
	"hass-poller/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.New(ctx, cfg.PGDSN)
	if err != nil {
		logger.Fatalf("connect to postgres: %v", err)
	}
	defer db.Close()

	if err := db.EnsureSchema(ctx, hasspoller.SchemaSQL); err != nil {
		logger.Fatalf("apply schema: %v", err)
	}

	haClient := ha.NewClient(cfg.HABaseURL, cfg.HAToken, cfg.HTTPTimeout)
	entityFilter := filter.NewGlobFilter(cfg.EntityAllowlist, cfg.EntityBlocklist)

	poller := engine.New(haClient, entityFilter, db, cfg.PollInterval, cfg.EpsilonDefault, cfg.EpsilonOverrides, logger)

	// Start HTTP server for health and metrics.
	httpSrv := httpserver.New(cfg.HTTPListenAddr, poller)
	go func() {
		logger.Printf("http server listening on %s", cfg.HTTPListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("http server error: %v", err)
		}
	}()

	// Run the poll loop (blocks until ctx is cancelled).
	if err := poller.Run(ctx); err != nil {
		logger.Printf("poller stopped: %v", err)
	}

	// Graceful shutdown: give in-flight work 5s to complete.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http server shutdown error: %v", err)
	}

	logger.Printf("shutdown complete")
}
