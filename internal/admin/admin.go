// Package admin provides the platform-operator console: a read-only view of
// every client account and the shops they own. It deliberately exposes no shop
// credentials and performs no mutations yet.
package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Summary is the headline count row shown at the top of the console.
type Summary struct {
	Clients       int
	ActiveClients int
	Shops         int
	WhatsappShops int
}

// Client is a client account together with an aggregate of its shops and its
// trailing-30-day message usage.
type Client struct {
	ID        uuid.UUID
	Name      string
	Email     string
	Status    string
	CreatedAt time.Time
	ShopCount int
	ShopNames string
	Whatsapp  bool

	// Usage is derived from the client's shops' messages over the last 30 days.
	Sent30d      int
	DeliveryRate float64
	HasDelivery  bool

	// Credit. HasCredit is false when no client_credits row exists (billing is
	// not enforced yet, so most clients have no balance on record).
	BalanceMicros int64
	HasCredit     bool
}

// Shop is a single shop joined to its owning client.
type Shop struct {
	ID              uuid.UUID
	Name            string
	ClientName      string
	ClientEmail     string
	Phone           string
	WhatsappEnabled bool
	TemplateCount   int
	CreatedAt       time.Time
}

// Alert is a derived platform notice shown in the console.
type Alert struct {
	Severity string // "danger" | "warning"
	Text     string
}

// Activity is one recent message shown in the live feed.
type Activity struct {
	Time    string
	Client  string
	Message string
	Status  string
}

// Metrics holds the platform-wide figures rendered by the overview dashboard.
// Every counter is a real aggregate; where the platform tracks nothing yet
// (quality rating, messaging tier, block rate) the view renders "—".
type Metrics struct {
	// Trailing 30 days vs the 30 days before them.
	Sent30d          int
	SentPrev30d      int
	Delivered30d     int
	DeliveredPrev30d int
	Failed30d        int
	FailedPrev30d    int
	RevenueMicros    int64
	RevenuePrev      int64

	// NewActiveClients is the number of active clients created in the last 30 days.
	NewActiveClients int

	// Volume is a 14-day daily message count, oldest first.
	Volume    []int
	VolumeMax int

	// QueuePending is the number of unclaimed jobs in the Postgres queue.
	QueuePending int
	// SentToday is the number of accepted messages since local midnight.
	SentToday int

	Alerts   []Alert
	Activity []Activity
}

// Overview bundles everything the console renders.
type Overview struct {
	Summary Summary
	Clients []Client
	Shops   []Shop
	Metrics Metrics
}

// DeliveryRate is the share of accepted messages (this 30-day window) that were
// delivered. ok is false when nothing was attempted, i.e. there is no data.
func (m Metrics) DeliveryRate() (rate float64, ok bool) {
	attempts := m.Sent30d + m.Failed30d
	if attempts == 0 {
		return 0, false
	}
	return float64(m.Delivered30d) / float64(attempts), true
}

// DeliveryRatePrev is the same rate for the preceding 30-day window.
func (m Metrics) DeliveryRatePrev() (rate float64, ok bool) {
	attempts := m.SentPrev30d + m.FailedPrev30d
	if attempts == 0 {
		return 0, false
	}
	return float64(m.DeliveredPrev30d) / float64(attempts), true
}

// RevenueUSD is the trailing-30-day charged amount in dollars.
func (m Metrics) RevenueUSD() float64 { return float64(m.RevenueMicros) / 1e6 }

// RevenuePrevUSD is the preceding-30-day charged amount in dollars.
func (m Metrics) RevenuePrevUSD() float64 { return float64(m.RevenuePrev) / 1e6 }

