package converty

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// WebhookEvent is a raw Converty webhook delivery with best-effort parsed
// fields. The payload is kept verbatim because the delivery shape is not
// fully documented yet.
type WebhookEvent struct {
	ShopID        uuid.UUID
	EventType     string
	OrderID       string
	OrderStatus   string
	CustomerPhone string
	CustomerName  string
	Payload       []byte
	PayloadHash   string
	ReceivedAt    time.Time
	// Duplicate reports whether the body was already ingested.
	Duplicate bool
	// IntegrationID is the connected Converty store the payload's store
	// reference resolved to, when any. IntegrationFound is false when the
	// payload names no store or names one the platform has not connected, and
	// IntegrationActive mirrors the canonical converty_integrations.active
	// column at ingestion time. Both are required before an event may drive an
	// automation (see server.dispatchConvertyEvent).
	IntegrationID     uuid.UUID
	IntegrationFound  bool
	IntegrationActive bool
}

// webhookDecoded holds the fields we defensively extract from a Converty
// payload. Extraction is best-effort and never blocks ingestion.
type webhookDecoded struct {
	EventType     string
	OrderID       string
	OrderStatus   string
	StoreID       string
	StoreSlug     string
	CustomerPhone string
	CustomerName  string
}

// CaptureWebhook ingests one Converty webhook delivery into order_events,
// deduplicating identical bodies by their sha256. Shop attribution is
// best-effort: when no store reference can be matched to an integration the
// event is still stored with a NULL shop_id.
func (s *Service) CaptureWebhook(ctx context.Context, body []byte) (WebhookEvent, error) {
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	decoded := decodeWebhook(body)

	var event WebhookEvent
	event.EventType = decoded.EventType
	event.OrderID = decoded.OrderID
	event.OrderStatus = decoded.OrderStatus
	event.CustomerPhone = decoded.CustomerPhone
	event.CustomerName = decoded.CustomerName
	event.Payload = body
	event.PayloadHash = hash
	event.ReceivedAt = time.Now().UTC()

	integ, found := s.lookupIntegration(ctx, decoded)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO order_events (shop_id, event_id, event_type, order_id, order_status, payload, payload_hash, processed_at)
		VALUES ($1, NULL, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (payload_hash) DO NOTHING`,
		nullableUUID(integ.shopID), decoded.EventType, decoded.OrderID, decoded.OrderStatus, body, hash, event.ReceivedAt,
	)
	if err != nil {
		return WebhookEvent{}, err
	}
	event.ShopID = integ.shopID
	event.IntegrationID = integ.id
	event.IntegrationFound = found
	event.IntegrationActive = found && integ.active
	event.Duplicate = tag.RowsAffected() == 0

	return event, nil
}

// webhookIntegration is the canonical integration record an inbound payload's
// store reference resolves to.
type webhookIntegration struct {
	id     uuid.UUID
	shopID uuid.UUID
	active bool
}

// lookupIntegration attributes a payload to a shop by matching the store id or
// slug it references against a connected integration. It always reads the
// canonical row (including the active flag) rather than trusting anything the
// payload claims, so a deactivated integration is recognized as such.
func (s *Service) lookupIntegration(ctx context.Context, d webhookDecoded) (webhookIntegration, bool) {
	if d.StoreID == "" && d.StoreSlug == "" {
		return webhookIntegration{}, false
	}
	var out webhookIntegration
	err := s.pool.QueryRow(ctx, `
		SELECT id, shop_id, active
		FROM converty_integrations
		WHERE ($1 <> '' AND converty_store_id = $1)
		   OR ($2 <> '' AND store_slug = $2)
		LIMIT 1`,
		d.StoreID, d.StoreSlug,
	).Scan(&out.id, &out.shopID, &out.active)
	if err != nil {
		return webhookIntegration{}, false
	}
	return out, true
}

// decodeWebhook walks a payload looking for the known field names under any
// of the common envelope keys (data, order, payload, store, result).
func decodeWebhook(body []byte) webhookDecoded {
	var d webhookDecoded
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return d
	}
	d.EventType = pick(root, "event", "eventType", "event_type", "webhookType", "type")
	d.OrderID = pick(root, "orderId", "order_id", "_id", "id")
	d.OrderStatus = pick(root, "status", "orderStatus", "order_status")
	d.StoreID = pick(root, "store", "storeId", "store_id")
	d.StoreSlug = pick(root, "storeSlug", "store_slug", "slug")
	d.CustomerPhone = pickCustomer(root, "phone", "phoneNumber", "phone_number")
	d.CustomerName = pickCustomer(root, "name", "customerName", "customer_name", "fullName")
	return d
}

// pickCustomer pulls a field off a nested customer/contact/address object,
// wherever it appears inside the payload.
func pickCustomer(root any, keys ...string) string {
	var walk func(any, int) string
	walk = func(node any, depth int) string {
		if depth > 6 {
			return ""
		}
		m, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		if customer, exists := m["customer"]; exists {
			if cm, ok := customer.(map[string]any); ok {
				for _, k := range keys {
					if v, ok := cm[k].(string); ok && v != "" {
						return v
					}
				}
			}
		}
		for _, envelope := range []string{"data", "order", "payload", "store", "result", "order_extra"} {
			if nested, exists := m[envelope]; exists {
				if res := walk(nested, depth+1); res != "" {
					return res
				}
			}
		}
		return ""
	}
	return walk(root, 0)
}

// pick returns the first non-empty string found for any of the candidate
// keys, recursing into common envelope maps so the field can be nested.
func pick(root any, keys ...string) string {
	var walk func(any, int) string
	walk = func(node any, depth int) string {
		if depth > 4 {
			return ""
		}
		m, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		for _, k := range keys {
			if v, exists := m[k]; exists {
				if str, ok := v.(string); ok && str != "" {
					return str
				}
			}
		}
		for _, envelope := range []string{"data", "order", "payload", "store", "result"} {
			if nested, exists := m[envelope]; exists {
				if res := walk(nested, depth+1); res != "" {
					return res
				}
			}
		}
		return ""
	}
	return walk(root, 0)
}

func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}
