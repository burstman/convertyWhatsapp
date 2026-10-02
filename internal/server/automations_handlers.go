package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/shops"
	"whatsappconverty/internal/whatsapp"
	viewshared "whatsappconverty/web/views/components"
	vdashboard "whatsappconverty/web/views/dashboard"
)

// handleAutomations renders the automation page: every configured trigger shown
// as a card (naming, trigger, template, delivery rule), plus the create button.
func (a *App) handleAutomations(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	shop, err := a.Shops.GetByID(ctx, shopID)
	if err != nil && !errors.Is(err, shops.ErrNotFound) {
		return err
	}

	list, err := a.Automations.List(ctx, shopID)
	if err != nil {
		return err
	}

	templates, err := a.WhatsApp.Templates(ctx, shopID)
	if err != nil {
		return err
	}
	approved, names, bodies := catalogFromTemplates(templates)

	flash := vdashboard.AutomationFlash{}
	switch k.Request.URL.Query().Get("flash") {
	case "":
	case "created":
		flash.Info = "Automation created. It fires on matching order events from now on."
	case "updated":
		flash.Info = "Automation updated."
	case "duplicate":
		flash.Error = "An automation for this trigger already exists."
	case "missing":
		flash.Error = "A name, trigger and template are required."
	case "badschedule":
		flash.Error = "The send rule is invalid — check the time, timezone, days or delay."
		if why := k.Request.URL.Query().Get("why"); why != "" {
			flash.Error = "The send rule is invalid: " + why + "."
		}
	case "notemplate":
		flash.Error = "Create and get an approved template first."
	case "wrongsource":
		flash.Error = "That template was written for " + k.Request.URL.Query().Get("for") +
			", so it cannot drive this trigger. Pick one written for the same source."
	case "tested":
		flash.Info = "Test message sent. Check the number in WhatsApp."
	case "testfailed":
		flash.Error = "Test message failed — the sender must be healthy: use a valid E.164 number that is in the WhatsApp test phone list or has an open 24h conversation."
	case "testnophone":
		flash.Error = "Enter a phone number to test the message."
	case "toggled":
		flash.Info = "Automation updated."
	case "deleted":
		flash.Info = "Automation removed."
	case "notfound":
		flash.Error = "That automation no longer exists."
	case "error", "internal":
		flash.Error = "Something went wrong — try again."
	}

	page := viewshared.Page{
		Title:    "Automations",
		Active:   "automations",
		ShopName: shop.Name,
		UserName: principal.User.Name,
	}
	return k.Render(vdashboard.AutomationsPage(page, list, approved, names, bodies, flash))
}

// handleAutomationHistory shows the messages one automation produced. The rows
// are filtered by the automation id, so the automation lookup (scoped to the
// user's shop) is what decides the page: an id that does not resolve redirects
// to the list rather than rendering.
func (a *App) handleAutomationHistory(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	shop, err := a.Shops.GetByID(ctx, shopID)
	if err != nil && !errors.Is(err, shops.ErrNotFound) {
		return err
	}

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}
	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation history lookup failed", "id", id, "error", err)
		return err
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	rows, err := a.WhatsApp.MessagesForAutomation(ctx, id, 200)
	if err != nil {
		a.Log.Error("automation history query failed", "automation_id", id, "error", err)
		return err
	}
	counts, err := a.WhatsApp.AutomationSendCounts(ctx, id)
	if err != nil {
		a.Log.Error("automation history counts failed", "automation_id", id, "error", err)
		return err
	}
	suppressions, err := automations.SuppressionsForAutomation(ctx, a.Pool, id, 200)
	if err != nil {
		a.Log.Error("automation history suppressions failed", "automation_id", id, "error", err)
		return err
	}

	templateName := ""
	if automation.TemplateID != uuid.Nil {
		if t, tErr := a.WhatsApp.Template(ctx, shopID, automation.TemplateID); tErr == nil {
			templateName = t.Name
		}
	}

	page := viewshared.Page{
		Title:    "Send history",
		Active:   "automations",
		ShopName: shop.Name,
		UserName: principal.User.Name,
	}
	retry := retryOutcome(k)
	return k.Render(vdashboard.AutomationHistoryPage(page, *automation, templateName, rows, counts, suppressions, retry))
}

