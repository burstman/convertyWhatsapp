# Converty WhatsApp — Phase 1A Security Findings

- **Review date:** 2026-10-13
- **Type:** read-only adversarial review of the Phase 1 remediation
- **Scope:** webhook trust boundary, consent integrity, deactivation/queue
  races, CSRF/session, tests.
- **Evidence basis:** code inspection only. Line references are to the tree at
  the time of review. No production APIs were called and no messages were sent.
- **Status legend:** `Open` (not fixed), `In remediation` (change in progress),
  `Fixed` (implemented **and** covered by a passing regression test),
  `Design` (requires a product decision).

The Phase 1 fixes themselves are present and sound: the inbound Converty
webhook has a single fail-closed trust boundary, the session cookie is hardened,
and a signed double-submit CSRF token guards unsafe requests. This review found
one residual CSRF gap (F-1) and one residual deactivation gap (F-2) plus lower
severity items.

## Confirmed vulnerabilities

### F-1 — State-changing `GET /templates/refresh` is CSRF-able
- **Severity:** Medium
- **Status:** Fixed (2026-10-13; route POST-only, UI POST form with CSRF token)
- **Affected:** `internal/server/app.go:138` (route), `internal/csrf/csrf.go:121`
  (`isSafeMethod` treats GET as safe), `web/views/dashboard/templates.templ:20`
  (link), `internal/server/templates_handlers.go:18` (handler).
- **Scenario:** `GET /templates/refresh` calls Meta's template list and writes
  the templates table. The CSRF middleware exempts all GET traffic, so no token
  is required. An attacker who gets an authenticated merchant to perform a
  top-level navigation to that URL (a link or `window.location`) rides the
  session cookie, which `SameSite=Lax` still sends on top-level GET
  navigations, and forces the refresh.
- **Impact:** Integrity and availability. Forced Meta API calls (quota
  consumption) and template-row churn, triggered without any form submission.
- **Recommended mitigation:** Make the route `POST`, update the UI to submit a
  POST form (CSRF token injected by `csrf.js`), and ensure GET can no longer
  reach the handler.
- **Regression test required:** The router must register only `POST` for
  `/templates/refresh`; the templates view must submit a POST form with the CSRF
  token and contain no `href="/templates/refresh"`.
- **Blocks staging?** Yes — fix before staging.
- **Blocks re-enabling webhook automations?** No.

### F-2 — Integration deactivation is not enforced on every send path
- **Severity:** Medium
- **Status:** Fixed (2026-10-13; single fail-closed delivery-layer gate)
- **Affected:** `internal/whatsapp/send.go:383` (`SendTemplateMessage` never
  consults integration state), `internal/worker/worker.go:90` (gate only runs
  for queued jobs), `internal/automations/processor.go:293` (instant sends
  inline), `internal/automations/suppressions.go:181` (retry-held builds a
  `SendInput` with no integration id).
- **Scenario:** Deactivating a Converty store stops new webhook events at
  ingestion (`dispatchConvertyEvent`) and stops *queued* scheduled jobs in the
  worker gate, but it does not stop:
  1. an instant/inline automation send racing deactivation, or
  2. a retry-held delivery triggered afterwards.
  In both cases `SendTemplateMessage` is the delivery point and it has no
  integration context, so a consented customer can still be messaged for a store
  the merchant has switched off.
- **Impact:** A deactivation control that does not hold at delivery. Especially
  relevant once Converty webhook automations are re-enabled.
- **Recommended mitigation:** One shared fail-closed integration gate at the
  delivery layer (`SendTemplateMessage`), carrying the integration id through
  every request/job/retry structure. Missing/deleted/inactive integration ⇒ do
  not send; lookup error ⇒ return an error so the existing retry policy applies;
  never silently permit.
- **Regression test required:** inactive before instant send; inactive before
  retry-held; deleted before delivery; lookup/database error; queued job retried
  after deactivation between attempts; legacy job with no integration id;
  active integration still sends; deactivate-then-reactivate semantics.
