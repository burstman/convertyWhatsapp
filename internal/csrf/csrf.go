// Package csrf protects browser-facing state-changing requests with a signed
// double-submit cookie. It is deliberately dependency-free and does not treat
// the presence of an HX-Request header, or any other client-supplied header,
// as proof that a request is safe: an attacker can set arbitrary headers from a
// cross-site request.
package csrf

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	// CookieName is the cookie holding the signed token. It is intentionally
	// not HttpOnly: the injected script copies it into the forms, and it is an
	// anti-CSRF nonce rather than a credential.
	CookieName = "csrf_token"
	// HeaderName carries the token on programmatic (JSON/fetch) requests.
	HeaderName = "X-CSRF-Token"
	// FormField is the hidden input name added to every POST form.
	FormField = "csrf_token"

	cookiePath   = "/"
	cookieMaxAge = 60 * 60 * 12
)

// Manager issues and verifies CSRF tokens.
type Manager struct {
	secret []byte
	secure bool
}

// New builds a Manager. The secret also signs the session cookie; it must be
// the deployment's SUPERKIT_SECRET so tokens cannot be forged (or tossed in by
// a sibling subdomain) without knowing it. secure marks the cookie Secure.
func New(secret string, secure bool) *Manager {
	return &Manager{secret: []byte(secret), secure: secure}
}

func (m *Manager) sign(token string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// issue returns a fresh signed cookie value (token.signature).
func (m *Manager) issue() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := hex.EncodeToString(raw)
	return token + "." + m.sign(token)
}

// verify reports whether a cookie value carries a token with a valid
// signature.
func (m *Manager) verify(value string) bool {
	token, sig, ok := strings.Cut(value, ".")
	if !ok || token == "" || sig == "" {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(m.sign(token)))
}

// Middleware ensures a signed token cookie exists on every request and rejects
// unsafe methods whose submitted token does not match it. Paths in exemptPaths
// and paths under any exemptPrefix are skipped entirely: they are the
// server-to-server webhooks and the static assets, which are not browser forms
// and authenticate by other means.
func (m *Manager) Middleware(exemptPaths map[string]bool, exemptPrefixes []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exemptPaths[r.URL.Path] || hasAnyPrefix(r.URL.Path, exemptPrefixes) {
				next.ServeHTTP(w, r)
				return
			}

			cookieValue := ""
			if c, err := r.Cookie(CookieName); err == nil && m.verify(c.Value) {
				cookieValue = c.Value
			}
			if cookieValue == "" {
				cookieValue = m.issue()
				http.SetCookie(w, &http.Cookie{
					Name:     CookieName,
					Value:    cookieValue,
					Path:     cookiePath,
					MaxAge:   cookieMaxAge,
					HttpOnly: false,
					Secure:   m.secure,
					SameSite: http.SameSiteLaxMode,
				})
			}

			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			submitted := r.Header.Get(HeaderName)
			if submitted == "" {
				submitted = r.FormValue(FormField)
			}
			if cookieValue == "" || submitted == "" ||
				subtle.ConstantTimeCompare([]byte(submitted), []byte(cookieValue)) != 1 {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
