package whatsapp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func rejCode(err error) string {
	var r *SendRejection
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func activeMerchant() MerchantState {
	ts := time.Now()
	return MerchantState{
		ShopID:          uuid.New(),
		Status:          "active",
		WhatsappEnabled: true,
		TermsAcceptedAt: &ts,
	}
}

func ownedCustomer() CustomerState {
	return CustomerState{ID: uuid.New(), Phone: "+21600000000", OwnedByShop: true}
}

func optedIn() ConsentState {
	return ConsentState{Status: "opt_in", Category: "order_updates"}
}

func approvedTemplate() TemplateState {
	return TemplateState{
		ID:             uuid.New(),
		Name:           "order_confirmed",
		Language:       "ar",
		Category:       "order_updates",
		ApprovalStatus: "approved",
		Components: []TemplateComponent{
			{Type: "body", Parameters: []TemplateParameter{
				{Type: "text", Text: "order {{1}} is {{2}} {{3}}"},
			}},
		},
		NumVariables: 3,
	}
}

func sendReq() SendRequest {
	return SendRequest{
		ShopID:     uuid.New(),
		CustomerID: uuid.New(),
		TemplateID: uuid.New(),
		Purpose:    "order_updates",
		Variables:  map[string]string{"1": "CVY-1", "2": "shipping", "3": "Hamed"},
	}
}

func TestEvaluateSendRuleAllowsValidSend(t *testing.T) {
	err := EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), approvedTemplate(), sendReq())
	if err != nil {
		t.Fatalf("expected send to be allowed, got %v", err)
	}
}

func TestEvaluateSendRuleConsentEnforcement(t *testing.T) {
	cases := []struct {
		name    string
		consent ConsentState
		want    string
	}{
		{"no record", ConsentState{}, ErrCodeOptInRequired},
		{"revoked", ConsentState{Status: "revoked"}, ErrCodeOptInRevoked},
		{"unknown status", ConsentState{Status: "pending"}, ErrCodeOptInRequired},
		{"wrong category", ConsentState{Status: "opt_in", Category: "marketing"}, ErrCodeOptInRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EvaluateSendRule(activeMerchant(), ownedCustomer(), tc.consent, approvedTemplate(), sendReq())
			if got := rejCode(err); got != tc.want {
				t.Fatalf("got code %q, want %q", got, tc.want)
			}
		})
	}
}

// A customer learned from a Converty order carries only a phone number, never
// consent. This is the send-side half of the fix that stopped inbound webhooks
// from minting opt-in: with no explicit consent record the order notification
// is refused.
func TestEvaluateSendRuleOrderPhoneIsNotConsent(t *testing.T) {
	// ownedCustomer models a row created by the order pipeline (phone present,
	// no consent), and ConsentState{} is the "no consent record" state.
	err := EvaluateSendRule(activeMerchant(), ownedCustomer(), ConsentState{}, approvedTemplate(), sendReq())
	if got := rejCode(err); got != ErrCodeOptInRequired {
		t.Fatalf("order phone without consent: got %q, want %q", got, ErrCodeOptInRequired)
	}
}

func TestEvaluateSendRuleMerchantGate(t *testing.T) {
	inactive := activeMerchant()
	inactive.Status = "suspended"
	if got := rejCode(EvaluateSendRule(inactive, ownedCustomer(), optedIn(), approvedTemplate(), sendReq())); got != ErrCodeMerchantNotAuthorized {
		t.Fatalf("inactive merchant: got %q", got)
	}

	disabled := activeMerchant()
	disabled.WhatsappEnabled = false
	if got := rejCode(EvaluateSendRule(disabled, ownedCustomer(), optedIn(), approvedTemplate(), sendReq())); got != ErrCodeServiceDisabled {
		t.Fatalf("disabled service: got %q", got)
	}

	noterms := activeMerchant()
	noterms.TermsAcceptedAt = nil
	if got := rejCode(EvaluateSendRule(noterms, ownedCustomer(), optedIn(), approvedTemplate(), sendReq())); got != ErrCodeTermsNotAccepted {
		t.Fatalf("terms not accepted: got %q", got)
	}
}

