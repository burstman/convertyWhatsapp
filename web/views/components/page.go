package components

import "whatsappconverty/internal/i18n"

// Page carries the common data rendered on every dashboard page.
type Page struct {
	Title    string
	Active   string
	ShopName string
	UserName string
	// IsOwner is true for a client that owns the active shop. It gates the
	// shop-management controls (onboarding, automation editing, Settings,
	// Billing).
	IsOwner bool
	// IsAdmin is true for a platform admin, who has no shop and instead gets
	// the admin console.
	IsAdmin bool
	// UnreadInbox is the total unread inbox conversations shown on the nav
	// badge.
	UnreadInbox int
	// I18N is the language table for the page. A nil table renders English.
	I18N *i18n.Dict
}

// T translates a key in the page language. A page rendered without an injected
// table (or with an unknown key) falls back to English rather than leaking a
// bare key.
func (p Page) T(key string) string {
	if p.I18N == nil {
		return i18n.New(i18n.En).T(key)
	}
	return p.I18N.T(key)
}

// Tf translates and formats a key with count-style arguments.
func (p Page) Tf(key string, args ...any) string {
	if p.I18N == nil {
		return i18n.New(i18n.En).Tf(key, args...)
	}
	return p.I18N.Tf(key, args...)
}

func (p Page) ActiveClass(section string) string {
	if p.Active == section {
		return "bg-indigo-600 text-white"
	}
	return "text-slate-300 hover:bg-slate-800 hover:text-white"
}