- **Blocks staging?** Conditional — limited live impact today only because
  webhook automations are fail-closed. Treat as a blocker for re-enabling
  Converty webhook automations.
- **Blocks re-enabling webhook automations?** Yes.

### F-3 — Consent can be self-asserted; no real opt-in proof
- **Severity:** Medium (business/legal)
- **Status:** Design
- **Affected:** `internal/server/api_whatsapp.go:51` (`handleAPIGrantConsent`
  accepts free-text `source`/`evidence`), `internal/whatsapp/state.go:91`
  (`GrantConsent` checks only customer ownership; category defaults to
  `order_updates`).
- **Scenario:** Any authenticated user (or a stolen session) can manufacture an
  opt-in for any customer they hold a phone number for, then drive UTILITY
  sends.
- **Impact:** Auth is treated as proof of consent. Regulatory/reputation risk.
- **Recommended mitigation:** Require documented evidence, restrict to owner,
  record an immutable granted-by/granted-at audit, and reconsider whether
  merchant-declared consent may authorize non-transactional categories.
- **Regression test required:** cross-shop customer grant fails; role
  restrictions enforced if adopted.
- **Blocks staging?** No (product decision).
- **Blocks re-enabling webhook automations?** No.

### F-4 — `SendTemplateTest` bypasses consent, onboarding flags and integration state
- **Severity:** Medium (by design, documented in code)
- **Status:** Fixed (2026-10-13; owner-only + dedicated rate limit + audit)
- **Affected:** `internal/whatsapp/send.go:557`, route
  `POST /automations/{id}/test` → `internal/server/automations_handlers.go:480`.
- **Scenario:** Any authenticated member can send an approved template to an
  arbitrary E.164 number. Billing lands on the platform's central Meta account;
  the path enables unsolicited messaging and recipient probing.
- **Impact:** Cost + abuse (spam), mitigated only by the shared shop rate limit.
- **Recommended mitigation:** Restrict to owner, add a dedicated test-send rate
  limit, log recipient and actor.
- **Regression test required:** role restriction and rate limit (if adopted).
- **Blocks staging?** No (product decision).
- **Blocks re-enabling webhook automations?** No.
- **Remediation (2026-10-13):**
  - **Owner-only.** `handleAutomationTest` returns the not-found page for
    non-owners *before* any lookup or send. It checks `IsOwner()` directly and
    returns unconditionally (the shared `requireOwner` helper was unused at the
    time because of F-8; with F-8 fixed either works).
  - **Dedicated rate limit.** `RateLimiter.AllowTestSend` (scope
    `test-send:<shop>:<actor>`, default 10/hour, `TEST_SEND_RATE_LIMIT` /
    `TEST_SEND_RATE_WINDOW`) is charged inside `SendTemplateTest` before any
    template read, message row or Meta call, and is independent of the
    customer-send windows. The scope carries neither template nor automation, so
    switching either cannot reset it. The upsert re-checks the ceiling as it
    increments (verified against PostgreSQL: the over-limit branch returns no
    row), so concurrent requests cannot both slip past.
  - **Audit.** Every authorized attempt writes one `test_send_audit` row
    (migration `0028`) with shop, actor, automation, template, outcome
    (`accepted` | `rejected` | `failed` | `rate_limited`), and a keyed,
    shop-scoped HMAC of the recipient — never the number or any body. "accepted"
    means Meta accepted the API request, not that the message was delivered.
  - Covered by `internal/whatsapp/ratelimit_test.go` (scope, concurrency,
    nil-db), `internal/whatsapp/audit_test.go` (hash) and
    `internal/server/automation_test_send_test.go` (member blocked; tenant
    scoping; accepted path; rate-limited; provider refusal; infra failure).
  - The raw recipient was removed from the test-send log line.

