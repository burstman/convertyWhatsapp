package automations

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"whatsappconverty/internal/whatsapp"
)

// The integration id must survive the job -> request conversion, otherwise the
// delivery-layer gate would see an empty id and refuse every real send.
func TestToSendRequestCarriesIntegrationID(t *testing.T) {
	integrationID := uuid.New()
	automationID := uuid.New()
	job := whatsapp.SendWhatsAppTemplateJob{
		ShopID:        uuid.New(),
		IntegrationID: integrationID,
		AutomationID:  automationID,
		CustomerID:    uuid.New(),
		TemplateID:    uuid.New(),
	}

	req := toSendRequest(job)
	if req.IntegrationID != integrationID {
		t.Fatalf("IntegrationID = %s, want %s", req.IntegrationID, integrationID)
	}
	if req.AutomationID != automationID {
		t.Fatalf("AutomationID = %s, want %s", req.AutomationID, automationID)
	}
}

// A legacy queued payload has no integration id. The plan is to refuse it
// (fail closed), so decoding must not invent one.
func TestLegacyJobWithoutIntegrationDecodesToNil(t *testing.T) {
	payload := []byte(`{"shop_id":"` + uuid.New().String() +
		`","automation_id":"` + uuid.New().String() +
		`","customer_id":"` + uuid.New().String() +
		`","template_id":"` + uuid.New().String() + `"}`)

	var job whatsapp.SendWhatsAppTemplateJob
	if err := json.Unmarshal(payload, &job); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if job.IntegrationID != uuid.Nil {
		t.Fatalf("IntegrationID = %s, want nil", job.IntegrationID)
	}
}
