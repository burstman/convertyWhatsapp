package whatsapp

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// TokenKey identifies an auto-filled message variable, shown in the template
// editor as a draggable chip and serialized in the body as {{token}}.
type TokenKey string

const (
	TokenCustomerName  TokenKey = "customer_name"
	TokenCustomerPhone TokenKey = "customer_phone"
	TokenOrderID       TokenKey = "order_id"
	TokenOrderStatus   TokenKey = "order_status"
	TokenTrackingCode  TokenKey = "tracking_code"
	TokenDriverName    TokenKey = "driver_name"
	TokenDriverPhone   TokenKey = "driver_phone"
)

// Event sources a template can be written for. SourceAny is the neutral
// default: the template may be attached to an automation of any source.
const (
	SourceConverty = "converty"
	SourceDelivery = "delivery"
	SourceAny      = "any"
)

// SourceOption is one entry in the editor's "this message is for" picker.
type SourceOption struct {
	Value string
	Label string
	Hint  string
}

// SourceOptions are the source choices offered when creating a template.
func SourceOptions() []SourceOption {
	return []SourceOption{
		{Value: SourceConverty, Label: "Converty orders", Hint: "Fires on order status changes from Converty"},
		{Value: SourceDelivery, Label: "Delivery", Hint: "Fires on parcel status changes from a delivery provider"},
		{Value: SourceAny, Label: "Both", Hint: "Usable by either kind of automation"},
	}
}

// SourceLabel renders a stored source for display.
func SourceLabel(source string) string {
	switch source {
	case SourceConverty:
		return "Converty"
	case SourceDelivery:
		return "Delivery"
	case SourceAny, "":
		return "Both"
	}
	return source
}

// NormalizeSource maps anything unrecognized to the neutral default, so a bad
// form value can never leave a template unattached.
func NormalizeSource(source string) string {
	switch source {
	case SourceConverty, SourceDelivery, SourceAny:
		return source
	}
	return SourceAny
}

// TokenAllowed reports whether a variable can appear in a template written for
// a given source. Driver details only exist on the delivery side, so a
// Converty template that used them would send an empty slot to the customer.
func TokenAllowed(source string, k TokenKey) bool {
	if source == SourceDelivery || source == SourceAny {
		return true
	}
	return k != TokenDriverName && k != TokenDriverPhone
}

// VariableChip is the palette entry rendered in the template editor. Keep the
// order here: it is the order chips are offered to the author (not the order
// they land in the body — that is decided by dropping).
func VariableChips() []VariableChip {
	return []VariableChip{
		{Value: string(TokenCustomerName), Label: "Customer name", Example: "Karima"},
		{Value: string(TokenCustomerPhone), Label: "Customer phone", Example: "+216 20 000 000"},
		{Value: string(TokenOrderID), Label: "Order id", Example: "ORD-12345"},
		{Value: string(TokenOrderStatus), Label: "Order status", Example: "out for delivery"},
		{Value: string(TokenTrackingCode), Label: "Tracking code", Example: "1234567890113"},
		{Value: string(TokenDriverName), Label: "Driver name", Example: "Ali Mansour"},
		{Value: string(TokenDriverPhone), Label: "Driver phone", Example: "+216 98 111 222"},
	}
}

// VariableChip is one draggable variable the author can drop into the body.
type VariableChip struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Example string `json:"example"`
}

func (k TokenKey) label() string {
	for _, c := range VariableChips() {
		if c.Value == string(k) {
			return c.Label
		}
	}
	return strings.TrimPrefix(string(k), "_")
}

// TokenLabel is the chip's human label for a token ("Driver name"). The send
// history names the missing tokens this way, so a merchant reads what the
// template was waiting for instead of a database key.
func TokenLabel(k TokenKey) string { return k.label() }

// TemplateVariableValues is the resolved per-send data the map draws from.
type TemplateVariableValues struct {
	CustomerName  string
	CustomerPhone string
	OrderID       string
	StatusLabel   string
	TrackingCode  string
	DriverName    string
	DriverPhone   string
}

// TokenValue resolves one variable to its send-time value. Unknown tokens
// resolve to "", which the sender replaces with a neutral placeholder.
func TokenValue(k TokenKey, v TemplateVariableValues) string {
	switch k {
	case TokenCustomerName:
		return v.CustomerName
	case TokenCustomerPhone:
		return v.CustomerPhone
	case TokenOrderID:
		return v.OrderID
	case TokenOrderStatus:
		return v.StatusLabel
	case TokenTrackingCode:
		return v.TrackingCode
	case TokenDriverName:
		return v.DriverName
	case TokenDriverPhone:
		return v.DriverPhone
	}
	return ""
}

// TokenExample returns a sensible sample value for a token, used by the
// editor's example-value prefill and the message preview.
func TokenExample(k TokenKey) string {
	for _, c := range VariableChips() {
		if c.Value == string(k) {
			return c.Example
		}
	}
	return ""
}