func TestEvaluateSendRuleTenantIsolation(t *testing.T) {
	// A customer that does not belong to the merchant must never pass.
	foreign := ownedCustomer()
	foreign.OwnedByShop = false
	if got := rejCode(EvaluateSendRule(activeMerchant(), foreign, optedIn(), approvedTemplate(), sendReq())); got != ErrCodeCustomerNotOwned {
		t.Fatalf("foreign customer: got %q", got)
	}

	// A template that is not scoped to the merchant must never pass.
	empty := TemplateState{}
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), empty, sendReq())); got != ErrCodeTemplateNotFound {
		t.Fatalf("missing template: got %q", got)
	}
}

func TestEvaluateSendRuleApprovalEnforcement(t *testing.T) {
	rejected := approvedTemplate()
	rejected.ApprovalStatus = "rejected"
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), rejected, sendReq())); got != ErrCodeTemplateNotApproved {
		t.Fatalf("rejected template: got %q", got)
	}

	pending := approvedTemplate()
	pending.ApprovalStatus = "pending"
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), pending, sendReq())); got != ErrCodeTemplateNotApproved {
		t.Fatalf("pending template: got %q", got)
	}

	// Approval must never come from the caller; the struct is loaded by the
	// backend only. A frontend cannot set Approved=true (no such field).
	if approvedTemplate().ApprovalStatus != "approved" {
		t.Fatal("test invariant broken")
	}
}

func TestEvaluateSendRuleMarketingBlocked(t *testing.T) {
	flagged := approvedTemplate()
	flagged.MarketingFlagged = true

	// Even though Meta approved the mechanics, a marketing-flagged template
	// must never be sent — the platform only transmits transactional content.
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), flagged, sendReq())); got != ErrCodeTemplateMarketingBlocked {
		t.Fatalf("marketing flagged template: got %q", got)
	}

	clean := approvedTemplate()
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), clean, sendReq())); got != "" {
		t.Fatalf("clean template should pass: got %q", got)
	}
}

func TestWarningsIndicateMarketing(t *testing.T) {
	cases := map[string]bool{
		"":                           false,
		"Your template is approved.": false,
		"This template will be treated as a marketing template and may not reach users' main inbox.": true,
		"Promotional content detected in this template":                                              true,
		"This is not a marketing template.":                                                          false,
		"The template is not considered marketing.":                                                  false,
	}
	for warn, want := range cases {
		if got := warningsIndicateMarketing(warn); got != want {
			t.Fatalf("warningsIndicateMarketing(%q) = %v, want %v", warn, got, want)
		}
	}
}

func TestMarketingSignal(t *testing.T) {
	if !marketingSignal("MARKETING", "") {
		t.Fatal("MARKETING category must flag")
	}
	if !marketingSignal("UTILITY", "the template violates the marketing policy") {
		t.Fatal("marketing rejection reason must flag")
	}
	if marketingSignal("UTILITY", "") {
		t.Fatal("clean utility must not flag")
	}
}

func TestPgInterval(t *testing.T) {
	if got := pgInterval(15 * time.Minute); got != "900 seconds" {
		t.Fatalf("pgInterval(15m) = %q, want %q", got, "900 seconds")
	}
}

func TestEvaluateSendRulePurposeMismatch(t *testing.T) {
	editorial := optedIn()
	editorial.Category = "marketing" // matches the requested purpose, not the template
	req := sendReq()
	req.Purpose = "marketing" // template category is order_updates
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), editorial, approvedTemplate(), req)); got != ErrCodeTemplateUnintendedPurpose {
		t.Fatalf("purpose mismatch: got %q", got)
	}
}

func TestEvaluateSendRuleVariableValidation(t *testing.T) {
	tmpl := approvedTemplate()

	missing := sendReq()
	missing.Variables = map[string]string{"1": "only"}
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), tmpl, missing)); got != ErrCodeTemplateVariableInvalid {
		t.Fatalf("missing variable: got %q", got)
	}

	extra := sendReq()
	extra.Variables = map[string]string{"1": "a", "2": "b", "3": "c", "4": "d"}
	if got := rejCode(EvaluateSendRule(activeMerchant(), ownedCustomer(), optedIn(), tmpl, extra)); got != ErrCodeTemplateVariableInvalid {
		t.Fatalf("extra variable: got %q", got)
	}
}

