package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// metaAPIVersion is the Graph API version the platform targets. The WhatsApp
// Cloud API is currently on v21.0.
const metaAPIVersion = "v21.0"

// TemplateComponent is one variable-bearing part of a template send
// (e.g. a BODY with positional parameters).
type TemplateComponent struct {
	Type       string              `json:"type"`
	SubType    string              `json:"sub_type,omitempty"`
	Index      string              `json:"index,omitempty"`
	Parameters []TemplateParameter `json:"parameters,omitempty"`
}

// TemplateParameter supplies a value for a {{N}} placeholder in a component.
type TemplateParameter struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Template is one approved/available template on a messaging account,
// listed via GET /<messaging_account_id>/message_templates. Components stay
// opaque (json.RawMessage) since their shape is version-specific.
type Template struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Status         string          `json:"status"`
	Category       string          `json:"category"`
	Language       string          `json:"language"`
	RejectedReason string          `json:"rejected_reason"`
	Components     json.RawMessage `json:"components"`
}

// PhoneNumber is a WhatsApp number owned by the messaging account, used for
// the connect-time sanity check.
type PhoneNumber struct {
	ID                     string `json:"id"`
	DisplayPhoneNumber     string `json:"display_phone_number"`
	VerifiedName           string `json:"verified_name"`
	QualityRating          string `json:"quality_rating"`
	NameStatus             string `json:"name_status"`
	CodeVerificationStatus string `json:"code_verification_status"`
}

// MessageNumber is the minimal shape of a number when listing the messaging
// account's phone_numbers connection. PhoneNumber is kept for the
// connect/seed sanity checks (it carries quality/verification fields);
// MessageNumber is what the BSP number-pool importer needs.
type MessageNumber struct {
	ID                 string `json:"id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name"`
}

// ListMessageNumbers returns the phone numbers registered under a messaging
// account — the platform's rentable number pool for the BSP onboarding.
func (s *Service) ListMessageNumbers(ctx context.Context, token, messagingAccountID string) ([]MessageNumber, error) {
	q := url.Values{}
	q.Set("fields", "id,display_phone_number,verified_name")
	q.Set("limit", "100")

	var resp struct {
		Data []MessageNumber `json:"data"`
	}
	if err := s.getJSON(ctx, token, s.cfg.MetaGraphURL+"/"+metaAPIVersion+"/"+messagingAccountID+"/phone_numbers?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Option is a functional option for the Meta build of a message send.
type Option func(map[string]any)

// MessagingAccountParam attaches the messaging_account_id parameter, which is
// optional in Phase 1 but required for multi-messaging-account setups from
// Phase 2 of the new account model.
func MessagingAccountParam(messagingAccountID string) Option {
	return func(m map[string]any) {
		if messagingAccountID != "" {
			m["messaging_account_id"] = messagingAccountID
		}
	}
}

// SendTemplate delivers a template message to a recipient. components must
// cover every {{N}} placeholder defined by the template. It returns the Meta
// message id assigned to the delivery.
func (s *Service) SendTemplate(ctx context.Context, token, phoneNumberID, to, name, language string, components []TemplateComponent, opts ...Option) (string, error) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "template",
		"template": map[string]any{
			"name": name,
			"language": map[string]string{
				"code": language,
			},
		},
	}
	if components != nil {
		payload["template"].(map[string]any)["components"] = components
	}
	for _, opt := range opts {
		opt(payload)
	}

	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.postJSON(ctx, token, fmt.Sprintf("%s/%s/%s/messages", s.cfg.MetaGraphURL, metaAPIVersion, phoneNumberID), payload, &resp); err != nil {
		return "", err
	}
	if len(resp.Messages) == 0 {
		return "", fmt.Errorf("whatsapp send accepted without a message id")
	}
	return resp.Messages[0].ID, nil
}

// SendText delivers a free-form message inside the customer service window
// (24h after the customer's last inbound message). Business Policy forbids
// free-form text outside that window; the UI enforces it before calling.
func (s *Service) SendText(ctx context.Context, token, phoneNumberID, to, text string, opts ...Option) (string, error) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "text",
		"text": map[string]any{
			"preview_url": false,
			"body":        text,
		},
	}
	for _, opt := range opts {
		opt(payload)
	}

	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.postJSON(ctx, token, fmt.Sprintf("%s/%s/%s/messages", s.cfg.MetaGraphURL, metaAPIVersion, phoneNumberID), payload, &resp); err != nil {
		return "", err
	}
	if len(resp.Messages) == 0 {
		return "", fmt.Errorf("whatsapp send accepted without a message id")
	}
	return resp.Messages[0].ID, nil
}

