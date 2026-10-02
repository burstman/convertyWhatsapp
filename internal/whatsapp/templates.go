package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MerchantTemplate is a merchant's template on the shared messaging account,
// tracked with Meta's review lifecycle.
type MerchantTemplate struct {
	ID               uuid.UUID
	ShopID           uuid.UUID
	Name             string
	Language         string
	Category         string
	Status           string // our lifecycle bookkeeping (submitted while awaiting review)
	ApprovalStatus   string // pending | approved | rejected | paused | deleted (Meta authority)
	RejectionReason  string
	MarketingFlagged bool   // Meta warned this template is/will be treated as marketing
	MetaWarnings     string // Meta's own warning text, verbatim, for client display
	MetaTemplateID   string
	Components       []byte // raw Meta components JSONB (also drives variable counting on send)
	NumVariables     int    // placeholder count derived from Components
	Variables        []TokenKey // semantic map in placeholder order (nil == legacy positional)
	Source           string     // converty | delivery | any: the event source this message is written for
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TemplateBody extracts the BODY text of a template's components, used by the
// automations UI to preview what a message looks like. Returns "" when the
// components cannot be parsed or carry no body.
func TemplateBody(t MerchantTemplate) string {
	var comps []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(t.Components, &comps); err != nil {
		return ""
	}
	for _, c := range comps {
		if c.Type == "BODY" {
			return c.Text
		}
	}
	return ""
}

// TemplateDraft is what a merchant submits. Raw components are validated by
// Meta itself on creation; the platform stores them verbatim so the send gate
// can count variables and the frontend can never claim approval.
type TemplateDraft struct {
	Name       string
	Language   string
	Category   string
	Components json.RawMessage
	Variables  []TokenKey // semantic variable map (nil == legacy positional)
	Source     string     // converty | delivery | any (see SourceConverty etc.)
}

// CreateTemplate submits a template to Meta through the platform's central
// account and records the merchant-scoped row. Approval is Meta's to give;
// the platform merely reflects pending/rejected/approved as it comes back.
// Meta's creation-time warnings are captured verbatim; a warning that the
// content is treated as marketing sets MarketingFlagged (and such templates
// are never sendable). Re-submitting the same name+language refreshes the
// existing row.
func (s *Service) CreateTemplate(ctx context.Context, shopID uuid.UUID, draft TemplateDraft) (MerchantTemplate, error) {
	if s.cfg.MetaSystemUserToken == "" || s.cfg.MetaMessagingAccountID == "" {
		return MerchantTemplate{}, &SendRejection{Code: ErrCodeMetaAPIError, Reason: "platform meta credentials not configured"}
	}

	body := map[string]any{
		"name":       draft.Name,
		"language":   draft.Language,
		"category":   draft.Category,
		"components": json.RawMessage(draft.Components),
	}

	var resp struct {
		ID       string `json:"id"`
		Warnings []struct {
			Message string `json:"message"`
		} `json:"warnings"`
	}
	err := s.postJSON(ctx, s.cfg.MetaSystemUserToken,
		fmt.Sprintf("%s/%s/%s/message_templates", s.cfg.MetaGraphURL, metaAPIVersion, s.cfg.MetaMessagingAccountID),
		body, &resp)

	approval := "pending"
	rejection := ""
	warnings := warningStrings(resp.Warnings)
	if err != nil {
		// Meta refused the submission (e.g. name collision, bad variables).
		approval = "rejected"
		rejection = err.Error()
	}

	var t MerchantTemplate
	t.ShopID = shopID
	t.Name = draft.Name
	t.Language = draft.Language
	t.Category = draft.Category
	t.Status = "submitted"
	t.ApprovalStatus = approval
	t.RejectionReason = rejection
	t.MarketingFlagged = warningsIndicateMarketing(warnings)
	t.MetaWarnings = warnings
	t.MetaTemplateID = resp.ID
	t.Components = draft.Components
	t.Variables = draft.Variables
	t.Source = NormalizeSource(draft.Source)

	rowErr := s.pool.QueryRow(ctx, `
		INSERT INTO templates (
			shop_id, meta_template_name, meta_template_id, language, category,
			status, approval_status, rejection_reason, marketing_flagged, meta_warnings,
			marketing_flagged_at, components, variables_map, source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14)
		ON CONFLICT (shop_id, meta_template_name, language) DO UPDATE SET
			meta_template_id   = EXCLUDED.meta_template_id,
			category           = EXCLUDED.category,
			status             = EXCLUDED.status,
			approval_status    = EXCLUDED.approval_status,
			rejection_reason   = EXCLUDED.rejection_reason,
			marketing_flagged  = EXCLUDED.marketing_flagged,
			meta_warnings      = EXCLUDED.meta_warnings,
			marketing_flagged_at = COALESCE(templates.marketing_flagged_at, EXCLUDED.marketing_flagged_at),
			purge_scheduled_at = CASE WHEN NOT EXCLUDED.marketing_flagged THEN NULL ELSE templates.purge_scheduled_at END,
			components         = EXCLUDED.components,
			variables_map      = EXCLUDED.variables_map,
			source             = EXCLUDED.source,
			updated_at         = now()
		RETURNING id, created_at, updated_at`,
		shopID, draft.Name, resp.ID, draft.Language, draft.Category,
		t.Status, approval, rejection, t.MarketingFlagged, t.MetaWarnings,
		flaggedAtExpr(t.MarketingFlagged), draft.Components, draft.Variables, t.Source,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if rowErr != nil {
		return MerchantTemplate{}, fmt.Errorf("store template: %w", rowErr)
	}
	if t.MarketingFlagged {
		if err := s.enqueueUnscheduledPurges(ctx); err != nil {
			s.log.Warn("marketing purge scheduling failed", "error", err)
		}
	}
	if err != nil {
		return t, &SendRejection{Code: ErrCodeMetaAPIError, Reason: rejection}
	}
	return t, nil
}

// flaggedAtExpr yields the column value assigned on insert: now() when Meta
// flagged the template right now, NULL otherwise.
func flaggedAtExpr(flagged bool) any {
	if flagged {
		return time.Now()
	}
	return nil
}

// warningStrings joins Meta's warning messages into a single display string.
func warningStrings(w warnings) string {
	var b strings.Builder
	for i, w := range w {
		if w.Message == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(w.Message)
		if i >= 5 {
			break
		}
	}
	return b.String()
}

// warningsIndicateMarketing reports whether Meta's creation-time warnings say
// the template is (or will be) treated as marketing content. The authoritative
// signal is Meta itself — the platform never guesses from the body text.
func warningsIndicateMarketing(warnings string) bool {
	low := strings.ToLower(warnings)
	if low == "" {
		return false
	}
	if strings.Contains(low, "not a marketing") || strings.Contains(low, "not marketing") ||
		strings.Contains(low, "isn't marketing") || strings.Contains(low, "not considered marketing") {
		return false
	}
	return strings.Contains(low, "marketing") || strings.Contains(low, "promotional")
}

// warnings is the payload subset of Meta's create-response warnings array.
type warnings []struct {
	Message string `json:"message"`
}

// SyncTemplates refreshes the approval state of every merchant template whose
// name+language now exists on the messaging account. Only status/rejection are
// updated — ownership is never touched, and approval never comes from the
// frontend. Templates Meta reports as MARKETING category (or rejected for
// marketing) are also flagged unsendable.
func (s *Service) SyncTemplates(ctx context.Context) (int, error) {
	if s.cfg.MetaSystemUserToken == "" || s.cfg.MetaMessagingAccountID == "" {
		return 0, nil
	}
	templates, err := s.ListTemplates(ctx, s.cfg.MetaSystemUserToken, s.cfg.MetaMessagingAccountID)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, t := range templates {
		marketing := marketingSignal(t.Category, t.RejectedReason)
		tag, uErr := s.pool.Exec(ctx, `
			UPDATE templates
			SET approval_status = $2, updated_at = now(),
			    marketing_flagged = $4,
			    marketing_flagged_at = CASE WHEN $4 THEN COALESCE(marketing_flagged_at, now()) ELSE NULL END,
			    purge_scheduled_at = CASE WHEN $4 THEN purge_scheduled_at ELSE NULL END
			WHERE meta_template_name = $1 AND language = $3
			  AND (approval_status <> $2 OR marketing_flagged <> $4
			       OR (marketing_flagged AND marketing_flagged_at IS NULL))`,
			t.Name, NormalizeApproval(t.Status), t.Language, marketing,
		)
		if uErr != nil {
			return updated, uErr
		}
		updated += int(tag.RowsAffected())
	}
	if err := s.enqueueUnscheduledPurges(ctx); err != nil {
		s.log.Warn("marketing purge scheduling failed after sync", "error", err)
	}
	return updated, nil
}

// marketingSignal is Meta's post-review signal that a template is marketing:
// explicit category, or a rejection that cites the marketing policies.
func marketingSignal(category, rejectedReason string) bool {
	if strings.EqualFold(strings.TrimSpace(category), "MARKETING") {
		return true
	}
	return warningsIndicateMarketing(rejectedReason)
}

// NormalizeApproval maps Meta's uppercase statuses to the stored vocabulary.
func NormalizeApproval(status string) string {
	switch status {
	case "APPROVED":
		return "approved"
	case "PENDING", "IN_APPEAL":
		return "pending"
	case "REJECTED":
		return "rejected"
	case "PAUSED":
		return "paused"
	case "DELETED":
		return "deleted"
	default:
		return "pending"
	}
}

// Templates lists a merchant's templates, newest first.
func (s *Service) Templates(ctx context.Context, shopID uuid.UUID) ([]MerchantTemplate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, shop_id, meta_template_name, language, category, status,
		       approval_status, rejection_reason, marketing_flagged, meta_warnings,
		       meta_template_id, components, COALESCE(variables_map, '[]'::jsonb), source,
		       created_at, updated_at
		FROM templates
		WHERE shop_id = $1
		ORDER BY created_at DESC`,
		shopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MerchantTemplate
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Template returns one merchant template scoped to its shop.
func (s *Service) Template(ctx context.Context, shopID, templateID uuid.UUID) (MerchantTemplate, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, meta_template_name, language, category, status,
		       approval_status, rejection_reason, marketing_flagged, meta_warnings,
		       meta_template_id, components, COALESCE(variables_map, '[]'::jsonb), source,
		       created_at, updated_at
		FROM templates
		WHERE id = $1 AND shop_id = $2`,
		templateID, shopID,
	)
	t, err := scanTemplate(row)
	if err == pgx.ErrNoRows {
		return MerchantTemplate{}, nil
	}
	return t, err
}

type templateScanner interface {
	Scan(dest ...any) error
}

func scanTemplate(row templateScanner) (MerchantTemplate, error) {
	var t MerchantTemplate
	var varJSON []byte
	err := row.Scan(&t.ID, &t.ShopID, &t.Name, &t.Language, &t.Category,
		&t.Status, &t.ApprovalStatus, &t.RejectionReason, &t.MarketingFlagged, &t.MetaWarnings,
		&t.MetaTemplateID, &t.Components, &varJSON, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return MerchantTemplate{}, err
	}
	if len(varJSON) > 0 && string(varJSON) != "[]" && string(varJSON) != "null" {
		_ = json.Unmarshal(varJSON, &t.Variables)
	}
	t.NumVariables = countVariablesFromJSON(t.Components)
	return t, nil
}

// countVariablesFromJSON derives the placeholder count from stored Meta
// components so senders can build the correct variable set without re-parsing.
func countVariablesFromJSON(raw []byte) int {
	return countTemplateVariablesRaw(raw)
}
