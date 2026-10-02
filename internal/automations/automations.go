package automations

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"whatsappconverty/internal/whatsapp"
)

// ErrDuplicate is returned when a (shop, event_source, order_status) automation
// already exists (the unique constraint mirrors the UI rule "one template per
// trigger").
var ErrDuplicate = errors.New("automation already exists for this trigger")

// Event sources an automation can trigger on. The target platform only feeds
// Converty order events today, so EventSource is stored on the row (the
// column constraint and index came from the shared schema) but every write
// uses whatsapp.SourceConverty.
const (
	SourceConverty = whatsapp.SourceConverty
	SourceDelivery = whatsapp.SourceDelivery
)

// DefaultTimezone is the zone a scheduled send uses when the form does not say
// otherwise. The platform serves Tunisian merchants, and a 10:00 send means 10:00
// in Tunis — not 10:00 UTC, which is 11:00 on the wall in front of them. Defaulting
// to UTC instead was how a "10 am" automation ended up firing an hour late.
const DefaultTimezone = "Africa/Tunis"

// Timezones are the zones offered in the automation form: the default first, then
// the ones a merchant plausibly operates in. Anything else a merchant has already
// saved stays selectable, so the list never silently rewrites a stored value.
var Timezones = []string{
	"Africa/Tunis",
	"Africa/Cairo",
	"Africa/Lagos",
	"Africa/Algiers",
	"Africa/Casablanca",
	"Europe/Paris",
	"Europe/London",
	"Europe/Istanbul",
	"Europe/Moscow",
	"Asia/Dubai",
	"Asia/Riyadh",
	"Asia/Karachi",
	"Asia/Kolkata",
	"Asia/Singapore",
	"Asia/Tokyo",
	"America/New_York",
	"America/Chicago",
	"America/Los_Angeles",
	"America/Sao_Paulo",
	"UTC",
}

// Automation maps a trigger (an order event) to an approved message template.
// EventSource disambiguates which origin the trigger key belongs to; only
// Converty events are fed today. The delivery rule is stored split across
// SendTime/SendTimezone/SendDays (a fixed daily window), DelayMinutes (delayed
// by N minutes), or neither (instant).
type Automation struct {
	ID           uuid.UUID
	ShopID       uuid.UUID
	Name         string
	Description  string
	EventSource  string
	OrderStatus  string
	TemplateID   uuid.UUID
	Enabled      bool
	SendTime     *time.Time
	SendTimezone string
	SendDays     []int
	DelayMinutes *int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Schedule captures the deliverability rule the UI writes to an automation.
// Both SendMinute and DelayMinute are nullable; a schedule with neither is an
// instant automation. Days is the ISO weekday set (1=Monday .. 7=Sunday) the
// fixed window is allowed to fire on.
type Schedule struct {
	SendMinute  *int
	DelayMinute *int
	Timezone    string
	Days        []int
}

// AllDays is the default weekday set (every day).
func AllDays() []int {
	return []int{1, 2, 3, 4, 5, 6, 7}
}

// ParseSchedule validates the "how to fire" form inputs and turns them into a
// rule. Modes: ""|"instant" (fire immediately), "fixed" (daily window at HH:MM
// on chosen weekdays in the given IANA zone), "delayed" (N minutes after the
// event). Validates the fixed window strictly (time, timezone, at least one
// day) and the delay strictly (positive).
func ParseSchedule(mode, hm, tz string, days []string, delayMinutes int) (*Schedule, error) {
	switch mode {
	case "", "instant":
		return nil, nil
	case "fixed":
		t, err := time.Parse("15:04", hm)
		if err != nil {
			return nil, errors.New("invalid send time")
		}
		if tz == "" {
			tz = DefaultTimezone
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, errors.New("invalid timezone")
		}
		var sendDays []int
		for _, d := range days {
			n, err := strconv.Atoi(d)
			if err != nil || n < 1 || n > 7 {
				return nil, errors.New("invalid send day")
			}
			sendDays = append(sendDays, n)
		}
		if len(sendDays) == 0 {
			return nil, errors.New("select at least one send day")
		}
		minute := t.Hour()*60 + t.Minute()
		return &Schedule{SendMinute: &minute, Timezone: tz, Days: sendDays}, nil
	case "delayed":
		if delayMinutes <= 0 {
			return nil, errors.New("delay must be a positive number of minutes")
		}
		return &Schedule{DelayMinute: &delayMinutes, Days: AllDays()}, nil
	default:
		return nil, errors.New("unknown automation type")
	}
}

// Schedule returns the automation's delivery rule, or nil when it fires
// instantly.
func (a Automation) Schedule() *Schedule {
	s := &Schedule{
		Timezone: a.SendTimezone,
		Days:     a.SendDays,
	}
	if a.SendTime != nil {
		m := a.SendTime.Hour()*60 + a.SendTime.Minute()
		s.SendMinute = &m
	}
	if a.DelayMinutes != nil {
		d := *a.DelayMinutes
		s.DelayMinute = &d
	}
	if s.SendMinute == nil && s.DelayMinute == nil {
		return nil
	}
	if len(s.Days) == 0 {
		s.Days = AllDays()
	}
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}
	return s
}