// ListTemplates returns all templates on the messaging account. Categories
// (utility/marketing/authentication) and status are included for UI use.
func (s *Service) ListTemplates(ctx context.Context, token, messagingAccountID string) ([]Template, error) {
	q := url.Values{}
	q.Set("fields", "id,name,status,category,language,rejected_reason,components")
	q.Set("limit", "100")

	var resp struct {
		Data []Template `json:"data"`
	}
	if err := s.getJSON(ctx, token, s.cfg.MetaGraphURL+"/"+metaAPIVersion+"/"+messagingAccountID+"/message_templates?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// DeleteTemplate removes a template from the messaging account by
// name+language.
func (s *Service) DeleteTemplate(ctx context.Context, token, messagingAccountID, name, language string) error {
	q := url.Values{}
	q.Set("name", name)
	q.Set("language", language)
	return s.deleteJSON(ctx, token,
		s.cfg.MetaGraphURL+"/"+metaAPIVersion+"/"+messagingAccountID+"/message_templates?"+q.Encode())
}

// metaUpdateTemplate edits a template in place. Meta only permits this for
// templates in APPROVED, REJECTED or PAUSED state and only for the category and
// components — name and language are immutable, so they are not sent. The
// template id is Meta's own id (stored as templates.meta_template_id).
func (s *Service) metaUpdateTemplate(ctx context.Context, token, metaTemplateID string, components json.RawMessage) error {
	if metaTemplateID == "" {
		return fmt.Errorf("template has no Meta id; refresh the template list first")
	}
	body := map[string]any{"components": json.RawMessage(components)}
	var resp struct {
		ID string `json:"id"`
	}
	return s.postJSON(ctx, token,
		fmt.Sprintf("%s/%s/%s", s.cfg.MetaGraphURL, metaAPIVersion, metaTemplateID), body, &resp)
}

// GetPhoneNumber verifies a phone-number id resolves and returns its identity
// details, for connect/seed-time sanity checks.
func (s *Service) GetPhoneNumber(ctx context.Context, token, phoneNumberID string) (PhoneNumber, error) {
	q := url.Values{}
	q.Set("fields", "id,display_phone_number,verified_name,quality_rating,name_status,code_verification_status")

	var p PhoneNumber
	if err := s.getJSON(ctx, token, s.cfg.MetaGraphURL+"/"+metaAPIVersion+"/"+phoneNumberID+"?"+q.Encode(), &p); err != nil {
		return PhoneNumber{}, err
	}
	return p, nil
}

// APIError carries a Meta Graph error payload.
type APIError struct {
	StatusCode int
	Code       int
	SubCode    int
	Message    string
	Type       string
	Details    string
	FBTraceID  string
}

func (e APIError) Error() string {
	// Meta's message is often just "Invalid parameter"; the subcode and
	// error_data are what actually say which parameter is wrong, so keep them
	// in the text the operator and the logs see.
	msg := fmt.Sprintf("meta api error (status %d): %s", e.StatusCode, e.Message)
	if e.Code != 0 || e.SubCode != 0 {
		msg += fmt.Sprintf(" (code %d", e.Code)
		if e.SubCode != 0 {
			msg += fmt.Sprintf(", subcode %d", e.SubCode)
		}
		msg += ")"
	}
	if e.Details != "" {
		msg += " — " + e.Details
	}
	return msg
}

// graphError holds the Meta Graph error envelope {error:{message,...}}.
type graphError struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		SubCode   int    `json:"error_subcode"`
		ErrorData struct {
			Details string `json:"details"`
		} `json:"error_data"`
		FBTraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

func (s *Service) getJSON(ctx context.Context, token, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return s.doJSON(req, out)
}

// maxInboundMediaBytes caps how large a customer media download we are willing
// to store. Voice notes are under ~1 MB; this leaves room for uploaded audio.
const maxInboundMediaBytes = 25 << 20 // 25 MB

// maxOutboundMediaBytes matches the Cloud API's audio upload ceiling (16 MB).
const maxOutboundMediaBytes = 16 << 20

// UploadMedia uploads a file to the account's media library and returns the
// media id to reference in a message. The caller must pass a Cloud-API media
// mime (audio: AAC/AMR/MP3/M4A/OGG-opus; image: JPEG/PNG/WEBP/GIF); anything
// else is rejected.
//
// The file part must carry the real Content-Type: Meta inspects that header
// and ignores the multipart "type" field, and Go's CreateFormFile would stamp
// application/octet-stream (rejected with "Param file must be a file with...").
func (s *Service) UploadMedia(ctx context.Context, token, phoneNumberID, contentType string, data []byte, filename string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty media upload")
	}
	if len(data) > maxOutboundMediaBytes {
		return "", fmt.Errorf("media too large: %d bytes", len(data))
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("messaging_product", "whatsapp"); err != nil {
		return "", fmt.Errorf("upload field: %w", err)
	}
	if err := mw.WriteField("type", contentType); err != nil {
		return "", fmt.Errorf("upload type: %w", err)
	}
	disposition := mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename})
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", disposition)
	header.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("upload file part: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return "", fmt.Errorf("upload write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("upload close: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/%s/media", s.cfg.MetaGraphURL, metaAPIVersion, phoneNumberID), &buf)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var resp struct {
		ID string `json:"id"`
	}
	if err := s.doJSON(req, &resp); err != nil {
		return "", fmt.Errorf("upload media: %w", err)
	}
	if resp.ID == "" {
		return "", errors.New("uploaded media returned no id")
	}
	return resp.ID, nil
}

