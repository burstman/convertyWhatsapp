package automations

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/database"
	"whatsappconverty/internal/whatsapp"
)

// Suppression is one event an automation matched but deliberately did not send.
// A semantic template places its variables on purpose, so a chip that resolves
// to nothing (an event that did not carry the value) must not become a blank
// line in the customer's message. Dropping the send is right; dropping it
// silently is not, because the merchant's history then looks identical to an
// automation that never fired.
type Suppression struct {
	ID             uuid.UUID
	ShopID         uuid.UUID
	AutomationID   uuid.UUID
	CustomerID     uuid.UUID
	CustomerName   string
	TemplateName   string
	Reason         string
	Missing        []whatsapp.TokenKey
	TriggerLabel   string
	OrderID        string
	IdempotencyKey string
	CreatedAt      time.Time
}

// reasonMissingVariables explains a suppression in the merchant's terms rather
// than leaking token keys.
const reasonMissingVariables = "a variable the template places has no value for this order yet"

// RecordSuppression stores one dropped event. It is best effort from the
// caller's point of view: the send has already been declined on purpose, so a
// failed audit write must not turn a deliberate skip into a pipeline error that
// gets retried.
func (p *Processor) RecordSuppression(ctx context.Context, in SendInput, missing []whatsapp.TokenKey) {
	if len(missing) == 0 {
		return
	}
	trackingCode := "" // Converty events have no tracking code to record
	encoded, err := json.Marshal(missing)
	if err != nil {
		encoded = []byte("[]")
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO automation_suppressions
			(shop_id, automation_id, customer_id, template_id, reason, missing_variables,
			 trigger_label, tracking_code, order_id, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (automation_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		in.ShopID, in.AutomationID, in.CustomerID, in.TemplateID,
		reasonMissingVariables, encoded, in.StatusLabel, trackingCode, in.OrderID, in.IdempotencyKey,
	); err != nil {
		p.log.Warn("automation: could not record suppressed send",
			"shop_id", in.ShopID, "automation_id", in.AutomationID, "error", err)
	}
}

// SuppressionsForAutomation lists the events an automation matched but did not
// send, newest first. The names are joined in rather than fetched per row, for
// the same reason the message history does it: this list runs to hundreds of
// rows on a busy shop.
func SuppressionsForAutomation(ctx context.Context, db database.Querier, automationID uuid.UUID, limit int) ([]Suppression, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Query(ctx, `
		SELECT s.id, s.shop_id, s.automation_id, s.customer_id, s.reason, s.missing_variables,
		       COALESCE(s.trigger_label, ''), COALESCE(s.order_id, ''),
		       COALESCE(s.idempotency_key, ''), s.created_at, COALESCE(c.name, ''),
		       COALESCE(t.meta_template_name, '')
		FROM automation_suppressions s
		LEFT JOIN customers c ON c.id = s.customer_id
		LEFT JOIN templates t ON t.id = s.template_id
		WHERE s.automation_id = $1
		ORDER BY s.created_at DESC
		LIMIT $2`,
		automationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Suppression{}
	for rows.Next() {
		var s Suppression
		var missing []byte
		if err := rows.Scan(
			&s.ID, &s.ShopID, &s.AutomationID, &s.CustomerID, &s.Reason, &missing,
			&s.TriggerLabel, &s.OrderID, &s.IdempotencyKey,
			&s.CreatedAt, &s.CustomerName, &s.TemplateName,
		); err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			_ = json.Unmarshal(missing, &s.Missing)
		}
		if s.Missing == nil {
			s.Missing = []whatsapp.TokenKey{}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RetryResult reports what a "send the held-back ones now" attempt did.
type RetryResult struct {
	Attempted       int
	Sent            int
	Scheduled       int
	StillHeld       int
	AlreadySent     int
	TemplateMissing int
	// Rejected counts sends the send rules refused: no consent for the category,
	// or a template the carrier treats as marketing. The event is untouched and
	// will be tried again, but no amount of retrying fixes it, so it is counted
	// apart from an order that is short of data.
	Rejected int
}

// pendingSend is one event the merchant may be owed a message for, assembled
// from whichever record survived to describe it.
type pendingSend struct {
	key         string
	customerID  uuid.UUID
	orderID     string
	label       string
	idempotency string
}

// RetrySuppressions re-runs the sends this automation did not deliver, and is
// the reason the suppression row carries the order and label rather than only
// the reason: a retry is self-contained, so it does not depend on replaying a
// webhook that arrived days ago.
//
// Events are keyed by the idempotency key, which is also what stops a message
// that did go out from being sent twice: an event that already has a message
// row is counted and skipped, not re-sent.
func (p *Processor) RetrySuppressions(ctx context.Context, shopID, automationID uuid.UUID) (RetryResult, error) {
	var res RetryResult

	automation, err := p.Automation(ctx, shopID, automationID)
	if err != nil {
		return res, err
	}
	if automation == nil {
		return res, nil
	}

	pending, err := p.pendingSends(ctx, automation)
	if err != nil {
		return res, err
	}

	// A merchant pressing this wants the messages out now, so the automation's
	// own window is deliberately bypassed: the event's moment has long passed.
	instant := fireRule{timezone: "UTC", days: AllDays()}

	for _, pend := range pending {
		exists, xErr := p.whatsapp.MessageExistsForIdempotencyKey(ctx, automation.ShopID, pend.key)
		if xErr != nil {
			p.log.Warn("automation: could not check whether a retry was already sent",
				"automation_id", automationID, "error", xErr)
			continue
		}
		if exists {
			res.AlreadySent++
			continue
		}

		in := SendInput{
			ShopID:         automation.ShopID,
			AutomationID:   automationID,
			CustomerID:     pend.customerID,
			TemplateID:     automation.TemplateID,
			OrderID:        pend.orderID,
			StatusLabel:    pend.label,
			IdempotencyKey: pend.key,
			fire:           instant,
		}
		if c, cErr := p.whatsapp.Customer(ctx, automation.ShopID, pend.customerID); cErr == nil {
			in.CustomerName, in.CustomerPhone = c.Name, c.Phone
		}

		res.Attempted++
		outcome, sErr := p.send(ctx, in)
		if sErr != nil {
			p.log.Warn("automation: retry of a held-back send failed",
				"automation_id", automationID, "order_id", pend.orderID, "error", sErr)
			res.StillHeld++
			continue
		}

		switch outcome {
		case outcomeTemplateMissing:
			// Not the order's fault, and retrying will not help: the automation
			// points at a template that is gone. Reported on its own so the page
			// does not tell the merchant their order is missing data.
			res.TemplateMissing++
			continue
		case outcomeSent:
			res.Sent++
			p.clearSuppression(ctx, automationID, pend.key)
			continue
		case outcomeScheduled:
			// On its way, but not sent yet: reporting it as sent would be a lie
			// the merchant checks against the history page.
			res.Scheduled++
			p.clearSuppression(ctx, automationID, pend.key)
			continue
		case outcomeRejected:
			// The send rules refused it, which is not the same as this order
			// being short of data. The row stays, so the event is not lost.
			res.Rejected++
			continue
		}

		// Held back again, or the send gate declined it. The row is refreshed in
		// place with the current reason, so the next attempt sees new data.
		res.StillHeld++
	}

	return res, nil
}

// pendingSends assembles the undelivered events for one automation. A Converty
// order event leaves no queryable state behind — the customer lives inside the
// stored webhook payload — so only the recorded suppression rows are available
// to retry.
func (p *Processor) pendingSends(ctx context.Context, automation *Automation) ([]pendingSend, error) {
	byKey := map[string]pendingSend{}

	held, err := SuppressionsForAutomation(ctx, p.pool, automation.ID, 500)
	if err != nil {
		return nil, err
	}
	for _, s := range held {
		if s.IdempotencyKey == "" {
			continue
		}
		byKey[s.IdempotencyKey] = pendingSend{
			key:         s.IdempotencyKey,
			customerID:  s.CustomerID,
			orderID:     s.OrderID,
			label:       s.TriggerLabel,
			idempotency: s.IdempotencyKey,
		}
	}

	out := make([]pendingSend, 0, len(byKey))
	for _, pend := range byKey {
		out = append(out, pend)
	}
	// Deterministic order so a retry sends oldest event first. The idempotency
	// key carries the order id, so two events on the same status sort together.
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].key, out[j].key) < 0
	})
	return out, nil
}

// clearSuppression drops a row once its message exists. Keeping it would leave
// the merchant staring at a problem that is already solved, and the message
// itself is the durable record that the send happened.
func (p *Processor) clearSuppression(ctx context.Context, automationID uuid.UUID, idempotencyKey string) {
	if idempotencyKey == "" {
		return
	}
	if _, err := p.pool.Exec(ctx, `
		DELETE FROM automation_suppressions
		WHERE automation_id = $1 AND idempotency_key = $2`,
		automationID, idempotencyKey,
	); err != nil {
		p.log.Warn("automation: could not clear retried suppression",
			"automation_id", automationID, "error", err)
	}
}