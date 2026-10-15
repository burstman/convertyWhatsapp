package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthdm/superkit/kit"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/config"
	"whatsappconverty/internal/whatsapp"
)

type recordedCall struct{ shopID, id uuid.UUID }

// fakeTestSend records every dependency call so tenant scoping, the send
// payload and the audit trail can be asserted without a database or Meta.
type fakeTestSend struct {
	automation    *automations.Automation
	automationErr error
	template      whatsapp.MerchantTemplate
	templateErr   error
	sendErr       error

	automationCalls []recordedCall
	templateCalls   []recordedCall
	sendCalls       []whatsapp.TestTemplateRequest
	audits          []whatsapp.TestSendAudit
}

func (f *fakeTestSend) deps() testSendDeps {
	log := discardLogger()
	return testSendDeps{
		automation: func(_ context.Context, shopID, id uuid.UUID) (*automations.Automation, error) {
			f.automationCalls = append(f.automationCalls, recordedCall{shopID, id})
			return f.automation, f.automationErr
		},
		template: func(_ context.Context, shopID, templateID uuid.UUID) (whatsapp.MerchantTemplate, error) {
			f.templateCalls = append(f.templateCalls, recordedCall{shopID, templateID})
			return f.template, f.templateErr
		},
		send: func(_ context.Context, req whatsapp.TestTemplateRequest) (*whatsapp.SendResult, error) {
			f.sendCalls = append(f.sendCalls, req)
			if f.sendErr != nil {
				return nil, f.sendErr
			}
			return &whatsapp.SendResult{Status: "sent"}, nil
		},
		audit: func(_ context.Context, e whatsapp.TestSendAudit) error {
			f.audits = append(f.audits, e)
			return nil
		},
		hash: func(shopID uuid.UUID, phone string) string {
			return whatsapp.HashRecipient("test-secret", shopID, phone)
		},
		now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
		log: log,
	}
}

func ownerUser(shopID uuid.UUID) auth.User {
	return auth.User{ID: uuid.New(), Role: auth.RoleClient, Status: auth.StatusActive}
}

func adminUser(shopID uuid.UUID) auth.User {
	return auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Status: auth.StatusActive}
}

func kitWithUser(user auth.User) (*kit.Kit, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/automations/x/test",
		strings.NewReader("phone=%2B21624118849"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), kit.AuthKey{}, auth.Auth{LoggedIn: true, User: user}))
	return &kit.Kit{Response: rec, Request: r}, rec
}

// A platform admin hitting the test endpoint gets the platform's not-found page
// and is never sent down the send path. (The nil service pointers would panic if
// the handler fell through.)
func TestAdminCannotTriggerTestSend(t *testing.T) {
	a := &App{Cfg: config.Config{SuperkitSecret: "test-superkit-secret-32-bytes-min"}, Log: discardLogger()}
	k, rec := kitWithUser(adminUser(uuid.New()))

	if err := a.handleAutomationTest(k); err != nil {
		t.Fatalf("admin request returned an error: %v", err)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("admin was redirected (%q) instead of blocked", loc)
	}
	if !strings.Contains(rec.Body.String(), "Page not found") {
		t.Fatalf("admin did not receive the not-found page: %q", rec.Body.String())
	}
}

// A cross-shop automation id resolves to nothing because the lookup is scoped to
// the actor's shop; no template is read and nothing is sent.
func TestTestSendAutomationIsTenantScoped(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	foreign := uuid.New()

	f := &fakeTestSend{automation: nil} // not found within this shop
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, foreign, "+21624118849", f.deps())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != testSendNotFound {
		t.Fatalf("outcome = %v, want testSendNotFound", outcome)
	}
	if len(f.automationCalls) != 1 || f.automationCalls[0].shopID != shop || f.automationCalls[0].id != foreign {
		t.Fatalf("automation lookup not scoped to the actor's shop: %+v", f.automationCalls)
	}
	if len(f.templateCalls) != 0 || len(f.sendCalls) != 0 {
		t.Fatalf("nothing should be read or sent for a foreign automation")
	}
}

// A template that does not resolve inside the actor's shop is refused before any
// send, even when the automation exists.
func TestTestSendTemplateIsTenantScoped(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID, tmplID := uuid.New(), uuid.New()

	f := &fakeTestSend{
		automation: &automations.Automation{ID: autoID, TemplateID: tmplID},
		template:   whatsapp.MerchantTemplate{}, // lookup miss => zero value
	}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, "+21624118849", f.deps())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != testSendNoTemplate {
		t.Fatalf("outcome = %v, want testSendNoTemplate", outcome)
	}
	if len(f.templateCalls) != 1 || f.templateCalls[0].shopID != shop {
		t.Fatalf("template lookup not scoped to the actor's shop: %+v", f.templateCalls)
	}
	if len(f.sendCalls) != 0 {
		t.Fatalf("send must not run when the template cannot be resolved")
	}
}

