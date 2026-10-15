package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/queue"
)

// PurgeMarketingTemplateJob is the delayed task payload for removing a
// template Meta flagged as marketing.
type PurgeMarketingTemplateJob struct {
	ShopID     uuid.UUID `json:"shop_id"`
	TemplateID uuid.UUID `json:"template_id"`
	Name       string    `json:"name"`
	Language   string    `json:"language"`
}

// enqueueUnscheduledPurges queues the purge job for every template that is
// flagged as marketing but has no scheduled purge yet. The job fires after the
// configured delay, keyed on the flagging episode so re-submitting the same
// template cannot pile up duplicate purges. PurgeExpiredMarketing is the lazy
// path that cleans up on the next templates page load even if a job is lost.
func (s *Service) enqueueUnscheduledPurges(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id,
		       (SELECT s.id FROM shops s WHERE s.client_id = t.client_id
		        ORDER BY s.created_at ASC LIMIT 1) AS shop_id,
		       meta_template_name, language, marketing_flagged_at
		FROM templates t
		WHERE marketing_flagged AND purge_scheduled_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type unscheduled struct {
		PurgeMarketingTemplateJob
		FlaggedAt time.Time
	}
	var jobs []unscheduled
	for rows.Next() {
		var j unscheduled
		var shopID *uuid.UUID
		if err := rows.Scan(&j.TemplateID, &shopID, &j.Name, &j.Language, &j.FlaggedAt); err != nil {
			return err
		}
		if shopID == nil {
			s.log.Warn("marketing purge skipped: template client has no shop", "template_id", j.TemplateID)
			continue
		}
		j.ShopID = *shopID
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, j := range jobs {
		if err := s.enqueuePurge(ctx, j.PurgeMarketingTemplateJob, j.FlaggedAt); err != nil {
			s.log.Warn("marketing purge enqueue failed", "template_id", j.TemplateID, "error", err)
			continue
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE templates SET purge_scheduled_at = now() WHERE id = $1`, j.TemplateID); err != nil {
			return err
		}
	}
	return nil
}

// enqueuePurge parks one purge until the marketing delay has passed. The dedupe
// key names the flagging episode (template id plus when it was flagged) so a
// later re-flag of the same template can still schedule its own purge.
func (s *Service) enqueuePurge(ctx context.Context, j PurgeMarketingTemplateJob, flaggedAt time.Time) error {
	payload, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return queue.Enqueue(ctx, s.pool, queue.Params{
		Kind:      queue.TaskPurgeMarketingTemplate,
		ShopID:    j.ShopID,
		Payload:   payload,
		RunAt:     time.Now().Add(s.cfg.MarketingPurgeDelay),
		DedupeKey: fmt.Sprintf("%s:%d", j.TemplateID, flaggedAt.Unix()),
	})
}

// PurgeExpiredMarketing marks every flagged template whose purge deadline has
// passed as deleted. It is the lazy fallback that keeps the "gone after N
// minutes" promise even when no worker is running, and the row stays visible
// to the merchant as "deleted". Returns the number newly deleted.
func (s *Service) PurgeExpiredMarketing(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, meta_template_name, language
		FROM templates
		WHERE marketing_flagged
		  AND marketing_flagged_at IS NOT NULL
		  AND marketing_flagged_at < now() - ($1::interval)
		  AND approval_status <> 'deleted'`,
		pgInterval(s.cfg.MarketingPurgeDelay),
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var expired []PurgeMarketingTemplateJob
	for rows.Next() {
		var j PurgeMarketingTemplateJob
		if err := rows.Scan(&j.TemplateID, &j.Name, &j.Language); err != nil {
			return 0, err
		}
		expired = append(expired, j)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, j := range expired {
		if err := s.purgeTemplate(ctx, j); err != nil {
			s.log.Warn("marketing purge failed; will retry", "template_id", j.TemplateID, "error", err)
		}
	}
	return len(expired), nil
}

// HandlePurgeMarketingTemplate is the queue handler for the delayed purge job.
// It is safe to run repeatedly: missing rows or templates that are no longer
// flagged are skipped.
func (s *Service) HandlePurgeMarketingTemplate(ctx context.Context, payload []byte) error {
	var j PurgeMarketingTemplateJob
	if err := json.Unmarshal(payload, &j); err != nil {
		return queue.Permanent(fmt.Errorf("whatsapp: decode purge job: %w", err))
	}

	var stillFlagged bool
	err := s.pool.QueryRow(ctx, `
		SELECT marketing_flagged FROM templates WHERE id = $1`,
		j.TemplateID,
	).Scan(&stillFlagged)
	if err != nil {
		// Row already gone — job is done.
		return nil
	}
	if !stillFlagged {
		return nil
	}
	return s.purgeTemplate(ctx, j)
}

// pgInterval converts a Go duration to text Postgres accepts as an interval
// literal (time.Duration.String() like "15m0s" is not valid Postgres syntax).
func pgInterval(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

// purgeTemplate moves the template to the "deleted" lifecycle state so the
// merchant sees the deletion notice, after best-effort removal from the
// central messaging account. Deleted templates can never be sent.
func (s *Service) purgeTemplate(ctx context.Context, j PurgeMarketingTemplateJob) error {
	s.log.Warn("deleting marketing-flagged template",
		"shop_id", j.ShopID, "template_id", j.TemplateID, "name", j.Name, "language", j.Language)

	if s.cfg.MetaSystemUserToken != "" && s.cfg.MetaMessagingAccountID != "" {
		if err := s.DeleteTemplate(ctx, s.cfg.MetaSystemUserToken, s.cfg.MetaMessagingAccountID, j.Name, j.Language); err != nil {
			// Best-effort: Meta may already have removed it (404). The local
			// row still transitions to deleted; the central account stays under
			// Meta's policy.
			s.log.Warn("meta delete of marketing template failed", "name", j.Name, "error", err)
		}
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE templates
		SET approval_status = 'deleted',
		    purge_scheduled_at = NULL,
		    updated_at        = now()
		WHERE id = $1 AND approval_status <> 'deleted'`,
		j.TemplateID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		s.log.Info("marketing template deleted", "shop_id", j.ShopID, "template_id", j.TemplateID)
	}
	return nil
}
