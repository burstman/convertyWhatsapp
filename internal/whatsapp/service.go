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

// Service talks to the Meta Graph API and enforces the platform's
// single-WABA sending rules. Meta credentials are held centrally in config;
// per-shop rows in whatsapp_integrations are legacy (Phase-1 token-paste).
type Service struct {
	cfg    config.Config
	pool   *pgxpool.Pool
	log    *slog.Logger
	cipher *encrypt.Cipher
	http   *http.Client
	rate   *RateLimiter
}

func NewService(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *Service {
	cipher, err := encrypt.New(cfg.EncryptionKeyResolved())
	if err != nil {
		log.Error("whatsapp encryption key invalid; tokens will not persist", "error", err)
	}
	return &Service{
		cfg:    cfg,
		pool:   pool,
		log:    log,
		cipher: cipher,
		http:   &http.Client{Timeout: 20 * time.Second},
		rate:   NewRateLimiter(pool, log),
	}
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