// SendAudio delivers an uploaded media id as a WhatsApp audio message. voice
// must only be true for OPUS-in-OGG (a true voice note); the other supported
// containers go out as a basic audio message.
func (s *Service) SendAudio(ctx context.Context, token, phoneNumberID, to, mediaID string, voice bool) (string, error) {
	audio := map[string]any{"id": mediaID}
	if voice {
		audio["voice"] = true
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "audio",
		"audio":             audio,
	}
	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.postJSON(ctx, token,
		fmt.Sprintf("%s/%s/%s/messages", s.cfg.MetaGraphURL, metaAPIVersion, phoneNumberID), payload, &resp); err != nil {
		return "", err
	}
	if len(resp.Messages) == 0 || resp.Messages[0].ID == "" {
		return "", errors.New("audio send returned no message id")
	}
	return resp.Messages[0].ID, nil
}

// SendImage delivers an uploaded media id as a WhatsApp image message. An empty
// caption sends the image on its own.
func (s *Service) SendImage(ctx context.Context, token, phoneNumberID, to, mediaID, caption string) (string, error) {
	image := map[string]any{"id": mediaID}
	if caption != "" {
		image["caption"] = caption
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "image",
		"image":             image,
	}
	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := s.postJSON(ctx, token,
		fmt.Sprintf("%s/%s/%s/messages", s.cfg.MetaGraphURL, metaAPIVersion, phoneNumberID), payload, &resp); err != nil {
		return "", err
	}
	if len(resp.Messages) == 0 || resp.Messages[0].ID == "" {
		return "", errors.New("image send returned no message id")
	}
	return resp.Messages[0].ID, nil
}

// DownloadMedia fetches a customer's media from Meta and returns the bytes plus
// its declared MIME type. The Media API resolves the media id directly (the id
// is a top-level Graph node) to a short-lived download URL, which is then
// streamed — with the same business token on both calls.
func (s *Service) DownloadMedia(ctx context.Context, token, phoneNumberID, mediaID string) ([]byte, string, error) {
	var info struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
	}
	q := url.Values{}
	if phoneNumberID != "" {
		q.Set("phone_number_id", phoneNumberID)
	}
	resolveURL := fmt.Sprintf("%s/%s/%s?%s", s.cfg.MetaGraphURL, metaAPIVersion, mediaID, q.Encode())
	if err := s.getJSON(ctx, token, resolveURL, &info); err != nil {
		return nil, "", fmt.Errorf("resolve media %s: %w", mediaID, err)
	}
	if info.URL == "" {
		return nil, "", fmt.Errorf("media %s resolved to no download url", mediaID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build media request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download media %s: %w", mediaID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var ge graphError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ge)
		return nil, "", fmt.Errorf("download media %s: unexpected status %d", mediaID, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMediaBytes))
	if err != nil {
		return nil, "", fmt.Errorf("read media %s: %w", mediaID, err)
	}
	mime := info.MimeType
	if mime == "" {
		mime = resp.Header.Get("Content-Type")
	}
	return data, mime, nil
}

func (s *Service) deleteJSON(ctx context.Context, token, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var out any
	return s.doJSON(req, &out)
}

func (s *Service) postJSON(ctx context.Context, token, endpoint string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return s.doJSON(req, out)
}

func (s *Service) doJSON(req *http.Request, out any) error {
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("meta request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		var g graphError
		_ = json.Unmarshal(body, &g)
		if g.Error.Message == "" {
			snippet := strings.TrimSpace(string(body))
			if len(snippet) > 300 {
				snippet = snippet[:300]
			}
			g.Error.Message = snippet
		}
		return APIError{
			StatusCode: resp.StatusCode,
			Code:       g.Error.Code,
			SubCode:    g.Error.SubCode,
			Message:    g.Error.Message,
			Type:       g.Error.Type,
			Details:    g.Error.ErrorData.Details,
			FBTraceID:  g.Error.FBTraceID,
		}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
