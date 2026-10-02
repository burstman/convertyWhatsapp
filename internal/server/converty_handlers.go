package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/converty"
	viewshared "whatsappconverty/web/views/components"
	vdashboard "whatsappconverty/web/views/dashboard"
)

const maxWebhookBody = 2 << 20

// handleConvertyWebhook is the target Converty POSTs order events to. The
// payload shape is not yet documented, so each delivery is persisted
// verbatim into order_events (deduplicated by body hash) with best-effort
// parsed fields, and we always acknowledge with 200.
func (a *App) handleConvertyWebhook(k *kit.Kit) error {
	body, err := io.ReadAll(io.LimitReader(k.Request.Body, maxWebhookBody))
	if err != nil {
		return err
	}

	event, err := a.Converty.CaptureWebhook(k.Request.Context(), body)
	if err != nil {
		a.Log.Error("converty webhook ingest failed", "error", err)
		return err
	}

	a.Log.Info("converty webhook captured",
		"duplicate", event.Duplicate,
		"shop_id", event.ShopID,
		"event_type", event.EventType,
		"order_id", event.OrderID,
		"order_status", event.OrderStatus,
		"body", truncateBytes(body, 800),
	)

	// Best-effort: learn the merchant's customers from the orders Converty
	// reports, so a phone number on record can be linked to a consent record
	// later. Never treat the phone number itself as WhatsApp consent.
	if event.ShopID != uuid.Nil && event.CustomerPhone != "" {
		if _, cErr := a.WhatsApp.UpsertCustomer(k.Request.Context(), event.ShopID, event.CustomerName, event.CustomerPhone); cErr != nil {
			a.Log.Warn("converty webhook customer upsert failed", "shop_id", event.ShopID, "error", cErr)
		}
	}

	// Feed genuine events through the automation pipeline. Duplicates are
	// re-ingested (the raw payload is stored verbatim) but must not fire the
	// merchant's automations twice.
	if !event.Duplicate {
		if err := a.Automations.OnConvertyEvent(k.Request.Context(), automations.ConvertyEvent{
			ShopID:        event.ShopID,
			OrderStatus:   event.OrderStatus,
			OrderID:       event.OrderID,
			CustomerName:  event.CustomerName,
			CustomerPhone: event.CustomerPhone,
		}); err != nil {
			a.Log.Warn("converty webhook automation pipeline failed",
				"shop_id", event.ShopID, "order_id", event.OrderID, "error", err)
		}
	}

	return k.Text(http.StatusOK, "ok")
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// handleIntegrations renders the Shop Integration page: every Converty store
// connection for the shop with its management actions.
func (a *App) handleIntegrations(k *kit.Kit) error {
	principal := auth.FromKit(k)
	shop, err := a.Shops.GetByID(k.Request.Context(), principal.User.ShopID)
	if err != nil {
		return err
	}

	integrations, err := a.Converty.Integrations(k.Request.Context(), principal.User.ShopID)
	if err != nil {
		return err
	}

	flash := vdashboard.IntegrationFlash{}
	switch k.Request.URL.Query().Get("flash") {
	case "connected":
		flash.Info = "Store connected — order webhooks are now subscribed."
	case "refreshed":
		flash.Info = "Access token refreshed."
	case "tested":
		flash.Info = "Connection test passed — Converty responds correctly."
	case "testfailed":
		flash.Error = "Connection test failed — the tokens may be revoked. Try refreshing them or re-connect the store."
	case "updated":
		flash.Info = "Store details updated."
	case "activated":
		flash.Info = "Integration activated."
	case "deactivated":
		flash.Info = "Integration deactivated — new order events will not be processed until you activate it again."
	case "deleted":
		flash.Info = "Integration deleted — its webhook subscriptions were removed."
	case "notfound":
		flash.Error = "That integration no longer exists."
	case "missing":
		flash.Error = "A store name is required."
	case "error", "internal":
		flash.Error = "Something went wrong — try again."
	}

	page := viewshared.Page{
		Title:    "Shop Integration",
		Active:   "integrations",
		ShopName: shop.Name,
		UserName: principal.User.Name,
	}
	return k.Render(vdashboard.IntegrationsPage(page, integrations, a.Converty.Configured(), flash))
}

func (a *App) handleConvertyConnect(k *kit.Kit) error {
	if !a.Converty.Configured() {
		return fmt.Errorf("converty is not configured on this deployment")
	}

	principal := auth.FromKit(k)

	state, err := randomHex(32)
	if err != nil {
		return err
	}

	sess := k.GetSession(auth.SessionCookieName)
	sess.Values["converty_oauth_state"] = state
	if err := sess.Save(k.Request, k.Response); err != nil {
		return err
	}

	a.Log.Info("converty connect started", "shop_id", principal.User.ShopID, "redirect", a.Cfg.ConvertyRedirectURI)
	return k.Redirect(http.StatusFound, a.Converty.AuthorizeURL(state))
}

func (a *App) handleConvertyCallback(k *kit.Kit) error {
	principal := auth.FromKit(k)

	sess := k.GetSession(auth.SessionCookieName)
	savedState, _ := sess.Values["converty_oauth_state"].(string)
	delete(sess.Values, "converty_oauth_state")
	_ = sess.Save(k.Request, k.Response)

	state := k.Request.URL.Query().Get("state")
	code := k.Request.URL.Query().Get("code")

	if !secureEqual(savedState, state) {
		a.Log.Warn("converty callback rejected: state mismatch")
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}
	if code == "" {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	ctx, cancel := context.WithTimeout(k.Request.Context(), 20*time.Second)
	defer cancel()

	tok, err := a.Converty.ExchangeCode(ctx, code)
	if err != nil {
		a.Log.Error("converty code exchange failed", "error", err)
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	store, err := a.Converty.GetStore(ctx, tok.AccessToken)
	if err != nil {
		// A failed store sync should not discard the freshly connected tokens:
		// persist them and subscribe webhooks, but flag the sync problem.
		a.Log.Error("converty stores/me failed", "error", err)
		integrationID, sErr := a.Converty.SaveIntegration(ctx, principal.User.ShopID, converty.Store{}, converty.DefaultScopes, tok, time.Now())
		if sErr != nil {
			a.Log.Error("converty integration save failed", "error", sErr)
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
		}
		a.subscribeWebhooks(ctx, principal.User.ShopID, integrationID)
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=connected")
	}

	integrationID, err := a.Converty.SaveIntegration(ctx, principal.User.ShopID, store, converty.DefaultScopes, tok, time.Now())
	if err != nil {
		a.Log.Error("converty integration save failed", "error", err)
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	a.subscribeWebhooks(ctx, principal.User.ShopID, integrationID)

	a.Log.Info("converty connected", "shop_id", principal.User.ShopID, "store", store.Name, "integration_id", integrationID)
	return k.Redirect(http.StatusSeeOther, "/integrations?flash=connected")
}

// subscribeWebhooks registers the order events against the app's Converty
// webhook endpoint for one integration, fetching/refreshing the access token
// as needed. The returned Converty hook ids are merged into the stored
// subscription map (so ids from hooks that already existed survive a
// reconnect) and persisted for later removal on disconnect. Failures are
// logged but do not fail the connect (the integration is already saved). A
// 409 means the hook already exists and is treated as success.
func (a *App) subscribeWebhooks(ctx context.Context, shopID, integrationID uuid.UUID) {
	targetURL := a.Converty.WebhookURL(a.Cfg.AppURL)

	subs := map[string]string{}
	existing, err := a.Converty.IntegrationByID(ctx, shopID, integrationID)
	if err == nil && existing.WebhookSubscriptions != nil {
		subs = existing.WebhookSubscriptions
	}

	err = a.Converty.WithAccessToken(ctx, shopID, integrationID, func(ctx context.Context, accessToken string) error {
		for _, event := range converty.SupportedEvents() {
			hookID, sErr := a.Converty.SubscribeHook(ctx, accessToken, targetURL, event)
			if sErr == nil {
				subs[event] = hookID
				a.Log.Info("converty hook subscribed", "shop_id", shopID, "integration_id", integrationID, "event", event, "hook_id", hookID, "target_url", targetURL)
				continue
			}
			var apiErr converty.APIError
			if errors.As(sErr, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				a.Log.Info("converty hook already subscribed", "shop_id", shopID, "event", event)
				continue
			}
			a.Log.Error("converty hook subscribe failed",
				"shop_id", shopID, "event", event, "target_url", targetURL, "error", sErr)
		}

		// Hooks that already existed (409) come back without an id, so pull
		// the live list and record their ids for later removal on disconnect.
		hooks, hErr := a.Converty.ListHooks(ctx, accessToken)
		if hErr != nil {
			a.Log.Warn("converty hooks list failed", "shop_id", shopID, "target_url", targetURL, "error", hErr)
			return nil
		}
		for _, h := range hooks {
			if h.TargetURL == targetURL && h.Event != "" && h.ID != "" {
				subs[h.Event] = h.ID
			}
		}
		return nil
	})
	if err != nil {
		a.Log.Error("converty hook subscribe skipped, no access token", "shop_id", shopID, "error", err)
	}

	if err := a.Converty.SaveWebhookSubscriptions(ctx, shopID, integrationID, subs); err != nil {
		a.Log.Error("converty webhook subscriptions save failed", "shop_id", shopID, "error", err)
	}
}

// handleIntegrationRefresh forces a Converty OAuth token rotation for one
// integration.
func (a *App) handleIntegrationRefresh(k *kit.Kit) error {
	principal := auth.FromKit(k)
	id, err := integrationID(k)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
	}

	ctx, cancel := context.WithTimeout(k.Request.Context(), 20*time.Second)
	defer cancel()

	if err := a.Converty.RefreshIntegrationToken(ctx, principal.User.ShopID, id); err != nil {
		a.Log.Error("converty token refresh failed", "shop_id", principal.User.ShopID, "integration_id", id, "error", err)
		if errors.Is(err, converty.ErrNotFound) {
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	a.Log.Info("converty token refreshed", "shop_id", principal.User.ShopID, "integration_id", id)
	return k.Redirect(http.StatusSeeOther, "/integrations?flash=refreshed")
}

// handleIntegrationTest verifies an integration by calling Converty with its
// access token (rotating first if needed).
func (a *App) handleIntegrationTest(k *kit.Kit) error {
	principal := auth.FromKit(k)
	id, err := integrationID(k)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
	}

	ctx, cancel := context.WithTimeout(k.Request.Context(), 20*time.Second)
	defer cancel()

	if err := a.Converty.TestConnection(ctx, principal.User.ShopID, id); err != nil {
		a.Log.Warn("converty connection test failed", "shop_id", principal.User.ShopID, "integration_id", id, "error", err)
		if errors.Is(err, converty.ErrNotFound) {
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=testfailed")
	}

	a.Log.Info("converty connection test passed", "shop_id", principal.User.ShopID, "integration_id", id)
	return k.Redirect(http.StatusSeeOther, "/integrations?flash=tested")
}

// handleIntegrationUpdate edits the display details of one integration.
func (a *App) handleIntegrationUpdate(k *kit.Kit) error {
	principal := auth.FromKit(k)
	id, err := integrationID(k)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
	}

	name := k.Request.FormValue("store_name")
	if name == "" {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=missing")
	}
	domain := k.Request.FormValue("store_domain")

	if err := a.Converty.UpdateIntegrationInfo(k.Request.Context(), principal.User.ShopID, id, name, domain); err != nil {
		a.Log.Error("converty integration update failed", "shop_id", principal.User.ShopID, "integration_id", id, "error", err)
		if errors.Is(err, converty.ErrNotFound) {
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	return k.Redirect(http.StatusSeeOther, "/integrations?flash=updated")
}

// handleIntegrationActivate toggles whether one integration processes new
// order events.
func (a *App) handleIntegrationActivate(k *kit.Kit) error {
	principal := auth.FromKit(k)
	id, err := integrationID(k)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
	}

	active := k.Request.FormValue("active") == "1"
	if err := a.Converty.SetIntegrationActive(k.Request.Context(), principal.User.ShopID, id, active); err != nil {
		a.Log.Error("converty integration activation failed", "shop_id", principal.User.ShopID, "integration_id", id, "error", err)
		if errors.Is(err, converty.ErrNotFound) {
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
		}
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=error")
	}

	flash := "activated"
	if !active {
		flash = "deactivated"
	}
	return k.Redirect(http.StatusSeeOther, "/integrations?flash="+flash)
}

// handleIntegrationDelete removes one integration: every stored Converty
// webhook subscription is unsubscribed (with an auto-refreshed token) before
// the local row is dropped. Unsubscribe failures are logged but never block
// the local delete, so the user is always freed from their account.
func (a *App) handleIntegrationDelete(k *kit.Kit) error {
	principal := auth.FromKit(k)
	id, err := integrationID(k)
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
	}

	ctx, cancel := context.WithTimeout(k.Request.Context(), 20*time.Second)
	defer cancel()

	integ, err := a.Converty.IntegrationByID(ctx, principal.User.ShopID, id)
	if err != nil {
		if errors.Is(err, converty.ErrNotFound) {
			return k.Redirect(http.StatusSeeOther, "/integrations?flash=notfound")
		}
		return err
	}

	err = a.Converty.WithAccessToken(ctx, principal.User.ShopID, id, func(ctx context.Context, accessToken string) error {
		for event, hookID := range integ.WebhookSubscriptions {
			if hookID == "" {
				continue
			}
			if uErr := a.Converty.UnsubscribeHook(ctx, accessToken, hookID); uErr != nil {
				var apiErr converty.APIError
				alreadyGone := errors.As(uErr, &apiErr) &&
					(apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusConflict)
				if !alreadyGone {
					a.Log.Warn("converty hook unsubscribe failed",
						"shop_id", principal.User.ShopID, "integration_id", id, "event", event, "hook_id", hookID, "error", uErr)
				}
				continue
			}
			a.Log.Info("converty hook unsubscribed", "shop_id", principal.User.ShopID, "integration_id", id, "event", event, "hook_id", hookID)
		}
		return nil
	})
	if err != nil {
		a.Log.Warn("converty delete: hook cleanup skipped, no access token", "shop_id", principal.User.ShopID, "error", err)
	}

	if err := a.Converty.DeleteIntegration(ctx, principal.User.ShopID, id); err != nil {
		a.Log.Error("converty integration delete failed", "shop_id", principal.User.ShopID, "integration_id", id, "error", err)
		return err
	}

	a.Log.Info("converty integration deleted", "shop_id", principal.User.ShopID, "integration_id", id)
	return k.Redirect(http.StatusSeeOther, "/integrations?flash=deleted")
}

func integrationID(k *kit.Kit) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse integration id: %w", err)
	}
	return id, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}