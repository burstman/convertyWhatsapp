package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsappconverty/internal/config"
	"whatsappconverty/internal/database"
	"whatsappconverty/internal/logutil"
	"whatsappconverty/internal/whatsapp"
)

// sendtest issues one live template message for a shop's WhatsApp
// integration. It needs a recipient whose number is either in the WABA test
// list or has a 24h open conversation.
//
// Env:
//
//	WAPI_TO        recipient phone in E.164 (e.g. +216...
//	META_TEMPLATE  template name (default jaspers_market_order_confirmation_v1)
//	WAPI_PARAMS    JSON array of body {{N}} values (default order-confirmation sample)
//	SHOP_ID        optional; defaults to first Converty-integrated shop
//
// On success the Meta message id is printed and a row is written to messages.
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

	to := os.Getenv("WAPI_TO")
	if to == "" {
		fatal("WAPI_TO (recipient E.164 phone) is required")
	}

	template := os.Getenv("META_TEMPLATE")
	if template == "" {
		template = "order_confirmed_v2"
	}

	shopID, err := resolveShop(ctx, pool, os.Getenv("SHOP_ID"))
	if err != nil {
		fatal(err.Error())
	}

	svc := whatsapp.NewService(cfg, pool, logger, nil)

	integ, err := svc.Integration(ctx, shopID)
	if err != nil {
		fatal(fmt.Sprintf("whatsapp integration for shop %s: %v", shopID, err))
	}
	token, err := svc.DecryptToken(integ.AccessTokenEncrypted)
	if err != nil {
		fatal("decrypt token: " + err.Error())
	}

	components, err := bodyComponents(os.Getenv("WAPI_PARAMS"))
	if err != nil {
		fatal(err.Error())
	}

	logger.Info("sending whatsapp template",
		"shop_id", shopID, "to", to, "template", template,
		"from", integ.PhoneNumberID, "messaging_account_id", integ.MessagingAccountID)

	language := os.Getenv("WAPI_LANG")
	if language == "" {
		language = "ar"
	}

	msgID, err := svc.SendTemplate(ctx, token, integ.PhoneNumberID, to, template, language, components,
		whatsapp.MessagingAccountParam(integ.MessagingAccountID))
	if err != nil {
		logger.Error("whatsapp send failed", "error", err)
		os.Exit(1)
	}

	if rErr := svc.RecordMessage(ctx, shopID, integ.ID, "", to, template, msgID, time.Now().UTC()); rErr != nil {
		logger.Warn("message row not recorded", "error", rErr)
	}

	logger.Info("whatsapp message sent", "meta_message_id", msgID, "to", to)
	fmt.Println(msgID)
}

// bodyComponents builds a single BODY component of text parameters, either
// from a WAPI_PARAMS JSON array or from a static sample.
func bodyComponents(raw string) ([]whatsapp.TemplateComponent, error) {
	values := []string{"Hamed", "CVY-1001", "today"}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, fmt.Errorf("WAPI_PARAMS must be a JSON array of strings: %w", err)
		}
	}
	params := make([]whatsapp.TemplateParameter, 0, len(values))
	for _, v := range values {
		params = append(params, whatsapp.TemplateParameter{Type: "text", Text: v})
	}
	return []whatsapp.TemplateComponent{{Type: "body", Parameters: params}}, nil
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
		return uuid.Nil, fmt.Errorf("no shop available: %w", err)
	}
	return id, nil
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "sendtest:", msg)
	os.Exit(1)
}
