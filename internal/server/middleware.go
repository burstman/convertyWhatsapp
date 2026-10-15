package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/anthdm/superkit/kit"

	"whatsappconverty/internal/auth"
)

const (
	// formBodyLimitBytes caps the body of ordinary browser-facing requests
	// (dashboard forms, HTMX posts, JSON API calls). It is enforced before any
	// downstream middleware or handler parses the body, so an oversized or
	// malformed submission cannot force unbounded buffering or temp-file
	// writes before it is rejected.
	formBodyLimitBytes int64 = 1 << 20 // 1 MiB
	// webhookBodyLimitBytes caps server-to-server webhook deliveries (Meta and
	// Converty), whose JSON payloads are legitimately larger than dashboard
	// forms.
	webhookBodyLimitBytes int64 = 2 << 20 // 2 MiB
)

// webhookPaths receive webhookBodyLimitBytes instead of formBodyLimitBytes.
var webhookPaths = map[string]bool{
	"/webhooks/converty": true,
	"/webhooks/meta":     true,
}

// bodyLimitMiddleware caps the request body before it reaches the CSRF
// middleware (which parses form bodies) or any handler. Webhook deliveries get
// the larger webhook limit; everything else gets the form limit.
func (a *App) bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := formBodyLimitBytes
		if webhookPaths[r.URL.Path] {
			limit = webhookBodyLimitBytes
		}
		enforceBodyLimit(limit, next).ServeHTTP(w, r)
	})
}

// enforceBodyLimit reads at most limit bytes from the request body before
// invoking next. A body within the limit is restored byte-for-byte, so
// signature verification and parsers observe exactly what the client sent. An
// oversized body is answered with 413 and never reaches next. Content-Length is
// deliberately ignored: the cap is applied to the bytes actually read, so a
// missing or understated length cannot bypass it.
func enforceBodyLimit(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		limited := http.MaxBytesReader(w, r.Body, limit)
		body, err := io.ReadAll(limited)
		_ = limited.Close()
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			// A transport error while reading the body: the request cannot be
			// processed safely, refuse it without leaking details.
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}

// shouldLogRequest filters high-frequency noise (health checks, static assets)
// from the request log.
func shouldLogRequest(path string) bool {
	if path == "/healthz" || path == "/readyz" {
		return false
	}
	if strings.HasPrefix(path, "/static/") {
		return false
	}
	return true
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := chimiddleware.GetReqID(r.Context())

		logger := a.Log.With(
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
		)

		// Capture the response status for the log line.
		rr := newStatusRecorder(w)
		next.ServeHTTP(rr, r)

		if !shouldLogRequest(r.URL.Path) {
			return
		}

		logger.Info("request",
			"status", rr.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// clientOnlyMiddleware keeps platform admins out of the shop-scoped merchant
// area: an admin (who has no active shop) is redirected to the admin console.
// It runs after kit.WithAuthentication, so the principal is always present.
func (a *App) clientOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := r.Context().Value(kit.AuthKey{}).(auth.Auth); ok && principal.IsAdmin() {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
