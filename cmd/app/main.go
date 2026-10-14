package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/server"
	"whatsappconverty/internal/worker"
	"whatsappconverty/web/static"
)

func main() {
	cfg := config.Load()

	logger := logutil.New(cfg)
	slog.SetDefault(logger)

	// superkit's kit.Setup() requires a SUPERKIT_SECRET in the environment and a
	// ".env" file on disk (godotenv.Load fatal otherwise). On Render there is no
	// ".env" file, so ensure one exists and propagate the secret into the
	// environment before initializing the session store.
	os.Setenv("SUPERKIT_SECRET", cfg.SuperkitSecret)
	os.Setenv("SUPERKIT_ENV", cfg.Env)
	if _, err := os.Stat(".env"); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(".env", []byte(""), 0o600); err != nil {
			logger.Error("failed to create .env placeholder", "error", err)
			os.Exit(1)
		}
	}
	kit.Setup()

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(rootCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if cfg.IsDevelopment() {
		if err := database.Migrate(rootCtx, cfg.DatabaseURL, logger); err != nil {
			logger.Error("migrations failed", "error", err)
			os.Exit(1)
		}
	}


	// Run the job processor in-process as well as in the standalone worker: the
	// queue is Postgres-backed and claims are leased, so two runners are safe, and
	// the API stays able to deliver queued sends even if the worker service is
	// stopped on a free tier.
	workerServ, werr := worker.Start(cfg, logger)
	if werr != nil {
		logger.Error("background worker failed to start; inbox/webhooks keep working, queued sends will wait", "error", werr)
	} else {
		defer workerServ.Shutdown()
	}

	app := server.New(cfg, logger, pool)
	if err := app.Auth.EnsureAdmin(rootCtx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		logger.Error("admin bootstrap failed", "error", err)
	}

	router := chi.NewMux()
	app.InitializeMiddleware(router)
	router.Handle("/static/*", staticHandler(cfg))
	kit.UseErrorHandler(app.ErrorHandler)
	app.InitializeRoutes(router)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-rootCtx.Done()

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// staticHandler serves the embedded frontend assets in production and the
// on-disk assets in development.
func staticHandler(cfg config.Config) http.Handler {
	if cfg.IsDevelopment() {
		return http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	}
	return http.StripPrefix("/static/", http.FileServerFS(static.FS))
}
