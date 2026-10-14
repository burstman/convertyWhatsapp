package csrf

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestManager(secure bool) *Manager {
	return New("test-secret-that-is-long-enough-1234567890", secure)
}

// serve runs one request through the middleware and reports whether the inner
// handler ran and the response code.
func serve(t *testing.T, m *Manager, exempt map[string]bool, r *http.Request) (called bool, rec *httptest.ResponseRecorder) {
	t.Helper()
	rec = httptest.NewRecorder()
	m.Middleware(exempt, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return called, rec
}

func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSafeMethodIssuesCookie(t *testing.T) {
	m := newTestManager(true)
	called, rec := serve(t, m, nil, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if !called {
		t.Fatal("GET request did not reach the handler")
	}
	c := cookieByName(rec, CookieName)
	if c == nil {
		t.Fatal("no CSRF cookie was issued on a safe request")
	}
	if c.HttpOnly {
		t.Fatal("CSRF cookie must not be HttpOnly (the injected script reads it)")
	}
	if !c.Secure {
		t.Fatal("CSRF cookie must be Secure in production")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("CSRF cookie SameSite = %v, want Lax", c.SameSite)
	}
	if !m.verify(c.Value) {
		t.Fatal("issued CSRF cookie does not carry a valid signature")
	}
}

func TestUnsafeWithoutTokenRejected(t *testing.T) {
	m := newTestManager(false)
	called, rec := serve(t, m, nil, httptest.NewRequest(http.MethodPost, "/team/invite", nil))
	if called {
		t.Fatal("POST without a CSRF token reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestUnsafeWithCookieButNoFieldRejected(t *testing.T) {
	m := newTestManager(false)
	token := m.issue()
	r := httptest.NewRequest(http.MethodPost, "/team/invite", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: token})

	called, rec := serve(t, m, nil, r)
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("POST with cookie but no submitted token: called=%v status=%d", called, rec.Code)
	}
}

func TestMismatchedTokenRejected(t *testing.T) {
	m := newTestManager(false)
	cookieToken := m.issue()
	otherToken := m.issue()

	form := url.Values{"csrf_token": {otherToken}}
	r := httptest.NewRequest(http.MethodPost, "/team/invite", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: cookieToken})

	called, rec := serve(t, m, nil, r)
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched token accepted: called=%v status=%d", called, rec.Code)
	}
}

func TestMatchingFormTokenPasses(t *testing.T) {
	m := newTestManager(false)
	token := m.issue()

	form := url.Values{"csrf_token": {token}, "name": {"Hamed"}}
	r := httptest.NewRequest(http.MethodPost, "/team/invite", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: token})

	called, rec := serve(t, m, nil, r)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("valid form token rejected: called=%v status=%d", called, rec.Code)
	}
	// The handler must still be able to read the form after the middleware parsed it.
	if err := r.ParseForm(); err != nil {
		t.Fatalf("handler could not parse form: %v", err)
	}
	if r.PostForm.Get("name") != "Hamed" {
		t.Fatalf("handler saw name=%q, want Hamed", r.PostForm.Get("name"))
	}
}

func TestMatchingHeaderTokenPasses(t *testing.T) {
	m := newTestManager(false)
	token := m.issue()

	r := httptest.NewRequest(http.MethodPost, "/api/whatsapp/consent", strings.NewReader(`{"a":1}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(HeaderName, token)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: token})

	called, rec := serve(t, m, nil, r)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("valid header token rejected: called=%v status=%d", called, rec.Code)
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	m := newTestManager(false)
	token := m.issue()
	tampered := "deadbeef." + strings.SplitN(token, ".", 2)[1]

	form := url.Values{"csrf_token": {tampered}}
	r := httptest.NewRequest(http.MethodPost, "/team/invite", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: tampered})

	called, rec := serve(t, m, nil, r)
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("tampered cookie accepted: called=%v status=%d", called, rec.Code)
	}
}

// A cross-site request can set any header and any body it wants; only the
// signed token tied to the cookie passes, so a request with a foreign Origin is
// rejected just like any other token-less request.
func TestCrossOriginWithoutTokenRejected(t *testing.T) {
	m := newTestManager(false)
	r := httptest.NewRequest(http.MethodPost, "/settings/disable", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("HX-Request", "true")

	called, rec := serve(t, m, nil, r)
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request accepted: called=%v status=%d", called, rec.Code)
	}
}

func TestExemptPathSkipsValidation(t *testing.T) {
	m := newTestManager(false)
	exempt := map[string]bool{"/webhooks/converty": true}

	called, rec := serve(t, m, exempt, httptest.NewRequest(http.MethodPost, "/webhooks/converty", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("exempt webhook rejected: called=%v status=%d", called, rec.Code)
	}
	if cookieByName(rec, CookieName) != nil {
		t.Fatal("exempt path should not be handed a CSRF cookie")
	}
}

func TestNoSecretStillSafe(t *testing.T) {
	// Even if signing material were empty, a request with no token is refused.
	m := New("", false)
	called, rec := serve(t, m, nil, httptest.NewRequest(http.MethodPost, "/x", nil))
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("token-less request accepted: called=%v status=%d", called, rec.Code)
	}
}
