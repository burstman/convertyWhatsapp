package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/database"
)

// Test-send outcomes. They separate "Meta accepted the API request for
// processing" from a confirmed delivery: nothing here observes delivery, only
// whether the platform handed the message to Meta.
const (
	TestSendOutcomeAccepted    = "accepted"
	TestSendOutcomeRejected    = "rejected"
	TestSendOutcomeFailed      = "failed"
	TestSendOutcomeRateLimited = "rate_limited"
)

// TestSendAudit is one authorized test-send attempt. It records who did it, for
// which shop and automation, against which template, and how it ended — without
// ever storing the recipient number or the message body.
type TestSendAudit struct {
	ShopID        uuid.UUID
	ActorID       uuid.UUID
	AutomationID  uuid.UUID
	TemplateID    uuid.UUID
	RecipientHash string
	Outcome       string
	CreatedAt     time.Time
}

// TestSendAuditor persists test-send audit entries. It is an interface so the
// send flow can be exercised without a database.
type TestSendAuditor interface {
	RecordTestSend(ctx context.Context, e TestSendAudit) error
}

// PostgresTestSendAuditor writes audit rows to test_send_audit.
type PostgresTestSendAuditor struct {
	db database.Querier
}

// NewPostgresTestSendAuditor builds an auditor over an existing database handle.
func NewPostgresTestSendAuditor(db database.Querier) *PostgresTestSendAuditor {
	return &PostgresTestSendAuditor{db: db}
}

// RecordTestSend appends one attempt. Zero UUIDs are stored as NULL so the
// audit survives a later deletion of the referenced automation or template.
func (a *PostgresTestSendAuditor) RecordTestSend(ctx context.Context, e TestSendAudit) error {
	_, err := a.db.Exec(ctx, `
		INSERT INTO test_send_audit
			(shop_id, actor_id, automation_id, template_id, recipient_hash, outcome, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		nullUUID(e.ShopID), nullUUID(e.ActorID), nullUUID(e.AutomationID),
		nullUUID(e.TemplateID), e.RecipientHash, e.Outcome, e.CreatedAt)
	return err
}

// HashRecipient returns a stable, keyed, non-reversible token for a recipient
// number so the audit can correlate attempts without keeping the number. The
// shop id is folded in, so the same number under two shops hashes differently
// and a cross-tenant reader cannot compare them. The key must be a server
// secret; the digest is a keyed hash, not a plain one, so an attacker with the
// audit table alone cannot brute-force the small phone-number space.
func HashRecipient(secret string, shopID uuid.UUID, phone string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(shopID.String()))
	mac.Write([]byte{0})
	mac.Write([]byte(normalizePhone(phone)))
	return hex.EncodeToString(mac.Sum(nil))
}

// normalizePhone strips the surrounding whitespace a form field may carry. It
// deliberately does not otherwise canonicalize: the hash is an audit correlator,
// not a matching key.
func normalizePhone(phone string) string {
	return strings.TrimSpace(phone)
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