// List returns the automations configured for one shop, newest first.
func (p *Processor) List(ctx context.Context, shopID uuid.UUID) ([]Automation, error) {
	return p.ListByShop(ctx, shopID)
}

// ListByShop returns the automations configured for a shop.
func (p *Processor) ListByShop(ctx context.Context, shopID uuid.UUID) ([]Automation, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, shop_id, name, description, event_source, order_status, template_id, enabled,
		       send_time, send_timezone, send_days, delay_minutes, created_at, updated_at
		FROM automations
		WHERE shop_id = $1
		ORDER BY name, created_at`,
		shopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Automation
	for rows.Next() {
		var a Automation
		if err := rows.Scan(&a.ID, &a.ShopID, &a.Name, &a.Description, &a.EventSource, &a.OrderStatus,
			&a.TemplateID, &a.Enabled, &a.SendTime, &a.SendTimezone, &a.SendDays, &a.DelayMinutes,
			&a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// createFields turns a schedule into the column values INSERT/UPDATE share.
type scheduleColumns struct {
	sendTime    *time.Time
	timezone    string
	days        []int
	delayMinute *int
}

func scheduleToColumns(s *Schedule) scheduleColumns {
	cols := scheduleColumns{timezone: "UTC", days: AllDays()}
	if s == nil {
		return cols
	}
	if s.Timezone != "" {
		cols.timezone = s.Timezone
	}
	if len(s.Days) > 0 {
		cols.days = s.Days
	}
	if s.SendMinute != nil {
		tm := time.Date(0, time.January, 1, *s.SendMinute/60, *s.SendMinute%60, 0, 0, time.UTC)
		cols.sendTime = &tm
	}
	if s.DelayMinute != nil {
		cols.delayMinute = s.DelayMinute
	}
	return cols
}

func validSource(source string) bool {
	return source == SourceConverty || source == SourceDelivery
}

// Create records a new automation. The source must be one of the known event
// sources; the pair (shop, source, status) is unique. A nil schedule fires the
// automation the moment the event arrives.
func (p *Processor) Create(ctx context.Context, shopID uuid.UUID, source, status string, templateID uuid.UUID, schedule *Schedule, name, description string) error {
	if !validSource(source) {
		return errors.New("unknown automation event source")
	}
	if status == "" {
		return errors.New("automation trigger is required")
	}
	if templateID == uuid.Nil {
		return errors.New("automation template is required")
	}

	cols := scheduleToColumns(schedule)
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO automations (
			shop_id, name, description, event_source, order_status, template_id, enabled,
			send_time, send_timezone, send_days, delay_minutes
		) VALUES ($1, $2, $3, $4, $5, $6, true, $7, $8, $9, $10)
		ON CONFLICT (shop_id, event_source, order_status) DO NOTHING`,
		shopID, name, description, source, status, templateID,
		cols.sendTime, cols.timezone, cols.days, cols.delayMinute,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDuplicate
	}
	return nil
}

