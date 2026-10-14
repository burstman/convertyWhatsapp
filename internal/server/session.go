package server

import (
	"net/http"

	"github.com/gorilla/sessions"
)

// applySessionOptions hardens the session cookie immediately before it is
// saved. gorilla's CookieStore defaults HttpOnly to false, so without this any
// script able to run on the page could read the session token and hijack the
// account. HttpOnly is forced on, the cookie is scoped to the whole site with
// SameSite=Lax so it is not attached to cross-site POSTs in modern browsers,
// and it is marked Secure in production.
func applySessionOptions(sess *sessions.Session, secure bool) {
	sess.Options.HttpOnly = true
	sess.Options.Secure = secure
	sess.Options.SameSite = http.SameSiteLaxMode
	if sess.Options.Path == "" {
		sess.Options.Path = "/"
	}
}
