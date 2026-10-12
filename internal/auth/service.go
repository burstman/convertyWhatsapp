package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"whatsappconverty/internal/database"
	"whatsappconverty/internal/shops"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailTaken         = errors.New("email already registered")
	ErrInviteInvalid      = errors.New("invitation is invalid or expired")
)

const (
	SessionCookieName = "user-session"
	sessionLifetime   = 30 * 24 * time.Hour
	inviteLifetime    = 7 * 24 * time.Hour
)

type Service struct {
	pool  *pgxpool.Pool
	shops shops.Repository
	now   func() time.Time
	rand  func([]byte) (int, error)
}

type RegisterInput struct {
	ShopName string
	UserName string
	Email    string
	Password string
}

func NewService(pool *pgxpool.Pool, shops shops.Repository) *Service {
	return &Service{
		pool:  pool,
		shops: shops,
		now:   time.Now,
	}
}

// Register creates a shop, its owner as the first user, and a session in a
// single transaction. It returns the session token that must be stored in the
// session cookie.
func (s *Service) Register(ctx context.Context, in RegisterInput) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	shop, err := s.shops.Create(ctx, tx, in.ShopName)
	if err != nil {
		return "", fmt.Errorf("create shop: %w", err)
	}

	userID, err := insertUser(ctx, tx, shop.ID, in.Email, string(hash), in.UserName)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrEmailTaken
		}
		return "", fmt.Errorf("insert user: %w", err)
	}

	token, err := s.createSession(ctx, tx, userID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return token, nil
}

// Login verifies credentials and returns a session token for the user.
func (s *Service) Login(ctx context.Context, email, password string) (string, error) {
	var (
		user         User
		passwordHash string
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, email, name, role, password_hash
		FROM users
		WHERE email = lower($1)`,
		email,
	).Scan(&user.ID, &user.ShopID, &user.Email, &user.Name, &user.Role, &passwordHash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidCredentials
		}
		return "", fmt.Errorf("query user: %w", err)
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		return "", ErrInvalidCredentials
	}

	user.Password = ""
	token, err := s.createSession(ctx, s.pool, user.ID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// Logout invalidates the session token server-side.
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE token = $1`, token)
	return err
}

// InviteMember creates a pending 'member' account for an existing shop. The
// owner shares the returned one-time invite token (a /invite/{token} link);
// only its SHA-256 hash is stored. The member is activated — and gets a
// password — when RedeemInvite succeeds on that link.
func (s *Service) InviteMember(ctx context.Context, shopID uuid.UUID, name, email string) (string, error) {
	raw, err := randomToken(24)
	if err != nil {
		return "", fmt.Errorf("generate invite token: %w", err)
	}

	tokenHash := inviteTokenHash(raw)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO users (shop_id, email, password_hash, name, role, invite_token_hash, invite_expires_at)
		VALUES ($1, lower($2), '', $3, 'member', $4, $5)`,
		shopID, email, name, tokenHash, s.now().Add(inviteLifetime),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrEmailTaken
		}
		return "", fmt.Errorf("insert member: %w", err)
	}
	return raw, nil
}

// RedeemInvite activates a pending member and returns a session token: the
// invite link must match a stored hash, must not have expired, and must not
// have been used before. On success the invite fields are cleared and the
// invitee's password is set for the first time.
func (s *Service) RedeemInvite(ctx context.Context, token, password string) (string, error) {
	tokenHash := inviteTokenHash(token)

	var userID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM users
		WHERE invite_token_hash = $1 AND invite_expires_at > now()`,
		tokenHash,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInviteInvalid
	}
	if err != nil {
		return "", fmt.Errorf("query invite: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, invite_token_hash = NULL, invite_expires_at = NULL, updated_at = now()
		WHERE id = $1 AND invite_token_hash = $3`,
		userID, string(hash), tokenHash,
	)
	if err != nil {
		return "", fmt.Errorf("redeem invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrInviteInvalid
	}

	return s.createSession(ctx, s.pool, userID)
}

// TeamMember is one row on the owner's /team page.
type TeamMember struct {
	ID      uuid.UUID
	Email   string
	Name    string
	Role    string
	Pending bool
}

// UsersByShop lists the shop's users for the owner's team management page.
func (s *Service) UsersByShop(ctx context.Context, shopID uuid.UUID) ([]TeamMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, name, role, password_hash
		FROM users
		WHERE shop_id = $1
		ORDER BY created_at ASC`,
		shopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		var hash string
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &hash); err != nil {
			return nil, err
		}
		m.Pending = m.Role == RoleMember && hash == ""
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMember removes a member account from a shop. Owners can never be
// removed this way — a shop always keeps its admin.
func (s *Service) DeleteMember(ctx context.Context, shopID, userID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM users
		WHERE id = $1 AND shop_id = $2 AND role = 'member'`,
		userID, shopID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// ErrNotMember reports that no member with that id exists for the shop.
var ErrNotMember = errors.New("no member found")

func inviteTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Service) createSession(ctx context.Context, q database.Querier, userID uuid.UUID) (string, error) {
	random, err := randomToken(32)
	if err != nil {
		return "", err
	}
	token := random
	expires := s.now().Add(sessionLifetime)

	query := `
		INSERT INTO auth_sessions (user_id, token, expires_at)
		VALUES ($1, $2, $3)`
	if _, err := q.Exec(ctx, query, userID, token, expires); err != nil {
		return "", err
	}
	return token, nil
}

func insertUser(ctx context.Context, q database.Querier, shopID uuid.UUID, email, hash, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO users (shop_id, email, password_hash, name)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		shopID, email, hash, name,
	).Scan(&id)
	return id, err
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
