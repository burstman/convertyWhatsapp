package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/auth"
	vadmin "whatsappconverty/web/views/admin"
	viewshared "whatsappconverty/web/views/components"
)

func adminConsolePage(principal auth.Auth, title, active string) viewshared.Page {
	return viewshared.Page{
		Title:    title,
		Active:   active,
		ShopName: "Platform",
		UserName: principal.User.Name,
		IsAdmin:  true,
	}
}

// whatsappFormView builds the credential editor view model from the current
// store snapshot. Secrets are represented only by a "configured" flag.
func (a *App) whatsappFormView(noticeKind, noticeText string) vadmin.WhatsappForm {
	m := a.Meta.Get()
	return vadmin.WhatsappForm{
		PhoneNumberID:       m.PhoneNumberID,
		MessagingAccountID:  m.MessagingAccountID,
		WaacID:              m.WaacID,
		BusinessPortfolioID: m.BusinessPortfolioID,
		PhoneNumber:         m.PhoneNumber,
		VerifiedName:        m.VerifiedName,
		SystemUserTokenSet:  m.SystemUserToken != "",
		WebhookTokenSet:     m.WebhookVerifyToken != "",
		AppSecretSet:        m.AppSecret != "",
		NoticeKind:          noticeKind,
		NoticeText:          noticeText,
	}
}

func whatsappFlashMessage(flash string) (kind, text string) {
	switch flash {
	case "saved":
		return "ok", "Credentials saved."
	case "error":
		return "error", "Could not save credentials. Please try again."
	default:
		return "", ""
	}
}

// handleAdminWhatsappConfig renders the admin-only editor for the platform's
// central Meta credentials. Non-admins get the 404 page.
func (a *App) handleAdminWhatsappConfig(k *kit.Kit) error {
	principal := auth.FromKit(k)
	if !principal.IsAdmin() {
		return a.handleNotFound(k)
	}
	kind, text := whatsappFlashMessage(k.Request.URL.Query().Get("flash"))
	form := a.whatsappFormView(kind, text)
	return k.Render(vadmin.WhatsappConfig(adminConsolePage(principal, "WhatsApp credentials", "admin"), form))
}

// handleAdminWhatsappConfigSave persists the submitted credentials. A blank
// secret field keeps the existing value, so the operator can edit public fields
// without re-entering secrets. Values apply to subsequent sends immediately.
func (a *App) handleAdminWhatsappConfigSave(k *kit.Kit) error {
	principal := auth.FromKit(k)
	if !principal.IsAdmin() {
		return a.handleNotFound(k)
	}

	v := a.Meta.Get()
	v.PhoneNumberID = strings.TrimSpace(k.Request.FormValue("phone_number_id"))
	v.MessagingAccountID = strings.TrimSpace(k.Request.FormValue("messaging_account_id"))
	v.WaacID = strings.TrimSpace(k.Request.FormValue("waac_id"))
	v.BusinessPortfolioID = strings.TrimSpace(k.Request.FormValue("business_portfolio_id"))
	v.PhoneNumber = strings.TrimSpace(k.Request.FormValue("phone_number"))
	v.VerifiedName = strings.TrimSpace(k.Request.FormValue("verified_name"))
	if s := strings.TrimSpace(k.Request.FormValue("system_user_token")); s != "" {
		v.SystemUserToken = s
	}
	if s := strings.TrimSpace(k.Request.FormValue("webhook_verify_token")); s != "" {
		v.WebhookVerifyToken = s
	}
	if s := strings.TrimSpace(k.Request.FormValue("app_secret")); s != "" {
		v.AppSecret = s
	}

	if v.SystemUserToken == "" || v.PhoneNumberID == "" || v.MessagingAccountID == "" {
		form := a.whatsappFormView("error", "System user token, phone number ID and messaging account ID are required.")
		form.PhoneNumberID = v.PhoneNumberID
		form.MessagingAccountID = v.MessagingAccountID
		form.WaacID = v.WaacID
		form.BusinessPortfolioID = v.BusinessPortfolioID
		form.PhoneNumber = v.PhoneNumber
		form.VerifiedName = v.VerifiedName
		return k.Render(vadmin.WhatsappConfig(adminConsolePage(principal, "WhatsApp credentials", "admin"), form))
	}

	if err := a.Meta.Save(k.Request.Context(), v); err != nil {
		a.Log.Error("meta credentials save failed", "admin", principal.User.Email, "error", err)
		return k.Redirect(http.StatusSeeOther, "/admin/whatsapp?flash=error")
	}
	a.Log.Info("meta credentials updated", "admin", principal.User.Email)
	return k.Redirect(http.StatusSeeOther, "/admin/whatsapp?flash=saved")
}

// handleAdminWhatsappVerify checks the saved credentials against the Graph API
// by resolving the configured phone number.
func (a *App) handleAdminWhatsappVerify(k *kit.Kit) error {
	principal := auth.FromKit(k)
	if !principal.IsAdmin() {
		return a.handleNotFound(k)
	}

	form := a.whatsappFormView("", "")
	pn, err := a.WhatsApp.VerifyCredentials(k.Request.Context())
	if err != nil {
		form.NoticeKind = "error"
		form.NoticeText = "Verification failed: " + err.Error()
	} else {
		form.NoticeKind = "ok"
		form.NoticeText = fmt.Sprintf("OK — %s (%s), quality %s", pn.DisplayPhoneNumber, pn.VerifiedName, pn.QualityRating)
	}
	return k.Render(vadmin.WhatsappConfig(adminConsolePage(principal, "WhatsApp credentials", "admin"), form))
}
