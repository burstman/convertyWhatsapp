package whatsapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/encrypt"
)

// ErrNotConfigured is returned when no WhatsApp integration exists for the
// shop (or its token cannot be decrypted).
var ErrNotConfigured = errors.New("whatsapp is not configured")

// IntegrationChecker reports whether a Converty integration is currently
// active. The delivery layer consults it through the single send-time
// integration gate; *automations.Processor implements it.
type IntegrationChecker interface {
	IntegrationActive(ctx context.Context, id uuid.UUID) (bool, error)
}

// Service talks to the Meta Graph API and enforces the platform's
// single-WABA sending rules. Meta credentials are held centrally in config;
// per-shop rows in whatsapp_integrations are legacy (Phase-1 token-paste).
type Service struct {
	cfg          config.Config
	pool         *pgxpool.Pool
	log          *slog.Logger
	cipher       *encrypt.Cipher
	http         *http.Client
	rate         *RateLimiter
	integrations IntegrationChecker
	auditor      TestSendAuditor
}

func NewService(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *Service {
	cipher, err := encrypt.New(cfg.EncryptionKeyResolved())
	if err != nil {
		log.Error("whatsapp encryption key invalid; tokens will not persist", "error", err)
	}
	return &Service{
		cfg:     cfg,
		pool:    pool,
		log:     log,
		cipher:  cipher,
		http:    &http.Client{Timeout: 20 * time.Second},
		rate:    NewRateLimiter(pool, log).WithTestSendLimit(cfg.TestSendLimit, cfg.TestSendWindow),
		auditor: NewPostgresTestSendAuditor(pool),
	}
}

// SetTestSendAuditor replaces the test-send audit sink. A nil sink makes audit
// writes no-ops, which keeps tests and misconfigured local runs from failing a
// send over the audit trail.
func (s *Service) SetTestSendAuditor(a TestSendAuditor) {
	s.auditor = a
}

// RecordTestSendAudit appends one test-send attempt. The caller treats the
// outcome as authoritative even if this write fails, so audit errors are
// returned for logging rather than retried into the send path.
func (s *Service) RecordTestSendAudit(ctx context.Context, e TestSendAudit) error {
	if s.auditor == nil {
		return nil
	}
	return s.auditor.RecordTestSend(ctx, e)
}

// AllowTestSend charges one send against the dedicated per-actor, per-shop
// test-send budget. It is the single choke point every test send passes through
// before any Meta call is made.
func (s *Service) AllowTestSend(ctx context.Context, shopID, actorID uuid.UUID) error {
	return s.rate.AllowTestSend(ctx, shopID, actorID)
}

// SetIntegrationChecker installs the Converty integration-state source used by
// the delivery-layer integration gate. Until it is set, the gate fails closed
// for integration-scoped sends (it never silently permits one).
func (s *Service) SetIntegrationChecker(c IntegrationChecker) {
	s.integrations = c
}

// DecryptToken unwraps an encrypted Meta token stored for a shop.
func (s *Service) DecryptToken(encrypted string) (string, error) {
	if s.cipher == nil {
		return "", errors.New("whatsapp encryption not configured")
	}
	return s.cipher.Decrypt(encrypted)
}

// Token returns the shop's decrypted Meta access token from the stored
// integration. A system-user token does not expire or rotate, so no refresh
// logic is needed (unlike the Converty OAuth flow).
func (s *Service) Token(ctx context.Context, shopID uuid.UUID) (string, error) {
	integ, err := s.Integration(ctx, shopID)
	if err != nil {
		return "", err
	}
	token, err := s.DecryptToken(integ.AccessTokenEncrypted)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNotConfigured
	}
	return token, nil
}
