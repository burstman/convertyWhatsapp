package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"whatsappconverty/internal/csrf"
)

// serveLimited sends a POST through enforceBodyLimit(limit) and reports the
// response status plus how many times the wrapped handler was invoked and the
// exact body the handler observed.
func serveLimited(limit int64, contentType string, body []byte) (int, int, []byte) {
	calls := 0
	var seen []byte
	h := enforceBodyLimit(limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/settings/phone", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, calls, seen
}

// A well-formed request within the limit reaches the handler untouched.
func TestBodyLimitAllowsRequestWithinLimit(t *testing.T) {
	const limit = 1024
	body := []byte("name=value")

	status, calls, seen := serveLimited(limit, "application/x-www-form-urlencoded", body)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if !bytes.Equal(seen, body) {
		t.Fatalf("handler saw %q, want %q", seen, body)
	}
}

// An oversized urlencoded body is rejected with 413 (not an accidental 500) and
// never parsed.
func TestBodyLimitRejectsOversizedForm(t *testing.T) {
	const limit = 64
	body := bytes.Repeat([]byte("a"), limit+1)

	status, calls, _ := serveLimited(limit, "application/x-www-form-urlencoded", body)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	if calls != 0 {
		t.Fatalf("oversized form reached the handler %d times", calls)
	}
}

// Multipart bodies — the case the CSRF middleware would otherwise buffer or
// spool to disk — are also capped.
func TestBodyLimitRejectsOversizedMultipart(t *testing.T) {
	const limit = 64
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormField("payload")
	if err != nil {
		t.Fatalf("create form field: %v", err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatalf("write form field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	status, calls, _ := serveLimited(limit, mw.FormDataContentType(), buf.Bytes())
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	if calls != 0 {
		t.Fatalf("oversized multipart reached the handler %d times", calls)
	}
}

// The limit must run before the CSRF middleware falls back to r.FormValue,
// which parses the body. If CSRF ran first it would answer 403 for a missing
// token; a 413 proves the body was rejected first.
func TestBodyLimitRunsBeforeCSRFParse(t *testing.T) {
	a := newRouteTestApp()
	const limit = 64
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ })
	h := enforceBodyLimit(limit, a.CSRF.Middleware(nil, nil)(handler))

	req := httptest.NewRequest(http.MethodPost, "/settings/phone", bytes.NewReader(bytes.Repeat([]byte("a"), limit+1)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (CSRF must not parse the body first)", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("handler ran %d times after an oversized body", calls)
	}
}

// A well-formed request with a valid CSRF token, within the limit, still
// succeeds and the body is unchanged.
func TestBodyLimitAllowsValidCSRFRequest(t *testing.T) {
	a := newRouteTestApp()
	token := issueCSRFToken(t, a.CSRF)

	body := []byte("name=value")
	var seen []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	h := enforceBodyLimit(1<<20, a.CSRF.Middleware(nil, nil)(handler))

	req := httptest.NewRequest(http.MethodPost, "/settings/phone", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrf.HeaderName, token)
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(seen, body) {
		t.Fatalf("handler saw %q, want %q", seen, body)
	}
}

// An oversized request must be stopped before it can trigger any side effect.
func TestBodyLimitOversizedHasNoSideEffects(t *testing.T) {
	const limit = 128
	var sideEffects int
	h := enforceBodyLimit(limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sideEffects++
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/automations/1/test", bytes.NewReader(bytes.Repeat([]byte("x"), limit+1)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if sideEffects != 0 {
		t.Fatalf("oversized request triggered %d side effects", sideEffects)
	}
}

// A webhook body within the limit is restored byte-for-byte so an HMAC
// signature over the raw payload still verifies.
func TestBodyLimitWebhookPreservesBytesForSignature(t *testing.T) {
	secret := []byte("webhook-secret")
	body := []byte(`{"event":"order.update","data":{"_id":"abc","status":"delivered"}}`)
	calls := 0
	var verified bool
	h := enforceBodyLimit(webhookBodyLimitBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		verified = hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(signBody(secret, raw))) &&
			bytes.Equal(raw, body)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/meta", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("webhook handler calls = %d, want 1", calls)
	}
	if !verified {
		t.Fatal("signature over the restored webhook body did not verify")
	}
}

// An oversized webhook delivery is rejected without any downstream processing.
func TestBodyLimitOversizedWebhookRejected(t *testing.T) {
	calls := 0
	h := enforceBodyLimit(webhookBodyLimitBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))

	body := bytes.Repeat([]byte("x"), int(webhookBodyLimitBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/converty", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("oversized webhook reached the handler %d times", calls)
	}
}

// The cap is applied to the bytes actually read, so a missing or understated
// Content-Length cannot smuggle an oversized body past it.
func TestBodyLimitIgnoresContentLength(t *testing.T) {
	const limit = 64
	body := bytes.Repeat([]byte("x"), limit+32)

	for _, claimed := range []int64{0, 4, -1} {
		t.Run(strconv.FormatInt(claimed, 10), func(t *testing.T) {
			calls := 0
			h := enforceBodyLimit(limit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
			}))
			req := httptest.NewRequest(http.MethodPost, "/settings/phone", bytes.NewReader(body))
			req.ContentLength = claimed
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 for claimed Content-Length %d", rec.Code, claimed)
			}
			if calls != 0 {
				t.Fatalf("oversized body reached the handler with claimed Content-Length %d", claimed)
			}
		})
	}
}