### F-5 — Inconsistent gate failure policy (automation fail-open, integration fail-closed)
- **Severity:** Low
- **Status:** **Fixed** (2026-10-13)
- **Affected (before fix):** `internal/worker/worker.go` `gateAutomation`
  (the `AutomationEnabled` lookup error was logged and the send continued, i.e.
  fail-open) vs the delivery-layer integration gate, which returns an error so
  the send is retried (fail-closed).
- **Scenario:** Under DB pressure the automation-enabled check could fail and let
  a paused automation's queued send proceed (the consent gate still applied).
- **Impact:** A pause control could be bypassed transiently.
- **Fix:** `gateAutomation` now returns the lookup error
  (`fmt.Errorf("automation gate: lookup %s: %w", ...)`) so the job follows the
  existing queue retry/backoff policy instead of sending. A confirmed inactive
  automation (`!active`, including a deleted row, which
  `automations.AutomationEnabled` reports as `(false, nil)` for `pgx.ErrNoRows`)
  still drops the job by returning `nil`; the distinction between "not found"
  and "infrastructure error" is preserved. The integration side is checked once
  at the delivery layer (`whatsapp.gateIntegration`), which was already
  fail-closed and is unchanged.
- **Regression tests:** `internal/worker/worker_test.go` — an enabled automation
  reaches the delivery handler; a paused automation does not; an automation
  lookup error returns an error and does not deliver; a full
  `queue.Runner.Drain` run with a fake `database.Querier` shows a transient
  lookup error schedules a backoff retry (`UPDATE job_queue`, 1-minute
  first-attempt backoff) with no delivery and no delete.
  `internal/whatsapp/gate_test.go` continues to cover the integration lookup
  failure failing closed (`TestGateIntegrationReturnsErrorOnLookupFailure`).
- **Blocks staging?** No.
- **Blocks re-enabling webhook automations?** No.

### F-6 — Legacy queued jobs skip the integration gate
- **Severity:** Low / Informational
- **Status:** Fixed (2026-10-13, with F-2; legacy sends are now refused)
- **Affected:** `internal/worker/worker.go:90` (`job.IntegrationID == uuid.Nil`
  skips the check); `internal/whatsapp/send_job.go:25`.
- **Scenario:** A job enqueued before this change carries no `integration_id`,
  so a scheduled send can be delivered after its store was deactivated.
- **Impact:** Bounded post-deploy window; scheduled sends can be hours out.
- **Recommended mitigation:** Treat a missing integration id on an
  automation-driven send as unverifiable and refuse it (fail closed) rather than
  bypassing. Document that legacy payloads are dropped.
- **Regression test required:** a legacy job with no integration id is not
  delivered.
- **Blocks staging?** No.
- **Blocks re-enabling webhook automations?** Yes (same mechanism as F-2).

### F-7 — CSRF middleware parses the request body before verifying the token
- **Severity:** Low (DoS)
- **Status:** Fixed (2026-10-14; request bodies are capped before any parse)
- **Affected:** `internal/csrf/csrf.go:106` (falls back to `r.FormValue`, which
  parses the body, before the comparison at `:110`); no global body-size limit in
  `internal/server/middleware.go`.
- **Scenario:** The CSRF middleware runs globally and, for an unsafe method with
  no `X-CSRF-Token` header, called `r.FormValue` — parsing the full body,
  including multipart, and spooling large uploads to disk — before it could know
  the request was unauthenticated. An unauthenticated `POST` with a large
  multipart body to any non-exempt path could therefore force
  buffering/temp-file writes before rejection for a missing token.
