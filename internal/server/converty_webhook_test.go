package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/converty"
)

// fakeVerifier records whether it was consulted and what it was asked about.
type fakeVerifier struct {
	err   error
	calls int
	got   converty.WebhookEvent
}

func (f *fakeVerifier) VerifyOrderEvent(_ context.Context, shopID, integrationID uuid.UUID, orderID, status, phone string) error {
	f.calls++
	f.got = converty.WebhookEvent{
		ShopID:        shopID,
		IntegrationID: integrationID,
		OrderID:       orderID,
		OrderStatus:   status,
		CustomerPhone: phone,
	}
	return f.err
}

// recordingSink counts how many events reached the automation pipeline.
type recordingSink struct {
	calls int
	got   automations.ConvertyEvent
	err   error
}

func (s *recordingSink) OnConvertyEvent(_ context.Context, e automations.ConvertyEvent) error {
	s.calls++
	s.got = e
	return s.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func attributedEvent() converty.WebhookEvent {
	return converty.WebhookEvent{
		ShopID:            uuid.New(),
		IntegrationID:     uuid.New(),
		IntegrationFound:  true,
		IntegrationActive: true,
		OrderID:           "order-1",
		OrderStatus:       "confirmed",
		CustomerPhone:     "+21600000000",
		CustomerName:      "Hamed",
	}
}

// A payload with no store reference (or an unknown store) is stored for audit
// but must never reach the automation pipeline or even be sent to the verifier.
func TestDispatchDropsUnattributedEvent(t *testing.T) {
	ev := attributedEvent()
	ev.ShopID = uuid.Nil
	ev.IntegrationID = uuid.Nil
	ev.IntegrationFound = false

	verifier := &fakeVerifier{}
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), ev, verifier, sink, discardLogger())

	if sink.calls != 0 {
		t.Fatalf("unattributed event reached the sink %d times", sink.calls)
	}
	if verifier.calls != 0 {
		t.Fatalf("unattributed event was sent to the verifier")
	}
}

// A deactivated integration must not trigger new automations.
func TestDispatchDropsDeactivatedIntegration(t *testing.T) {
	ev := attributedEvent()
	ev.IntegrationActive = false

	verifier := &fakeVerifier{}
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), ev, verifier, sink, discardLogger())

	if sink.calls != 0 {
		t.Fatalf("event from a deactivated integration reached the sink %d times", sink.calls)
	}
	if verifier.calls != 0 {
		t.Fatalf("deactivated integration event was verified (should be dropped first)")
	}
}

// A duplicate body is re-ingested for audit but must not fire automations twice.
func TestDispatchDropsDuplicate(t *testing.T) {
	ev := attributedEvent()
	ev.Duplicate = true

	verifier := &fakeVerifier{}
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), ev, verifier, sink, discardLogger())

	if sink.calls != 0 {
		t.Fatalf("duplicate event reached the sink %d times", sink.calls)
	}
}

// An event that cannot be verified must not reach the sink.
func TestDispatchDropsUnverifiedEvent(t *testing.T) {
	verifier := &fakeVerifier{err: converty.ErrNotVerified}
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), attributedEvent(), verifier, sink, discardLogger())

	if sink.calls != 0 {
		t.Fatalf("unverified event reached the sink %d times", sink.calls)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", verifier.calls)
	}
}

// No verifier configured must fail closed, not open.
func TestDispatchDropsWhenNoVerifier(t *testing.T) {
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), attributedEvent(), nil, sink, discardLogger())

	if sink.calls != 0 {
		t.Fatalf("event reached the sink with no verifier configured")
	}
}

// A verified event from an active, connected store flows through, carrying the
// integration id so a queued send can re-check the store later.
func TestDispatchPassesVerifiedActiveEvent(t *testing.T) {
	ev := attributedEvent()
	verifier := &fakeVerifier{}
	sink := &recordingSink{}
	dispatchConvertyEvent(context.Background(), ev, verifier, sink, discardLogger())

	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", verifier.calls)
	}
	if sink.calls != 1 {
		t.Fatalf("verified active event reached the sink %d times, want 1", sink.calls)
	}
	if sink.got.IntegrationID != ev.IntegrationID {
		t.Fatalf("sink integration id = %s, want %s", sink.got.IntegrationID, ev.IntegrationID)
	}
	if sink.got.OrderID != ev.OrderID || sink.got.OrderStatus != ev.OrderStatus {
		t.Fatalf("sink event = %+v, want order %s/%s", sink.got, ev.OrderID, ev.OrderStatus)
	}
}

// The production verifier has no way to authenticate a delivery, so it refuses
// every event. This is the documented fail-closed posture.
func TestFailClosedVerifierRefuses(t *testing.T) {
	err := converty.FailClosedVerifier{}.VerifyOrderEvent(
		context.Background(), uuid.New(), uuid.New(), "order-1", "confirmed", "+21600000000")
	if !errors.Is(err, converty.ErrVerificationUnavailable) {
		t.Fatalf("err = %v, want ErrVerificationUnavailable", err)
	}
}