// Exactly the limit is accepted; one byte over is rejected.
func TestBodyLimitBoundary(t *testing.T) {
	const limit = 64

	status, calls, seen := serveLimited(limit, "application/x-www-form-urlencoded", bytes.Repeat([]byte("x"), limit))
	if status != http.StatusOK {
		t.Fatalf("at-limit status = %d, want 200", status)
	}
	if calls != 1 || len(seen) != limit {
		t.Fatalf("at-limit handler calls = %d, body = %d bytes; want 1 call, %d bytes", calls, len(seen), limit)
	}

	status, calls, _ = serveLimited(limit, "application/x-www-form-urlencoded", bytes.Repeat([]byte("x"), limit+1))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit status = %d, want 413", status)
	}
	if calls != 0 {
		t.Fatalf("over-limit handler calls = %d, want 0", calls)
	}
}

// The middleware picks the webhook limit for webhook paths and the tighter form
// limit everywhere else.
func TestBodyLimitSelectsLimitByPath(t *testing.T) {
	a := newRouteTestApp()
	reached := 0
	h := a.bodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	// Just over the form limit but still within the webhook limit.
	body := bytes.Repeat([]byte("x"), int(formBodyLimitBytes)+1)

	formRec := httptest.NewRecorder()
	h.ServeHTTP(formRec, httptest.NewRequest(http.MethodPost, "/settings/phone", bytes.NewReader(body)))
	if formRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("form path status = %d, want 413", formRec.Code)
	}
	if reached != 0 {
		t.Fatalf("form-path handler reached %d times, want 0", reached)
	}

	hookRec := httptest.NewRecorder()
	h.ServeHTTP(hookRec, httptest.NewRequest(http.MethodPost, "/webhooks/converty", bytes.NewReader(body)))
	if hookRec.Code != http.StatusOK {
		t.Fatalf("webhook path status = %d, want 200", hookRec.Code)
	}
	if reached != 1 {
		t.Fatalf("webhook-path handler reached %d times, want 1", reached)
	}
}

// A read failure that is not a size overflow is refused as 400, not 500.
func TestBodyLimitReadErrorBadRequest(t *testing.T) {
	calls := 0
	h := enforceBodyLimit(1024, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	req := httptest.NewRequest(http.MethodPost, "/settings/phone", failingReader{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("handler reached after a body read error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func signBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// issueCSRFToken obtains a valid signed token by running one request through
// the manager and reading back the cookie it issues.
func issueCSRFToken(t *testing.T, m *csrf.Manager) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Middleware(nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrf.CookieName {
			return c.Value
		}
	}
	t.Fatal("csrf manager did not issue a cookie")
	return ""
}
