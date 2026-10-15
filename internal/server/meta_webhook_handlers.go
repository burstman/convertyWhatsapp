package server

import (
	"io"
	"net/http"

	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/whatsapp"
)

// handleMetaWebhookVerify answers Meta's GET subscription handshake. The
// configured hub.verify_token must match, otherwise Meta's subscribe call
// fails and nothing is registered.
func (a *App) handleMetaWebhookVerify(k *kit.Kit) error {
	q := k.Request.URL.Query()
	mode := q.Get("hub.mode")
	token := q.Get("hub.verify_token")
	challenge := q.Get("hub.challenge")

	if !whatsapp.VerifyWebhookChallenge(a.Meta.Get().WebhookVerifyToken, mode, token, challenge) {
		a.Log.Warn("meta webhook verify challenge rejected", "mode", mode)
		return k.Text(http.StatusForbidden, "verification failed")
	}
	return k.Text(http.StatusOK, challenge)
}

// handleMetaWebhook is the POST target Meta delivers status updates to. Only
// deliveries carrying a valid X-Hub-Signature-256 are processed; raw payloads
// are never returned to any merchant. Status updates are idempotent and only
// ever move a message forward.
func (a *App) handleMetaWebhook(k *kit.Kit) error {
	body, err := io.ReadAll(io.LimitReader(k.Request.Body, webhookBodyLimitBytes))
	if err != nil {
		return err
	}

	appSecret := a.Meta.Get().AppSecret
	if appSecret == "" {
		a.Log.Error("meta webhook: app secret not configured")
		return k.Text(http.StatusInternalServerError, "not configured")
	}
	sig := k.Request.Header.Get("X-Hub-Signature-256")
	if !whatsapp.VerifyWebhookSignature(appSecret, sig, body) {
		a.exposeRemoteIP(k)
		a.Log.Warn("meta webhook signature verification failed")
		return k.Text(http.StatusForbidden, "forbidden")
	}

	updates, err := whatsapp.ParseWebhook(body)
	if err != nil {
		a.Log.Warn("meta webhook parse failed", "error", err.Error())
		return k.Text(http.StatusOK, "ok") // acknowledge; nothing actionable here
	}

	for _, u := range updates {
		if applyErr := a.WhatsApp.ApplyStatusUpdate(k.Request.Context(), u); applyErr != nil {
			a.Log.Warn("meta webhook status apply failed",
				"meta_message_id", u.MetaMessageID, "status", u.Status, "error", applyErr.Error())
			continue
		}
		a.Log.Info("meta webhook status applied",
			"meta_message_id", u.MetaMessageID, "status", u.Status,
			"errors", len(u.Errors))
	}

	return k.Text(http.StatusOK, "EVENT_RECEIVED")
}

// exposeRemoteIP logs the caller's IP for abuse watching without echoing the
// payload (Raw Meta webhook content is never echoed back to callers).
func (a *App) exposeRemoteIP(k *kit.Kit) {
	a.Log.Warn("meta webhook consumer ip", "remote", k.Request.RemoteAddr)
}
