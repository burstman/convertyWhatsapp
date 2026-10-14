package auth

import (
	"github.com/anthdm/superkit/kit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Roles for the new admin/client model.
const (
	RoleAdmin  = "admin"
	RoleClient = "client"
)

// Account statuses for clients.
const (
	StatusActive    = "active"
	StatusPending   = "pending"
	StatusSuspended = "suspended"
)

// User represents an authenticated account.
type User struct {
	ID       uuid.UUID `json:"id"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Status   string    `json:"status"`
	Password string    `json:"-"`
}

// ActiveShop is the shop currently selected by the client.
type ActiveShop struct {
	ShopID   uuid.UUID
	ShopName string
	Enabled  bool
}

func (u User) IsAdmin() bool  { return u.Role == RoleAdmin }
func (u User) IsClient() bool { return u.Role == RoleClient }
func (u User) CanLogin() bool {
	if u.Role == RoleAdmin {
		return u.Status == "" || u.Status == StatusActive
	}
	return u.Status == StatusActive
}

// Auth is the authenticated principal for a request.
type Auth struct {
	LoggedIn   bool
	User       User
	ActiveShop ActiveShop
}

func (a Auth) Check() bool { return a.LoggedIn }
func (a Auth) IsAdmin() bool {
	return a.LoggedIn && a.User.IsAdmin()
}
func (a Auth) IsClient() bool {
	return a.LoggedIn && a.User.IsClient()
}

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
