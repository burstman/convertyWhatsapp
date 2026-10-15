package dashboard

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Stats struct {
	WhatsappConnected bool
	ConvertyConnected bool
	ConvertyStoreName string
	MessagesSent      int
	MessagesDelivered int
	MessagesFailed    int
	ActiveAutomations int
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Stats(ctx context.Context, shopID uuid.UUID) (Stats, error) {
	var s Stats

	err := r.pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status IN ('sent','delivered','read') THEN 1 ELSE 0 END), 0) AS sent,
			COALESCE(SUM(CASE WHEN status IN ('delivered','read') THEN 1 ELSE 0 END), 0) AS delivered,
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS failed
		FROM messages
		WHERE shop_id = $1`,
		shopID,
	).Scan(&s.MessagesSent, &s.MessagesDelivered, &s.MessagesFailed)
	if err != nil {
		return Stats{}, err
	}

	err = r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM automations
		WHERE shop_id = $1 AND enabled = true`,
		shopID,
	).Scan(&s.ActiveAutomations)
	if err != nil {
		return Stats{}, err
	}

	err = r.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT whatsapp_enabled FROM shops WHERE id = $1), false)`,
		shopID,
	).Scan(&s.WhatsappConnected)
	if err != nil {
		return Stats{}, err
	}

	err = r.pool.QueryRow(ctx, `
		SELECT
			EXISTS(
				SELECT 1 FROM converty_integrations
				WHERE shop_id = $1 AND status = 'connected'
			),
			COALESCE((SELECT store_name FROM converty_integrations WHERE shop_id = $1 AND status = 'connected' LIMIT 1), '')`,
		shopID,
	).Scan(&s.ConvertyConnected, &s.ConvertyStoreName)
	if err != nil {
		return Stats{}, err
	}

	return s, nil
}