// DefaultTokenForPosition is the legacy positional mapping kept for templates
// written with {{1..N}} before semantic chips existed.
func DefaultTokenForPosition(position int) TokenKey {
	switch position {
	case 1:
		return TokenCustomerName
	case 2:
		return TokenOrderID
	case 3:
		return TokenOrderStatus
	}
	return ""
}

var tokenPattern = regexp.MustCompile(`\{\{([a-zA-Z][a-zA-Z0-9_]*)\}\}`)
var positionalPattern = regexp.MustCompile(`\{\{([0-9]+)\}\}`)

// MinStaticWordsPerVariable is Meta's review rule: a body must carry static
// words around the variables or it is refused as a blank container that could
// carry any content. Verified against the API: one variable needs at least
// three static words, and clustered variables ("Name: {{1}}, Phone: {{2}}")
// are rejected outright with subcode 2388293.
const MinStaticWordsPerVariable = 3

// ExplainTemplateRejection turns Meta's terse template-creation failures into
// something the operator can act on. Verified subcodes: 2388293 (too little
// static text around the variables) and 2388299 (a variable opens or closes
// the body). Returns "" for anything not recognized.
func ExplainTemplateRejection(err error) string {
	var api APIError
	sub := 0
	if errors.As(err, &api) {
		sub = api.SubCode
	} else {
		var ptr *APIError
		if !errors.As(err, &ptr) {
			return ""
		}
		sub = ptr.SubCode
	}
	switch sub {
	case 2388293:
		return "Meta wants more words around the variables — at least " +
			strconv.Itoa(MinStaticWordsPerVariable) + " words of message per variable."
	case 2388299:
		return "A variable cannot be the first or last thing in the message. Open and close with words, e.g. \"... Merci!\""
	}
	return ""
}

// ValidateTemplateBody checks a positional template body against the two rules
// Meta enforces on creation, so the operator gets an actionable message instead
// of a bare "Invalid parameter": a variable may not open or close the body
// (subcode 2388299) and the static text must be at least
// MinStaticWordsPerVariable words per variable (subcode 2388293).
func ValidateTemplateBody(positional string) error {
	last := positionalPattern.FindAllStringIndex(strings.TrimSpace(positional), -1)
	if len(last) == 0 {
		return nil
	}

	trimmed := strings.TrimSpace(positional)
	if m := positionalPattern.FindStringIndex(trimmed); m != nil && m[0] == 0 {
		return fmt.Errorf("start the message with a few words before the %s variable", placeholderLabel(trimmed, m[0]))
	}
	if tail := trimmed[last[len(last)-1][1]:]; !isStaticTail(tail) {
		return fmt.Errorf("end the message with a few words after the %s variable (e.g. \". Merci!\")",
			placeholderLabel(trimmed, last[len(last)-1][0]))
	}

	static := countStaticWords(positionalPattern.ReplaceAllString(positional, " "))
	need := MinStaticWordsPerVariable * len(last)
	if static < need {
		return fmt.Errorf("Meta wants at least %d words of text around the variables — this message has %d. Write the sentence out (e.g. \"Your order {{1}} is on its way, tracking {{2}}\")",
			need, static)
	}
	return nil
}

// isStaticTail reports whether the text following the last variable counts as
// static content. Neither whitespace nor sentence-ending punctuation does:
// Meta refuses "Tel: {{1}}", "Tel: {{1}}." and "Tel: {{1}}!" with subcode
// 2388299, while "{{1}}," and "{{1}}. Merci!" are both accepted.
func isStaticTail(tail string) bool {
	rest := strings.TrimRightFunc(strings.TrimSpace(tail), func(r rune) bool {
		return r == '.' || r == '!' || r == '?'
	})
	return strings.TrimSpace(rest) != ""
}

func placeholderLabel(text string, at int) string {
	m := positionalPattern.FindString(text[at:])
	if m == "" {
		return "first"
	}
	return strings.Trim(m, "{}")
}

func countStaticWords(text string) int {
	return len(strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))
}

// TokenizeTemplateBody converts a semantic body (with {{customer_name}} chips)
// into Meta's positional {{1..N}} body and returns the ordered token keys that
// map to each position. Re-using the same chip re-uses its position. A body
// with no semantic chips is returned unchanged with nil tokens (legacy).
func TokenizeTemplateBody(text string) (positional string, tokens []TokenKey) {
	if text == "" {
		return text, nil
	}

	seen := map[TokenKey]int{}
	var b strings.Builder
	next := 1
	last := 0
	for _, m := range tokenPattern.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:m[0]])
		last = m[1]
		key := TokenKey(text[m[2]:m[3]])
		pos, ok := seen[key]
		if !ok {
			pos = next
			next++
			seen[key] = pos
			tokens = append(tokens, key)
		}
		b.WriteString(fmt.Sprintf("{{%d}}", pos))
	}
	b.WriteString(text[last:])

	if len(tokens) == 0 {
		return text, nil
	}
	return b.String(), tokens
}