package server

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/csrf"
)

// newRouteTestApp builds just enough app state to register the real route
// table. Handlers are never invoked, so no database is required.
func newRouteTestApp() *App {
	cfg := config.Config{SuperkitSecret: "test-superkit-secret-32-bytes-min"}
	return &App{
		Cfg:  cfg,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		CSRF: csrf.New(cfg.SuperkitSecret, false),
	}
}

// /templates/refresh calls Meta and rewrites the templates table, so it must not
// be reachable by a GET: that is exactly the CSRF-free, top-level-navigation
// path the review flagged. It must be POST-only, where the CSRF middleware
// applies.
func TestTemplatesRefreshRouteIsPostOnly(t *testing.T) {
	a := newRouteTestApp()
	r := chi.NewRouter()
	a.InitializeMiddleware(r)
	a.InitializeRoutes(r)

	var gets, posts int
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route != "/templates/refresh" {
			return nil
		}
		switch method {
		case http.MethodGet:
			gets++
		case http.MethodPost:
			posts++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if gets != 0 {
		t.Fatalf("GET /templates/refresh is still routable (%d); it must be POST-only", gets)
	}
	if posts != 1 {
		t.Fatalf("POST /templates/refresh routes = %d, want 1", posts)
	}
}
