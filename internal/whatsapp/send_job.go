package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	// waiting in the job table for a future scheduled time.
	AutomationID    uuid.UUID         `json:"automation_id,omitempty"`
	CustomerID      uuid.UUID         `json:"customer_id"`
	TemplateID      uuid.UUID         `json:"template_id"`
	ConvertyOrderID string            `json:"converty_order_id"`
	Purpose         string            `json:"purpose"`
	Variables       map[string]string `json:"variables"`
	IdempotencyKey  string            `json:"idempotency_key"`
}

// EnqueueSendAt parks a send in the job queue until its moment. A zero time, or
// a time already past, means "at the next tick" — but a send that is due right
// now does not come through here at all: the caller sends it inline, so an order
// notification reaches the customer without waiting for a tick.
//
// The job is keyed on the send's idempotency key, so a webhook that arrives
// twice cannot queue the same message twice.
func (s *Service) EnqueueSendAt(ctx context.Context, job SendWhatsAppTemplateJob, at time.Time) error {
	if job.ShopID == uuid.Nil || job.CustomerID == uuid.Nil || job.TemplateID == uuid.Nil {
		return errors.New("whatsapp: send job is missing shop, customer or template")
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return queue.Enqueue(ctx, s.pool, queue.Params{
		Kind:      queue.TaskSendWhatsAppTemplate,
		ShopID:    job.ShopID,
		Payload:   payload,
		RunAt:     at,
		DedupeKey: job.IdempotencyKey,
	})
}

// HandleSendWhatsAppTemplate is the queue handler for automated sends. A send
// refused by the send gate (*SendRejection) is logged and dropped — the message
// row already records the failure and no retry would change the answer — while a
// transient infrastructure error bubbles up so the runner retries it.
func (s *Service) HandleSendWhatsAppTemplate(ctx context.Context, payload []byte) error {
	var job SendWhatsAppTemplateJob
	if err := json.Unmarshal(payload, &job); err != nil {
		// A payload we cannot read will never become readable.
		return queue.Permanent(fmt.Errorf("whatsapp: decode send job: %w", err))
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
	return err
}
