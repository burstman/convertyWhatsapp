package whatsapp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/database"
)

// Message is the API-facing shape of one sent message.
type Message struct {
	ID                uuid.UUID
	ShopID            uuid.UUID
	CustomerID        *uuid.UUID
	TemplateID        *uuid.UUID
	AutomationID      *uuid.UUID
	ConvertyOrderID   string
	RecipientPhone    string
	MetaMessageID     string
	Status            string
	ErrorCode         string
	ErrorMessage      string
	MetaErrors        []MetaError
	TemplateVariables map[string]string
	// BodyText is the rendered message as it was sent, snapshotted onto the row
	// at send time. Empty on rows written before that snapshot existed, where
	// the history falls back to the template's variable list.
	BodyText    string
	SentAt      *time.Time
	DeliveredAt *time.Time
	ReadAt      *time.Time
	FailedAt    *time.Time
	CreatedAt   time.Time
}

// AutomationMessage is one row of an automation's send history: the message
// itself plus the labels the page shows, resolved in the same query so the view
// needs no follow-up lookups.
type AutomationMessage struct {
	Message
	TemplateName string
	CustomerName string
}

// AutomationSendCounts summarises an automation's history for the page header,
// so a merchant can see "40 sent, 2 failed" without scanning the rows.
type AutomationSendCounts struct {
	Total     int
	Sent      int
	Delivered int
	Read      int
	Failed    int
	Queued    int
}

// MetaError mirrors a webhook failure element for the dashboard/API.
type MetaError struct {
	Code  int    `json:"code"`
	Title string `json:"title"`
}

// Messages lists a merchant's messages, newest first.
func (s *Service) Messages(ctx context.Context, shopID uuid.UUID) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, shop_id, customer_id, template_id, converty_order_id,
		       recipient_phone, meta_message_id, status, error_code, error_message,
		       meta_errors, template_variables, COALESCE(body_text, ''),
		       sent_at, delivered_at, read_at, failed_at, created_at
		FROM messages
		WHERE shop_id = $1
		ORDER BY created_at DESC
		LIMIT 500`,
		shopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message returns one merchant message scoped to its shop.
func (s *Service) Message(ctx context.Context, shopID, messageID uuid.UUID) (Message, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, customer_id, template_id, converty_order_id,
		       recipient_phone, meta_message_id, status, error_code, error_message,
		       meta_errors, template_variables, COALESCE(body_text, ''),
		       sent_at, delivered_at, read_at, failed_at, created_at
		FROM messages
		WHERE id = $1 AND shop_id = $2`,
		messageID, shopID,
	)
	return scanMessage(row)
}

type messageScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row messageScanner) (Message, error) {
	var m Message
	var metaErrors []byte
	var vars []byte
	err := row.Scan(
		&m.ID, &m.ShopID, &m.CustomerID, &m.TemplateID, &m.ConvertyOrderID,
		&m.RecipientPhone, &m.MetaMessageID, &m.Status, &m.ErrorCode, &m.ErrorMessage,
		&metaErrors, &vars, &m.BodyText, &m.SentAt, &m.DeliveredAt, &m.ReadAt, &m.FailedAt, &m.CreatedAt,
	)
	if err != nil {
		return Message{}, err
	}
	if len(metaErrors) > 0 {
		_ = json.Unmarshal(metaErrors, &m.MetaErrors)
	}
	if len(vars) > 0 && string(vars) != "{}" && string(vars) != "null" {
		_ = json.Unmarshal(vars, &m.TemplateVariables)
	}
	if m.TemplateVariables == nil {
		m.TemplateVariables = map[string]string{}
	}
	return m, nil
}

// MessagesForAutomation lists the messages one automation produced, newest
// first. The template and customer names are joined in rather than fetched per
// row: this page is a list of hundreds of rows and a lookup each would be
// hundreds of extra queries. Messages sent before the automation link existed
// are attributed by the migration's backfill where the idempotency key proves
// which automation produced them; anything else stays off this list.
func (s *Service) MessagesForAutomation(ctx context.Context, automationID uuid.UUID, limit int) ([]AutomationMessage, error) {
	return messagesForAutomation(ctx, s.pool, automationID, limit)
}

func messagesForAutomation(ctx context.Context, db database.Querier, automationID uuid.UUID, limit int) ([]AutomationMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Query(ctx, `
		SELECT m.id, m.shop_id, m.customer_id, m.template_id, m.converty_order_id,
		       m.recipient_phone, m.meta_message_id, m.status, m.error_code, m.error_message,
		       m.meta_errors, m.template_variables, COALESCE(m.body_text, ''),
		       m.sent_at, m.delivered_at, m.read_at, m.failed_at, m.created_at,
		       m.automation_id, COALESCE(t.meta_template_name, ''), COALESCE(c.name, '')
		FROM messages m
		LEFT JOIN templates t ON t.id = m.template_id
		LEFT JOIN customers c ON c.id = m.customer_id
		WHERE m.automation_id = $1
		ORDER BY m.created_at DESC
		LIMIT $2`,
		automationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AutomationMessage{}
	for rows.Next() {
		var row AutomationMessage
		var metaErrors []byte
		vars := []byte("{}")
		if err := rows.Scan(
			&row.ID, &row.ShopID, &row.CustomerID, &row.TemplateID, &row.ConvertyOrderID,
			&row.RecipientPhone, &row.MetaMessageID, &row.Status, &row.ErrorCode, &row.ErrorMessage,
			&metaErrors, &vars, &row.BodyText, &row.SentAt, &row.DeliveredAt, &row.ReadAt,
			&row.FailedAt, &row.CreatedAt,
			&row.AutomationID, &row.TemplateName, &row.CustomerName,
		); err != nil {
			return nil, err
		}
		if len(metaErrors) > 0 {
			_ = json.Unmarshal(metaErrors, &row.MetaErrors)
		}
		if len(vars) > 0 && string(vars) != "{}" && string(vars) != "null" {
			_ = json.Unmarshal(vars, &row.TemplateVariables)
		}
		if row.TemplateVariables == nil {
			row.TemplateVariables = map[string]string{}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AutomationSendCounts tallies an automation's history by status, so a merchant
// sees "40 sent, 2 failed" without scanning the rows.
func (s *Service) AutomationSendCounts(ctx context.Context, automationID uuid.UUID) (AutomationSendCounts, error) {
	return automationSendCounts(ctx, s.pool, automationID)
}

func automationSendCounts(ctx context.Context, db database.Querier, automationID uuid.UUID) (AutomationSendCounts, error) {
	var c AutomationSendCounts
	err := db.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE status = 'sent'),
			count(*) FILTER (WHERE status = 'delivered'),
			count(*) FILTER (WHERE status = 'read'),
			count(*) FILTER (WHERE status = 'failed'),
			count(*) FILTER (WHERE status = 'queued')
		FROM messages
		WHERE automation_id = $1`,
		automationID,
	).Scan(&c.Total, &c.Sent, &c.Delivered, &c.Read, &c.Failed, &c.Queued)
	if err != nil {
		return c, err
	}
	return c, nil
}

// MessageExistsForIdempotencyKey reports whether a send for this key has already
// been recorded. A retry deliberately reuses the original key so a message that
// did go out is never sent twice; this is how the caller learns the retry
// actually landed rather than silently re-recording it.
func (s *Service) MessageExistsForIdempotencyKey(ctx context.Context, shopID uuid.UUID, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages WHERE shop_id = $1 AND idempotency_key = $2
		)`, shopID, key).Scan(&exists)
	return exists, err
}