package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/google/uuid"

	"whatsappconverty/internal/queue"
)

// SendWhatsAppTemplateJob is the queue payload for an automated send: order
// events materialize into this job and the worker runs the full tenant-scoped
// send gate. It carries the same fields as SendRequest so a worker has
// everything needed without re-deriving context.
type SendWhatsAppTemplateJob struct {
	ShopID uuid.UUID `json:"shop_id"`
	// AutomationID is the automation that queued this send, empty for a send that
	// did not come from one. The worker re-checks it before sending so pausing an
	// automation also stops a message that was already queued — including one
	// waiting in the queue for a future scheduled time.
	AutomationID    uuid.UUID         `json:"automation_id,omitempty"`
	CustomerID      uuid.UUID         `json:"customer_id"`
	TemplateID      uuid.UUID         `json:"template_id"`
	ConvertyOrderID string            `json:"converty_order_id"`
	Purpose         string            `json:"purpose"`
	Variables       map[string]string `json:"variables"`
	IdempotencyKey  string            `json:"idempotency_key"`
}

// EnqueueSendAt parks a send in the task queue until its moment. A zero time
// means "the event just happened" — but a send that is due right now does not
// come through here at all: the caller sends it inline, so an order
// notification reaches the customer without waiting for a worker.
//
// The job is uniquely keyed on the send's idempotency key, so a webhook that
// arrives twice cannot queue the same message twice. No asynq client (no Redis)
// surfaces an error, so the caller knows the scheduled send did not land.
func (s *Service) EnqueueSendAt(ctx context.Context, job SendWhatsAppTemplateJob, at time.Time) error {
	if job.ShopID == uuid.Nil || job.CustomerID == uuid.Nil || job.TemplateID == uuid.Nil {
		return errors.New("whatsapp: send job is missing shop, customer or template")
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	task := asynq.NewTask(queue.TaskSendWhatsAppTemplate, payload)
	opts := []asynq.Option{asynq.Retention(48 * time.Hour)}
	if job.IdempotencyKey != "" {
		opts = append(opts, asynq.Unique(30*24*time.Hour))
	}
	if at.IsZero() {
		opts = append(opts, asynq.ProcessIn(time.Second))
	} else {
		opts = append(opts, asynq.ProcessAt(at))
	}
	if s.queue == nil {
		return errors.New("whatsapp: send queue unavailable (no Redis client)")
	}
	_, err = s.queue.Enqueue(task, opts...)
	return err
}

// HandleSendWhatsAppTemplate is the queue handler for automated sends. A send
// refused by the send gate (*SendRejection) is logged and dropped — no retry
// would change the answer — while a transient infrastructure error bubbles up
// so asynq retries it.
func (s *Service) HandleSendWhatsAppTemplate(ctx context.Context, task *asynq.Task) error {
	var job SendWhatsAppTemplateJob
	if err := json.Unmarshal(task.Payload(), &job); err != nil {
		// A payload we cannot read will never become readable.
		return asynq.SkipRetry
	}

	_, err := s.SendTemplateMessage(ctx, SendRequest{
		ShopID:          job.ShopID,
		CustomerID:      job.CustomerID,
		TemplateID:      job.TemplateID,
		ConvertyOrderID: job.ConvertyOrderID,
		Purpose:         job.Purpose,
		Variables:       job.Variables,
		IdempotencyKey:  job.IdempotencyKey,
		AutomationID:    job.AutomationID,
	})
	var rej *SendRejection
	if errors.As(err, &rej) {
		s.log.Warn("scheduled send rejected, no retry",
			"shop_id", job.ShopID, "customer_id", job.CustomerID,
			"template_id", job.TemplateID, "code", rej.Code, "reason", rej.Reason)
		return nil
	}
	if err != nil {
		return fmt.Errorf("whatsapp: scheduled send failed: %w", err)
	}
	return nil
}