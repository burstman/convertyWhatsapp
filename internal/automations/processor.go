package automations

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/whatsapp"
)

// Processor evaluates order events against the shop's automations and turns a
// match into a WhatsApp send: inline when the rule says now, queued when it
// says later. It is the single entry point used by the webhook handler.
type Processor struct {
	cfg      config.Config
	pool     *pgxpool.Pool
	log      *slog.Logger
	whatsapp *whatsapp.Service
}

func NewProcessor(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, wa *whatsapp.Service) *Processor {
	return &Processor{
		cfg:      cfg,
		pool:     pool,
		log:      log,
		whatsapp: wa,
	}
}

// ConvertyEvent is the normalized subset of an order webhook that automation
// needs.
type ConvertyEvent struct {
	ShopID        uuid.UUID
	OrderStatus   string
	OrderID       string
	CustomerName  string
	CustomerPhone string
}

// OnConvertyEvent feeds one captured Converty order event through the
// automation pipeline: the customer is learned (consent for the template's
// category is granted on the matching automation send) and the order-status
// automation fires if configured.
func (p *Processor) OnConvertyEvent(ctx context.Context, e ConvertyEvent) error {
	if e.ShopID == uuid.Nil {
		return nil
	}

	cid := uuid.Nil
	if e.CustomerPhone != "" {
		c, err := p.whatsapp.UpsertCustomer(ctx, e.ShopID, e.CustomerName, e.CustomerPhone)
		if err != nil {
			p.log.Warn("automation: customer upsert failed", "shop_id", e.ShopID, "error", err)
		} else {
			cid = c
		}
	}

	automation, err := p.match(ctx, e.ShopID, SourceConverty, e.OrderStatus)
	if err != nil {
		return err
	}
	if automation.ID == uuid.Nil {
		return nil
	}

	if cid == uuid.Nil {
		p.log.Debug("automation: event carries no customer phone, nothing to send",
			"shop_id", e.ShopID, "status", e.OrderStatus)
		return nil
	}

	_, err = p.send(ctx, SendInput{
		ShopID:         e.ShopID,
		AutomationID:   automation.ID,
		CustomerID:     cid,
		TemplateID:     automation.TemplateID,
		CustomerName:   e.CustomerName,
		CustomerPhone:  e.CustomerPhone,
		OrderID:        e.OrderID,
		StatusLabel:    e.OrderStatus,
		IdempotencyKey: "conv:" + e.ShopID.String() + ":" + e.OrderStatus + ":" + e.OrderID,
		fire:           ruleFromSchedule(automation.Schedule()),
	})
	return err
}

// SendInput is the resolved, ready-to-send automation input. fire decides when
// the send may happen (see fireRule below).
type SendInput struct {
	ShopID uuid.UUID
	// AutomationID is the automation this send belongs to. It rides along so the
	// message row can be attributed, which is what the merchant's per-automation
	// send history is built from.
	AutomationID   uuid.UUID
	CustomerID     uuid.UUID
	TemplateID     uuid.UUID
	CustomerName   string
	CustomerPhone  string
	OrderID        string
	StatusLabel    string
	IdempotencyKey string
	fire           fireRule
}

// fireRule is the normalized deliverability rule for one automation.
type fireRule struct {
	minute   *int   // minute of day 0..1439, set for a fixed daily window
	delay    *int   // minutes to wait after the event, set for a delayed send
	timezone string // IANA zone the wall-clock rule lives in
	days     []int  // ISO weekdays 1..7 the daily window is allowed on
}

func ruleFromSchedule(s *Schedule) fireRule {
	r := fireRule{timezone: "UTC", days: AllDays()}
	if s == nil {
		return r
	}
	if s.Timezone != "" {
		r.timezone = s.Timezone
	}
	if len(s.Days) > 0 {
		r.days = s.Days
	}
	r.minute = s.SendMinute
	r.delay = s.DelayMinute
	return r
}

func (r fireRule) location() *time.Location {
	if l, err := time.LoadLocation(r.timezone); err == nil {
		return l
	}
	return time.UTC
}