- **Impact:** Resource-amplification DoS.
- **Fix:** A new `bodyLimitMiddleware` (`internal/server/middleware.go`) is
  registered in `InitializeMiddleware` **before** the CSRF middleware, so it runs
  ahead of every body parser and handler. It reads the body through
  `http.MaxBytesReader` and restores the bytes verbatim on the request when
  within the limit, so signature verification and parsers observe exactly what
  the client sent. An oversized body is answered `413 Request Entity Too Large`
  and never reaches downstream middleware. `Content-Length` is deliberately
  ignored (a missing or understated length cannot bypass the cap). Limits are
  named constants: `formBodyLimitBytes` (1 MiB) for dashboard/HTMX/JSON requests
  and the larger `webhookBodyLimitBytes` (2 MiB) for the Meta and Converty
  webhook paths, selected by path. The two webhook handlers now share
  `webhookBodyLimitBytes` instead of their own magic numbers (kept as a
  defense-in-depth `io.LimitReader`).
- **Regression test required:** covered by
  `internal/server/body_limit_test.go`: in-limit request passes untouched;
  oversized urlencoded and multipart are 413; the cap runs before the CSRF parse
  (413, never 403); a valid-CSRF in-limit request still succeeds; oversized
  requests reach no handler/side effect; a webhook body is restored
  byte-for-byte so its HMAC still verifies; an oversized webhook is rejected
  before dispatch; a missing/understated `Content-Length` cannot bypass; the
  boundary accepts exactly the limit and rejects one byte over; the path selects
  the webhook vs form limit; a non-overflow read error is 400.
- **Blocks staging?** No.
- **Blocks re-enabling webhook automations?** No.

### F-8 — The shared `requireOwner` gate did not stop members (owner-only handlers fell through)
- **Severity:** High (authorization bypass)
- **Status:** **Fixed** (2026-10-13)
- **Root cause:** `internal/server/whatsapp_handlers.go` `requireOwner` rendered the
  not-found page but returned the render error, which is nil on success, so every
  caller using `if err := a.requireOwner(k); err != nil { return err }` fell
  through and kept executing the privileged work:
  `whatsapp_handlers.go` (`/settings`, `/settings/phone`, `/settings/disable`,
  `/whatsapp/onboard`, `/billing`) and `team_handlers.go` (`/team`,
  `/team/invite`, `/team/{id}/delete`).
- **Impact (before fix):** any signed-in **member** could perform owner-only
  actions (edit the shop phone, disable the service, invite/remove users, accept
  onboarding terms, read billing). The UI hid the controls, but the routes were
  directly reachable. Confirmed by test: `requireOwner` returned `err=<nil>` and
  rendered "Page not found", i.e. the caller's `if err != nil` never returned.
- **Fix:** `requireOwner` now returns an explicit `(allowed bool, err error)`
  result. For a non-owner it renders the not-found page and returns
  `(false, nil)`; if that render fails it returns `(false, err)`. All 9 call sites
  use:
  ```go
  allowed, err := a.requireOwner(k)
  if err != nil {
      return err
  }
  if !allowed {
      return nil
  }
  ```
  so a non-owner can no longer continue into the handler, the render error is no
  longer swallowed, and execution never depends on the response alone.
- **Regression tests:** `internal/server/require_owner_test.go` —
  - gate returns `(false, nil)` + the not-found page for a **member** and for an
    **unauthenticated** request, and `(true, nil)` + no body for an **owner**;
  - a table test drives **every** owner-only handler with a member against an
    `App` with nil services (a fall-through would panic), asserting no redirect
    and the not-found page — proving no side effect is reached;
  - the owner counterpart proves owners are not stopped: service-free handlers
    reach their own redirect, and service-touching handlers reach their protected
    service (a nil repository panics, i.e. the dependency was exercised);
  - a route-registration test confirms each path is wired exactly as in `app.go`.
- **Blocks staging?** No (fixed).
- **Blocks re-enabling webhook automations?** No.

## Potential risks (not independently confirmed as exploitable)

- **Idempotency precedes the consent gate:** `SendTemplateMessage`
  (`internal/whatsapp/send.go:384`) returns an existing message row before
  CAN_SEND is evaluated. A row left `queued`/`failed` short-circuits a later send
  even if consent was revoked in between. Bounded (no new send), but the
  returned result can mask a revoked-consent state. Confirm intended.