// The happy path sends with the actor's shop and ids, and audits an "accepted"
// attempt without ever storing the raw number.
func TestOwnerTestSendAcceptedAndAuditedWithoutPhone(t *testing.T) {
	const phone = "+21624118849"
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID, tmplID := uuid.New(), uuid.New()

	f := &fakeTestSend{
		automation: &automations.Automation{ID: autoID, TemplateID: tmplID},
		template:   whatsapp.MerchantTemplate{ID: tmplID},
	}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, phone, f.deps())
	if err != nil || outcome != testSendAccepted {
		t.Fatalf("outcome = %v, err = %v, want accepted", outcome, err)
	}

	if len(f.sendCalls) != 1 {
		t.Fatalf("send calls = %d, want 1", len(f.sendCalls))
	}
	req := f.sendCalls[0]
	if req.ShopID != shop || req.TemplateID != tmplID || req.To != phone {
		t.Fatalf("send request not tenant-scoped: %+v", req)
	}
	if req.ActorID != actor.ID || req.AutomationID != autoID {
		t.Fatalf("send request missing actor/automation attribution: %+v", req)
	}

	if len(f.audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(f.audits))
	}
	e := f.audits[0]
	if e.Outcome != whatsapp.TestSendOutcomeAccepted {
		t.Fatalf("audit outcome = %q, want accepted", e.Outcome)
	}
	if e.ShopID != shop || e.ActorID != actor.ID || e.AutomationID != autoID || e.TemplateID != tmplID {
		t.Fatalf("audit not attributed correctly: %+v", e)
	}
	if e.RecipientHash == "" || e.RecipientHash == phone || strings.Contains(e.RecipientHash, "21624118849") {
		t.Fatalf("audit recipient hash leaks the phone: %q", e.RecipientHash)
	}
	if e.CreatedAt.IsZero() {
		t.Fatal("audit timestamp missing")
	}
}

// A rate-limit refusal is audited as rate_limited, never accepted.
func TestRateLimitedTestSendIsAuditedAndNotAccepted(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID, tmplID := uuid.New(), uuid.New()

	f := &fakeTestSend{
		automation: &automations.Automation{ID: autoID, TemplateID: tmplID},
		template:   whatsapp.MerchantTemplate{ID: tmplID},
		sendErr:    &whatsapp.RateLimitExceeded{PerShop: true, TestSend: true},
	}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, "+21624118849", f.deps())
	if err != nil {
		t.Fatalf("rate limit is a business refusal, not an infra error: %v", err)
	}
	if outcome != testSendRateLimited {
		t.Fatalf("outcome = %v, want testSendRateLimited", outcome)
	}
	if len(f.audits) != 1 || f.audits[0].Outcome != whatsapp.TestSendOutcomeRateLimited {
		t.Fatalf("audit = %+v, want a single rate_limited record", f.audits)
	}
}

// A provider/send-rule refusal is "rejected", distinct from an infrastructure
// failure and from a confirmed delivery.
func TestProviderRefusalAuditedAsRejected(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID, tmplID := uuid.New(), uuid.New()

	f := &fakeTestSend{
		automation: &automations.Automation{ID: autoID, TemplateID: tmplID},
		template:   whatsapp.MerchantTemplate{ID: tmplID},
		sendErr:    whatsapp.NewSendRejection(whatsapp.ErrCodeMetaAPIError, "provider refused"),
	}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, "+21624118849", f.deps())
	if err != nil {
		t.Fatalf("a provider refusal is a business result: %v", err)
	}
	if outcome != testSendRejected {
		t.Fatalf("outcome = %v, want testSendRejected", outcome)
	}
	if len(f.audits) != 1 || f.audits[0].Outcome != whatsapp.TestSendOutcomeRejected {
		t.Fatalf("audit = %+v, want a single rejected record", f.audits)
	}
}

// An infrastructure error is surfaced (so the handler logs it) and audited as
// failed, never as accepted.
func TestInfrastructureFailureAuditedAsFailed(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID := uuid.New()

	boom := errors.New("database down")
	f := &fakeTestSend{automationErr: boom}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, "+21624118849", f.deps())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying failure", err)
	}
	if outcome != testSendFailed {
		t.Fatalf("outcome = %v, want testSendFailed", outcome)
	}
	if len(f.sendCalls) != 0 || len(f.audits) != 0 {
		t.Fatalf("nothing should be sent or audited when the lookup fails: %+v", f)
	}
}

// A missing phone is refused before any lookup beyond the automation.
func TestTestSendRequiresPhone(t *testing.T) {
	shop := uuid.New()
	actor := ownerUser(shop)
	autoID, tmplID := uuid.New(), uuid.New()

	f := &fakeTestSend{
		automation: &automations.Automation{ID: autoID, TemplateID: tmplID},
	}
	outcome, err := runAutomationTest(context.Background(), auth.Auth{LoggedIn: true, User: actor, ActiveShop: auth.ActiveShop{ShopID: shop}}, autoID, "   ", f.deps())
	if err != nil || outcome != testSendNoPhone {
		t.Fatalf("outcome = %v, err = %v, want testSendNoPhone", outcome, err)
	}
	if len(f.templateCalls) != 0 || len(f.sendCalls) != 0 {
		t.Fatalf("no template/send work should happen without a phone")
	}
}
