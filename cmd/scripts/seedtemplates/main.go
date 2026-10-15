package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/whatsapp"
)

// seedtemplates imports the platform's templates from the central messaging
// account into the global templates table. In the multi-tenant single-WABA
// model every merchant sends through the platform's WABA, so the platform's
// approved template library IS the pool of global templates every client can
// use. Only Meta's own statuses are stored; approval never comes from the
// frontend.
//
// Required env:
//
//	META_SYSTEM_USER_TOKEN  system-user access token
//
// Optional: META_MESSAGING_ACCOUNT_ID (defaults to the central one in config).
func main() {
	cfg := config.Load()
	logger := logutil.New(cfg)

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	token := os.Getenv("META_SYSTEM_USER_TOKEN")
	if token == "" {
		fatal("META_SYSTEM_USER_TOKEN is required")
	}
	messagingAccountID := os.Getenv("META_MESSAGING_ACCOUNT_ID")
	if messagingAccountID == "" {
		messagingAccountID = cfg.MetaMessagingAccountID
	}

	svc := whatsapp.NewService(cfg, pool, logger, nil)
	templates, err := svc.ListTemplates(ctx, token, messagingAccountID)
	if err != nil {
		fatal(fmt.Sprintf("list templates: %v", err))
	}

	imported := 0
	for _, t := range templates {
		approval := whatsapp.NormalizeApproval(t.Status)
		if approval == "deleted" {
			continue
		}

		var id uuid.UUID
		rowErr := pool.QueryRow(ctx, `
			INSERT INTO templates (
				scope, client_id, meta_template_name, meta_template_id, language, category,
				status, approval_status, rejection_reason, components
			) VALUES ('global', NULL, $1, $2, $3, $4, $5, $6, $7, $8::jsonb)
			ON CONFLICT (meta_template_name, language) WHERE scope = 'global' DO UPDATE SET
				meta_template_id   = EXCLUDED.meta_template_id,
				category          = EXCLUDED.category,
				status            = EXCLUDED.status,
				approval_status   = EXCLUDED.approval_status,
				rejection_reason  = EXCLUDED.rejection_reason,
				components        = EXCLUDED.components,
				updated_at        = now()
			RETURNING id`,
			t.Name, t.ID, t.Language, t.Category,
			"imported", approval, "", t.Components,
		).Scan(&id)
		if rowErr != nil {
			logger.Error("template import failed", "name", t.Name, "error", rowErr)
			continue
		}
		imported++
		logger.Info("template imported", "name", t.Name, "language", t.Language,
			"category", t.Category, "approval_status", approval, "id", id)
	}

	logger.Info("template import complete", "found", len(templates), "imported", imported)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "seedtemplates:", msg)
	os.Exit(1)
}