// Update overwrites everything about an existing automation (trigger, template,
// naming, delivery rule) for one shop. Changing the trigger onto an already
// used one surfaces ErrDuplicate via the unique constraint.
func (p *Processor) Update(ctx context.Context, shopID, id uuid.UUID, source, status string, templateID uuid.UUID, schedule *Schedule, name, description string) error {
	if !validSource(source) {
		return errors.New("unknown automation event source")
	}
	if status == "" {
		return errors.New("automation trigger is required")
	}
	if templateID == uuid.Nil {
		return errors.New("automation template is required")
	}

	cols := scheduleToColumns(schedule)
	_, err := p.pool.Exec(ctx, `
		UPDATE automations
		SET name = $3, description = $4, event_source = $5, order_status = $6, template_id = $7,
		    send_time = $8, send_timezone = $9, send_days = $10, delay_minutes = $11,
		    updated_at = now()
		WHERE id = $1 AND shop_id = $2`,
		id, shopID, name, description, source, status, templateID,
		cols.sendTime, cols.timezone, cols.days, cols.delayMinute,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	}
	return nil
}

// SetEnabled toggles an automation on or off.
func (p *Processor) SetEnabled(ctx context.Context, shopID, id uuid.UUID, enabled bool) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE automations
		SET enabled = $3, updated_at = now()
		WHERE id = $1 AND shop_id = $2`,
		id, shopID, enabled,
	)
	return err
}

// Delete removes an automation.
func (p *Processor) Delete(ctx context.Context, shopID, id uuid.UUID) error {
	_, err := p.pool.Exec(ctx, `
		DELETE FROM automations
		WHERE id = $1 AND shop_id = $2`,
		id, shopID,
	)
	return err
}

// Automation returns one automation by id, or nil when it does not belong to
// the shop.
func (p *Processor) Automation(ctx context.Context, shopID, id uuid.UUID) (*Automation, error) {
	var a Automation
	err := p.pool.QueryRow(ctx, `
		SELECT id, shop_id, name, description, event_source, order_status, template_id, enabled,
		       send_time, send_timezone, send_days, delay_minutes, created_at, updated_at
		FROM automations
		WHERE id = $1 AND shop_id = $2`,
		id, shopID,
	).Scan(&a.ID, &a.ShopID, &a.Name, &a.Description, &a.EventSource, &a.OrderStatus,
		&a.TemplateID, &a.Enabled, &a.SendTime, &a.SendTimezone, &a.SendDays, &a.DelayMinutes,
		&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// AutomationEnabled reports whether an automation is still active. A queued send
// is re-checked through it immediately before delivery: an automation that was
// paused or deleted after the event fired must not deliver a message the
// merchant has switched off. Unknown ids report false, so a deleted automation
// cancels the sends it had already queued.
func (p *Processor) AutomationEnabled(ctx context.Context, id uuid.UUID) (bool, error) {
	if id == uuid.Nil {
		return true, nil
	}
	var enabled bool
	err := p.pool.QueryRow(ctx, `SELECT enabled FROM automations WHERE id = $1`, id).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// match returns the enabled automation for a trigger, or an empty automation
// when none (or one whose template was deleted) is configured. The template
// join pulls in the lifecycle gates so no row can match a template the send
// rules would refuse anyway.
func (p *Processor) match(ctx context.Context, shopID uuid.UUID, source, status string) (Automation, error) {
	var a Automation
	err := p.pool.QueryRow(ctx, `
		SELECT a.id, a.shop_id, a.name, a.description, a.event_source, a.order_status, a.template_id, a.enabled,
		       a.send_time, a.send_timezone, a.send_days, a.delay_minutes, a.created_at, a.updated_at
		FROM automations a
		JOIN templates t ON t.id = a.template_id
		WHERE a.shop_id = $1 AND a.event_source = $2 AND a.order_status = $3
		  AND a.enabled AND t.approval_status = 'approved' AND t.marketing_flagged = false
		LIMIT 1`,
		shopID, source, status,
	).Scan(&a.ID, &a.ShopID, &a.Name, &a.Description, &a.EventSource, &a.OrderStatus, &a.TemplateID,
		&a.Enabled, &a.SendTime, &a.SendTimezone, &a.SendDays, &a.DelayMinutes, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Automation{}, nil
	}
	return a, err
}

// ConvertyStatuses returns the order trigger statuses the automation form
// offers for Converty, exactly as documented in the Converty OAuth docs
// (POST /orders + PATCH /orders/:id).
func ConvertyStatuses() []string {
	return []string{
		"pending",
		"confirmed",
		"exchange",
		"packed",
		"attempt",
		"uploaded",
		"rejected",
		"in transit",
		"delivered",
		"returned",
	}
}

// SourceLabel is a human label for the event-source select.
func SourceLabel(source string) string {
	if source == SourceConverty {
		return "Order event (Converty)"
	}
	return source
}