- **CSRF token is not bound to the session:** safe against forgery (needs the
  HMAC secret) but a valid token is reusable for its 12h life; consider binding
  it to the session token.
- **Raw Meta error text stored in `messages.error_message`:** PII/operational
  detail persists in the ledger (carried over from the Phase 1 audit, MEDIUM 9).

## Verified-good controls (inspected, no issue found)

- **Single webhook trust boundary:** `internal/server/converty_handlers.go:84`
  (`dispatchConvertyEvent`) drops duplicates, unattributed stores, inactive
  integrations, a nil verifier and unverifiable events *before* the verifier and
  the automation sink. The default verifier fails closed
  (`internal/converty/verify.go:35`). No consent is created on the webhook path
  (`internal/automations/processor.go:59`).
- **Worker re-checks per attempt:** the gate wraps the handler
  (`internal/worker/worker.go:44`) and the queue re-invokes the handler on each
  attempt (`internal/queue/queue.go:260`); leases use `FOR UPDATE SKIP LOCKED`.
- **CSRF robustness:** `internal/csrf/csrf_test.go` covers tampered cookie,
  mismatch, missing token, cookie-only, cross-origin, exempt path and empty
  secret; `HX-Request`/`Origin` are not trusted.
- **Session hardening:** HttpOnly + Secure(prod) + SameSite=Lax on every save
  (`internal/server/session.go`); login rotates the token.
- **Tenant isolation** is consistently enforced by `shop_id` scoping; consent
  rows append (no unique constraint), so re-grant after revoke works.

## Staging conclusion

After this pass, the two staging-relevant issues are resolved: F-1 (state-changing
GET) is now a CSRF-guarded POST, and F-2 (deactivation not enforced at delivery)
is fixed by a single fail-closed integration gate that covers inline, queued and
retry-held sends, with legacy sends without an integration id refused. F-2/F-6
were the blocker for re-enabling Converty webhook automations; they no longer
block on this mechanism, though re-enabling still depends on the unresolved
webhook-verification product decision below. F-4 (test sends) is now fixed:
owner-only, dedicated rate limit and an audit trail. F-8 (the `requireOwner`
authorization bypass) and F-5 (automation-lookup fail-open) are also fixed. F-7
(the CSRF body-parse DoS) is fixed: a path-aware request-body cap runs before
the CSRF middleware and every handler. F-3 is the only remaining open item; it
requires a product decision, not a code fix, and does not block staging.

## Most important missing regression tests

1. Retry-held delivery after integration deactivation delivers nothing (F-2).
2. `GET /templates/refresh` is not routable / non-mutating; the UI submits POST
   with a CSRF token (F-1).
3. Webhook end-to-end: an unverified delivery returns 200, writes no message or
   consent row, and never reaches the sink (already partly covered).
4. Queued job retried after deactivation between attempts is gated on the retry
   (F-2 / F-6).
5. `SendTemplateTest` role / rate-limit enforcement (F-4) — **now covered**; and
   the `requireOwner` bypass (F-8) for each owner-only route — **now covered**.
6. Automation-lookup failure fails closed and the queue retries it (F-5) —
   **now covered**.
7. Request bodies are capped before CSRF/handler parsing: oversized urlencoded
   and multipart bodies get 413, webhook bodies are preserved byte-for-byte for
   signature checks, and Content-Length cannot bypass the cap (F-7) —
   **now covered** by `internal/server/body_limit_test.go`.

## Unresolved product decisions

1. **Fail-closed webhook verification.** Converty signs no webhooks and exposes
   no order-by-id read, so webhook-driven automations intentionally do not fire.
   Keep disabled, or invest in a verification mechanism? Keep storing/showing
   the events?
2. **Consent model (F-3).** Is merchant-declared consent acceptable, and for
   which categories? Who may grant (owner vs member)?
