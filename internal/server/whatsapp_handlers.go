package server

import (
	"net/http"

	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/shops"
	"whatsappconverty/internal/whatsapp"
	viewshared "whatsappconverty/web/views/components"
	vsettings "whatsappconverty/web/views/settings"
)

// requireOwner restricts a handler to the shop owner. Non-owners receive the
// platform's 404 page so owner-only pages do not reveal that they exist.
func (a *App) requireOwner(k *kit.Kit) error {
	if auth.FromKit(k).User.IsOwner() {
		return nil
	}
	return a.handleNotFound(k)
}

// handleWhatsappStatus renders the client's read-only WhatsApp status: the
// state of the platform's shared service for this shop and the number
// customers will see the message from. It exposes no Meta credentials and
// offers no configuration — the sending identity is centrally owned, so a
// merchant has nothing to tune.
func (a *App) handleWhatsappStatus(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shop, err := a.Shops.GetByID(k.Request.Context(), principal.User.ShopID)
	if err != nil && err != shops.ErrNotFound {
		return err
	}

	page := viewshared.Page{
		Title:    "WhatsApp",
		Active:   "whatsapp",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  principal.User.IsOwner(),
	}
	return k.Render(vsettings.StatusCard(page, shop, a.Cfg.MetaPhoneNumber, a.Cfg.MetaVerifiedName))
}

// handleWhatsappSettings renders the owner-only WhatsApp settings page. It is
// a management view over the centrally owned sending identity: it shows the
// platform number (read-only — merchants never supply Meta credentials), lets
// the owner edit the shop contact phone and disable the service. Templating and
// the shared send path live at /whatsapp (status) and /whatsapp/onboard.
func (a *App) handleWhatsappSettings(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)
	shop, err := a.Shops.GetByID(k.Request.Context(), principal.User.ShopID)
	if err != nil && err != shops.ErrNotFound {
		return err
	}

	page := viewshared.Page{
		Title:    "WhatsApp settings",
		Active:   "settings",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  true,
	}
	flash := k.Request.URL.Query().Get("flash")
	return k.Render(vsettings.SettingsPage(page, shop, flash, a.Cfg.MetaPhoneNumber, a.Cfg.MetaVerifiedName))
}

// handleWhatsappSettingsPhone updates the shop contact phone used on the
// status card. Owner-only.
func (a *App) handleWhatsappSettingsPhone(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)
	phone := k.Request.FormValue("shop_phone")
	if !whatsapp.ValidPhone(phone) {
		a.Log.Warn("whatsapp settings: invalid phone", "shop_id", principal.User.ShopID)
		return k.Redirect(http.StatusSeeOther, "/settings?flash=invalid_phone")
	}

	if err := a.Shops.UpdatePhone(k.Request.Context(), principal.User.ShopID, phone); err != nil {
		a.Log.Error("whatsapp settings phone update failed", "shop_id", principal.User.ShopID, "error", err)
		return k.Redirect(http.StatusSeeOther, "/settings?flash=error")
	}

	a.Log.Info("whatsapp settings phone updated", "shop_id", principal.User.ShopID)
	return k.Redirect(http.StatusSeeOther, "/settings?flash=phone_saved")
}

// handleWhatsappSettingsDisable turns the WhatsApp service off for the shop.
// Owner-only.
func (a *App) handleWhatsappSettingsDisable(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)
	if err := a.Shops.DisableWhatsApp(k.Request.Context(), principal.User.ShopID); err != nil {
		a.Log.Error("whatsapp settings disable failed", "shop_id", principal.User.ShopID, "error", err)
		return k.Redirect(http.StatusSeeOther, "/settings?flash=error")
	}
	a.Log.Info("whatsapp service disabled", "shop_id", principal.User.ShopID)
	return k.Redirect(http.StatusSeeOther, "/settings?flash=disabled")
}

// handleBilling renders the owner-only Billing page: the shop's message usage
// (billable sends) and the estimated spend at the configured per-message price.
func (a *App) handleBilling(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)
	ctx := k.Request.Context()

	shop, err := a.Shops.GetByID(ctx, principal.User.ShopID)
	if err != nil && err != shops.ErrNotFound {
		return err
	}

	usage, err := a.WhatsApp.MessagesUsage(ctx, principal.User.ShopID)
	if err != nil {
		a.Log.Error("billing usage query failed", "shop_id", principal.User.ShopID, "error", err)
		return err
	}

	page := viewshared.Page{
		Title:    "Billing",
		Active:   "billing",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  true,
	}
	return k.Render(vsettings.BillingPage(page, usage, a.Cfg.PricePerMessage))
}

// handleWhatsappOnboard renders the WhatsApp enable page: the owner opts into
// the platform's shared number, records a contact phone, and accepts the
// service terms (which include the customer opt-in obligation). Owner-only:
// accepting the terms is an audited legal commitment.
func (a *App) handleWhatsappOnboard(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)

	shop, err := a.Shops.GetByID(k.Request.Context(), principal.User.ShopID)
	if err != nil && err != shops.ErrNotFound {
		return err
	}

	page := viewshared.Page{
		Title:    "Enable WhatsApp",
		Active:   "settings",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  principal.User.IsOwner(),
	}
	return k.Render(vsettings.Onboarding(page, shop))
}

// handleWhatsappOnboardPost records the owner's opt-in: terms accepted, service
// enabled, contact phone saved. No Meta credentials are handled here — sending
// happens through the platform's centrally owned number. Owner-only.
func (a *App) handleWhatsappOnboardPost(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)

	if k.Request.FormValue("accept_terms") != "1" {
		return k.Redirect(http.StatusSeeOther, "/whatsapp/onboard?error=terms")
	}
	phone := k.Request.FormValue("shop_phone")

	if err := a.Shops.EnableWhatsApp(k.Request.Context(), principal.User.ShopID, phone); err != nil {
		a.Log.Error("whatsapp onboard enable failed", "shop_id", principal.User.ShopID, "error", err)
		return k.Redirect(http.StatusSeeOther, "/whatsapp/onboard?error=enable")
	}

	a.Log.Info("whatsapp service enabled", "shop_id", principal.User.ShopID, "phone", phone)
	return k.Redirect(http.StatusSeeOther, "/whatsapp?enabled=1")
}