// isoWeekday maps a time to 1..7 with Monday=1, Sunday=7 (ISO 8601).
// time.Weekday() returns 0 for Sunday, so Sunday must land on 7.
func isoWeekday(t time.Time) int {
	return (int(t.Weekday())+6)%7 + 1
}

func (r fireRule) allows(isoWeekday int) bool {
	for _, d := range r.days {
		if d == isoWeekday {
			return true
		}
	}
	return false
}

// scheduledFire resolves the delivery rule into an absolute send instant.
//
//   - delayed:  the event time + the configured delay.
//   - instant:  zero — send right now.
//   - fixed:    on an allowed weekday, an event before the window waits until
//     it, and an event at/after it fires immediately (the window already
//     opened). On a non-allowed weekday the send waits for the next allowed
//     weekday at the same window.
func scheduledFire(r fireRule, now time.Time) time.Time {
	loc := r.location()
	now = now.In(loc)

	if r.delay != nil {
		return now.Add(time.Duration(*r.delay) * time.Minute)
	}
	if r.minute == nil {
		return time.Time{}
	}
	if !r.allows(isoWeekday(now)) {
		return r.nextAllowedDay(now, loc)
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), *r.minute/60, *r.minute%60, 0, 0, loc)
	if at.After(now) {
		return at
	}
	return time.Time{}
}

// nextAllowedDay scans the next two weeks for the first allowed weekday and
// returns that day's window time. Empty day sets never match (treat as today).
func (r fireRule) nextAllowedDay(now time.Time, loc *time.Location) time.Time {
	for d := 1; d <= 14; d++ {
		candidate := now.AddDate(0, 0, d)
		if !r.allows(isoWeekday(candidate)) {
			continue
		}
		return time.Date(candidate.Year(), candidate.Month(), candidate.Day(),
			*r.minute/60, *r.minute%60, 0, 0, loc)
	}
	return time.Time{}
}

// sendOutcome is what one run of the pipeline actually did. Without it a
// caller can only report "not sent", and the page then blames the order for
// every failure — which is how a misconfigured template came to be reported to
// the merchant as "the order is missing data".
type sendOutcome string

const (
	outcomeSent            sendOutcome = "sent"
	outcomeScheduled       sendOutcome = "scheduled"
	outcomeHeldBack        sendOutcome = "held_back"
	outcomeNoCustomer      sendOutcome = "no_customer"
	outcomeTemplateMissing sendOutcome = "template_missing"
	outcomeRejected        sendOutcome = "rejected"
)

