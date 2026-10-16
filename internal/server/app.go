package server

import (
	"log/slog"

	"github.com/anthdm/superkit/kit"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/admin"
	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/automations"
	"whatsappconverty/internal/config"
	"whatsappconverty/internal/converty"
	"whatsappconverty/internal/csrf"
	"whatsappconverty/internal/dashboard"
	"whatsappconverty/internal/metaconfig"
	"whatsappconverty/internal/shops"
	"whatsappconverty/internal/whatsapp"
)

// App wires the dependencies for the web/API process and owns the HTTP router.
type App struct {
	Cfg         config.Config
	Log         *slog.Logger
	Pool        *pgxpool.Pool
	Auth        *auth.Service
	Admin       *admin.Repository
	Shops       *shops.Repository
	Dashboard   *dashboard.Repository
	Converty    *converty.Service
	WhatsApp    *whatsapp.Service
	Automations *automations.Processor
	Meta        *metaconfig.Store

	// ConvertyVerifier authenticates inbound Converty order events before they
	// may drive a send. It defaults to converty.FailClosedVerifier, which
	// refuses every event because Converty webhooks carry no signature; a real
	// verifier can be substituted once a trustworthy mechanism exists.
	ConvertyVerifier converty.EventVerifier

	// CSRF is the signed double-submit token manager guarding every
	// browser-facing state-changing request.
	CSRF *csrf.Manager
}

func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, meta *metaconfig.Store) *App {
	shopsRepo := shops.NewRepository(pool)
	wa := whatsapp.NewService(cfg, pool, log, meta)
	auto := automations.NewProcessor(cfg, pool, log, wa)
	// The delivery layer re-checks the Converty integration immediately before
	// every send (see whatsapp.gateIntegration); give it the store-state reader.
	wa.SetIntegrationChecker(auto)
	return &App{
		Cfg:         cfg,
		Log:         log,
		Pool:        pool,
		Auth:        auth.NewService(pool, *shopsRepo),
		Admin:       admin.NewRepository(pool),
		Shops:       shopsRepo,
		Dashboard:   dashboard.NewRepository(pool),
		Converty:    converty.NewService(cfg, pool, log),
		WhatsApp:    wa,
		Automations: auto,
		Meta:        meta,

		ConvertyVerifier: converty.FailClosedVerifier{},
		CSRF:             csrf.New(cfg.SuperkitSecret, cfg.IsProduction()),
	}
}

// csrfExemptPaths are the routes CSRF protection is skipped for. They are
// server-to-server callbacks (Meta and Converty webhooks), which are not
// browser requests and authenticate by their own means. Marking the health
// checks exempt just avoids handing them a cookie.
var csrfExemptPaths = map[string]bool{
	"/webhooks/converty": true,
	"/webhooks/meta":     true,
	"/healthz":           true,
	"/readyz":            true,
}

