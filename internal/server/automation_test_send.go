package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/whatsapp"
)

// testSendOutcome describes how an authorized test-send request ended. It is a
// business result, distinct from an infrastructure error.
type testSendOutcome int

const (
	testSendAccepted testSendOutcome = iota
	testSendNotFound
	testSendNoPhone
	testSendNoTemplate
	testSendRateLimited
	testSendRejected
	testSendFailed
)

// testSendDeps narrows the app to the operations the test-send flow performs.
// Every dependency is a function so the owner check, tenant scoping, dedicated
// rate limit, audit and provider handling can be exercised without a database or
// a Meta call.
type testSendDeps struct {
	automation func(ctx context.Context, shopID, id uuid.UUID) (*automations.Automation, error)
	template   func(ctx context.Context, shopID, templateID uuid.UUID) (whatsapp.MerchantTemplate, error)
	send       func(ctx context.Context, req whatsapp.TestTemplateRequest) (*whatsapp.SendResult, error)
	audit      func(ctx context.Context, e whatsapp.TestSendAudit) error
	hash       func(shopID uuid.UUID, phone string) string
	now        func() time.Time
	log        *slog.Logger
}

// testSendDeps builds the real dependencies from the app. It is only reached
// after the owner gate, so a member never gets this far.
func (a *App) testSendDeps() testSendDeps {
	return testSendDeps{
		automation: a.Automations.Automation,
		template:   a.WhatsApp.Template,
		send:       a.WhatsApp.SendTemplateTest,
		audit:      a.WhatsApp.RecordTestSendAudit,
		hash: func(shopID uuid.UUID, phone string) string {
			return whatsapp.HashRecipient(a.Cfg.SuperkitSecret, shopID, phone)
		},
		now: time.Now,
		log: a.Log,
	}
}

// runAutomationTest performs the tenant-scoped lookup, the send (which charges
// the dedicated test-send budget before touching Meta) and the audit. It does
// not authorize: the handler has already enforced owner access. A non-nil error
// is an infrastructure failure, never a business refusal.
func runAutomationTest(ctx context.Context, actor auth.Auth, automationID uuid.UUID, phone string, d testSendDeps) (testSendOutcome, error) {
	automation, err := d.automation(ctx, actor.ActiveShop.ShopID, automationID)
	if err != nil {
		return testSendFailed, err
	}
	if automation == nil {
		return testSendNotFound, nil
	}
	if automation.TemplateID == uuid.Nil {
		return testSendNoTemplate, nil
	}
	if strings.TrimSpace(phone) == "" {
		return testSendNoPhone, nil
	}

	// The template is looked up inside the actor's shop, so a template id
	// belonging to another tenant resolves to nothing and is refused before
	// any send.
	template, err := d.template(ctx, actor.ActiveShop.ShopID, automation.TemplateID)
	if err != nil {
		return testSendFailed, err
	}
	if template.ID == uuid.Nil {
		return testSendNoTemplate, nil
	}

	_, sendErr := d.send(ctx, whatsapp.TestTemplateRequest{
		ShopID:       actor.ActiveShop.ShopID,
		TemplateID:   automation.TemplateID,
		To:           phone,
		Variables:    testVariables(template),
		ActorID:      actor.User.ID,
		AutomationID: automationID,
	})
	switch {
	case sendErr == nil:
		d.auditAttempt(ctx, actor, automationID, automation.TemplateID, phone, whatsapp.TestSendOutcomeAccepted)
		return testSendAccepted, nil
	}

	var rateLimited *whatsapp.RateLimitExceeded
	if errors.As(sendErr, &rateLimited) {
		d.auditAttempt(ctx, actor, automationID, automation.TemplateID, phone, whatsapp.TestSendOutcomeRateLimited)
		return testSendRateLimited, nil
	}

	var rejection *whatsapp.SendRejection
	if errors.As(sendErr, &rejection) {
		if d.log != nil {
			d.log.Warn("automation test send rejected", "automation_id", automationID, "code", rejection.Code)
		}
		d.auditAttempt(ctx, actor, automationID, automation.TemplateID, phone, whatsapp.TestSendOutcomeRejected)
		return testSendRejected, nil
	}

	d.auditAttempt(ctx, actor, automationID, automation.TemplateID, phone, whatsapp.TestSendOutcomeFailed)
	return testSendFailed, sendErr
}

// auditAttempt records one attempt. A failed audit write is logged, never
// surfaced: the send result is authoritative and the audit must not block it.
func (d testSendDeps) auditAttempt(ctx context.Context, actor auth.Auth, automationID, templateID uuid.UUID, phone, outcome string) {
	if d.audit == nil {
		return
	}
	entry := whatsapp.TestSendAudit{
		ShopID:        actor.ActiveShop.ShopID,
		ActorID:       actor.User.ID,
		AutomationID:  automationID,
		TemplateID:    templateID,
		RecipientHash: d.hash(actor.ActiveShop.ShopID, phone),
		Outcome:       outcome,
		CreatedAt:     d.now(),
	}
	if err := d.audit(ctx, entry); err != nil && d.log != nil {
		d.log.Warn("test send audit write failed", "shop_id", actor.ActiveShop.ShopID, "outcome", outcome, "error", err)
	}
}
