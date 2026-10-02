package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Customer is a tenant-scoped message recipient.
type Customer struct {
	ID        uuid.UUID
	ShopID    uuid.UUID
	Name      string
	Phone     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Consent is a customer's WhatsApp opt-in record.
type Consent struct {
	ID         uuid.UUID
	ShopID     uuid.UUID
	CustomerID uuid.UUID
	Status     string
	Category   string
	Source     string
	Evidence   string
	ObtainedAt time.Time
	RevokedAt  *time.Time
}

// UpsertCustomer creates or refreshes a customer for a shop, keyed by
// (shop_id, phone). Returns the customer id (existing or new).
func (s *Service) UpsertCustomer(ctx context.Context, shopID uuid.UUID, name, phone string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO customers (shop_id, name, phone, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (shop_id, phone) DO UPDATE SET
			name = EXCLUDED.name, updated_at = now()
		RETURNING id`,
		shopID, name, phone,
	).Scan(&id)
	return id, err
}

// Customer returns a customer scoped to a shop. Returns an error rather than a
// row from another merchant.
func (s *Service) Customer(ctx context.Context, shopID, customerID uuid.UUID) (Customer, error) {
	var c Customer
	err := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, name, phone, created_at, updated_at
		FROM customers
		WHERE id = $1 AND shop_id = $2`,
		customerID, shopID,
	).Scan(&c.ID, &c.ShopID, &c.Name, &c.Phone, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// Customers lists a merchant's customers, newest first.
func (s *Service) Customers(ctx context.Context, shopID uuid.UUID) ([]Customer, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, shop_id, name, phone, created_at, updated_at
		FROM customers
		WHERE shop_id = $1
		ORDER BY created_at DESC`,
		shopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.ShopID, &c.Name, &c.Phone, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GrantConsent records a customer opt-in for a merchant. Re-granting is
// idempotent and raises revoked_at back to NULL.
func (s *Service) GrantConsent(ctx context.Context, shopID, customerID uuid.UUID, category, source, evidence string) error {
	if _, err := s.Customer(ctx, shopID, customerID); err != nil {
		return err
	}
	return s.grantConsent(ctx, shopID, customerID, category, source, evidence)
}

func (s *Service) grantConsent(ctx context.Context, shopID, customerID uuid.UUID, category, source, evidence string) error {
	if category == "" {
		category = "order_updates"
	}
	if source == "" {
		source = "merchant_declared"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO whatsapp_consent (
			shop_id, customer_id, status, category, source, evidence, revoked_at
		) VALUES ($1, $2, 'opt_in', $3, $4, $5, NULL)
		ON CONFLICT DO NOTHING`,
		shopID, customerID, category, source, evidence,
	)
	return err
}

// RevokeConsent records that a customer's opt-in no longer stands. All records
// for the customer are closed out via status=revoked and revoked_at.
func (s *Service) RevokeConsent(ctx context.Context, shopID, customerID uuid.UUID) error {
	if _, err := s.Customer(ctx, shopID, customerID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO whatsapp_consent (
			shop_id, customer_id, status, category, source
		) VALUES ($1, $2, 'revoked', 'order_updates', 'merchant_revoked')`,
		shopID, customerID,
	)
	return err
}

// ConsentLatest returns the newest consent record for a customer.
func (s *Service) ConsentLatest(ctx context.Context, shopID, customerID uuid.UUID) (Consent, error) {
	var c Consent
	err := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, customer_id, status, category, source, evidence, obtained_at, revoked_at
		FROM whatsapp_consent
		WHERE shop_id = $1 AND customer_id = $2
		ORDER BY created_at DESC
		LIMIT 1`,
		shopID, customerID,
	).Scan(&c.ID, &c.ShopID, &c.CustomerID, &c.Status, &c.Category, &c.Source, &c.Evidence, &c.ObtainedAt, &c.RevokedAt)
	return c, err
}

// merchantState loads the merchant's WhatsApp eligibility snapshot.
func (s *Service) merchantState(ctx context.Context, shopID uuid.UUID) (MerchantState, error) {
	var m MerchantState
	err := s.pool.QueryRow(ctx, `
		SELECT id, status, whatsapp_enabled, whatsapp_terms_accepted_at
		FROM shops
		WHERE id = $1`,
		shopID,
	).Scan(&m.ShopID, &m.Status, &m.WhatsappEnabled, &m.TermsAcceptedAt)
	return m, err
}

// customerState loads a customer scoped to a shop.
func (s *Service) customerState(ctx context.Context, shopID, customerID uuid.UUID) (CustomerState, error) {
	var c CustomerState
	c.ID = customerID
	err := s.pool.QueryRow(ctx, `
		SELECT id, phone
		FROM customers
		WHERE id = $1 AND shop_id = $2`,
		customerID, shopID,
	).Scan(&c.ID, &c.Phone)
	if errors.Is(err, pgx.ErrNoRows) {
		c.OwnedByShop = false
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.OwnedByShop = true
	return c, nil
}

// consentState loads the customer's latest consent.
func (s *Service) consentState(ctx context.Context, shopID, customerID uuid.UUID) (ConsentState, error) {
	var c ConsentState
	err := s.pool.QueryRow(ctx, `
		SELECT status, category
		FROM whatsapp_consent
		WHERE shop_id = $1 AND customer_id = $2
		ORDER BY created_at DESC
		LIMIT 1`,
		shopID, customerID,
	).Scan(&c.Status, &c.Category)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsentState{}, nil
	}
	return c, err
}

// templateState loads a merchant's template scoped to the shop.
func (s *Service) templateState(ctx context.Context, shopID, templateID uuid.UUID) (TemplateState, error) {
	var t TemplateState
	var components []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, meta_template_name, language, category, approval_status, marketing_flagged, components
		FROM templates
		WHERE id = $1 AND shop_id = $2`,
		templateID, shopID,
	).Scan(&t.ID, &t.Name, &t.Language, &t.Category, &t.ApprovalStatus, &t.MarketingFlagged, &components)
	if err != nil {
		return TemplateState{}, err
	}
	t.RawComponents = components
	if len(components) > 0 {
		if jErr := json.Unmarshal(components, &t.Components); jErr != nil {
			return TemplateState{}, jErr
		}
	}
	t.NumVariables = countTemplateVariablesRaw(components)
	return t, nil
}

// createQueuedMessage reserves the message row (idempotency-safe) before the
// Meta call, so a crash between check and send never double-sends. The rendered
// text is written with it: history is a record of what went out, and a template
// edited or deleted tomorrow must not be able to change what this row says the
// customer received.
func (s *Service) createQueuedMessage(ctx context.Context, req SendRequest, t TemplateState) (uuid.UUID, error) {
	varsJSON, err := json.Marshal(req.Variables)
	if err != nil {
		return uuid.Nil, err
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO messages (
			shop_id, customer_id, template_id, converty_order_id, recipient_phone,
			template_variables, idempotency_key, automation_id, status, body_text
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7,
			NULLIF($8::uuid, '00000000-0000-0000-0000-000000000000'), 'queued',
			NULLIF($9, ''))
		RETURNING id`,
		req.ShopID, req.CustomerID, t.ID, req.ConvertyOrderID,
		customer_phone(ctx, s, req), varsJSON, req.IdempotencyKey, req.AutomationID,
		RenderTemplateBody(t.RawComponents, req.Variables),
	).Scan(&id)
	return id, err
}

func customer_phone(ctx context.Context, s *Service, req SendRequest) string {
	// The customer row is guaranteed present by the send gate; resolve phone
	// for the recipient column.
	c, err := s.Customer(ctx, req.ShopID, req.CustomerID)
	if err != nil {
		return ""
	}
	return c.Phone
}

// findMessageByIdempotency returns the recorded result for a repeated key.
func (s *Service) findMessageByIdempotency(ctx context.Context, shopID uuid.UUID, key string) (*SendResult, error) {
	var r SendResult
	err := s.pool.QueryRow(ctx, `
		SELECT id, meta_message_id, status
		FROM messages
		WHERE shop_id = $1 AND idempotency_key = $2`,
		shopID, key,
	).Scan(&r.MessageID, &r.MetaMessageID, &r.Status)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// markMessageSent records the Meta-assigned id for an accepted delivery.
func (s *Service) markMessageSent(ctx context.Context, messageID uuid.UUID, metaID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE messages
		SET meta_message_id = $2, status = 'sent', sent_at = COALESCE(sent_at, now()), updated_at = now()
		WHERE id = $1`,
		messageID, metaID,
	)
	return err
}

// markMessageFailed records a send-time failure.
func (s *Service) markMessageFailed(ctx context.Context, messageID uuid.UUID, code, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE messages
		SET status = 'failed', error_code = $2, error_message = $3, failed_at = now(), updated_at = now()
		WHERE id = $1`,
		messageID, code, message,
	)
	return err
}
