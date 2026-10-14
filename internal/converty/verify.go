package converty

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// EventVerifier decides whether an inbound Converty order event may be acted
// upon. An implementation must only return nil when the event has been checked
// against a trustworthy source of truth. Converty does not sign webhook
// deliveries, so an unverified event is untrusted input and must never reach
// the automation pipeline.
type EventVerifier interface {
	VerifyOrderEvent(ctx context.Context, shopID, integrationID uuid.UUID, orderID, status, phone string) error
}

// ErrNotVerified is returned when an event cannot be confirmed as genuine.
var ErrNotVerified = errors.New("converty event could not be verified")

// ErrVerificationUnavailable is returned when no verification mechanism is
// configured.
var ErrVerificationUnavailable = errors.New("converty event verification is unavailable")

// FailClosedVerifier refuses every event. It is the production default because
// Converty neither signs webhook deliveries nor exposes a documented
// single-order read (the API offers only GET /api/v1/orders, a paginated list
// with no id filter), so the platform cannot establish that a given delivery
// really came from the merchant's connected store. Until a trustworthy
// verification mechanism exists (a webhook signature, or an order-by-id
// endpoint), Converty automations deliberately do not fire from webhooks;
// events are still stored for audit. Merchant-declared consent recorded through
// the authenticated API is unaffected by this verifier.
type FailClosedVerifier struct{}

// VerifyOrderEvent always refuses. See FailClosedVerifier.
func (FailClosedVerifier) VerifyOrderEvent(context.Context, uuid.UUID, uuid.UUID, string, string, string) error {
	return ErrVerificationUnavailable
}
