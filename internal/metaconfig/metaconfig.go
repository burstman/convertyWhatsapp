// Package metaconfig holds the platform's central WhatsApp/Meta credentials and
// lets a platform admin change them at runtime. Values are persisted in the
// app_settings table; secrets are encrypted at rest (AES-256-GCM). The Store
// keeps an in-memory snapshot so reads on the send path never touch the
// database, and swaps it atomically when the admin saves.
package metaconfig

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/encrypt"
)

// Values is a snapshot of the platform's central WhatsApp credentials.
type Values struct {
	SystemUserToken     string
	PhoneNumberID       string
	MessagingAccountID  string
	WaacID              string
	BusinessPortfolioID string
	PhoneNumber         string
	VerifiedName        string
	WebhookVerifyToken  string
	AppSecret           string
}

// app_settings keys backing each field.
const (
	keySystemUserToken     = "meta.system_user_token"
	keyPhoneNumberID       = "meta.phone_number_id"
	keyMessagingAccountID  = "meta.messaging_account_id"
	keyWaacID              = "meta.waac_id"
	keyBusinessPortfolioID = "meta.business_portfolio_id"
	keyPhoneNumber         = "meta.phone_number"
	keyVerifiedName        = "meta.verified_name"
	keyWebhookVerifyToken  = "meta.webhook_verify_token"
	keyAppSecret           = "meta.app_secret"
)

// secretKeys are encrypted at rest; everything else is stored as plain text.
var secretKeys = map[string]bool{
	keySystemUserToken:    true,
	keyWebhookVerifyToken: true,
	keyAppSecret:          true,
}

// Store is the process-wide credential holder. It is safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	pool   *pgxpool.Pool
	cipher *encrypt.Cipher
	values Values
}

// DefaultsFromConfig derives the initial values from the environment, so a
// deployment that has not been configured through the console keeps working
// from its META_* variables.
func DefaultsFromConfig(cfg config.Config) Values {
	return Values{
		SystemUserToken:     cfg.MetaSystemUserToken,
		PhoneNumberID:       cfg.MetaPhoneNumberID,
		MessagingAccountID:  cfg.MetaMessagingAccountID,
		WaacID:              cfg.MetaWaacID,
		BusinessPortfolioID: cfg.MetaBusinessPortfolioID,
		PhoneNumber:         cfg.MetaPhoneNumber,
		VerifiedName:        cfg.MetaVerifiedName,
		WebhookVerifyToken:  cfg.MetaWebhookVerifyToken,
		AppSecret:           cfg.MetaAppSecret,
	}
}

// Static returns a read-only store seeded with the given values. Load and Save
// are no-ops, which lets scripts and tests run without a database.
func Static(v Values) *Store {
	return &Store{values: v}
}

// NewStore builds a database-backed store seeded with the environment defaults.
func NewStore(pool *pgxpool.Pool, key []byte, defaults Values) (*Store, error) {
	cipher, err := encrypt.New(key)
	if err != nil {
		return nil, fmt.Errorf("metaconfig: encryption: %w", err)
	}
	return &Store{pool: pool, cipher: cipher, values: defaults}, nil
}

// Get returns the current snapshot.
func (s *Store) Get() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values
}

// Load overrides the snapshot with any values stored in app_settings. Missing
// keys keep the environment default. A secret that cannot be decrypted is
// skipped (the default is kept) rather than failing the whole load.
func (s *Store) Load(ctx context.Context) error {
	if s.pool == nil {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM app_settings WHERE key LIKE 'meta.%'`)
	if err != nil {
		return fmt.Errorf("metaconfig: load: %w", err)
	}
	defer rows.Close()

	v := s.Get()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		val := raw
		if secretKeys[key] {
			dec, err := s.cipher.Decrypt(raw)
			if err != nil {
				continue
			}
			val = dec
		}
		applyKey(&v, key, val)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.values = v
	s.mu.Unlock()
	return nil
}

// Save persists every field and atomically swaps the in-memory snapshot so the
// new credentials apply immediately to subsequent sends.
func (s *Store) Save(ctx context.Context, v Values) error {
	if s.pool == nil {
		return fmt.Errorf("metaconfig: no database configured")
	}
	pairs := map[string]string{
		keySystemUserToken:     v.SystemUserToken,
		keyPhoneNumberID:       v.PhoneNumberID,
		keyMessagingAccountID:  v.MessagingAccountID,
		keyWaacID:              v.WaacID,
		keyBusinessPortfolioID: v.BusinessPortfolioID,
		keyPhoneNumber:         v.PhoneNumber,
		keyVerifiedName:        v.VerifiedName,
		keyWebhookVerifyToken:  v.WebhookVerifyToken,
		keyAppSecret:           v.AppSecret,
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("metaconfig: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for key, value := range pairs {
		stored := value
		if secretKeys[key] {
			enc, err := s.cipher.Encrypt(value)
			if err != nil {
				return fmt.Errorf("metaconfig: encrypt %s: %w", key, err)
			}
			stored = enc
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO app_settings (key, value, updated_at)
			VALUES ($1, $2, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			key, stored); err != nil {
			return fmt.Errorf("metaconfig: save %s: %w", key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("metaconfig: commit: %w", err)
	}

	s.mu.Lock()
	s.values = v
	s.mu.Unlock()
	return nil
}

func applyKey(v *Values, key, val string) {
	switch key {
	case keySystemUserToken:
		v.SystemUserToken = val
	case keyPhoneNumberID:
		v.PhoneNumberID = val
	case keyMessagingAccountID:
		v.MessagingAccountID = val
	case keyWaacID:
		v.WaacID = val
	case keyBusinessPortfolioID:
		v.BusinessPortfolioID = val
	case keyPhoneNumber:
		v.PhoneNumber = val
	case keyVerifiedName:
		v.VerifiedName = val
	case keyWebhookVerifyToken:
		v.WebhookVerifyToken = val
	case keyAppSecret:
		v.AppSecret = val
	}
}
