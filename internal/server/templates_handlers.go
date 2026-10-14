package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/whatsapp"
)

// handleTemplateRefresh re-syncs the merchant's template approval statuses
// from Meta's review engine and returns to the templates page.
func (a *App) handleTemplateRefresh(k *kit.Kit) error {
	if purged, err := a.WhatsApp.PurgeExpiredMarketing(k.Request.Context()); err != nil {
		a.Log.Warn("marketing purge check failed", "error", err.Error())
	} else if purged > 0 {
		a.Log.Info("expired marketing templates purged", "count", purged)
	}

	updated, err := a.WhatsApp.SyncTemplates(k.Request.Context())
	if err != nil {
		a.Log.Warn("template refresh failed", "error", err.Error())
		return k.Redirect(http.StatusSeeOther, "/templates?flash=refresh")
	}
	a.Log.Info("templates refreshed from meta", "updated", updated)
	return k.Redirect(http.StatusSeeOther, "/templates?flash=synced")
}

// handleTemplateCreate submits a merchant-authored template to Meta's review
// through the platform's central account. Only UTILITY-category templates are
// accepted here; marketing templates are outside this platform's scope. The
// stored status (pending/rejected/approved) is always Meta's verdict.
func (a *App) handleTemplateCreate(k *kit.Kit) error {
	principal := auth.FromKit(k)

	name := strings.TrimSpace(k.Request.FormValue("name"))
	language := strings.TrimSpace(k.Request.FormValue("language"))
	body := strings.TrimSpace(k.Request.FormValue("body"))
	if name == "" || language == "" || body == "" {
		return k.Redirect(http.StatusSeeOther, "/templates?flash=missing")
	}

	numbers := whatsapp.PlaceholderNumbers(body)
	if len(numbers) == 0 {
		return k.Redirect(http.StatusSeeOther, "/templates?flash=novariables")
	}

	examples := make([]string, 0, len(numbers))
	for _, n := range numbers {
		v := strings.TrimSpace(k.Request.FormValue("example_" + strconv.Itoa(n)))
		if v == "" {
			return k.Redirect(http.StatusSeeOther, "/templates?flash=example")
		}
		examples = append(examples, v)
	}

	components, err := json.Marshal([]map[string]any{
		{
			"type": "BODY",
			"text": body,
			"example": map[string]any{
				"body_text": [][]string{examples},
			},
		},
	})
	if err != nil {
		return err
	}

	tmpl, err := a.WhatsApp.CreateTemplate(k.Request.Context(), principal.ActiveShop.ShopID, whatsapp.TemplateDraft{
		Name:       name,
		Language:   language,
		Category:   "UTILITY",
		Components: components,
	})
	if err != nil {
		var rej *whatsapp.SendRejection
		if errors.As(err, &rej) {
			// Meta refused the submission; the row is stored rejected with the
			// reason. The template page already surfaces the reason.
			return k.Redirect(http.StatusSeeOther, "/templates?flash=rejected")
		}
		a.Log.Error("template create internal failure", "error", err.Error())
		return k.Redirect(http.StatusSeeOther, "/templates?flash=internal")
	}

	if tmpl.ApprovalStatus == "rejected" {
		return k.Redirect(http.StatusSeeOther, "/templates?flash=rejected")
	}
	if tmpl.MarketingFlagged {
		a.Log.Warn("template flagged as marketing by meta",
			"shop_id", principal.ActiveShop.ShopID, "name", tmpl.Name, "warning", tmpl.MetaWarnings)
		return k.Redirect(http.StatusSeeOther, "/templates?flash=marketing")
	}
	a.Log.Info("template submitted for review",
		"shop_id", principal.ActiveShop.ShopID, "name", tmpl.Name, "language", tmpl.Language)
	return k.Redirect(http.StatusSeeOther, "/templates?flash=created")
}
