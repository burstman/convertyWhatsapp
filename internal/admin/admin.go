// Package admin provides the platform-operator console: a read-only view of
// every client account and the shops they own. It deliberately exposes no shop
// credentials and performs no mutations yet.
package admin

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Summary is the headline count row shown at the top of the console.
type Summary struct {
	Clients       int
	ActiveClients int
	Shops         int
	WhatsappShops int
}

// Client is a client account together with an aggregate of its shops.
type Client struct {
	ID        uuid.UUID
	Name      string
	Email     string
	Status    string
	CreatedAt time.Time
	ShopCount int
	ShopNames string
	Whatsapp  bool
}

// Shop is a single shop joined to its owning client.
type Shop struct {
	ID              uuid.UUID
	Name            string
	ClientName      string
	ClientEmail     string
	Phone           string
	WhatsappEnabled bool
	TemplateCount   int
	CreatedAt       time.Time
}

// Overview bundles everything the console renders.
type Overview struct {
	Summary Summary
	Clients []Client
	Shops   []Shop
}

// Repository reads platform-wide data for the admin console.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Overview loads the console's summary, clients and shops.
func (r *Repository) Overview(ctx context.Context) (Overview, error) {
	var o Overview

	if err := r.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM users WHERE role = 'client'),
			(SELECT COUNT(*) FROM users WHERE role = 'client' AND status = 'active'),
			(SELECT COUNT(*) FROM shops),
			(SELECT COUNT(*) FROM shops WHERE whatsapp_enabled)`,
	).Scan(&o.Summary.Clients, &o.Summary.ActiveClients, &o.Summary.Shops, &o.Summary.WhatsappShops); err != nil {
		return Overview{}, err
	}

	clients, err := r.clients(ctx)
	if err != nil {
		return Overview{}, err
	}
	o.Clients = clients

	shops, err := r.shops(ctx)
	if err != nil {
		return Overview{}, err
	}
	o.Shops = shops

	return o, nil
}

func (r *Repository) clients(ctx context.Context) ([]Client, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.name, u.email, u.status, u.created_at,
			(SELECT COUNT(*) FROM shops s WHERE s.client_id = u.id) AS shop_count,
			COALESCE((SELECT string_agg(s.name, ', ' ORDER BY s.created_at) FROM shops s WHERE s.client_id = u.id), '') AS shop_names,
			COALESCE((SELECT bool_or(s.whatsapp_enabled) FROM shops s WHERE s.client_id = u.id), false) AS whatsapp
		FROM users u
		WHERE u.role = 'client'
		ORDER BY u.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clients []Client
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.ID, &c.Name, &c.Email, &c.Status, &c.CreatedAt,
			&c.ShopCount, &c.ShopNames, &c.Whatsapp); err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}

func (r *Repository) shops(ctx context.Context) ([]Shop, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT s.id, s.name, COALESCE(u.name, ''), COALESCE(u.email, ''),
			COALESCE(s.phone, ''), s.whatsapp_enabled,
			(SELECT COUNT(*) FROM templates t WHERE t.client_id = s.client_id AND t.scope = 'client'),
			s.created_at
		FROM shops s
		LEFT JOIN users u ON u.id = s.client_id
		ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var shops []Shop
	for rows.Next() {
		var s Shop
		if err := rows.Scan(&s.ID, &s.Name, &s.ClientName, &s.ClientEmail,
			&s.Phone, &s.WhatsappEnabled, &s.TemplateCount, &s.CreatedAt); err != nil {
			return nil, err
		}
		shops = append(shops, s)
	}
	return shops, rows.Err()
}