// send executes one automation: it resolves the template (driving the variable
// set and the declared purpose), ensures opt-in consent, and enqueues the send
// — falling back to an inline send when no queue is available.
func (p *Processor) send(ctx context.Context, in SendInput) (sendOutcome, error) {
	if in.CustomerID == uuid.Nil {
		p.log.Debug("automation send skipped: no customer", "shop_id", in.ShopID)
		return outcomeNoCustomer, nil
	}

	t, err := p.whatsapp.Template(ctx, in.ShopID, in.TemplateID)
	if err != nil {
		p.log.Warn("automation: template lookup failed", "template_id", in.TemplateID, "error", err)
		return "", err
	}
	if t.ID == uuid.Nil {
		p.log.Warn("automation: the template this automation points at is gone",
			"shop_id", in.ShopID, "template_id", in.TemplateID)
		return outcomeTemplateMissing, nil
	}

	purpose := t.Category
	if err := p.whatsapp.GrantConsent(ctx, in.ShopID, in.CustomerID, purpose, "automation", in.OrderID); err != nil {
		p.log.Warn("automation: consent grant failed", "shop_id", in.ShopID, "error", err)
	}

	vars, missing := buildVariables(t, in)
	if len(missing) > 0 {
		// Sending now would deliver a message with blanks where the merchant
		// deliberately placed variables. Drop it and say why instead.
		p.log.Warn("automation send skipped: variable has no value for this event",
			"shop_id", in.ShopID, "template_id", in.TemplateID,
			"trigger", in.StatusLabel, "missing", missing)
		p.RecordSuppression(ctx, in, missing)
		return outcomeHeldBack, nil
	}

	job := whatsapp.SendWhatsAppTemplateJob{
		ShopID:          in.ShopID,
		AutomationID:    in.AutomationID,
		CustomerID:      in.CustomerID,
		TemplateID:      in.TemplateID,
		ConvertyOrderID: in.OrderID,
		Purpose:         purpose,
		Variables:       vars,
		IdempotencyKey:  in.IdempotencyKey,
	}

	// An instant automation sends right here: the event already happened, so a
	// queue hop would only make the customer wait. A delayed or fixed-window
	// rule parks the job until its moment instead.
	if at := scheduledFire(in.fire, time.Now()); !at.IsZero() {
		if err := p.whatsapp.EnqueueSendAt(ctx, job, at); err != nil {
			// Deliberately no inline fallback: sending now would deliver a message
			// hours before the merchant asked for it. Reporting the failure lets
			// the event be retried instead.
			p.log.Warn("automation: scheduling send failed",
				"shop_id", in.ShopID, "template_id", in.TemplateID,
				"trigger", in.StatusLabel, "fire_at", at, "error", err)
			return "", err
		}
		p.log.Info("automation send scheduled",
			"shop_id", in.ShopID, "customer_id", in.CustomerID,
			"template_id", in.TemplateID, "trigger", in.StatusLabel, "fire_at", at)
		p.clearSuppression(ctx, in.AutomationID, in.IdempotencyKey)
		return outcomeScheduled, nil
	}

	if _, err := p.whatsapp.SendTemplateMessage(ctx, toSendRequest(job)); err != nil {
		var rej *whatsapp.SendRejection
		if errors.As(err, &rej) {
			p.log.Debug("automation inline send rejected",
				"shop_id", in.ShopID, "code", rej.Code, "reason", rej.Reason)
			return outcomeRejected, nil
		}
		p.log.Warn("automation inline send failed", "shop_id", in.ShopID, "error", err)
		return "", err
	}
	// The customer has the message now, so the held-back row that described the
	// wait is over: leaving it would leave the merchant holding a message they
	// already sent and told us to send again.
	p.clearSuppression(ctx, in.AutomationID, in.IdempotencyKey)
	return outcomeSent, nil
}

// buildVariables fills every template placeholder. Semantic templates map each
// position through the stored token key ({{customer_name}}, {{order_id}}, …);
// legacy positional templates fall back to the classic mapping {{1}}=customer,
// {{2}}=order, {{3}}=status.
//
// It also reports the author-placed variables that resolved to nothing. A
// semantic template is written with those chips on purpose, so a blank slot
// means the event did not carry the data and the message must not go out with a
// placeholder in it. Legacy positional templates predate the chip editor and
// keep the neutral em dash, since their intent cannot be reconstructed.
func buildVariables(t whatsapp.MerchantTemplate, in SendInput) (map[string]string, []whatsapp.TokenKey) {
	vals := whatsapp.TemplateVariableValues{
		CustomerName:  in.CustomerName,
		CustomerPhone: in.CustomerPhone,
		OrderID:       in.OrderID,
		StatusLabel:   in.StatusLabel,
	}

	semantic := len(t.Variables) > 0
	vars := make(map[string]string, t.NumVariables)
	var missing []whatsapp.TokenKey
	for i := 1; i <= t.NumVariables; i++ {
		key := whatsapp.DefaultTokenForPosition(i)
		if i-1 < len(t.Variables) {
			key = t.Variables[i-1]
		}
		v := whatsapp.TokenValue(key, vals)
		if v == "" {
			if semantic {
				missing = append(missing, key)
			}
			v = "—"
		}
		vars[strconv.Itoa(i)] = v
	}
	return vars, missing
}

func toSendRequest(job whatsapp.SendWhatsAppTemplateJob) whatsapp.SendRequest {
	return whatsapp.SendRequest{
		ShopID:          job.ShopID,
		CustomerID:      job.CustomerID,
		AutomationID:    job.AutomationID,
		TemplateID:      job.TemplateID,
		ConvertyOrderID: job.ConvertyOrderID,
		Purpose:         job.Purpose,
		Variables:       job.Variables,
		IdempotencyKey:  job.IdempotencyKey,
	}
}