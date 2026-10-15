package components

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
}

func (p Page) ActiveClass(section string) string {
	if p.Active == section {
		return "bg-indigo-600 text-white"
	}
	return "text-slate-300 hover:bg-slate-800 hover:text-white"
}
