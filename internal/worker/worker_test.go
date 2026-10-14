package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"whatsappconverty/internal/queue"
	"whatsappconverty/internal/whatsapp"
)

type fakeLookup struct {
	active bool
	err    error
	calls  int
	gotID  uuid.UUID
}

func (f *fakeLookup) AutomationEnabled(_ context.Context, id uuid.UUID) (bool, error) {
	f.calls++
	f.gotID = id
	return f.active, f.err
}

// sendPayload is a queued send as the runner hands it to a handler.
func sendPayload(t *testing.T, job whatsapp.SendWhatsAppTemplateJob) []byte {
	t.Helper()
	payload, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return payload
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A send queued while the automation was on must not go out after a pause, and
// the job itself is what the gate must consult — not any cached trigger state.
func TestGateDropsSendForPausedAutomation(t *testing.T) {
	automationID := uuid.New()
	lookup := &fakeLookup{active: false}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:       uuid.New(),
		AutomationID: automationID,
		CustomerID:   uuid.New(),
		TemplateID:   uuid.New(),
	}))
	if err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 0 {
		t.Fatalf("paused automation send was delivered (%d sends)", sent)
	}
	if lookup.calls != 1 || lookup.gotID != automationID {
		t.Fatalf("automation lookup: calls=%d id=%s, want 1 call for %s", lookup.calls, lookup.gotID, automationID)
	}
}

// A send with no automation (manual or legacy) must pass straight through.
func TestGatePassesNonAutomationSend(t *testing.T) {
	lookup := &fakeLookup{}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	if err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:     uuid.New(),
		CustomerID: uuid.New(),
		TemplateID: uuid.New(),
	})); err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("non-automation send was not delivered")
	}
	if lookup.calls != 0 {
		t.Fatalf("automation was consulted for a non-automation send")
	}
}

// An enabled automation whose integration is still active must reach the
// delivery handler: the worker gate only checks the automation, the integration
// is checked once at the delivery layer.
func TestGateDeliversEnabledAutomation(t *testing.T) {
	automationID := uuid.New()
	lookup := &fakeLookup{active: true}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:        uuid.New(),
		IntegrationID: uuid.New(),
		AutomationID:  automationID,
		CustomerID:    uuid.New(),
		TemplateID:    uuid.New(),
	}))
	if err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("enabled automation send was not delivered (%d sends)", sent)
	}
	if lookup.calls != 1 || lookup.gotID != automationID {
		t.Fatalf("automation lookup: calls=%d id=%s, want 1 call for %s", lookup.calls, lookup.gotID, automationID)
	}
}

// A failed enabled-lookup must fail closed: we cannot prove the automation is
// still on, so the message is neither delivered nor dropped — the error is
// returned so the runner retries it.
func TestGateFailsClosedWhenLookupErrors(t *testing.T) {
	sentinel := errors.New("db down")
	lookup := &fakeLookup{err: sentinel}
	sent := 0
	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	err := handler(context.Background(), sendPayload(t, whatsapp.SendWhatsAppTemplateJob{
		ShopID:       uuid.New(),
		AutomationID: uuid.New(),
		CustomerID:   uuid.New(),
		TemplateID:   uuid.New(),
	}))
	if err == nil {
		t.Fatal("lookup failure must return an error so the job is retried")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the lookup failure", err)
	}
	if sent != 0 {
		t.Fatalf("send delivered despite a failed automation lookup")
	}
}

