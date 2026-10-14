package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/config"
)

const ownerTestSecret = "test-superkit-secret-32-bytes-min"

func kitWithAuth(method, target string, user auth.User) (*kit.Kit, *httptest.ResponseRecorder) {
	return kitWithAuthState(method, target, auth.Auth{LoggedIn: true, User: user})
}

func kitWithAuthState(method, target string, state auth.Auth) (*kit.Kit, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), kit.AuthKey{}, state))
	return &kit.Kit{Response: rec, Request: r}, rec
}

func ownerOnlyApp() *App {
	return &App{Cfg: config.Config{SuperkitSecret: ownerTestSecret}, Log: discardLogger()}
}

// ownerOnlyRoute describes one protected handler, its route, and whether its
// first step after the authorization gate is a database/service call.
type ownerOnlyRoute struct {
	name           string
	method         string
	target         string
	pattern        string // the chi route pattern registered in app.go
	handler        func(*App, *kit.Kit) error
	touchesService bool
}

// ownerOnlyRoutes mirrors every owner-only route in app.go's route table.
// handleAutomationTest is also gated by requireOwner but is covered separately
// by the F-4 test-send suite.
func ownerOnlyRoutes() []ownerOnlyRoute {
	return []ownerOnlyRoute{
		{"GET /settings", http.MethodGet, "/settings", "/settings", (*App).handleWhatsappSettings, true},
		{"POST /settings/phone", http.MethodPost, "/settings/phone", "/settings/phone", (*App).handleWhatsappSettingsPhone, false},
		{"POST /settings/disable", http.MethodPost, "/settings/disable", "/settings/disable", (*App).handleWhatsappSettingsDisable, true},
		{"GET /billing", http.MethodGet, "/billing", "/billing", (*App).handleBilling, true},
		{"GET /team", http.MethodGet, "/team", "/team", (*App).handleTeamGet, true},
		{"POST /team/invite", http.MethodPost, "/team/invite", "/team/invite", (*App).handleTeamInvitePost, false},
		{"POST /team/{id}/delete", http.MethodPost, "/team/" + uuid.NewString() + "/delete", "/team/{id}/delete", (*App).handleTeamDeletePost, false},
		{"GET /whatsapp/onboard", http.MethodGet, "/whatsapp/onboard", "/whatsapp/onboard", (*App).handleWhatsappOnboard, true},
		{"POST /whatsapp/onboard", http.MethodPost, "/whatsapp/onboard", "/whatsapp/onboard", (*App).handleWhatsappOnboardPost, false},
	}
}

func assertNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("request was redirected (%q) instead of blocked", loc)
	}
	if !strings.Contains(rec.Body.String(), "Page not found") {
		t.Fatalf("did not receive the not-found page: %q", rec.Body.String())
	}
}

// The gate must report false for a member and render the 404 page, because the
// render itself succeeds and cannot be used to detect "blocked".
func TestRequireOwnerGateStopsMembers(t *testing.T) {
	a := ownerOnlyApp()
	k, rec := kitWithAuth(http.MethodGet, "/settings", memberUser(uuid.New()))

	ok, err := a.requireOwner(k)
	if err != nil {
		t.Fatalf("gate returned an error: %v", err)
	}
	if ok {
		t.Fatal("member passed the owner gate")
	}
	assertNotFound(t, rec)
}

// An unauthenticated request (no logged-in user) is not an owner and must be
// stopped the same way.
func TestRequireOwnerGateStopsUnauthenticated(t *testing.T) {
	a := ownerOnlyApp()
	k, rec := kitWithAuthState(http.MethodGet, "/settings", auth.Auth{})

	ok, err := a.requireOwner(k)
	if err != nil {
		t.Fatalf("gate returned an error: %v", err)
	}
	if ok {
		t.Fatal("unauthenticated request passed the owner gate")
	}
	assertNotFound(t, rec)
}

// The gate must report true for an owner and render nothing of its own.
func TestRequireOwnerGateAllowsOwner(t *testing.T) {
	a := ownerOnlyApp()
	k, rec := kitWithAuth(http.MethodGet, "/settings", ownerUser(uuid.New()))

	ok, err := a.requireOwner(k)
	if err != nil {
		t.Fatalf("gate returned an error: %v", err)
	}
	if !ok {
		t.Fatal("owner was rejected by the owner gate")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("owner should not get a body from the gate: %q", rec.Body.String())
	}
}

// Every protected handler must stop a member before it touches any service.
// The App has nil service pointers, so a handler that fell through the gate
// would panic instead of returning cleanly.
func TestMembersCannotReachOwnerOnlyHandlers(t *testing.T) {
	member := memberUser(uuid.New())

	for _, route := range ownerOnlyRoutes() {
		t.Run(route.name, func(t *testing.T) {
			a := ownerOnlyApp()
			k, rec := kitWithAuth(route.method, route.target, member)

			if err := route.handler(a, k); err != nil {
				t.Fatalf("member request returned an error: %v", err)
			}
			assertNotFound(t, rec)
		})
	}
}

// Handlers that decide before touching a service must let an owner through to
// that decision: the owner gets the handler's own redirect, never the 404 page.
// This is the positive counterpart to TestMembersCannotReachOwnerOnlyHandlers.
func TestOwnersAreNotStoppedByOwnerGate(t *testing.T) {
	owner := ownerUser(uuid.New())

	for _, route := range ownerOnlyRoutes() {
		if route.touchesService {
			continue
		}
		t.Run(route.name, func(t *testing.T) {
			a := ownerOnlyApp()
			k, rec := kitWithAuth(route.method, route.target, owner)

			if err := route.handler(a, k); err != nil {
				t.Fatalf("owner request returned an error: %v", err)
			}
			if rec.Header().Get("Location") == "" {
				t.Fatalf("owner was not allowed to reach the handler's redirect")
			}
			if strings.Contains(rec.Body.String(), "Page not found") {
				t.Fatalf("owner received the not-found page")
			}
		})
	}
}

// For handlers that go straight to a service after the gate, an owner request
// must reach that service. With a nil repository that is a nil-pointer panic;
// recovering it proves the owner was not stopped by the gate. A member is
// stopped and returns the 404 (covered above), so this is the inverse check.
func TestOwnersReachProtectedServices(t *testing.T) {
	owner := ownerUser(uuid.New())

	for _, route := range ownerOnlyRoutes() {
		if !route.touchesService {
			continue
		}
		t.Run(route.name, func(t *testing.T) {
			a := ownerOnlyApp()
			k, _ := kitWithAuth(route.method, route.target, owner)

			reached := false
			func() {
				defer func() {
					if recover() != nil {
						reached = true
					}
				}()
				_ = route.handler(a, k)
			}()
			if !reached {
				t.Fatalf("owner did not reach the protected service")
			}
		})
	}
}

// The routes under test must actually be registered (mirroring app.go) so the
// member/owner handler tests correspond to reachable endpoints.
func TestOwnerOnlyRoutesAreRegistered(t *testing.T) {
	a := newRouteTestApp()
	r := chi.NewRouter()
	a.InitializeMiddleware(r)
	a.InitializeRoutes(r)

	registered := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	for _, route := range ownerOnlyRoutes() {
		want := route.method + " " + route.pattern
		if !registered[want] {
			t.Fatalf("route %q is not registered", want)
		}
	}
}
