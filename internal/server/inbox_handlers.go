package server

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/i18n"
	"whatsappconverty/internal/whatsapp"
	vadmin "whatsappconverty/web/views/admin"
	viewshared "whatsappconverty/web/views/components"
)

// maxSendAudioBytes is the Cloud API ceiling for outbound audio (16 MB). The
// parser is allowed one extra megabyte to fit the multipart overhead.
const maxSendAudioBytes = 16 << 20

// maxSendImageBytes is the Cloud API ceiling for outbound images (5 MB).
const maxSendImageBytes = 5 << 20

// inboxFlash maps the ?flash= query value onto a user-facing line for the
// inbox list page.
func inboxFlash(dict *i18n.Dict, q string) string {
	switch q {
	case "sent":
		return dict.T("inbox.replySent")
	case "notfound":
		return dict.T("inbox.flashNotFound")
	case "error":
		return dict.T("inbox.flashError")
	}
	return ""
}

var inboxThreadPathRe = regexp.MustCompile(`/inbox/([0-9a-fA-F-]{36})`)

// inboxDict is the language table for inbox fragments rendered outside a full
// page. The platform console is English-only for now.
func (a *App) inboxDict(k *kit.Kit) *i18n.Dict {
	return i18n.New(i18n.En)
}

// adminInboxPage builds the shared page header for inbox screens.
func (a *App) adminInboxPage(k *kit.Kit) viewshared.Page {
	principal := auth.FromKit(k)
	return viewshared.Page{
		Title:    "Inbox",
		Active:   "inbox",
		ShopName: "Platform",
		UserName: principal.User.Name,
		IsAdmin:  true,
		I18N:     i18n.New(i18n.En),
	}
}

// activeInboxID recovers which conversation is open from htmx's current URL
// header, so the polling sidebar can keep that row highlighted.
func activeInboxID(k *kit.Kit) uuid.UUID {
	m := inboxThreadPathRe.FindStringSubmatch(k.Request.Header.Get("HX-Current-URL"))
	if len(m) < 2 {
		return uuid.Nil
	}
	id, err := uuid.Parse(m[1])
	if err != nil {
		return uuid.Nil
	}
	return id
}

// inboxThreads loads the platform's conversations and marks the page's unread
// badge.
func (a *App) inboxThreads(k *kit.Kit, page *viewshared.Page) ([]whatsapp.Conversation, error) {
	threads, err := a.WhatsApp.Threads(k.Request.Context())
	if err != nil {
		return nil, err
	}
	page.UnreadInbox = totalUnread(threads)
	return threads, nil
}

func totalUnread(threads []whatsapp.Conversation) int {
	n := 0
	for _, t := range threads {
		n += t.UnreadCount
	}
	return n
}

// handleInbox lists the customer conversations on the left and (if one is
// open) shows it on the right.
func (a *App) handleInbox(k *kit.Kit) error {
	page := a.adminInboxPage(k)

	threads, err := a.inboxThreads(k, &page)
	if err != nil {
		a.Log.Error("inbox threads failed", "error", err.Error())
		return k.Render(vadmin.InboxPage(page, nil, nil, page.T("inbox.flashError")))
	}

	return k.Render(vadmin.InboxPage(page, threads, nil, inboxFlash(page.I18N, k.Request.URL.Query().Get("flash"))))
}

// handleInboxSidebar is the polling response for the conversation list: the
// sidebar rows plus the nav unread badge.
func (a *App) handleInboxSidebar(k *kit.Kit) error {
	page := a.adminInboxPage(k)

	threads, err := a.inboxThreads(k, &page)
	if err != nil {
		return k.Text(http.StatusOK, "")
	}
	return k.Render(vadmin.InboxSidebar(page.I18N, threads, activeInboxID(k)))
}

