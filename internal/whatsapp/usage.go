package whatsapp

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UsageSummary aggregates a shop's message ledger for the owner's Billing page.
// "Billable" means the message was accepted by Meta (sent_at is set) — the
// per-message price applies whether or not the delivery later succeeds.
type UsageSummary struct {
	// MonthLabel is the human label for the current billing window.
	MonthLabel string

	// ThisMonth is the number of billable messages in the current calendar month.
	ThisMonth int64

	// AllTime is the total billable messages recorded for the shop.
	AllTime int64

	// Monthly is the per-calendar-month billable series, most recent first.
	Monthly []MonthlyUsage

	// StatusCounts is the delivery-status breakdown of this-month messages.
	StatusCounts map[string]int64

	// ByTemplate is the top templates by billable messages sent this month.
	ByTemplate []UsageItem

	// ByAutomation is the top automations by billable messages sent this month.
	ByAutomation []UsageItem

	// CustomerCount is the number of distinct recipients messaged all-time.
	CustomerCount int64
}

type MonthlyUsage struct {
	Month string // "2026-09"
	Count int64
}

type UsageItem struct {
	Label string
	Count int64
}

// MessagesUsage computes the shop's usage summary from the message ledger.
func (s *Service) MessagesUsage(ctx context.Context, shopID uuid.UUID) (UsageSummary, error) {
	var (
		out  UsageSummary
		now  = time.Now()
		thC  int64
		allC int64
		cc   int64
	)

	out.MonthLabel = now.Format("January 2006")

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE shop_id = $1 AND sent_at IS NOT NULL AND created_at >= date_trunc('month', now())`,
		shopID,
	).Scan(&thC); err != nil {
		return out, err
	}
	out.ThisMonth = thC

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE shop_id = $1 AND sent_at IS NOT NULL`,
		shopID,
	).Scan(&allC); err != nil {
		return out, err
	}
	out.AllTime = allC

	rows, err := s.pool.Query(ctx, `
		SELECT to_char(date_trunc('month', created_at)::date, 'YYYY-MM'), count(*)
		FROM messages
		WHERE shop_id = $1 AND sent_at IS NOT NULL
		GROUP BY 1
		ORDER BY 1 DESC
		LIMIT 12`,
		shopID,
	)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m MonthlyUsage
		if err := rows.Scan(&m.Month, &m.Count); err != nil {
			rows.Close()
			return out, err
		}
		out.Monthly = append(out.Monthly, m)
	}
	rows.Close()

	statusRows, err := s.pool.Query(ctx, `
		SELECT status, count(*)
		FROM messages
		WHERE shop_id = $1 AND created_at >= date_trunc('month', now())
		GROUP BY status`,
		shopID,
	)
	if err != nil {
		return out, err
	}
	out.StatusCounts = make(map[string]int64)
	for statusRows.Next() {
		var (
			status string
			count  int64
		)
		if err := statusRows.Scan(&status, &count); err != nil {
			statusRows.Close()
			return out, err
		}
		out.StatusCounts[status] += count
	}
	statusRows.Close()

	templateRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(t.name, '(deleted template)'), count(*)
		FROM messages m
		LEFT JOIN templates t ON t.id = m.template_id
		WHERE m.shop_id = $1 AND m.sent_at IS NOT NULL AND m.created_at >= date_trunc('month', now())
		GROUP BY 1
		ORDER BY count(*) DESC
		LIMIT 10`,
		shopID,
	)
	if err != nil {
		return out, err
	}
	for templateRows.Next() {
		var it UsageItem
		if err := templateRows.Scan(&it.Label, &it.Count); err != nil {
			templateRows.Close()
			return out, err
		}
		out.ByTemplate = append(out.ByTemplate, it)
	}
	templateRows.Close()

	automationRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(a.name, '(deleted automation)'), count(*)
		FROM messages m
		LEFT JOIN automations a ON a.id = m.automation_id
		WHERE m.shop_id = $1 AND m.sent_at IS NOT NULL AND m.created_at >= date_trunc('month', now())
		GROUP BY 1
		ORDER BY count(*) DESC
		LIMIT 10`,
		shopID,
	)
	if err != nil {
		return out, err
	}
	for automationRows.Next() {
		var it UsageItem
		if err := automationRows.Scan(&it.Label, &it.Count); err != nil {
			automationRows.Close()
			return out, err
		}
		out.ByAutomation = append(out.ByAutomation, it)
	}
	automationRows.Close()

	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT recipient_phone) FROM messages WHERE shop_id = $1`,
		shopID,
	).Scan(&cc); err != nil {
		return out, err
	}
	out.CustomerCount = cc

	return out, nil
}
