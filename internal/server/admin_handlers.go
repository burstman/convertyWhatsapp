package server

import (
	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/auth"
	vadmin "whatsappconverty/web/views/admin"
	viewshared "whatsappconverty/web/views/components"
)

// handleAdminConsole renders the platform-operator console. Non-admins get the
// 404 page (the console's existence is not revealed to clients).
func (a *App) handleAdminConsole(k *kit.Kit) error {
	principal := auth.FromKit(k)
	if !principal.IsAdmin() {
		return a.handleNotFound(k)
	}

	overview, err := a.Admin.Overview(k.Request.Context())
	if err != nil {
		a.Log.Error("admin overview failed", "error", err)
		return err
	}

	page := viewshared.Page{
		Title:    "Admin",
		Active:   "admin",
		ShopName: "Platform",
		UserName: principal.User.Name,
		IsAdmin:  true,
	}
	return k.Render(vadmin.Index(page, overview))
}