func TestCountTemplateVariables(t *testing.T) {
	// Meta list shape (uppercase types, text field).
	metaShape := []byte(`[
		{"type":"HEADER","format":"TEXT","text":"أهلا"},
		{"type":"BODY","text":"مرحبًا {{1}}، طلبك {{2}}. التسليم {{3}}"},
		{"type":"BUTTONS","buttons":[{"type":"URL","url":"https://x/{{1}}","text":"عرض"}]}
	]`)
	if got := countTemplateVariablesRaw(metaShape); got != 3 {
		t.Fatalf("meta shape: got %d variables, want 3", got)
	}

	// Provision shape (lowercase types, parameters).
	provisionShape := []byte(`[
		{"type":"body","parameters":[{"type":"text","text":"hi {{1}}, order {{2}} ready"}]},
		{"type":"button","parameters":[{"type":"text","text":"https://track/{{1}}"}]}
	]`)
	// Button placeholders are NOT counted as body variables; only body/header.
	if got := countTemplateVariablesRaw(provisionShape); got != 2 {
		t.Fatalf("provision shape: got %d body variables, want 2", got)
	}
}

func TestBuildComponentsBodyAndURLButton(t *testing.T) {
	// order_confirmed_v2-style stored components (Meta list shape).
	raw := []byte(`[
		{"type":"HEADER","format":"TEXT","text":"تم تأكيد الطلب"},
		{"type":"BODY","text":"مرحبًا {{1}}، تم تأكيد طلبك ورقم طلبك هو {{2}}.\nالتسليم: {{3}}."},
		{"type":"BUTTONS","buttons":[{"type":"URL","url":"https://shop-ecommerce-9kak.onrender.com/tracking{{1}}","text":"عرض"}]}
	]`)
	vars := map[string]string{"1": "CVY-7", "2": "12345", "3": "1 يناير 2024"}
	components, err := buildComponents(raw, vars)
	if err != nil {
		t.Fatalf("buildComponents: %v", err)
	}

	var bodyParams []string
	var foundURLParam bool
	for _, c := range components {
		if c.Type == "body" {
			for _, p := range c.Parameters {
				bodyParams = append(bodyParams, p.Text)
			}
		}
		if c.Type == "button" && len(c.Parameters) == 1 && c.Parameters[0].Type == "text" && c.Parameters[0].Text == "CVY-7" {
			// The URL-button substitution set appended for {{1}}.
			foundURLParam = true
		}
	}
	if !foundURLParam {
		t.Fatalf("url button substitution missing: %+v", components)
	}
	if len(bodyParams) != 3 || bodyParams[0] != "CVY-7" || bodyParams[1] != "12345" || bodyParams[2] != "1 يناير 2024" {
		t.Fatalf("body params wrong: %v", bodyParams)
	}
}

func TestBuildComponentsMissingVariable(t *testing.T) {
	raw := []byte(`[
		{"type":"BODY","text":"{{1}} / {{2}} / {{3}}"},
		{"type":"BUTTONS","buttons":[{"type":"URL","url":"https://x/{{1}}"}]}
	]`)
	_, err := buildComponents(raw, map[string]string{"1": "x", "3": "y"})
	if got := rejCode(err); got != ErrCodeTemplateVariableInvalid {
		t.Fatalf("got %q, want %q", got, ErrCodeTemplateVariableInvalid)
	}
}

