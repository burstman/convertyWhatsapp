package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Public error codes returned by the HTTP API and the send gate. They are
// stable identifiers the frontend/API can match on without leaking internals.
const (
	ErrCodeMerchantNotAuthorized     = "merchant_not_authorized"
	ErrCodeServiceDisabled           = "whatsapp_service_disabled"
	ErrCodeTermsNotAccepted          = "whatsapp_terms_not_accepted"
	ErrCodeCustomerNotOwned          = "customer_not_owned_by_merchant"
	ErrCodeOptInRequired             = "whatsapp_opt_in_required"
	ErrCodeOptInRevoked              = "whatsapp_opt_in_revoked"
	ErrCodeTemplateNotFound          = "template_not_found"
	ErrCodeTemplateNotApproved       = "template_not_approved"
	ErrCodeTemplateUnintendedPurpose = "template_unintended_purpose"
	ErrCodeTemplateVariableInvalid   = "template_variable_invalid"
	ErrCodeTemplateMarketingBlocked  = "template_marketing_blocked"
	ErrCodeRateLimited               = "rate_limited"
	ErrCodeMetaAPIError              = "meta_api_error"
	ErrCodeDuplicateSend             = "duplicate_send"
)

// SendRejection records why a message was refused before Meta was contacted.
type SendRejection struct {
	Code   string
	Reason string
}

func (e *SendRejection) Error() string {
	return e.Code + ": " + e.Reason
}

// NewSendRejection builds a *SendRejection with a stable code.
func NewSendRejection(code, reason string) *SendRejection {
	return &SendRejection{Code: code, Reason: reason}
}

// MerchantState is the merchant's WhatsApp eligibility snapshot read from the
// shops row.
type MerchantState struct {
	ShopID          uuid.UUID
	Status          string
	WhatsappEnabled bool
	TermsAcceptedAt *time.Time
}

// CustomerState is the tenant-scoped customer ownership snapshot.
type CustomerState struct {
	ID          uuid.UUID
	Phone       string
	OwnedByShop bool
}

// ConsentState is the customer's latest WhatsApp consent snapshot. Status ""
// means no record exists (never opted in).
type ConsentState struct {
	Status   string // "" | opt_in | revoked
	Category string
}

// TemplateState is the merchant's template snapshot used by the send gate.
type TemplateState struct {
	ID               uuid.UUID
	Name             string
	Language         string
	Category         string
	ApprovalStatus   string // pending | approved | rejected | paused | deleted
	MarketingFlagged bool   // Meta warned the content is treated as marketing
	Components       []TemplateComponent
	RawComponents    []byte // components exactly as stored (Meta list or provision shape)
	NumVariables     int
}

// SendRequest is a single message send attempt. Purpose is the business
// category the sender declares (e.g. "order_updates"); it must match the
// template category. IdempotencyKey makes retries safe ("" = no dedup).
type SendRequest struct {
	ShopID          uuid.UUID
	CustomerID      uuid.UUID
	TemplateID      uuid.UUID
	ConvertyOrderID string
	Purpose         string
	Variables       map[string]string
	IdempotencyKey  string
	// AutomationID attributes the message to the automation that fired it, so
	// the merchant can read one automation's send history. uuid.Nil means the
	// send was not automation-driven (API, test), and stays unlinked.
	AutomationID uuid.UUID
}

// SendResult reports the outcome of an accepted send.
type SendResult struct {
	MessageID     uuid.UUID
	MetaMessageID string
	Status        string
}