// Repository reads platform-wide data for the admin console.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Overview loads the console's summary, clients, shops and metrics.
func (r *Repository) Overview(ctx context.Context) (Overview, error) {
	var o Overview

	if err := r.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM users WHERE role = 'client'),
			(SELECT COUNT(*) FROM users WHERE role = 'client' AND status = 'active'),
			(SELECT COUNT(*) FROM shops),
			(SELECT COUNT(*) FROM shops WHERE whatsapp_enabled)`,
	).Scan(&o.Summary.Clients, &o.Summary.ActiveClients, &o.Summary.Shops, &o.Summary.WhatsappShops); err != nil {
		return Overview{}, err
	}

	if err := r.loadMetrics(ctx, &o.Metrics); err != nil {
		return Overview{}, err
	}

	clients, err := r.clients(ctx)
	if err != nil {
		return Overview{}, err
	}
	// Attach per-client usage and credit.
	if err := r.attachClientUsage(ctx, clients); err != nil {
		return Overview{}, err
	}
	o.Clients = clients

	shops, err := r.shops(ctx)
	if err != nil {
		return Overview{}, err
	}
	o.Shops = shops

	return o, nil
}

// loadMetrics fills the platform-wide dashboard figures.
func (r *Repository) loadMetrics(ctx context.Context, m *Metrics) error {
	const window = `created_at >= now() - interval '30 days'`
	const prev = `created_at >= now() - interval '60 days' AND created_at < now() - interval '30 days'`

	if err := r.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE `+window+` AND status IN ('sent','delivered','read')),
			COUNT(*) FILTER (WHERE `+prev+` AND status IN ('sent','delivered','read')),
			COUNT(*) FILTER (WHERE `+window+` AND status IN ('delivered','read')),
			COUNT(*) FILTER (WHERE `+prev+` AND status IN ('delivered','read')),
			COUNT(*) FILTER (WHERE `+window+` AND status = 'failed'),
			COUNT(*) FILTER (WHERE `+prev+` AND status = 'failed'),
			COALESCE(SUM(price_micros) FILTER (WHERE `+window+`), 0),
			COALESCE(SUM(price_micros) FILTER (WHERE `+prev+`), 0),
			COUNT(*) FILTER (WHERE created_at >= date_trunc('day', now()) AND status IN ('sent','delivered','read'))
		FROM messages`,
	).Scan(&m.Sent30d, &m.SentPrev30d, &m.Delivered30d, &m.DeliveredPrev30d, &m.Failed30d, &m.FailedPrev30d,
		&m.RevenueMicros, &m.RevenuePrev, &m.SentToday); err != nil {
		return err
	}

	if err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM users
		WHERE role = 'client' AND status = 'active' AND created_at >= now() - interval '30 days'`,
	).Scan(&m.NewActiveClients); err != nil {
		return err
	}

	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(COUNT(*), 0) FROM job_queue WHERE lease_until IS NULL`,
	).Scan(&m.QueuePending); err != nil {
		return err
	}

	volume, max, err := r.dailyVolume(ctx, 14)
	if err != nil {
		return err
	}
	m.Volume, m.VolumeMax = volume, max

	if err := r.loadAlerts(ctx, m); err != nil {
		return err
	}

	activity, err := r.activity(ctx)
	if err != nil {
		return err
	}
	m.Activity = activity

	return nil
}

// dailyVolume returns the message count per day for the last days (oldest
// first) plus the maximum daily count.
func (r *Repository) dailyVolume(ctx context.Context, days int) ([]int, int, error) {
	counts := map[string]int{}
	rows, err := r.pool.Query(ctx, `
		SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD'), COUNT(*)
		FROM messages
		WHERE created_at >= now() - ($1::int - 1) * interval '1 day'
		GROUP BY 1`,
		days)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			day string
			n   int
		)
		if err := rows.Scan(&day, &n); err != nil {
			return nil, 0, err
		}
		counts[day] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	out := make([]int, days)
	now := time.Now()
	max := 0
	for i := 0; i < days; i++ {
		day := now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		out[i] = counts[day]
		if out[i] > max {
			max = out[i]
		}
	}
	return out, max, nil
}