// handleInboxThread shows one conversation with its messages and reply box. On
// an htmx request only the thread card (plus out-of-band sidebar and badge
// updates) is returned, so opening a conversation from the list is a single
// swap; a plain request renders the full two-pane page.
func (a *App) handleInboxThread(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	}

	page := a.adminInboxPage(k)

	conv, msgs, err := a.WhatsApp.Thread(k.Request.Context(), id)
	if err != nil {
		if errors.Is(err, whatsapp.ErrConversationNotFound) {
			return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
		}
		a.Log.Error("inbox thread failed", "id", id, "error", err.Error())
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
	}

	if err := a.WhatsApp.MarkThreadRead(k.Request.Context(), id); err != nil {
		a.Log.Warn("inbox mark read failed", "id", id, "error", err.Error())
	}

	threads, err := a.inboxThreads(k, &page)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
	}

	ok := ""
	if k.Request.URL.Query().Get("flash") == "sent" {
		ok = page.T("inbox.replySent")
	}

	if k.Request.Header.Get("HX-Request") == "true" {
		return k.Render(vadmin.InboxThreadSwap(page, conv, msgs, "", ok, threads, id))
	}

	return k.Render(vadmin.InboxPage(page, threads, &vadmin.ActiveInboxThread{Conv: conv, Msgs: msgs, Ok: ok}, ""))
}

// handleInboxFragment is the polling endpoint: the thread's bubbles only, so
// the open reply form keeps its focus while new inbound appears.
func (a *App) handleInboxFragment(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Text(http.StatusOK, "")
	}
	_, msgs, err := a.WhatsApp.Thread(k.Request.Context(), id)
	if err != nil {
		return k.Text(http.StatusOK, "")
	}
	return k.Render(vadmin.BubblesList(a.inboxDict(k), id, msgs))
}

// explanationForReply turns a failed reply into a line the operator can act
// on, mirroring the template-rejection explanations.
func explanationForReply(dict *i18n.Dict, err error) string {
	switch {
	case errors.Is(err, whatsapp.ErrReplyWindowClosed):
		return dict.T("inbox.explWinClosed")
	case errors.Is(err, whatsapp.ErrNotConfigured):
		return dict.T("inbox.explNotConfigured")
	default:
		return dict.T("inbox.explFailed") + " " + err.Error()
	}
}

// handleInboxReply sends a free-form reply inside the service window. On
// success it responds with just the new bubble as an OOB swap so htmx appends
// it to #thread-bubbles without rebuilding the chat; errors re-render the whole
// card so the recorded failed bubble / banner shows.
func (a *App) handleInboxReply(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	}

	page := a.adminInboxPage(k)
	body := k.Request.FormValue("body")

	conv, msgs, err := a.WhatsApp.Thread(k.Request.Context(), id)
	if err != nil {
		if errors.Is(err, whatsapp.ErrConversationNotFound) {
			return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
	}

	errMsg, okMsg := "", ""
	_, sendErr := a.WhatsApp.SendReply(k.Request.Context(), id, body)
	switch {
	case sendErr == nil:
		okMsg = page.T("inbox.replySent")
		// Re-fetch so the outgoing bubble shows up in the swapped card right away
		// instead of waiting for the fragment poll.
		conv, msgs, _ = a.WhatsApp.Thread(k.Request.Context(), id)
	case errors.Is(sendErr, whatsapp.ErrConversationNotFound):
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	default:
		a.Log.Warn("inbox reply failed", "id", id, "error", sendErr.Error())
		errMsg = explanationForReply(page.I18N, sendErr)
		// Re-fetch so a recorded failed bubble (e.g. a meta rejection) shows.
		conv, msgs, _ = a.WhatsApp.Thread(k.Request.Context(), id)
	}

	if k.Request.Header.Get("HX-Request") == "" {
		if errMsg == "" {
			return k.Redirect(http.StatusSeeOther, "/inbox/"+id.String()+"?flash=sent")
		}
		// Non-htmx fallback: render the two-pane page with the error inline.
		threads, terr := a.inboxThreads(k, &page)
		if terr != nil {
			return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
		}
		return k.Render(vadmin.InboxPage(page, threads, &vadmin.ActiveInboxThread{Conv: conv, Msgs: msgs, Err: errMsg, Ok: okMsg}, ""))
	}
	if errMsg == "" && len(msgs) > 0 && msgs[len(msgs)-1].Direction == "outbound" {
		return k.Render(vadmin.OOBNewBubble(page.I18N, conv.ID, msgs[len(msgs)-1]))
	}
	return k.Render(vadmin.ThreadCard(page, conv, msgs, errMsg, okMsg))
}

