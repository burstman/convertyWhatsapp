package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/worker"
)

func main() {
	cfg := config.Load()

	logger := logutil.New(cfg)
	slog.SetDefault(logger)

	// Mirror the app: migrations run automatically in development only;
	// production databases are migrated via `make migrate`.
	if cfg.IsDevelopment() {
		if err := database.Migrate(context.Background(), cfg.DatabaseURL, logger); err != nil {
			logger.Error("migrations failed", "error", err)
			os.Exit(1)
		}
	}

	srv, err := worker.Start(cfg, logger, nil)
	if err != nil {
		logger.Error("worker failed to start", "error", err)
		os.Exit(1)
	}
	defer srv.Shutdown()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	logger.Info("worker shutting down")
}