// handleAutomationRetryHeld re-runs the sends this automation held back. A
// held-back event is not retried on its own: the status transition it came
// from has already passed, so without this the only way to deliver it is to
// wait for a new event that may never come. Everything that could have arrived
// since is re-read before sending.
func (a *App) handleAutomationRetryHeld(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}
	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation retry lookup failed", "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	res, err := a.Automations.RetrySuppressions(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation retry failed", "shop_id", shopID, "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	a.Log.Info("automation held-back sends retried",
		"shop_id", shopID, "automation_id", id,
		"attempted", res.Attempted, "sent", res.Sent, "scheduled", res.Scheduled,
		"still_held", res.StillHeld, "template_missing", res.TemplateMissing,
		"rejected", res.Rejected)

	return k.Redirect(http.StatusSeeOther, fmt.Sprintf(
		"/automations/%s/history?attempted=%d&sent=%d&held=%d&already=%d&late=%d&missing=%d&rejected=%d",
		id, res.Attempted, res.Sent, res.StillHeld, res.AlreadySent, res.Scheduled, res.TemplateMissing, res.Rejected))
}

// handleAutomationEdit renders the create form when no id is given, or the
// prefilled edit form for one automation. Everything is scoped to the logged-in
// user's shop.
func (a *App) handleAutomationEdit(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	shop, err := a.Shops.GetByID(ctx, shopID)
	if err != nil && !errors.Is(err, shops.ErrNotFound) {
		return err
	}

	var automation *automations.Automation
	if idParam := chi.URLParam(k.Request, "id"); idParam != "" {
		id, pErr := uuid.Parse(idParam)
		if pErr != nil {
			return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
		}
		automation, err = a.Automations.Automation(ctx, shopID, id)
		if err != nil {
			a.Log.Error("automation edit lookup failed", "id", id, "error", err)
			return err
		}
		if automation == nil {
			return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
		}
	}

	templates, err := a.WhatsApp.Templates(ctx, shopID)
	if err != nil {
		return err
	}
	approved := make([]whatsapp.MerchantTemplate, 0, len(templates))
	for _, t := range templates {
		if t.ApprovalStatus == "approved" && !t.MarketingFlagged {
			approved = append(approved, t)
		}
	}

	flash := vdashboard.AutomationFlash{}
	switch k.Request.URL.Query().Get("flash") {
	case "", "created":
	case "missing":
		flash.Error = "A name, trigger and template are required."
	case "badschedule":
		flash.Error = "The send rule is invalid — check the time, timezone, days or delay."
		if why := k.Request.URL.Query().Get("why"); why != "" {
			flash.Error = "The send rule is invalid: " + why + "."
		}
	case "duplicate":
		flash.Error = "An automation for this trigger already exists."
	case "notemplate":
		flash.Error = "Create and get an approved template first."
	case "wrongsource":
		flash.Error = "That template was written for " + k.Request.URL.Query().Get("for") +
			", so it cannot drive this trigger. Pick one written for the same source."
	case "notfound":
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	case "error", "internal":
		flash.Error = "Something went wrong — try again."
	}

	page := viewshared.Page{
		Title:    "Automation",
		Active:   "automations",
		ShopName: shop.Name,
		UserName: principal.User.Name,
	}
	source, sourceCounts := defaultEventSource(automation)
	return k.Render(vdashboard.AutomationFormPage(page, automation, fitSource(approved, source),
		automation != nil, flash, source, sourceCounts))
}

// defaultEventSource picks the event source the form opens on: the automation's
// own, or — when creating — the single source this platform feeds (Converty).
func defaultEventSource(a *automations.Automation) (string, map[string]int) {
	source := automations.SourceConverty
	if a != nil && a.EventSource != "" {
		source = a.EventSource
	}
	counts := map[string]int{automations.SourceConverty: 1}
	return source, counts
}

// fitSource narrows a template list to the ones usable for one event source.
func fitSource(templates []whatsapp.MerchantTemplate, source string) []whatsapp.MerchantTemplate {
	out := make([]whatsapp.MerchantTemplate, 0, len(templates))
	for _, t := range templates {
		if templateFitsSource(t, source) {
			out = append(out, t)
		}
	}
	return out
}