func TestShouldAdvanceStatus(t *testing.T) {
	cases := []struct {
		stored, incoming string
		want             bool
	}{
		{"sent", "delivered", true},
		{"delivered", "read", true},
		{"sent", "read", true},         // granted read implies delivered
		{"delivered", "sent", false},   // regression
		{"read", "delivered", false},   // regression
		{"failed", "delivered", false}, // terminal
		{"sent", "failed", true},
		{"", "delivered", true},
	}
	for _, tc := range cases {
		if got := ShouldAdvanceStatus(tc.stored, tc.incoming); got != tc.want {
			t.Fatalf("ShouldAdvanceStatus(%q,%q)=%v, want %v", tc.stored, tc.incoming, got, tc.want)
		}
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "appsecret"
	body := []byte(`{"entry":[]}`)
	sig := fmt.Sprintf("sha256=%x", hmacSHA256(secret, body))

	if !VerifyWebhookSignature(secret, sig, body) {
		t.Fatal("valid signature rejected")
	}
	if VerifyWebhookSignature("wrongsecret", sig, body) {
		t.Fatal("wrong secret accepted")
	}
	tampered := fmt.Sprintf("sha256=%x", hmacSHA256(secret, []byte(`{"entry":[1]}`)))
	if VerifyWebhookSignature(secret, tampered, body) {
		t.Fatal("tampered body accepted")
	}
	if VerifyWebhookSignature(secret, "sha1=abc", body) {
		t.Fatal("wrong prefix accepted")
	}
	if VerifyWebhookSignature(secret, "nothex", body) {
		t.Fatal("non-hex accepted")
	}
}

func TestVerifyWebhookChallenge(t *testing.T) {
	if !VerifyWebhookChallenge("tok", "subscribe", "tok", "123456") {
		t.Fatal("valid challenge rejected")
	}
	if VerifyWebhookChallenge("tok", "subscribe", "wrong", "123") {
		t.Fatal("wrong token accepted")
	}
	if VerifyWebhookChallenge("tok", "unsubscribe", "tok", "123") {
		t.Fatal("wrong mode accepted")
	}
	if VerifyWebhookChallenge("tok", "subscribe", "tok", "") {
		t.Fatal("empty challenge accepted")
	}
}

func TestParseWebhookStatuses(t *testing.T) {
	payload := `{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"903255096273971",
			"changes":[{
				"value":{
					"messaging_product":"whatsapp",
					"metadata":{"display_phone_number":"+21624118849"},
					"statuses":[{
						"id":"wamid.HBgLMjE2NTQxMTY1ODQVAgARGBI0OEIzM0MzRjM2RkJFNUE0NDkA",
						"status":"delivered",
						"timestamp":"1780000000",
						"recipient_id":"21654116580"
					},{
						"id":"wamid.FAILED123",
						"status":"failed",
						"timestamp":"1780000001",
						"errors":[{"code":131047,"title":"Message failed to send because more than 24 hours have passed since the customer last replied"}]
					}]
				}
			}]
		}]
	}`

	delivery, err := ParseWebhook([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	updates := delivery.Statuses
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	if updates[0].Status != "delivered" || updates[0].MetaMessageID != "wamid.HBgLMjE2NTQxMTY1ODQVAgARGBI0OEIzM0MzRjM2RkJFNUE0NDkA" {
		t.Fatalf("update[0] wrong: %+v", updates[0])
	}
	if updates[1].Status != "failed" || len(updates[1].Errors) != 1 || updates[1].Errors[0].Code != 131047 {
		t.Fatalf("update[1] wrong: %+v", updates[1])
	}
}

func TestParseWebhookNoStatuses(t *testing.T) {
	payload := `{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{}}]}]}`
	if _, err := ParseWebhook([]byte(payload)); err == nil {
		t.Fatal("expected error for delivery without statuses")
	}
}

func TestNormalizeApproval(t *testing.T) {
	for meta, want := range map[string]string{
		"APPROVED": "approved", "PENDING": "pending", "IN_APPEAL": "pending",
		"REJECTED": "rejected", "PAUSED": "paused", "DELETED": "deleted", "OTHER": "pending",
	} {
		if got := NormalizeApproval(meta); got != want {
			t.Fatalf("NormalizeApproval(%q)=%q, want %q", meta, got, want)
		}
	}
}

func TestWrapMetaError(t *testing.T) {
	apiErr := APIError{StatusCode: 400, Code: 131026, Message: "Message template not found"}
	got := wrapMetaError(apiErr)
	var rej *SendRejection
	if !errors.As(got, &rej) || rej.Code != ErrCodeMetaAPIError {
		t.Fatalf("got %v, want %q", got, ErrCodeMetaAPIError)
	}

	got = wrapMetaError(errors.New("network down"))
	if !errors.As(got, &rej) || rej.Code != ErrCodeMetaAPIError {
		t.Fatalf("generic error: got %v", got)
	}
}

func TestSendRejectionErrorPrefix(t *testing.T) {
	r := NewSendRejection(ErrCodeOptInRequired, "missing")
	if !strings.HasPrefix(r.Error(), ErrCodeOptInRequired) {
		t.Fatalf("error should start with the code: %s", r.Error())
	}
}