3. **Test sends (F-4).** Now owner-only with a default 10/hour per-actor,
   per-shop budget (`TEST_SEND_RATE_LIMIT`/`TEST_SEND_RATE_WINDOW`). Tune the
   ceiling if needed.
4. **Deactivation semantics (F-2).** Should deactivation also block manual test
   and retry-held sends? (This pass chooses the safest behavior: block.)
5. **Member blast radius (prior audit LOW-11).** Members can delete integrations
   and automations.

## Remediation log

- **F-1:** `GET /templates/refresh` → `POST` (`internal/server/app.go`); UI link
  → POST form (`web/views/dashboard/templates.templ`). Covered by
  `internal/server/routes_test.go` (route table POST-only) and
  `web/views/dashboard/templates_test.go` (POST form, no GET link). Status:
  **Fixed**, tests passing.
- **F-2 / F-6:** single fail-closed integration gate in
  `whatsapp.gateIntegration` (called from `SendTemplateMessage`); integration id
  added to `SendRequest`, `SendWhatsAppTemplateJob` and `automation_suppressions`
  (migration `0027`); carried through inline, queued and retry-held sends; the
  worker no longer performs a second (divergent) integration check; legacy
  sends without an id are refused. Covered by `internal/whatsapp/gate_test.go`,
  `internal/worker/worker_test.go` and `internal/automations/processor_test.go`.
  Status: **Fixed**, tests passing.
- **F-5:** `gateAutomation` (`internal/worker/worker.go`) now returns the
  `AutomationEnabled` lookup error instead of sending anyway, so a transient
  database error follows the queue retry policy; a confirmed inactive/deleted
  automation still drops the job. Covered by `internal/worker/worker_test.go`
  (enabled reaches the handler; paused does not; lookup error returns an error
  and does not deliver; a `queue.Runner.Drain` run schedules a backoff retry with
  no send or delete). The delivery-layer integration gate remains fail-closed
  (`internal/whatsapp/gate_test.go`). Status: **Fixed**, tests passing.
- **F-4:** `handleAutomationTest` is owner-only (explicit `IsOwner()` early
  return); `SendTemplateTest` charges `RateLimiter.AllowTestSend`
  (`test-send:<shop>:<actor>`, default 10/hour, config
  `TEST_SEND_RATE_LIMIT`/`TEST_SEND_RATE_WINDOW`) before any Meta call;
  `test_send_audit` (migration `0028`) records actor/automation/template/outcome
  and a keyed recipient hash. The owner-only Test control is hidden from members
  in `web/views/dashboard/automations.templ`. Covered by
  `internal/whatsapp/ratelimit_test.go`, `internal/whatsapp/audit_test.go` and
  `internal/server/automation_test_send_test.go`. Status: **Fixed**, tests
  passing (migration applied to a throwaway local Postgres; over-limit SQL
  returns no row as expected).
- **F-8 (new):** discovered while fixing F-4; `requireOwner` rendered the
  404 page but returned nil, so its callers fell through. **Fixed:** the helper
  now returns an explicit `(allowed bool, err error)` and all 9 owner-only call
  sites stop unless `allowed` is true. Covered by
  `internal/server/require_owner_test.go` (member/unauthenticated blocked for
  every route, owner allowed, route registration). Status: **Fixed**.
- **F-7:** added `bodyLimitMiddleware` (`internal/server/middleware.go`),
  registered in `InitializeMiddleware` **before** the CSRF middleware so the
  request body is capped ahead of any parser. Bodies within the limit are
  restored byte-for-byte; oversized bodies get `413` and never reach downstream
  middleware or handlers. `Content-Length` is ignored. Named limits
  `formBodyLimitBytes` (1 MiB) and `webhookBodyLimitBytes` (2 MiB), selected by
  path; the Meta and Converty handlers now share the webhook limit. Covered by
  `internal/server/body_limit_test.go`. Status: **Fixed**, tests passing.