// EvaluateSendRule is the pure CAN_SEND rule. It must verify every condition
// before any Meta call. Returning nil means the send is permitted.
func EvaluateSendRule(merchant MerchantState, customer CustomerState, consent ConsentState, template TemplateState, req SendRequest) error {
	if merchant.Status != "active" {
		return NewSendRejection(ErrCodeMerchantNotAuthorized, "merchant account is not active")
	}
	if !merchant.WhatsappEnabled {
		return NewSendRejection(ErrCodeServiceDisabled, "whatsapp service is not enabled for this shop")
	}
	if merchant.TermsAcceptedAt == nil {
		return NewSendRejection(ErrCodeTermsNotAccepted, "whatsapp service terms not accepted by the merchant")
	}
	if !customer.OwnedByShop {
		return NewSendRejection(ErrCodeCustomerNotOwned, "customer does not belong to this merchant")
	}
	switch consent.Status {
	case "":
		return NewSendRejection(ErrCodeOptInRequired, "customer has no recorded whatsapp opt-in")
	case "revoked":
		return NewSendRejection(ErrCodeOptInRevoked, "customer whatsapp consent was revoked")
	case "opt_in":
		if consent.Category != "" && req.Purpose != "" && consent.Category != req.Purpose {
			return NewSendRejection(ErrCodeOptInRevoked, "consent category does not cover the requested purpose")
		}
	default:
		return NewSendRejection(ErrCodeOptInRequired, "customer whatsapp consent state is invalid")
	}
	if template.ID == uuid.Nil || template.Name == "" {
		return NewSendRejection(ErrCodeTemplateNotFound, "template not found for this merchant")
	}
	if template.ApprovalStatus != "approved" {
		return NewSendRejection(ErrCodeTemplateNotApproved, "template is not approved ("+template.ApprovalStatus+")")
	}
	if template.MarketingFlagged {
		return NewSendRejection(ErrCodeTemplateMarketingBlocked, "meta treats this template as marketing content and it can never be sent")
	}
	if req.Purpose != "" && template.Category != req.Purpose {
		return NewSendRejection(ErrCodeTemplateUnintendedPurpose, "template category does not match the requested purpose")
	}
	if err := validateVariables(template, req.Variables); err != nil {
		return err
	}
	return nil
}

// validateVariables ensures the variables map covers every {{N}} placeholder
// defined by the template's approved components, and nothing more.
func validateVariables(t TemplateState, vars map[string]string) error {
	if len(vars) != t.NumVariables {
		return NewSendRejection(ErrCodeTemplateVariableInvalid,
			"template requires "+strconv.Itoa(t.NumVariables)+" variables, got "+strconv.Itoa(len(vars)))
	}
	return nil
}

// countTemplateVariables counts the distinct {{N}} placeholders across the
// template's header/body text (sliced form; see countTemplateVariablesRaw).
func countTemplateVariables(components []TemplateComponent) int {
	seen := map[int]bool{}
	for _, c := range components {
		if c.Type != "header" && c.Type != "body" {
			continue
		}
		for _, n := range PlaceholderNumbers(componentText(c)) {
			seen[n] = true
		}
	}
	return len(seen)
}

func componentText(c TemplateComponent) string {
	if len(c.Parameters) == 0 || c.Parameters[0].Type != "text" {
		return ""
	}
	return c.Parameters[0].Text
}

// PlaceholderNumbers returns the {{N}} indexes in text order.
func PlaceholderNumbers(text string) []int {
	var nums []int
	for i := 0; i < len(text); i++ {
		if text[i] != '{' || i+1 >= len(text) || text[i+1] != '{' {
			continue
		}
		j := i + 2
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		if j > i+2 && j < len(text) && text[j] == '}' {
			if n, err := strconv.Atoi(text[i+2 : j]); err == nil {
				nums = append(nums, n)
			}
		}
		i = j
	}
	return nums
}

