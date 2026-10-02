package server

import (
	"log/slog"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/config"
	"whatsappconverty/internal/converty"
	"whatsappconverty/internal/dashboard"
	"whatsappconverty/internal/shops"
	"whatsappconverty/internal/whatsapp"
)

// App wires the dependencies for the web/API process and owns the HTTP router.
type App struct {
	Cfg       config.Config
	Log       *slog.Logger
	Pool      *pgxpool.Pool
	Auth      *auth.Service
	Shops     *shops.Repository
	Dashboard *dashboard.Repository
	Converty  *converty.Service
	WhatsApp  *whatsapp.Service
	Automations *automations.Processor
}

func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool) *App {
	shopsRepo := shops.NewRepository(pool)
	wa := whatsapp.NewService(cfg, pool, log)
	return &App{
		Cfg:         cfg,
		Log:         log,
		Pool:        pool,
		Auth:        auth.NewService(pool, *shopsRepo),
		Shops:       shopsRepo,
		Dashboard:   dashboard.NewRepository(pool),
		Converty:    converty.NewService(cfg, pool, log),
		WhatsApp:    wa,
		Automations: automations.NewProcessor(cfg, pool, log, wa),
	}
}

func (a *App) InitializeMiddleware(r *chi.Mux) {
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(a.loggingMiddleware)

	if a.Cfg.IsProduction() {
		r.Use(middleware.Compress(5))
	}
}

func (a *App) InitializeRoutes(r *chi.Mux) {
	authConfig := kit.AuthenticationConfig{
		AuthFunc:    a.Auth.AuthenticateUser,
		RedirectURL: "/login",
	}

	// Public health and readiness.
	r.Get("/healthz", kit.Handler(a.handleLiveness))
	r.Get("/readyz", kit.Handler(a.handleReadiness))

	// Optional authentication: unauthenticated visitors can reach these.
	r.Group(func(pr chi.Router) {
		pr.Use(kit.WithAuthentication(authConfig, false))
		pr.Get("/", kit.Handler(a.handleIndex))
		pr.Get("/privacy", kit.Handler(a.handlePrivacyPage))
		pr.Get("/login", kit.Handler(a.handleLoginGet))
		pr.Post("/login", kit.Handler(a.handleLoginPost))
		pr.Get("/register", kit.Handler(a.handleRegisterGet))
		pr.Post("/register", kit.Handler(a.handleRegisterPost))
		pr.Post("/logout", kit.Handler(a.handleLogout))
		pr.Post("/webhooks/converty", kit.Handler(a.handleConvertyWebhook))
		pr.Get("/webhooks/meta", kit.Handler(a.handleMetaWebhookVerify))
		pr.Post("/webhooks/meta", kit.Handler(a.handleMetaWebhook))
	})

	// Required authentication for the merchant dashboard.
	r.Group(func(pr chi.Router) {
		pr.Use(kit.WithAuthentication(authConfig, true))
		pr.Get("/dashboard", kit.Handler(a.handleOverview))
		pr.Get("/auth/converty/connect", kit.Handler(a.handleConvertyConnect))
		pr.Get("/auth/converty/callback", kit.Handler(a.handleConvertyCallback))
		pr.Get("/integrations", kit.Handler(a.handleIntegrations))
		pr.Post("/integrations/{id}/refresh", kit.Handler(a.handleIntegrationRefresh))
		pr.Post("/integrations/{id}/test", kit.Handler(a.handleIntegrationTest))
		pr.Post("/integrations/{id}/update", kit.Handler(a.handleIntegrationUpdate))
		pr.Post("/integrations/{id}/activate", kit.Handler(a.handleIntegrationActivate))
		pr.Post("/integrations/{id}/delete", kit.Handler(a.handleIntegrationDelete))
		pr.Get("/automations", kit.Handler(a.handleAutomations))
		pr.Get("/automations/new", kit.Handler(a.handleAutomationEdit))
		pr.Post("/automations", kit.Handler(a.handleAutomationCreate))
		pr.Get("/automations/{id}/edit", kit.Handler(a.handleAutomationEdit))
		pr.Post("/automations/{id}/update", kit.Handler(a.handleAutomationUpdate))
		pr.Post("/automations/{id}/toggle", kit.Handler(a.handleAutomationToggle))
		pr.Post("/automations/{id}/test", kit.Handler(a.handleAutomationTest))
		pr.Post("/automations/{id}/delete", kit.Handler(a.handleAutomationDelete))
		pr.Get("/automations/{id}/history", kit.Handler(a.handleAutomationHistory))
		pr.Post("/automations/{id}/retry-held", kit.Handler(a.handleAutomationRetryHeld))
		pr.Get("/templates", kit.Handler(a.handleTemplates))
		pr.Post("/templates/create", kit.Handler(a.handleTemplateCreate))
		pr.Get("/templates/refresh", kit.Handler(a.handleTemplateRefresh))
		pr.Get("/settings", kit.Handler(a.handleWhatsappSettings))
		pr.Get("/whatsapp/onboard", kit.Handler(a.handleWhatsappOnboard))
		pr.Post("/whatsapp/onboard", kit.Handler(a.handleWhatsappOnboardPost))
		pr.Post("/whatsapp/connect", kit.Handler(a.handleWhatsappConnect))
		pr.Post("/whatsapp/disconnect", kit.Handler(a.handleWhatsappDisconnect))

		// JSON API for the dashboard and integrations. All endpoints are
		// tenant-scoped from the authenticated shop.
		pr.Get("/api/whatsapp/customers", kit.Handler(a.handleAPICustomers))
		pr.Post("/api/whatsapp/customers", kit.Handler(a.handleAPICreateCustomer))
		pr.Post("/api/whatsapp/consent", kit.Handler(a.handleAPIGrantConsent))
		pr.Post("/api/whatsapp/revoke", kit.Handler(a.handleAPIRevokeConsent))
		pr.Get("/api/whatsapp/templates", kit.Handler(a.handleAPITemplates))
		pr.Post("/api/whatsapp/templates", kit.Handler(a.handleAPICreateTemplate))
	})

	r.NotFound(kit.Handler(a.handleNotFound))
}