// The worker gate must NOT be a second integration check: deactivation is
// enforced once at the delivery layer (whatsapp.gateIntegration), so a job the
// worker lets through still reaches the sender, which is where the store state
// is read. This guards against the two code paths diverging again.
func TestGateDoesNotConsultIntegrationState(t *testing.T) {
	job := whatsapp.SendWhatsAppTemplateJob{
		ShopID:        uuid.New(),
		IntegrationID: uuid.New(),
		AutomationID:  uuid.New(),
		CustomerID:    uuid.New(),
		TemplateID:    uuid.New(),
	}
	lookup := &fakeLookup{active: true}
	sent := 0

	handler := gateAutomation(lookup, func(context.Context, []byte) error {
		sent++
		return nil
	}, quietLogger())

	if err := handler(context.Background(), sendPayload(t, job)); err != nil {
		t.Fatalf("gate error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("queued send was dropped by the worker; integration must be gated only at delivery")
	}
}

// execCall records one statement the runner issued against its store.
type execCall struct {
	sql  string
	args []any
}

// fakeQueueDB is a database.Querier that hands the runner one pre-claimed job
// and records the bookkeeping writes (delete on complete/drop, update on
// retry), so the retry policy can be observed without a database.
type fakeQueueDB struct {
	mu     sync.Mutex
	claims []queue.Job
	execs  []execCall
}

func (d *fakeQueueDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs = append(d.execs, execCall{sql: sql, args: args})
	return pgconn.CommandTag{}, nil
}

func (d *fakeQueueDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := &fakeRows{jobs: d.claims}
	d.claims = nil
	return rows, nil
}

func (d *fakeQueueDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return errorRow{err: errors.New("unexpected QueryRow")}
}

func (d *fakeQueueDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

// fakeRows yields the claimed jobs. It embeds the pgx.Rows interface (left nil)
// and overrides only the methods the claim loop uses.
type fakeRows struct {
	pgx.Rows
	jobs []queue.Job
	i    int
	cur  *queue.Job
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.jobs) {
		return false
	}
	r.cur = &r.jobs[r.i]
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	j := r.cur
	*(dest[0].(*uuid.UUID)) = j.ID
	*(dest[1].(*string)) = j.Kind
	*(dest[2].(*uuid.UUID)) = j.ShopID
	*(dest[3].(*[]byte)) = j.Payload
	*(dest[4].(*int)) = j.Attempts
	*(dest[5].(*int)) = j.MaxAttempts
	return nil
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

// A transient automation-lookup failure must spend a queue attempt: the runner
// schedules a backoff retry and nothing is delivered on the failed attempt.
func TestQueueRetriesTransientAutomationLookupErrorWithoutSending(t *testing.T) {
	job := whatsapp.SendWhatsAppTemplateJob{
		ShopID:        uuid.New(),
		IntegrationID: uuid.New(),
		AutomationID:  uuid.New(),
		CustomerID:    uuid.New(),
		TemplateID:    uuid.New(),
	}

	db := &fakeQueueDB{claims: []queue.Job{{
		ID:          uuid.New(),
		Kind:        queue.TaskSendWhatsAppTemplate,
		ShopID:      job.ShopID,
		Payload:     sendPayload(t, job),
		Attempts:    1,
		MaxAttempts: 5,
	}}}

	sent := 0
	lookup := &fakeLookup{err: errors.New("db blip")}
	runner := queue.NewRunner(db, quietLogger())
	runner.Handle(queue.TaskSendWhatsAppTemplate, gateAutomation(lookup,
		func(context.Context, []byte) error { sent++; return nil }, quietLogger()))

	if err := runner.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sent != 0 {
		t.Fatalf("delivered on a failed lookup (%d sends)", sent)
	}
	if lookup.calls != 1 {
		t.Fatalf("automation lookup calls = %d, want 1", lookup.calls)
	}

	var retries, deletes int
	var delay float64
	for _, e := range db.execs {
		switch {
		case strings.Contains(e.sql, "UPDATE job_queue"):
			retries++
			if len(e.args) >= 3 {
				delay, _ = e.args[2].(float64)
			}
		case strings.Contains(e.sql, "DELETE FROM job_queue"):
			deletes++
		}
	}
	if retries != 1 {
		t.Fatalf("retry updates = %d, want 1 (execs=%+v)", retries, db.execs)
	}
	if deletes != 0 {
		t.Fatalf("job was deleted (%d deletes); a transient failure must be retried, not dropped", deletes)
	}
	if delay != (time.Minute).Seconds() {
		t.Fatalf("retry delay = %vs, want %vs (first-attempt backoff)", delay, (time.Minute).Seconds())
	}
}