// scheduleWhy turns a ParseSchedule error into a sentence the form can show,
// so a bounced submit says which field is wrong.
func scheduleWhy(err error) string {
	switch err.Error() {
	case "invalid send time":
		return "a scheduled send needs a send time"
	case "select at least one send day":
		return "a scheduled send needs at least one send day"
	case "invalid timezone":
		return "the timezone must be an IANA name such as Africa/Tunis"
	case "delay must be a positive number of minutes":
		return "a delayed send needs a delay of at least one minute"
	case "invalid delay", "invalid send day", "unknown automation type":
		return "the send rule fields are not valid"
	}
	return "check the time, timezone, days or delay"
}

func scheduleFromForm(k *kit.Kit) (*automations.Schedule, error) {
	if err := k.Request.ParseForm(); err != nil {
		return nil, err
	}
	delayMinutes := 0
	if v := strings.TrimSpace(k.Request.FormValue("send_delay_minutes")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, errors.New("invalid delay")
		}
		delayMinutes = n
	}
	return automations.ParseSchedule(
		k.Request.FormValue("send_mode"),
		strings.TrimSpace(k.Request.FormValue("send_time")),
		strings.TrimSpace(k.Request.FormValue("send_timezone")),
		k.Request.Form["send_days"],
		delayMinutes,
	)
}