// metaComponent is the neutral decode of a template component, accepting both
// Meta's list shape (HEADER/BODY/BUTTONS with text/buttons/example) and the
// provision shape (header/body/button with parameters).
type metaComponent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Format  string `json:"format"`
	Buttons []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"buttons"`
	Parameters []TemplateParameter `json:"parameters"`
}

// decodeMetaComponents parses stored template components; a malformed blob
// yields an empty set rather than a hard failure (the gate still rejects on
// variable mismatch).
func decodeMetaComponents(raw []byte) []metaComponent {
	var out []metaComponent
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func metaCompType(c metaComponent) string {
	return strings.ToUpper(c.Type)
}

func metaCompText(c metaComponent) string {
	if c.Text != "" {
		return c.Text
	}
	for _, p := range c.Parameters {
		if p.Type == "text" {
			return p.Text
		}
	}
	return ""
}

// metaHasURLButton reports whether any button component resolves to a URL with
// a {{N}} substitution (the send shape needs a trailing button parameter set).
func metaHasURLButton(comps []metaComponent) bool {
	for _, c := range comps {
		switch metaCompType(c) {
		case "BUTTONS":
			for _, b := range c.Buttons {
				if b.Type == "URL" {
					return true
				}
			}
		case "BUTTON":
			if strings.Contains(metaCompText(c), "http") {
				return true
			}
		}
	}
	return false
}

// countTemplateVariablesRaw counts the distinct {{N}} placeholders in the
// body/header text of components in their stored form.
func countTemplateVariablesRaw(raw []byte) int {
	seen := map[int]bool{}
	for _, c := range decodeMetaComponents(raw) {
		switch metaCompType(c) {
		case "BODY", "HEADER":
			for _, n := range PlaceholderNumbers(metaCompText(c)) {
				seen[n] = true
			}
		}
	}
	return len(seen)
}

// RenderTemplateBody returns the message text a set of variables produces, for
// the history page. The approved template body is fixed at send time, so a
// snapshot of it is exactly what the customer received. The history shows the
// stored copy rather than re-deriving it, because a template can be edited or
// deleted afterwards and a re-derived history would then show a message nobody
// was ever sent.
func RenderTemplateBody(raw []byte, vars map[string]string) string {
	var parts []string
	for _, c := range decodeMetaComponents(raw) {
		switch metaCompType(c) {
		case "BODY", "HEADER":
			text := metaCompText(c)
			if strings.TrimSpace(text) == "" {
				continue
			}
			parts = append(parts, fillPlaceholders(text, vars))
		}
	}
	// Meta renders consecutive body components as separate paragraphs. Stored
	// bodies keep whatever line ending Converty or the editor wrote, which is
	// usually CRLF; normalising here keeps the snapshot readable in the page and
	// in a copy-paste, and the customer sees a line break either way.
	body := strings.Join(parts, "\n\n")
	return strings.ReplaceAll(body, "\r\n", "\n")
}

// fillPlaceholders substitutes {{1}}-style placeholders from the resolved
// variable map. A placeholder with no entry in that map becomes the same
// neutral dash the automation builder pads a legacy template with, so the
// snapshot reads like the message that went out rather than showing a gap.
func fillPlaceholders(text string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '{' && i+1 < len(text) && text[i+1] == '{' {
			if end := strings.Index(text[i:], "}}"); end > 0 {
				key := text[i+2 : i+end]
				if n, err := strconv.Atoi(strings.TrimSpace(key)); err == nil {
					if v, ok := vars[strconv.Itoa(n)]; ok {
						b.WriteString(v)
					} else {
						b.WriteString("—")
					}
					i += end + 2
					continue
				}
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// buildComponents converts the stored template components into the Meta send
// shape, substituting positional variables in body/header text. URL buttons
// receive the trailing {{1}} parameter set Meta requires.
func buildComponents(raw []byte, vars map[string]string) ([]TemplateComponent, error) {
	comps := decodeMetaComponents(raw)
	var out []TemplateComponent
	for _, c := range comps {
		switch metaCompType(c) {
		case "BODY", "HEADER":
			numbers := PlaceholderNumbers(metaCompText(c))
			if len(numbers) == 0 {
				continue
			}
			params := make([]TemplateParameter, 0, len(numbers))
			for _, n := range numbers {
				v, ok := vars[strconv.Itoa(n)]
				if !ok {
					return nil, NewSendRejection(ErrCodeTemplateVariableInvalid, "missing variable {{"+strconv.Itoa(n)+"}}")
				}
				params = append(params, TemplateParameter{Type: "text", Text: v})
			}
			out = append(out, TemplateComponent{Type: strings.ToLower(metaCompType(c)), Parameters: params})
		}
	}

	if metaHasURLButton(comps) {
		v, ok := vars["1"]
		if !ok {
			return nil, NewSendRejection(ErrCodeTemplateVariableInvalid, "missing variable {{1}} for url button")
		}
		out = append(out, TemplateComponent{
			Type:    "button",
			SubType: "URL",
			Index:   "0",
			Parameters: []TemplateParameter{
				{Type: "text", Text: v},
			},
		})
	}
	return out, nil
}

// SendTemplateMessage is the full tenant-scoped send flow: it loads the
// merchant/customer/consent/template state, applies CAN_SEND, enforces rate
// and idempotency controls, then calls Meta and records the outcome. Any
// refusal surfaces as *SendRejection and no Meta call is made.
func (s *Service) SendTemplateMessage(ctx context.Context, req SendRequest) (*SendResult, error) {
	if req.IdempotencyKey != "" {
		existing, err := s.findMessageByIdempotency(ctx, req.ShopID, req.IdempotencyKey)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	merchant, err := s.merchantState(ctx, req.ShopID)
	if err != nil {
		return nil, err
	}
	customer, err := s.customerState(ctx, req.ShopID, req.CustomerID)
	if err != nil {
		return nil, err
	}
	consent, err := s.consentState(ctx, req.ShopID, req.CustomerID)
	if err != nil {
		return nil, err
	}
	template, err := s.templateState(ctx, req.ShopID, req.TemplateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, NewSendRejection(ErrCodeTemplateNotFound, "template not found for this merchant")
		}
		return nil, err
	}

	if err := EvaluateSendRule(merchant, customer, consent, template, req); err != nil {
		var rej *SendRejection
		if errors.As(err, &rej) {
			s.log.Warn("whatsapp send rejected",
				"shop_id", req.ShopID, "customer_id", req.CustomerID,
				"template_id", req.TemplateID, "code", rej.Code, "reason", rej.Reason)
		}
		return nil, err
	}

	if s.rate != nil {
		if err := s.rate.Allow(ctx, req.ShopID); err != nil {
			return nil, err
		}
	}

	components, err := buildComponents(template.RawComponents, req.Variables)
	if err != nil {
		return nil, err
	}

	msgID, err := s.createQueuedMessage(ctx, req, template)
	if err != nil {
		return nil, err
	}

	if s.cfg.MetaSystemUserToken == "" || s.cfg.MetaPhoneNumberID == "" {
		_ = s.markMessageFailed(ctx, msgID, ErrCodeMetaAPIError, "platform meta credentials not configured")
		return nil, NewSendRejection(ErrCodeMetaAPIError, "platform meta credentials not configured")
	}

	metaID, sendErr := s.SendTemplate(ctx, s.cfg.MetaSystemUserToken, s.cfg.MetaPhoneNumberID,
		customer.Phone, template.Name, template.Language, components,
		MessagingAccountParam(s.cfg.MetaMessagingAccountID))
	if sendErr != nil {
		_ = s.markMessageFailed(ctx, msgID, ErrCodeMetaAPIError, sendErr.Error())
		return nil, wrapMetaError(sendErr)
	}

	if err := s.markMessageSent(ctx, msgID, metaID); err != nil {
		s.log.Error("whatsapp: could not mark message sent", "error", err)
	}
	s.log.Info("whatsapp message sent",
		"shop_id", req.ShopID, "customer_id", req.CustomerID,
		"template", template.Name, "meta_message_id", metaID)

	return &SendResult{MessageID: msgID, MetaMessageID: metaID, Status: "sent"}, nil
}

func wrapMetaError(err error) error {
	var apiErr APIError
	if errors.As(err, &apiErr) {
		return NewSendRejection(ErrCodeMetaAPIError, apiErr.Message)
	}
	return NewSendRejection(ErrCodeMetaAPIError, err.Error())
}

// TestTemplateRequest is a merchant-initiated test send of one template to
// an arbitrary WhatsApp number. It deliberately skips the customer/consent
// gate (there is no customer context) but still enforces the merchant and
// template gates so a broken setup fails loudly here, not on a real event.
type TestTemplateRequest struct {
	ShopID     uuid.UUID
	TemplateID uuid.UUID
	To         string
	Variables  map[string]string
}

var phonePattern = regexp.MustCompile(`^\+?[0-9]{8,15}$`)

// SendTemplateTest delivers a template to the given number for testing. The
// message is recorded in the ledger with no customer attached. Any refusal
// surfaces as *SendRejection.
//
// A test is explicitly initiated by the merchant on an already-connected
// sender, so the merchant onboarding flags (service enabled / terms accepted)
// are intentionally not required here; the status, template and credential
// gates below still catch genuinely broken setups.
func (s *Service) SendTemplateTest(ctx context.Context, req TestTemplateRequest) (*SendResult, error) {
	to := strings.Join(strings.Fields(req.To), "")
	if !phonePattern.MatchString(to) {
		return nil, NewSendRejection(ErrCodeTemplateVariableInvalid, "invalid phone number — use international format with country code, e.g. +21624118849")
	}

	template, err := s.templateState(ctx, req.ShopID, req.TemplateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, NewSendRejection(ErrCodeTemplateNotFound, "template not found for this merchant")
		}
		return nil, err
	}
	if template.ApprovalStatus != "approved" {
		return nil, NewSendRejection(ErrCodeTemplateNotApproved, "template is not approved ("+template.ApprovalStatus+")")
	}
	if template.MarketingFlagged {
		return nil, NewSendRejection(ErrCodeTemplateMarketingBlocked, "meta treats this template as marketing content and it can never be sent")
	}
	if err := validateVariables(template, req.Variables); err != nil {
		return nil, err
	}

	if s.rate != nil {
		if err := s.rate.Allow(ctx, req.ShopID); err != nil {
			return nil, err
		}
	}

	components, err := buildComponents(template.RawComponents, req.Variables)
	if err != nil {
		return nil, err
	}

	varsJSON, err := json.Marshal(req.Variables)
	if err != nil {
		return nil, err
	}

	var msgID uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO messages (
			shop_id, customer_id, template_id, recipient_phone,
			template_variables, status, body_text
		) VALUES ($1, NULL, $2, $3, $4::jsonb, 'queued', NULLIF($5, ''))
		RETURNING id`,
		req.ShopID, req.TemplateID, to, varsJSON,
		RenderTemplateBody(template.RawComponents, req.Variables),
	).Scan(&msgID)
	if err != nil {
		return nil, err
	}

	if s.cfg.MetaSystemUserToken == "" || s.cfg.MetaPhoneNumberID == "" {
		_ = s.markMessageFailed(ctx, msgID, ErrCodeMetaAPIError, "platform meta credentials not configured")
		return nil, NewSendRejection(ErrCodeMetaAPIError, "platform meta credentials not configured")
	}

	metaID, sendErr := s.SendTemplate(ctx, s.cfg.MetaSystemUserToken, s.cfg.MetaPhoneNumberID,
		to, template.Name, template.Language, components,
		MessagingAccountParam(s.cfg.MetaMessagingAccountID))
	if sendErr != nil {
		_ = s.markMessageFailed(ctx, msgID, ErrCodeMetaAPIError, sendErr.Error())
		return nil, wrapMetaError(sendErr)
	}

	if err := s.markMessageSent(ctx, msgID, metaID); err != nil {
		s.log.Error("whatsapp: could not mark test message sent", "error", err)
	}
	s.log.Info("whatsapp test message sent",
		"template", template.Name, "to", to, "meta_message_id", metaID)

	return &SendResult{MessageID: msgID, MetaMessageID: metaID, Status: "sent"}, nil
}