// loadAlerts derives the console alerts from current platform state. Any
// condition with no rows simply yields no alert.
func (r *Repository) loadAlerts(ctx context.Context, m *Metrics) error {
	var failed24h, zeroCredit, pendingTemplates int
	if err := r.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM messages WHERE status = 'failed' AND created_at >= now() - interval '24 hours'),
			(SELECT COUNT(*) FROM client_credits WHERE balance_micros <= 0),
			(SELECT COUNT(*) FROM templates WHERE approval_status = 'pending')`,
	).Scan(&failed24h, &zeroCredit, &pendingTemplates); err != nil {
		return err
	}

	if failed24h > 0 {
		m.Alerts = append(m.Alerts, Alert{
			Severity: "danger",
			Text:     plural(failed24h, "message failed", "messages failed") + " in the last 24 hours",
		})
	}
	if zeroCredit > 0 {
		m.Alerts = append(m.Alerts, Alert{
			Severity: "warning",
			Text:     plural(zeroCredit, "client has", "clients have") + " no credit balance",
		})
	}
	if pendingTemplates > 0 {
		m.Alerts = append(m.Alerts, Alert{
			Severity: "warning",
			Text:     plural(pendingTemplates, "template is", "templates are") + " pending Meta approval",
		})
	}
	if m.QueuePending > 0 {
		m.Alerts = append(m.Alerts, Alert{
			Severity: "warning",
			Text:     plural(m.QueuePending, "job is", "jobs are") + " waiting in the send queue",
		})
	}
	return nil
}

// activity returns the most recent messages for the live feed.
func (r *Repository) activity(ctx context.Context) ([]Activity, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.created_at,
		       COALESCE(u.name, s.name, 'Shop'),
		       COALESCE(NULLIF(m.body_text, ''), t.meta_template_name, '(message)'),
		       m.status
		FROM messages m
		JOIN shops s ON s.id = m.shop_id
		LEFT JOIN users u ON u.id = s.client_id
		LEFT JOIN templates t ON t.id = m.template_id
		ORDER BY m.created_at DESC
		LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Activity
	for rows.Next() {
		var (
			at   time.Time
			a    Activity
			body string
		)
		if err := rows.Scan(&at, &a.Client, &body, &a.Status); err != nil {
			return nil, err
		}
		a.Time = at.Local().Format("15:04")
		a.Message = firstLine(body)
		out = append(out, a)
	}
	return out, rows.Err()
}

// attachClientUsage fills each client's 30-day usage and credit balance.
func (r *Repository) attachClientUsage(ctx context.Context, clients []Client) error {
	if len(clients) == 0 {
		return nil
	}
	idx := make(map[uuid.UUID]int, len(clients))
	for i := range clients {
		idx[clients[i].ID] = i
	}

	rows, err := r.pool.Query(ctx, `
		SELECT s.client_id,
		       COUNT(*) FILTER (WHERE m.status IN ('sent','delivered','read')),
		       COUNT(*) FILTER (WHERE m.status IN ('delivered','read')),
		       COUNT(*) FILTER (WHERE m.status IN ('sent','delivered','read','failed'))
		FROM messages m
		JOIN shops s ON s.id = m.shop_id
		WHERE s.client_id IS NOT NULL AND m.created_at >= now() - interval '30 days'
		GROUP BY s.client_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			cid       uuid.UUID
			sent      int
			delivered int
			attempts  int
		)
		if err := rows.Scan(&cid, &sent, &delivered, &attempts); err != nil {
			rows.Close()
			return err
		}
		if i, ok := idx[cid]; ok {
			clients[i].Sent30d = sent
			if attempts > 0 {
				clients[i].DeliveryRate = float64(delivered) / float64(attempts)
				clients[i].HasDelivery = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	creditRows, err := r.pool.Query(ctx, `SELECT client_id, balance_micros FROM client_credits`)
	if err != nil {
		return err
	}
	for creditRows.Next() {
		var (
			cid     uuid.UUID
			balance int64
		)
		if err := creditRows.Scan(&cid, &balance); err != nil {
			creditRows.Close()
			return err
		}
		if i, ok := idx[cid]; ok {
			clients[i].BalanceMicros = balance
			clients[i].HasCredit = true
		}
	}
	creditRows.Close()
	return creditRows.Err()
}

func (r *Repository) clients(ctx context.Context) ([]Client, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.name, u.email, u.status, u.created_at,
			(SELECT COUNT(*) FROM shops s WHERE s.client_id = u.id) AS shop_count,
			COALESCE((SELECT string_agg(s.name, ', ' ORDER BY s.created_at) FROM shops s WHERE s.client_id = u.id), '') AS shop_names,
			COALESCE((SELECT bool_or(s.whatsapp_enabled) FROM shops s WHERE s.client_id = u.id), false) AS whatsapp
		FROM users u
		WHERE u.role = 'client'
		ORDER BY u.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clients []Client
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.ID, &c.Name, &c.Email, &c.Status, &c.CreatedAt,
			&c.ShopCount, &c.ShopNames, &c.Whatsapp); err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

func (r *Repository) shops(ctx context.Context) ([]Shop, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT s.id, s.name, COALESCE(u.name, ''), COALESCE(u.email, ''),
			COALESCE(s.phone, ''), s.whatsapp_enabled,
			(SELECT COUNT(*) FROM templates t WHERE t.client_id = s.client_id AND t.scope = 'client'),
			s.created_at
		FROM shops s
		LEFT JOIN users u ON u.id = s.client_id
		ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var shops []Shop
	for rows.Next() {
		var s Shop
		if err := rows.Scan(&s.ID, &s.Name, &s.ClientName, &s.ClientEmail,
			&s.Phone, &s.WhatsappEnabled, &s.TemplateCount, &s.CreatedAt); err != nil {
			return nil, err
		}
		shops = append(shops, s)
	}
	return shops, rows.Err()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// firstLine trims a multi-line message body to its first non-empty line.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
