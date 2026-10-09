package auth

import (
	"github.com/anthdm/superkit/kit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User roles. The shop's first user (created at registration) is the owner;
// the row defaults to 'owner' so existing accounts need no backfill. Owners
// administer the shop — today that means the WhatsApp settings and the
// one-time enable/terms flow. Members get the day-to-day features only.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// User is a merchant account. Users belong to exactly one shop.
type User struct {
	ID       uuid.UUID `json:"id"`
	ShopID   uuid.UUID `json:"shop_id"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Password string    `json:"-"`
}

// IsOwner reports whether the user administers the shop.
func (u User) IsOwner() bool { return u.Role == RoleOwner }

// IsMember reports whether the user is a day-to-day (non-admin) member.
func (u User) IsMember() bool { return u.Role == RoleMember }

// Auth is the authenticated principal for a request. It implements
// superkit's kit.Auth interface and carries the shop context.
type Auth struct {
	LoggedIn bool
	User     User
}

func (a Auth) Check() bool { return a.LoggedIn }

// FromKit returns the authenticated principal (or the zero value).
func FromKit(k *kit.Kit) Auth {
	return k.Auth().(Auth)
}

type Account struct {
	pool *pgxpool.Pool
}

func NewAccount(pool *pgxpool.Pool) *Account {
	return &Account{pool: pool}
}
