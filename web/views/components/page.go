package components

// Page carries the common data rendered on every dashboard page.
type Page struct {
	Title    string
	Active   string
	ShopName string
	UserName string
	// IsOwner lets the shell render owner-only navigation (the WhatsApp
	// settings link) without the handlers duplicating the role check.
	IsOwner bool
}

func (p Page) ActiveClass(section string) string {
	if p.Active == section {
		return "bg-indigo-600 text-white"
	}
	return "text-slate-300 hover:bg-slate-800 hover:text-white"
}
