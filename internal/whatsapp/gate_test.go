package whatsapp

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeIntegrationChecker stands in for the canonical store-state reader so the
// delivery-layer gate can be exercised without a database.
type fakeIntegrationChecker struct {
	active bool
	err    error
	calls  int
	ids    []uuid.UUID
}

func (f *fakeIntegrationChecker) IntegrationActive(_ context.Context, id uuid.UUID) (bool, error) {
	f.calls++
	f.ids = append(f.ids, id)
	return f.active, f.err
}

func serviceWithChecker(c IntegrationChecker) *Service {
	return &Service{integrations: c}
}

// An active store lets the send through the gate.
func TestGateIntegrationAllowsActiveStore(t *testing.T) {
	id := uuid.New()
	checker := &fakeIntegrationChecker{active: true}
	s := serviceWithChecker(checker)

	if err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: id,
	}); err != nil {
		t.Fatalf("active store was refused: %v", err)
	}
	if checker.calls != 1 {
		t.Fatalf("checker calls = %d, want 1", checker.calls)
	}
	if checker.ids[0] != id {
		t.Fatalf("checked id = %s, want %s", checker.ids[0], id)
	}
}

// A deactivated store must refuse the send with the stable rejection code.
func TestGateIntegrationRefusesInactiveStore(t *testing.T) {
	checker := &fakeIntegrationChecker{active: false}
	s := serviceWithChecker(checker)

	err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	})
	if got := rejCode(err); got != ErrCodeIntegrationInactive {
		t.Fatalf("code = %q, want %q (err=%v)", got, ErrCodeIntegrationInactive, err)
	}
}

// A deleted store is indistinguishable from inactive to the gate (the lookup
// returns no active row), and must equally refuse.
func TestGateIntegrationRefusesDeletedStore(t *testing.T) {
	checker := &fakeIntegrationChecker{active: false}
	s := serviceWithChecker(checker)

	err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	})
	if got := rejCode(err); got != ErrCodeIntegrationInactive {
		t.Fatalf("code = %q, want %q (err=%v)", got, ErrCodeIntegrationInactive, err)
	}
}

// A lookup failure must not be turned into a policy rejection: the send is
// retryable, so a plain wrapped error is returned and nothing is delivered.
func TestGateIntegrationReturnsErrorOnLookupFailure(t *testing.T) {
	sentinel := errors.New("db down")
	checker := &fakeIntegrationChecker{err: sentinel}
	s := serviceWithChecker(checker)

	err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	})
	if err == nil {
		t.Fatal("expected an error so the send is retried, got nil")
	}
	if !errors.Is(err, ErrIntegrationUnverifiable) {
		t.Fatalf("error = %v, want it to wrap ErrIntegrationUnverifiable", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the lookup error", err)
	}
	if got := rejCode(err); got != "" {
		t.Fatalf("lookup failure produced a SendRejection (%q); it must be retryable", got)
	}
}

// A queued send is re-checked on every attempt, so a store deactivated between
// attempts must be caught on the retry even though the first attempt passed.
func TestGateIntegrationRechecksOnRetry(t *testing.T) {
	checker := &fakeIntegrationChecker{active: true}
	s := serviceWithChecker(checker)
	req := SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	}

	if err := s.gateIntegration(context.Background(), req); err != nil {
		t.Fatalf("first attempt refused while store active: %v", err)
	}
	checker.active = false
	err := s.gateIntegration(context.Background(), req)
	if got := rejCode(err); got != ErrCodeIntegrationInactive {
		t.Fatalf("retry code = %q, want %q (err=%v)", got, ErrCodeIntegrationInactive, err)
	}
	if checker.calls != 2 {
		t.Fatalf("checker calls = %d, want 2 (one per attempt)", checker.calls)
	}
}

// A reactivated store must be allowed again: the gate reads live state, so a
// deactivate/reactivate cycle does not permanently poison the send.
func TestGateIntegrationAllowsReactivatedStore(t *testing.T) {
	checker := &fakeIntegrationChecker{active: false}
	s := serviceWithChecker(checker)
	req := SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	}

	if got := rejCode(s.gateIntegration(context.Background(), req)); got != ErrCodeIntegrationInactive {
		t.Fatalf("inactive code = %q, want %q", got, ErrCodeIntegrationInactive)
	}
	checker.active = true
	if err := s.gateIntegration(context.Background(), req); err != nil {
		t.Fatalf("reactivated store was refused: %v", err)
	}
}

// A legacy automation job carries no integration id. It cannot be verified, so
// it must be refused rather than delivered unchecked.
func TestGateIntegrationRefusesLegacyAutomationSend(t *testing.T) {
	checker := &fakeIntegrationChecker{active: true}
	s := serviceWithChecker(checker)

	err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:       uuid.New(),
		AutomationID: uuid.New(),
	})
	if got := rejCode(err); got != ErrCodeIntegrationInactive {
		t.Fatalf("code = %q, want %q (err=%v)", got, ErrCodeIntegrationInactive, err)
	}
	if checker.calls != 0 {
		t.Fatalf("legacy send reached the store lookup; it must be refused before")
	}
}

// A send that is not automation-driven and not integration-scoped (a manual or
// legacy API send) is not the gate's concern and must pass.
func TestGateIntegrationAllowsNonIntegrationSend(t *testing.T) {
	checker := &fakeIntegrationChecker{active: false}
	s := serviceWithChecker(checker)

	if err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:     uuid.New(),
		CustomerID: uuid.New(),
	}); err != nil {
		t.Fatalf("non-integration send was refused: %v", err)
	}
	if checker.calls != 0 {
		t.Fatalf("non-integration send consulted the store lookup")
	}
}

// With no checker wired the gate must fail closed for an integration-scoped
// send rather than silently permitting it.
func TestGateIntegrationFailsClosedWithoutChecker(t *testing.T) {
	s := serviceWithChecker(nil)

	err := s.gateIntegration(context.Background(), SendRequest{
		ShopID:        uuid.New(),
		AutomationID:  uuid.New(),
		IntegrationID: uuid.New(),
	})
	if err == nil {
		t.Fatal("expected fail-closed error with no checker configured")
	}
	if !errors.Is(err, ErrIntegrationUnverifiable) {
		t.Fatalf("error = %v, want ErrIntegrationUnverifiable", err)
	}
}
