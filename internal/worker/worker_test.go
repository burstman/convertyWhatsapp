package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"whatsappconverty/internal/whatsapp"
)

type fakeLookup struct {
	active bool
	err    error
	calls  int
	gotID  uuid.UUID
}

func (f *fakeLookup) AutomationEnabled(_ context.Context, id uuid.UUID) (bool, error) {
	f.calls++
	f.gotID = id
	return f.active, f.err
}

// sendPayload is a queued send as the runner hands it to a handler.
func sendPayload(t *testing.T, job whatsapp.SendWhatsAppTemplateJob) []byte {
	t.Helper()
	payload, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return payload
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A send queued while the automation was on must not go out after a pause, and
// the job itself is what the gate must consult — not any cached trigger state.
func TestGateDropsSendForPausedAutomation(t *testing.T) {
	automationID := uuid.New()
	lookup := &fakeLookup{active: false}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:       uuid.New(),
		AutomationID: automationID,
		CustomerID:   uuid.New(),
		TemplateID:   uuid.New(),
	}))
	if err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 0 {
		t.Fatalf("paused automation send was delivered (%d sends)", sent)
	}
	if lookup.calls != 1 || lookup.gotID != automationID {
		t.Fatalf("automation lookup: calls=%d id=%s, want 1 call for %s", lookup.calls, lookup.gotID, automationID)
	}
}

// A send with no automation (manual or legacy) must pass straight through.
func TestGatePassesNonAutomationSend(t *testing.T) {
	lookup := &fakeLookup{}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	if err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:     uuid.New(),
		CustomerID: uuid.New(),
		TemplateID: uuid.New(),
	})); err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("non-automation send was not delivered")
	}
	if lookup.calls != 0 {
		t.Fatalf("automation was consulted for a non-automation send")
	}
}

// A failed enabled-lookup must not drop the customer's message: send on and
// let the send gate decide.
func TestGateSendsWhenLookupFails(t *testing.T) {
	lookup := &fakeLookup{err: errors.New("db down")}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:       uuid.New(),
		AutomationID: uuid.New(),
		CustomerID:   uuid.New(),
		TemplateID:   uuid.New(),
	}))
	if err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("send after failed lookup was dropped")
	}
}