// handleAutomationCreate records a new event → template mapping for the user's
// shop.
func (a *App) handleAutomationCreate(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	if err := k.Request.ParseForm(); err != nil {
		return err
	}
	name := strings.TrimSpace(k.Request.FormValue("name"))
	description := strings.TrimSpace(k.Request.FormValue("description"))
	source := k.Request.FormValue("event_source")
	status := k.Request.FormValue("event_status")
	templateID, pErr := uuid.Parse(k.Request.FormValue("template_id"))
	if name == "" || (source != automations.SourceConverty && source != automations.SourceDelivery) ||
		status == "" || pErr != nil {
		return k.Redirect(http.StatusSeeOther, "/automations/new?flash=missing")
	}

	owns, tErr := a.shopHasTemplate(ctx, shopID, templateID)
	if tErr != nil || !owns {
		a.Log.Warn("automation create rejected (template)", "shop_id", shopID, "template_id", templateID, "error", tErr)
		return k.Redirect(http.StatusSeeOther, "/automations/new?flash=notemplate")
	}

	if r, sErr := a.templateSource(ctx, shopID, templateID); sErr != nil || !templateFitsSource(whatsapp.MerchantTemplate{Source: r}, source) {
		a.Log.Warn("automation create rejected (source mismatch)",
			"shop_id", shopID, "template_id", templateID, "source", source, "template_source", r, "error", sErr)
		return k.Redirect(http.StatusSeeOther, "/automations/new?flash=wrongsource&for="+url.QueryEscape(whatsapp.SourceLabel(r)))
	}

	schedule, sErr2 := scheduleFromForm(k)
	if sErr2 != nil {
		a.Log.Warn("automation create rejected", "shop_id", shopID, "error", sErr2)
		return k.Redirect(http.StatusSeeOther, "/automations/new?flash=badschedule&why="+url.QueryEscape(scheduleWhy(sErr2)))
	}

	if err := a.Automations.Create(ctx, shopID, source, status, templateID, schedule, name, description); err != nil {
		if errors.Is(err, automations.ErrDuplicate) {
			return k.Redirect(http.StatusSeeOther, "/automations/new?flash=duplicate")
		}
		a.Log.Error("automation create failed", "shop_id", shopID, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations/new?flash=error")
	}

	a.Log.Info("automation created", "shop_id", shopID, "source", source, "status", status)
	return k.Redirect(http.StatusSeeOther, "/automations?flash=created")
}

// handleAutomationUpdate saves edits to an existing automation, keeping the
// automation's own shop.
func (a *App) handleAutomationUpdate(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation update lookup failed", "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	if err := k.Request.ParseForm(); err != nil {
		return err
	}
	name := strings.TrimSpace(k.Request.FormValue("name"))
	description := strings.TrimSpace(k.Request.FormValue("description"))
	source := k.Request.FormValue("event_source")
	status := k.Request.FormValue("event_status")
	templateID, pErr := uuid.Parse(k.Request.FormValue("template_id"))
	if name == "" || (source != automations.SourceConverty && source != automations.SourceDelivery) ||
		status == "" || pErr != nil {
		return k.Redirect(http.StatusSeeOther, "/automations/"+id.String()+"/edit?flash=missing")
	}

	owns, tErr := a.shopHasTemplate(ctx, shopID, templateID)
	if tErr != nil || !owns {
		a.Log.Warn("automation update rejected (template)", "shop_id", shopID, "template_id", templateID, "error", tErr)
		return k.Redirect(http.StatusSeeOther, "/automations/"+id.String()+"/edit?flash=notemplate")
	}

	if r, sErr := a.templateSource(ctx, shopID, templateID); sErr != nil || !templateFitsSource(whatsapp.MerchantTemplate{Source: r}, source) {
		a.Log.Warn("automation update rejected (source mismatch)",
			"shop_id", shopID, "template_id", templateID, "source", source, "template_source", r, "error", sErr)
		return k.Redirect(http.StatusSeeOther,
			"/automations/"+id.String()+"/edit?flash=wrongsource&for="+url.QueryEscape(whatsapp.SourceLabel(r)))
	}

	schedule, sErr := scheduleFromForm(k)
	if sErr != nil {
		a.Log.Warn("automation update rejected", "shop_id", shopID, "error", sErr)
		return k.Redirect(http.StatusSeeOther,
			"/automations/"+id.String()+"/edit?flash=badschedule&why="+url.QueryEscape(scheduleWhy(sErr)))
	}

	if err := a.Automations.Update(ctx, shopID, id, source, status, templateID, schedule, name, description); err != nil {
		if errors.Is(err, automations.ErrDuplicate) {
			return k.Redirect(http.StatusSeeOther, "/automations/"+id.String()+"/edit?flash=duplicate")
		}
		a.Log.Error("automation update failed", "shop_id", shopID, "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations/"+id.String()+"/edit?flash=error")
	}

	a.Log.Info("automation updated", "shop_id", shopID, "id", id)
	return k.Redirect(http.StatusSeeOther, "/automations?flash=updated")
}

// handleAutomationToggle enables or disables an automation.
func (a *App) handleAutomationToggle(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation toggle lookup failed", "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	enabled := k.Request.FormValue("enabled") == "1"
	if err := a.Automations.SetEnabled(ctx, shopID, id, enabled); err != nil {
		a.Log.Error("automation toggle failed", "shop_id", shopID, "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	return k.Redirect(http.StatusSeeOther, "/automations?flash=toggled")
}

// handleAutomationTest sends the automation's template to a number the user
// fills in, so the message can be previewed live before it fires on real
// events. Sample variables stand in for the real order data.
func (a *App) handleAutomationTest(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}
	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation test lookup failed", "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}
	if automation.TemplateID == uuid.Nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notemplate")
	}

	to := strings.TrimSpace(k.Request.FormValue("phone"))
	if to == "" {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=testnophone")
	}

	templates, err := a.WhatsApp.Templates(ctx, shopID)
	if err != nil {
		return err
	}
	var chosen whatsapp.MerchantTemplate
	found := false
	for _, t := range templates {
		if t.ID == automation.TemplateID {
			chosen = t
			found = true
			break
		}
	}
	if !found {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notemplate")
	}

	if _, err := a.WhatsApp.SendTemplateTest(ctx, whatsapp.TestTemplateRequest{
		ShopID:     shopID,
		TemplateID: automation.TemplateID,
		To:         to,
		Variables:  testVariables(chosen),
	}); err != nil {
		var rej *whatsapp.SendRejection
		if errors.As(err, &rej) {
			a.Log.Warn("automation test send rejected",
				"automation_id", id, "code", rej.Code, "reason", rej.Reason)
			return k.Redirect(http.StatusSeeOther, "/automations?flash=testfailed")
		}
		a.Log.Error("automation test send failed", "automation_id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}

	a.Log.Info("automation test message sent", "automation_id", id, "to", to)
	return k.Redirect(http.StatusSeeOther, "/automations?flash=tested")
}

// testVariables fills the template slots so a test message looks like the real
// one. A semantic template is filled with the example of the chip the merchant
// dropped at each position; a legacy positional template falls back to the
// classic sample vocabulary, and any slot left with nothing gets an em dash so
// the send gate's variable-count check still passes.
func testVariables(t whatsapp.MerchantTemplate) map[string]string {
	vocab := []string{"Hamed", "CVY-TEST", "out for delivery", "1234567890113", "Ali Mansour", "+216 98 111 222"}
	vars := make(map[string]string, t.NumVariables)
	for i := 0; i < t.NumVariables; i++ {
		v := ""
		if i < len(t.Variables) {
			v = whatsapp.TokenExample(t.Variables[i])
		}
		if v == "" && i < len(vocab) {
			v = vocab[i]
		}
		if v == "" {
			v = "—"
		}
		vars[strconv.Itoa(i+1)] = v
	}
	return vars
}

// handleAutomationDelete removes an automation.
func (a *App) handleAutomationDelete(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shopID := principal.User.ShopID
	ctx := k.Request.Context()

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	automation, err := a.Automations.Automation(ctx, shopID, id)
	if err != nil {
		a.Log.Error("automation delete lookup failed", "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	if automation == nil {
		return k.Redirect(http.StatusSeeOther, "/automations?flash=notfound")
	}

	if err := a.Automations.Delete(ctx, shopID, id); err != nil {
		a.Log.Error("automation delete failed", "shop_id", shopID, "id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/automations?flash=error")
	}
	return k.Redirect(http.StatusSeeOther, "/automations?flash=deleted")
}

// shopHasTemplate reports whether a template with the given id belongs to the
// shop.
func (a *App) shopHasTemplate(ctx context.Context, shopID, templateID uuid.UUID) (bool, error) {
	t, err := a.WhatsApp.Template(ctx, shopID, templateID)
	if err != nil {
		return false, err
	}
	return t.ID != uuid.Nil, nil
}

// templateSource reads a template's event source for the save-time guard.
func (a *App) templateSource(ctx context.Context, shopID, templateID uuid.UUID) (string, error) {
	t, err := a.WhatsApp.Template(ctx, shopID, templateID)
	if err != nil {
		return "", err
	}
	return t.Source, nil
}

// templateFitsSource reports whether a template may be attached to an
// automation of the given event source. Templates created before sources
// existed carry "any" and stay attachable everywhere.
func templateFitsSource(t whatsapp.MerchantTemplate, source string) bool {
	switch source {
	case "converty", "delivery":
		return t.Source == source || t.Source == whatsapp.SourceAny
	}
	return true
}

// catalogFromTemplates splits a shop's templates into the approved sendable
// list and id → name / id → body maps used by the cards and preview.
func catalogFromTemplates(templates []whatsapp.MerchantTemplate) (approved []whatsapp.MerchantTemplate, names map[uuid.UUID]string, bodies map[uuid.UUID]string) {
	names = make(map[uuid.UUID]string, len(templates))
	bodies = make(map[uuid.UUID]string, len(templates))
	approved = make([]whatsapp.MerchantTemplate, 0, len(templates))
	for _, t := range templates {
		names[t.ID] = t.Name
		bodies[t.ID] = whatsapp.TemplateBody(t)
		if t.ApprovalStatus == "approved" && !t.MarketingFlagged {
			approved = append(approved, t)
		}
	}
	return approved, names, bodies
}

// retryOutcome reports what a "send the held-back ones now" click actually did,
// so the merchant is told "2 sent, 1 still held back" instead of watching the
// list for a change they cannot tell apart from the list not refreshing.
func retryOutcome(k *kit.Kit) automations.RetryResult {
	q := k.Request.URL.Query()
	if !q.Has("attempted") {
		return automations.RetryResult{}
	}
	return automations.RetryResult{
		Attempted:       atoiOr(q.Get("attempted"), 0),
		Sent:            atoiOr(q.Get("sent"), 0),
		StillHeld:       atoiOr(q.Get("held"), 0),
		AlreadySent:     atoiOr(q.Get("already"), 0),
		Scheduled:       atoiOr(q.Get("late"), 0),
		TemplateMissing: atoiOr(q.Get("missing"), 0),
		Rejected:        atoiOr(q.Get("rejected"), 0),
	}
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}