// handleInboxReplyAudio sends an operator audio recording or file to the
// customer. The web composer POSTs a multipart form; the new voice-note bubble
// is returned as an OOB swap so the chat does not reload.
func (a *App) handleInboxReplyAudio(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	}

	page := a.adminInboxPage(k)

	_, _, err = a.WhatsApp.Thread(k.Request.Context(), id)
	if err != nil {
		if errors.Is(err, whatsapp.ErrConversationNotFound) {
			return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
	}

	k.Request.Body = http.MaxBytesReader(k.Response, k.Request.Body, maxSendAudioBytes+1<<20)
	if err := k.Request.ParseMultipartForm(maxSendAudioBytes + 1<<20); err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.audioTooLarge"))
	}

	file, hdr, err := k.Request.FormFile("audio")
	if err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.missingAudio"))
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.readAudio"))
	}
	if len(data) == 0 || len(data) > maxSendAudioBytes {
		return k.Text(http.StatusBadRequest, page.T("inbox.audioBadSize"))
	}
	mime := canonicalSendAudioMime(hdr.Header.Get("Content-Type"))
	if mime == "" {
		return k.Text(http.StatusBadRequest, page.T("inbox.badAudioFmt"))
	}

	durationMS, _ := strconv.Atoi(k.Request.FormValue("duration_ms"))
	if durationMS < 0 || durationMS > 24*60*60*1000 {
		durationMS = 0
	}

	errMsg, okMsg := "", ""
	_, asendErr := a.WhatsApp.SendAudioReply(k.Request.Context(), id, mime, audioFilename(hdr.Filename, mime), data, durationMS)
	switch {
	case asendErr == nil:
		okMsg = page.T("inbox.voiceSent")
	case errors.Is(asendErr, whatsapp.ErrConversationNotFound):
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	default:
		a.Log.Warn("inbox audio reply failed", "id", id, "error", asendErr.Error())
		errMsg = explanationForReply(page.I18N, asendErr)
	}

	conv, msgs, _ := a.WhatsApp.Thread(k.Request.Context(), id)
	if errMsg == "" && len(msgs) > 0 && msgs[len(msgs)-1].Direction == "outbound" {
		return k.Render(vadmin.OOBNewBubble(page.I18N, conv.ID, msgs[len(msgs)-1]))
	}
	return k.Render(vadmin.ThreadCard(page, conv, msgs, errMsg, okMsg))
}

// handleInboxReplyImage sends an attached photo to the customer. The web
// composer POSTs a multipart form; the new image bubble is returned as an OOB
// swap so the chat does not reload.
func (a *App) handleInboxReplyImage(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	}

	page := a.adminInboxPage(k)

	_, _, err = a.WhatsApp.Thread(k.Request.Context(), id)
	if err != nil {
		if errors.Is(err, whatsapp.ErrConversationNotFound) {
			return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=error")
	}

	k.Request.Body = http.MaxBytesReader(k.Response, k.Request.Body, maxSendImageBytes+1<<20)
	if err := k.Request.ParseMultipartForm(maxSendImageBytes + 1<<20); err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.imageTooLarge"))
	}

	file, hdr, err := k.Request.FormFile("image")
	if err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.missingImage"))
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return k.Text(http.StatusBadRequest, page.T("inbox.readImage"))
	}
	if len(data) == 0 || len(data) > maxSendImageBytes {
		return k.Text(http.StatusBadRequest, page.T("inbox.imageBadSize"))
	}
	mime := canonicalSendImageMime(hdr.Header.Get("Content-Type"))
	if mime == "" {
		return k.Text(http.StatusBadRequest, page.T("inbox.badImageFmt"))
	}

	errMsg, okMsg := "", ""
	_, isendErr := a.WhatsApp.SendImageReply(k.Request.Context(), id, mime, imageFilename(mime), data, "")
	switch {
	case isendErr == nil:
		okMsg = page.T("inbox.imageSent")
	case errors.Is(isendErr, whatsapp.ErrConversationNotFound):
		return k.Redirect(http.StatusSeeOther, "/inbox?flash=notfound")
	default:
		a.Log.Warn("inbox image reply failed", "id", id, "error", isendErr.Error())
		errMsg = explanationForReply(page.I18N, isendErr)
	}

	conv, msgs, _ := a.WhatsApp.Thread(k.Request.Context(), id)
	if errMsg == "" && len(msgs) > 0 && msgs[len(msgs)-1].Direction == "outbound" {
		return k.Render(vadmin.OOBNewBubble(page.I18N, conv.ID, msgs[len(msgs)-1]))
	}
	return k.Render(vadmin.ThreadCard(page, conv, msgs, errMsg, okMsg))
}

