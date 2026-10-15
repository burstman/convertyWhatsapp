package shops

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/database"
)

var ErrNotFound = errors.New("shop not found")

type Shop struct {
	ID                      uuid.UUID  `json:"id"`
	ClientID                uuid.UUID  `json:"client_id"`
	Name                    string     `json:"name"`
	Phone                   string     `json:"phone"`
	Status                  string     `json:"status"`
	WhatsappEnabled         bool       `json:"whatsapp_enabled"`
	WhatsappTermsAcceptedAt *time.Time `json:"whatsapp_terms_accepted_at"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create makes a shop owned by a client. Every shop belongs to exactly one
// client account.
func (r *Repository) Create(ctx context.Context, q database.Querier, clientID uuid.UUID, name string) (Shop, error) {
	var s Shop
	err := q.QueryRow(ctx, `
		INSERT INTO shops (client_id, name)
		VALUES ($1, $2)
		RETURNING id, client_id, name, status, updated_at, created_at`,
		clientID, name,
	).Scan(&s.ID, &s.ClientID, &s.Name, &s.Status, &s.UpdatedAt, &s.CreatedAt)
	if err != nil {
		return Shop{}, err
	}
	return s, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (Shop, error) {
	var s Shop
	err := r.pool.QueryRow(ctx, `
		SELECT id, client_id, name, phone, status, whatsapp_enabled, whatsapp_terms_accepted_at, created_at, updated_at
		FROM shops
		WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.ClientID, &s.Name, &s.Phone, &s.Status, &s.WhatsappEnabled, &s.WhatsappTermsAcceptedAt, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shop{}, ErrNotFound
	}
	if err != nil {
		return Shop{}, err
	}
	return s, nil
}

// EnableWhatsApp marks the merchant as opted into the platform's WhatsApp
// service, records their contact phone and that they accepted the service
// terms. This is the onboarding gateway for sending.
func (r *Repository) EnableWhatsApp(ctx context.Context, id uuid.UUID, phone string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE shops
		SET whatsapp_enabled = true,
		    whatsapp_terms_accepted_at = COALESCE(whatsapp_terms_accepted_at, now()),
		    phone = CASE WHEN $2 <> '' THEN $2 ELSE phone END,
		    updated_at = now()
		WHERE id = $1`,
		id, phone,
	)
	return err
}

// UpdatePhone changes the shop's contact phone. Owner-only: it is the number
// shown on the WhatsApp status card and used as the merchant's contact line.
func (r *Repository) UpdatePhone(ctx context.Context, id uuid.UUID, phone string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE shops
		SET phone = $2, updated_at = now()
		WHERE id = $1`,
		id, phone,
	)
	return err
}

// DisableWhatsApp turns the shop's WhatsApp service off. The terms acceptance
// timestamp is kept for the audit trail; re-enabling goes through onboard.
func (r *Repository) DisableWhatsApp(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE shops
		SET whatsapp_enabled = false, updated_at = now()
		WHERE id = $1`,
		id,
	)
	return err
}
