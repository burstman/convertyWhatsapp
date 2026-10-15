package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/whatsapp"
)

// seednumbers populates the platform's rentable number pool
// (whatsapp_numbers) from the phone numbers registered on a Meta messaging
// account. Under the BSP model these are the numbers the platform owns and
// can provision to shops during onboarding. Idempotent: already-known
// phone_number_ids are refreshed, not duplicated.
//
// Env:
//
//	META_SYSTEM_USER_TOKEN   platform system-user access token
//	META_MESSAGING_ACCOUNT_ID  messaging account id whose numbers to import
//
// Optional: META_WAAC_ID, META_BUSINESS_PORTFOLIO_ID.
//
// The platform token used here is the one stored in whatsapp_numbers-free
// config; provisioning later encrypts the same token into each shop's
// whatsapp_integrations row.
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
	messagingAccountID := os.Getenv("META_MESSAGING_ACCOUNT_ID")
	if token == "" || messagingAccountID == "" {
		fatal("META_SYSTEM_USER_TOKEN and META_MESSAGING_ACCOUNT_ID are required")
	}

	svc := whatsapp.NewService(cfg, pool, logger, nil)

	numbers, err := svc.ListMessageNumbers(ctx, token, messagingAccountID)
	if err != nil {
		logger.Error("meta phone number listing failed", "error", err)
		os.Exit(1)
	}
	if len(numbers) == 0 {
		logger.Warn("messaging account has no phone numbers", "messaging_account_id", messagingAccountID)
	}

	waac := os.Getenv("META_WAAC_ID")
	if waac == "" {
		waac = messagingAccountID
	}
	portfolio := os.Getenv("META_BUSINESS_PORTFOLIO_ID")

	for _, n := range numbers {
		id, err := upsertNumber(ctx, pool, n, messagingAccountID, waac, portfolio)
		if err != nil {
			logger.Error("number upsert failed", "number", n.DisplayPhoneNumber, "error", err)
			continue
		}
		logger.Info("number available for rent",
			"id", id, "phone", n.DisplayPhoneNumber, "verified_name", n.VerifiedName,
			"phone_number_id", n.ID)
	}

	logger.Info("number pool refreshed",
		"messaging_account_id", messagingAccountID, "count", len(numbers))
}

func upsertNumber(ctx context.Context, pool *pgxpool.Pool, n whatsapp.MessageNumber, messagingAccountID, waac, portfolio string) (string, error) {
	var existing string
	err := pool.QueryRow(ctx, `
		SELECT id
		FROM whatsapp_numbers
		WHERE phone_number_id = $1`,
		n.ID,
	).Scan(&existing)
	if err == nil {
		_, err = pool.Exec(ctx, `
			UPDATE whatsapp_numbers
			SET display_phone_number = $2, verified_name = $3, messaging_account_id = $4,
			    waac_id = $5, updated_at = now()
			WHERE phone_number_id = $1`,
			n.ID, n.DisplayPhoneNumber, n.VerifiedName, messagingAccountID, waac)
		return existing, err
	}
	if err != pgx.ErrNoRows {
		return "", err
	}

	var newID string
	err = pool.QueryRow(ctx, `
		INSERT INTO whatsapp_numbers (
			display_phone_number, phone_number_id, messaging_account_id, waac_id, verified_name, status
		) VALUES ($1, $2, $3, $4, $5, 'available')
		RETURNING id`,
		n.DisplayPhoneNumber, n.ID, messagingAccountID, waac, n.VerifiedName,
	).Scan(&newID)
	return newID, err
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "seednumbers:", msg)
	os.Exit(1)
}