func (a *App) InitializeMiddleware(r *chi.Mux) {
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(a.loggingMiddleware)
	r.Use(a.bodyLimitMiddleware)
	r.Use(a.CSRF.Middleware(csrfExemptPaths, []string{"/static/"}))

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

	// Required authentication for the platform console and merchant dashboard.
	r.Group(func(pr chi.Router) {
		pr.Use(kit.WithAuthentication(authConfig, true))

		// Platform operator console.
		pr.Get("/admin", kit.Handler(a.handleAdminConsole))
		pr.Get("/admin/whatsapp", kit.Handler(a.handleAdminWhatsappConfig))
		pr.Post("/admin/whatsapp", kit.Handler(a.handleAdminWhatsappConfigSave))
		pr.Post("/admin/whatsapp/verify", kit.Handler(a.handleAdminWhatsappVerify))

		// Platform inbox (admin-only): customers messaging the central
		// WhatsApp number land here.
		pr.Group(func(ir chi.Router) {
			ir.Use(a.requireAdminMiddleware)

			ir.Get("/inbox", kit.Handler(a.handleInbox))
			ir.Get("/inbox/sidebar", kit.Handler(a.handleInboxSidebar))
			ir.Get("/inbox/{id}", kit.Handler(a.handleInboxThread))
			ir.Get("/inbox/{id}/fragment", kit.Handler(a.handleInboxFragment))
			ir.Get("/inbox/{id}/media/{msg}", kit.Handler(a.handleChatMedia))
			ir.Post("/inbox/{id}/reply", kit.Handler(a.handleInboxReply))
			ir.Post("/inbox/{id}/reply-audio", kit.Handler(a.handleInboxReplyAudio))
			ir.Post("/inbox/{id}/reply-image", kit.Handler(a.handleInboxReplyImage))
			ir.Post("/inbox/{id}/read", kit.Handler(a.handleInboxRead))
		})

		// Merchant (shop-scoped) area. Admins are redirected to the console.
		pr.Group(func(cr chi.Router) {
			cr.Use(a.clientOnlyMiddleware)

			cr.Get("/dashboard", kit.Handler(a.handleOverview))
			cr.Get("/auth/converty/connect", kit.Handler(a.handleConvertyConnect))
			cr.Get("/auth/converty/callback", kit.Handler(a.handleConvertyCallback))
			cr.Get("/integrations", kit.Handler(a.handleIntegrations))
			cr.Post("/integrations/{id}/refresh", kit.Handler(a.handleIntegrationRefresh))
			cr.Post("/integrations/{id}/test", kit.Handler(a.handleIntegrationTest))
			cr.Post("/integrations/{id}/update", kit.Handler(a.handleIntegrationUpdate))
			cr.Post("/integrations/{id}/activate", kit.Handler(a.handleIntegrationActivate))
			cr.Get("/integrations/{id}/delete", kit.Handler(a.handleIntegrationDeletePreview))
			cr.Post("/integrations/{id}/delete", kit.Handler(a.handleIntegrationDelete))
			cr.Get("/automations", kit.Handler(a.handleAutomations))
			cr.Get("/automations/new", kit.Handler(a.handleAutomationEdit))
			cr.Post("/automations", kit.Handler(a.handleAutomationCreate))
			cr.Get("/automations/{id}/edit", kit.Handler(a.handleAutomationEdit))
			cr.Post("/automations/{id}/update", kit.Handler(a.handleAutomationUpdate))
			cr.Post("/automations/{id}/toggle", kit.Handler(a.handleAutomationToggle))
			cr.Post("/automations/{id}/test", kit.Handler(a.handleAutomationTest))
			cr.Post("/automations/{id}/delete", kit.Handler(a.handleAutomationDelete))
			cr.Get("/automations/{id}/history", kit.Handler(a.handleAutomationHistory))
			cr.Post("/automations/{id}/retry-held", kit.Handler(a.handleAutomationRetryHeld))
			cr.Get("/templates", kit.Handler(a.handleTemplates))
			cr.Post("/templates/create", kit.Handler(a.handleTemplateCreate))
			cr.Post("/templates/refresh", kit.Handler(a.handleTemplateRefresh))
			cr.Get("/whatsapp", kit.Handler(a.handleWhatsappStatus))
			cr.Get("/settings", kit.Handler(a.handleWhatsappSettings))
			cr.Post("/settings/phone", kit.Handler(a.handleWhatsappSettingsPhone))
			cr.Post("/settings/disable", kit.Handler(a.handleWhatsappSettingsDisable))
			cr.Get("/billing", kit.Handler(a.handleBilling))
			cr.Get("/whatsapp/onboard", kit.Handler(a.handleWhatsappOnboard))
			cr.Post("/whatsapp/onboard", kit.Handler(a.handleWhatsappOnboardPost))

			// JSON API for the dashboard and integrations. All endpoints are
			// tenant-scoped from the authenticated shop.
			cr.Get("/api/whatsapp/customers", kit.Handler(a.handleAPICustomers))
			cr.Post("/api/whatsapp/customers", kit.Handler(a.handleAPICreateCustomer))
			cr.Post("/api/whatsapp/consent", kit.Handler(a.handleAPIGrantConsent))
			cr.Post("/api/whatsapp/revoke", kit.Handler(a.handleAPIRevokeConsent))
			cr.Get("/api/whatsapp/templates", kit.Handler(a.handleAPITemplates))
			cr.Post("/api/whatsapp/templates", kit.Handler(a.handleAPICreateTemplate))
		})
	})

	r.NotFound(kit.Handler(a.handleNotFound))
}
