package viewsadmin

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"whatsappconverty/web/views/components"
)

// The credential editor must never render secret values: only whether each
// secret is configured. It must post the save form through the CSRF-guarded
// /admin/whatsapp endpoint.
func TestWhatsappConfigMasksSecrets(t *testing.T) {
	page := WhatsappConfig(
		components.Page{Title: "WhatsApp credentials", IsAdmin: true},
		WhatsappForm{
			PhoneNumberID:      "1302152316314738",
			SystemUserTokenSet: true,
			WebhookTokenSet:    true,
			AppSecretSet:       true,
		},
	)

	var buf bytes.Buffer
	if err := page.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `action="/admin/whatsapp"`) {
		t.Fatalf("save form does not post to /admin/whatsapp")
	}
	if !strings.Contains(html, `name="system_user_token"`) || !strings.Contains(html, `name="app_secret"`) {
		t.Fatalf("secret fields missing")
	}
	if strings.Count(html, "configured") < 3 {
		t.Fatalf("configured secrets are not indicated as configured")
	}
	if strings.Contains(html, "value=\"\"") && strings.Contains(html, `type="password" value=`) {
		t.Fatalf("secret field must not carry a value attribute")
	}
}