// canonicalSendImageMime maps any upload content type onto the Cloud API's
// supported image set (GIF is NOT accepted), or "" when the format cannot be
// sent to WhatsApp.
func canonicalSendImageMime(mime string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch base {
	case "image/jpg", "image/jpeg", "image/pjpeg":
		return "image/jpeg"
	case "image/png":
		return "image/png"
	case "image/webp":
		return "image/webp"
	default:
		return ""
	}
}

// imageFilename keeps a safe, correctly-extended upload name for Meta.
func imageFilename(mime string) string {
	exts := map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"}
	return "photo." + exts[mime]
}

// canonicalSendAudioMime maps any upload content type onto the Cloud API's
// supported audio set, or "" when the format cannot be sent to WhatsApp.
func canonicalSendAudioMime(mime string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch base {
	case "audio/ogg", "audio/opus":
		return "audio/ogg"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/m4b":
		return "audio/mp4"
	case "audio/mpeg", "audio/mp3", "audio/mpga":
		return "audio/mpeg"
	case "audio/aac":
		return "audio/aac"
	case "audio/amr", "audio/amr-nb", "audio/x-amr":
		return "audio/amr"
	default:
		return ""
	}
}

// audioFilename keeps a safe, correctly-extended upload name for Meta.
func audioFilename(original, mime string) string {
	exts := map[string]string{"audio/ogg": "ogg", "audio/mp4": "m4a", "audio/mpeg": "mp3", "audio/aac": "aac", "audio/amr": "amr"}
	name := "voice." + exts[mime]
	if original != "" {
		orig := regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(original, "_")
		if len(orig) > 80 {
			orig = orig[:80]
		}
		if orig != "" {
			name = orig
		}
	}
	return name
}

// handleInboxRead marks an open thread read.
func (a *App) handleInboxRead(k *kit.Kit) error {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Text(http.StatusOK, "")
	}
	if err := a.WhatsApp.MarkThreadRead(k.Request.Context(), id); err != nil {
		a.Log.Warn("inbox mark read failed", "id", id, "error", err.Error())
	}
	return k.Text(http.StatusOK, "")
}

// handleChatMedia streams a stored inbound voice note back to the inbox. The
// message must belong to the conversation in the URL, and its 30-day retention
// may have already cleared the bytes.
func (a *App) handleChatMedia(k *kit.Kit) error {
	convID, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Text(http.StatusNotFound, "")
	}
	msgID, err := uuid.Parse(chi.URLParam(k.Request, "msg"))
	if err != nil {
		return k.Text(http.StatusNotFound, "")
	}
	data, mime, err := a.WhatsApp.MediaForMessage(k.Request.Context(), convID, msgID)
	if err != nil || len(data) == 0 {
		return k.Text(http.StatusNotFound, "")
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	k.Response.Header().Set("Content-Type", mime)
	k.Response.Header().Set("X-Content-Type-Options", "nosniff")
	k.Response.Header().Set("Cache-Control", "private, no-store")
	k.Response.WriteHeader(http.StatusOK)
	_, err = k.Response.Write(data)
	return err
}
