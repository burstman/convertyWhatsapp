package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/metaconfig"
	"whatsappconverty/internal/queue"
	"whatsappconverty/internal/whatsapp"
)

// mediaRetention is how long stored inbox media (voice notes, images) is kept.
// The bytes are dropped after this, not the message rows, so the conversation
// history keeps reading its label with no player.
const mediaRetention = 30 * 24 * time.Hour

// mediaPurgeInterval is how often the retention job scans for expired media.
const mediaPurgeInterval = 12 * time.Hour

// Server runs the background job processor. It can run inside the web process
// (Render free tier has no background-worker service type) or standalone via
// cmd/worker. The queue lives in the same database, so a failure here leaves
// nothing but delayed jobs undone; the API must keep serving either way.
type Server struct {
	log    *slog.Logger
	runner *queue.Runner
	pool   *pgxpool.Pool
	stop   context.CancelFunc
	closed bool
}

// Start wires the job runner and begins draining queued work. It owns its own
// DB pool so it can run inside either process (app or worker). When meta is
// non-nil the caller (the web process) shares its admin-editable credential
// store so an update applies to in-process sends without a restart; otherwise
// the worker loads its own from the database.
func Start(cfg config.Config, logger *slog.Logger, meta *metaconfig.Store) (*Server, error) {
	pool, err := database.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("worker database connection failed: %w", err)
	}

	if meta == nil {
		meta, err = metaconfig.NewStore(pool, cfg.EncryptionKeyResolved(), metaconfig.DefaultsFromConfig(cfg))
		if err != nil {
			return nil, fmt.Errorf("worker metaconfig: %w", err)
		}
		if err := meta.Load(context.Background()); err != nil {
			logger.Warn("worker could not load stored meta credentials; using environment defaults", "error", err)
		}
	}

	wa := whatsapp.NewService(cfg, pool, logger, meta)
	auto := automations.NewProcessor(cfg, pool, logger, wa)
	// The delivery layer re-checks the Converty integration immediately before
	// every send, so the worker itself must not gate on it: one authoritative
	// check, applied on the first attempt and every retry alike.
	wa.SetIntegrationChecker(auto)

	runner := queue.NewRunner(pool, logger)
	runner.Handle(queue.TaskPurgeMarketingTemplate, wa.HandlePurgeMarketingTemplate)
	runner.Handle(queue.TaskSendWhatsAppTemplate, gateAutomation(auto, wa.HandleSendWhatsAppTemplate, logger))

	ctx, stop := context.WithCancel(context.Background())
	runner.Start(ctx)
	go mediaPurgeLoop(ctx, logger, wa)

	logger.Info("background worker started",
		"queue", "postgres", "job_interval", queue.DefaultInterval)
	return &Server{log: logger, runner: runner, pool: pool, stop: stop}, nil
}

// mediaPurgeLoop clears stored inbox media once it is older than the 30-day
// retention window. The bytes are dropped, not the message rows, so the
// conversation history keeps its label with no player.
func mediaPurgeLoop(ctx context.Context, logger *slog.Logger, wa *whatsapp.Service) {
	run := func() {
		n, err := wa.PurgeChatMedia(ctx, time.Now().Add(-mediaRetention))
		if err != nil {
			logger.Warn("chat media purge failed", "error", err)
			return
		}
		if n > 0 {
			logger.Info("chat media purge cleared media rows", "count", n)
		}
	}

	// Run once at boot so a long-idle instance catches up promptly, then on the
	// regular interval.
	run()

	ticker := time.NewTicker(mediaPurgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// automationLookup is the part of the automation processor the gate needs. It is
// an interface so the gate can be tested without a database.
type automationLookup interface {
	AutomationEnabled(ctx context.Context, id uuid.UUID) (bool, error)
}

// gateAutomation re-checks a queued send immediately before delivery: pausing
// the automation that produced it must also stop a message already parked in
// the queue. A scheduled send can sit for hours, so without this check a pause
// would only stop future events and the pending message would still go out.
//
// Converty integration deactivation is deliberately NOT checked here. That is a
// delivery-time concern handled once by the send gate inside SendTemplateMessage
// (whatsapp.gateIntegration), so it applies uniformly to inline, retry-held and
// queued sends without a second, divergent code path.
func gateAutomation(lookup automationLookup, next queue.Handler, logger *slog.Logger) queue.Handler {
	return func(ctx context.Context, payload []byte) error {
		var job whatsapp.SendWhatsAppTemplateJob
		if err := json.Unmarshal(payload, &job); err != nil {
			// Malformed payload: let the sender deal with it rather than guessing here.
			return next(ctx, payload)
		}

		if job.AutomationID != uuid.Nil {
			active, err := lookup.AutomationEnabled(ctx, job.AutomationID)
			if err != nil {
				// Fail closed: we cannot establish that the automation is still
				// enabled, so the message must not be delivered on this attempt.
				// Return the error so the runner retries it with the standard
				// backoff — a transient database blip is resolved on a later
				// attempt rather than sent blind or silently dropped.
				return fmt.Errorf("automation gate: lookup %s: %w", job.AutomationID, err)
			}
			if !active {
				logger.Info("queued send dropped: automation paused or deleted",
					"automation_id", job.AutomationID, "shop_id", job.ShopID,
					"customer_id", job.CustomerID, "template_id", job.TemplateID)
				return nil
			}
		}

		return next(ctx, payload)
	}
}

// Shutdown stops the job runner and closes its database pool.
func (s *Server) Shutdown() {
	if s.closed {
		return
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	s.pool.Close()
	s.log.Info("background worker shut down")
}
