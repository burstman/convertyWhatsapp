package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/whatsapp"
)

// seedwhatsapp stores a shop's Meta WhatsApp credentials in
// whatsapp_integrations (encrypted). It is the connect-time equivalent used
// until the dashboard connect form exists.
//
// Required env:
//
//	META_SYSTEM_USER_TOKEN  system-user access token
//	META_PHONE_NUMBER_ID    WhatsApp phone-number id (sending target)
//	META_MESSAGING_ACCOUNT_ID  WABA / Messaging Account id
//
// Optional: SHOP_ID (defaults to the first shop that has a Converty
// integration), META_WAAC_ID, META_BUSINESS_PORTFOLIO_ID, META_PHONE_NUMBER.
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
	phoneNumberID := os.Getenv("META_PHONE_NUMBER_ID")
	messagingAccountID := os.Getenv("META_MESSAGING_ACCOUNT_ID")
	if messagingAccountID == "" {
		// TOP DEALS001 (WABA 2883242225383967) owns our sending number
		// 1302152316314738; its approved templates are Arabic-only.
		messagingAccountID = "2883242225383967"
	}
	if phoneNumberID == "" || messagingAccountID == "" {
		fatal("META_PHONE_NUMBER_ID and META_MESSAGING_ACCOUNT_ID are required")
	}

	shopID, err := resolveShop(ctx, pool, os.Getenv("SHOP_ID"))
	if err != nil {
		fatal(err.Error())
	}

	svc := whatsapp.NewService(cfg, pool, logger, nil)

	// Sanity-check the token + ids before persisting anything.
	if pn, err := svc.GetPhoneNumber(ctx, token, phoneNumberID); err != nil {
		logger.Warn("phone number check failed", "error", err)
	} else {
		logger.Info("phone number verified",
			"display_phone_number", pn.DisplayPhoneNumber,
			"verified_name", pn.VerifiedName,
			"quality_rating", pn.QualityRating)
	}

	if err := svc.SaveIntegration(ctx, shopID, token, whatsapp.Integration{
		ShopID:              shopID,
		WaacID:              os.Getenv("META_WAAC_ID"),
		PhoneNumberID:       phoneNumberID,
		MessagingAccountID:  messagingAccountID,
		BusinessPortfolioID: os.Getenv("META_BUSINESS_PORTFOLIO_ID"),
		PhoneNumber:         os.Getenv("META_PHONE_NUMBER"),
		Status:              "connected",
	}); err != nil {
		logger.Error("whatsapp integration save failed", "error", err)
		os.Exit(1)
	}

	logger.Info("whatsapp integration saved",
		"shop_id", shopID,
		"phone_number_id", phoneNumberID,
		"messaging_account_id", messagingAccountID)
}

func resolveShop(ctx context.Context, pool *pgxpool.Pool, shopIDFlag string) (uuid.UUID, error) {
	if shopIDFlag != "" {
		id, err := uuid.Parse(shopIDFlag)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid SHOP_ID: %w", err)
		}
		return id, nil
	}

	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT shop_id
		FROM converty_integrations
		WHERE shop_id IS NOT NULL
		ORDER BY created_at
		LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}

	err = pool.QueryRow(ctx, `SELECT id FROM shops ORDER BY created_at LIMIT 1`).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no shop available to seed: %w", err)
	}
	return id, nil
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "seedwhatsapp:", msg)
	os.Exit(1)
}
