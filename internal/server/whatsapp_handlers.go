package server

import (
	"net/http"

	"github.com/anthdm/superkit/kit"
	"github.com/jackc/pgx/v5"

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

// handleWhatsappSettings renders the (owner-only) WhatsApp settings page. This
// is a legacy view that predates the shared single-WABA model: template
// listings belong to the shop-scoped /templates page only. A shop must never
// see the platform's account-level templates (other merchants' content), so no
// template list is rendered here.
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
		Title:    "Settings",
		Active:   "settings",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  principal.User.IsOwner(),
	}

	integ, err := a.WhatsApp.Integration(k.Request.Context(), principal.User.ShopID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return k.Render(vsettings.Settings(page, nil, nil))
		}
		return err
	}

	return k.Render(vsettings.Settings(page, &integ, nil))
}

// handleWhatsappConnect stores the shop's Meta WhatsApp credentials after a
// live sanity check of the token and ids against the Graph API. Owner-only.
func (a *App) handleWhatsappConnect(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)

	ctx := k.Request.Context()
	token := k.Request.FormValue("token")
	phoneNumberID := k.Request.FormValue("phone_number_id")
	messagingAccountID := k.Request.FormValue("messaging_account_id")
	phoneNumber := k.Request.FormValue("phone_number")

	if token == "" || phoneNumberID == "" || messagingAccountID == "" {
		a.Log.Warn("whatsapp connect rejected: missing fields")
		return k.Redirect(http.StatusSeeOther, "/settings?connect=error")
	}

	// Verify the credentials are live before persisting anything.
	pn, err := a.WhatsApp.GetPhoneNumber(ctx, token, phoneNumberID)
	if err != nil {
		a.Log.Warn("whatsapp connect rejected: phone number check failed", "error", err)
		return k.Redirect(http.StatusSeeOther, "/settings?connect=error")
	}
	if phoneNumber == "" {
		phoneNumber = pn.DisplayPhoneNumber
	}

	if err := a.WhatsApp.SaveIntegration(ctx, principal.User.ShopID, token, whatsapp.Integration{
		ShopID:             principal.User.ShopID,
		WaacID:             phoneNumberID,
		PhoneNumberID:      phoneNumberID,
		MessagingAccountID: messagingAccountID,
		PhoneNumber:        phoneNumber,
		Status:             "connected",
	}); err != nil {
		a.Log.Error("whatsapp connect failed to save", "error", err)
		return k.Redirect(http.StatusSeeOther, "/settings?connect=error")
	}

	a.Log.Info("whatsapp connected",
		"shop_id", principal.User.ShopID,
		"phone_number_id", phoneNumberID,
		"messaging_account_id", messagingAccountID,
		"phone", phoneNumber,
		"verified_name", pn.VerifiedName)
	return k.Redirect(http.StatusSeeOther, "/settings?connect=success")
}

// handleWhatsappDisconnect removes the shop's WhatsApp credentials. Owner-only.
func (a *App) handleWhatsappDisconnect(k *kit.Kit) error {
	if err := a.requireOwner(k); err != nil {
		return err
	}

	principal := auth.FromKit(k)

	if err := a.WhatsApp.DeleteIntegration(k.Request.Context(), principal.User.ShopID); err != nil {
		a.Log.Error("whatsapp disconnect failed", "error", err)
		return k.Redirect(http.StatusSeeOther, "/settings?connect=error")
	}
	a.Log.Info("whatsapp disconnected", "shop_id", principal.User.ShopID)
	return k.Redirect(http.StatusSeeOther, "/settings?connect=disconnected")
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
