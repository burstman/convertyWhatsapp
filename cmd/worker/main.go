package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/queue"
	"whatsappconverty/internal/whatsapp"
)

func main() {
	cfg := config.Load()

	logger := logutil.New(cfg)
	slog.SetDefault(logger)

	redisOpt, err := queue.RedisClientOpt(cfg.RedisURL)
	if err != nil {
		logger.Error("invalid redis url", "error", err)
		os.Exit(1)
	}

	pool, err := database.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	svc := whatsapp.NewService(cfg, pool, logger)
	auto := automations.NewProcessor(cfg, pool, logger, svc)

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: 10,
		Logger:      asynqLogger{logger},
		Queues:      map[string]int{"default": 6, "critical": 4},
	})

	mux := asynq.NewServeMux()
	registerHandlers(mux, svc, auto)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(mux); err != nil {
		logger.Error("worker failed to start", "error", err)
		os.Exit(1)
	}
	logger.Info("worker started", "redis", redisOpt.Addr, "concurrency", 10)

	<-ctx.Done()
	logger.Info("worker shutting down")
	srv.Shutdown()
}

func registerHandlers(mux *asynq.ServeMux, svc *whatsapp.Service, auto *automations.Processor) {
	mux.HandleFunc(queue.TaskPurgeMarketingTemplate, svc.HandlePurgeMarketingTemplate)
	mux.HandleFunc(queue.TaskSendWhatsAppTemplate, gateAutomation(auto, svc.HandleSendWhatsAppTemplate, slog.Default()))
}

// gateAutomation re-checks an automation before its queued send is delivered.
// Pausing switches off what fires next, but the job was already parked in the
// queue — a scheduled send can sit there for hours — so without this check a
// pause would only stop future events and the pending message would still go
// out. Non-automation sends pass through untouched.
func gateAutomation(auto *automations.Processor, next func(context.Context, *asynq.Task) error, log *slog.Logger) func(context.Context, *asynq.Task) error {
	return func(ctx context.Context, task *asynq.Task) error {
		var job whatsapp.SendWhatsAppTemplateJob
		if err := json.Unmarshal(task.Payload(), &job); err != nil {
			return next(ctx, task)
		}
		if job.AutomationID == uuid.Nil {
			return next(ctx, task)
		}

		active, err := auto.AutomationEnabled(ctx, job.AutomationID)
		if err != nil {
			log.Warn("automation gate: lookup failed, sending anyway",
				"automation_id", job.AutomationID, "shop_id", job.ShopID, "error", err)
			return next(ctx, task)
		}
		if !active {
			log.Info("queued send dropped: automation paused or deleted",
				"automation_id", job.AutomationID, "shop_id", job.ShopID,
				"customer_id", job.CustomerID, "template_id", job.TemplateID)
			return nil
		}
		return next(ctx, task)
	}
}

type asynqLogger struct{ log *slog.Logger }

func (l asynqLogger) Debug(args ...any) { l.log.Debug("asynq", "msg", args) }
func (l asynqLogger) Info(args ...any)  { l.log.Info("asynq", "msg", args) }
func (l asynqLogger) Warn(args ...any)  { l.log.Warn("asynq", "msg", args) }
func (l asynqLogger) Error(args ...any) { l.log.Error("asynq", "msg", args) }
func (l asynqLogger) Fatal(args ...any) { l.log.Error("asynq fatal", "msg", args) }
