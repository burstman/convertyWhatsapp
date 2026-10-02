package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/anthdm/superkit/kit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/whatsapp"
)

// POST /api/whatsapp/customers — register a customer for the merchant.
func (a *App) handleAPICreateCustomer(k *kit.Kit) error {
	principal := auth.FromKit(k)

	var in struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(k.Request.Body).Decode(&in); err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, errors.New("invalid request body"))
	}
	if in.Phone == "" {
		return a.writeAPIError(k, http.StatusBadRequest, whatsapp.NewSendRejection(whatsapp.ErrCodeTemplateVariableInvalid, "phone is required"))
	}

	id, err := a.WhatsApp.UpsertCustomer(k.Request.Context(), principal.User.ShopID, in.Name, in.Phone)
	if err != nil {
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusCreated, map[string]string{"id": id.String()})
}

// GET /api/whatsapp/customers — list the merchant's customers.
func (a *App) handleAPICustomers(k *kit.Kit) error {
	principal := auth.FromKit(k)
	customers, err := a.WhatsApp.Customers(k.Request.Context(), principal.User.ShopID)
	if err != nil {
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusOK, customers)
}

// POST /api/whatsapp/consent — record a customer opt-in for the merchant.
func (a *App) handleAPIGrantConsent(k *kit.Kit) error {
	principal := auth.FromKit(k)

	var in struct {
		CustomerID string `json:"customer_id"`
		Category   string `json:"category"`
		Source     string `json:"source"`
		Evidence   string `json:"evidence"`
	}
	if err := json.NewDecoder(k.Request.Body).Decode(&in); err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, errors.New("invalid request body"))
	}
	cid, err := uuid.Parse(in.CustomerID)
	if err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, whatsapp.NewSendRejection(whatsapp.ErrCodeCustomerNotOwned, "customer_id is required"))
	}

	if err := a.WhatsApp.GrantConsent(k.Request.Context(), principal.User.ShopID, cid, in.Category, in.Source, in.Evidence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return a.writeAPIError(k, http.StatusUnprocessableEntity, whatsapp.NewSendRejection(whatsapp.ErrCodeCustomerNotOwned, "customer not found for this merchant"))
		}
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusCreated, map[string]string{"status": "opt_in"})
}

// POST /api/whatsapp/revoke — revoke a customer's opt-in.
func (a *App) handleAPIRevokeConsent(k *kit.Kit) error {
	principal := auth.FromKit(k)

	var in struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.NewDecoder(k.Request.Body).Decode(&in); err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, errors.New("invalid request body"))
	}
	cid, err := uuid.Parse(in.CustomerID)
	if err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, whatsapp.NewSendRejection(whatsapp.ErrCodeCustomerNotOwned, "customer_id is required"))
	}

	if err := a.WhatsApp.RevokeConsent(k.Request.Context(), principal.User.ShopID, cid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return a.writeAPIError(k, http.StatusUnprocessableEntity, whatsapp.NewSendRejection(whatsapp.ErrCodeCustomerNotOwned, "customer not found for this merchant"))
		}
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusOK, map[string]string{"status": "revoked"})
}

// POST /api/whatsapp/templates — submit a new template for Meta review.
func (a *App) handleAPICreateTemplate(k *kit.Kit) error {
	principal := auth.FromKit(k)

	var in struct {
		Name       string          `json:"name"`
		Language   string          `json:"language"`
		Category   string          `json:"category"`
		Components json.RawMessage `json:"components"`
	}
	if err := json.NewDecoder(k.Request.Body).Decode(&in); err != nil {
		return a.writeAPIError(k, http.StatusBadRequest, errors.New("invalid request body"))
	}
	if in.Name == "" || in.Language == "" || in.Category == "" || len(in.Components) == 0 {
		return a.writeAPIError(k, http.StatusBadRequest,
			whatsapp.NewSendRejection(whatsapp.ErrCodeTemplateVariableInvalid, "name, language, category and components are required"))
	}

	tmpl, err := a.WhatsApp.CreateTemplate(k.Request.Context(), principal.User.ShopID, whatsapp.TemplateDraft{
		Name:       in.Name,
		Language:   in.Language,
		Category:   in.Category,
		Components: in.Components,
	})
	if err != nil {
		var rej *whatsapp.SendRejection
		if errors.As(err, &rej) {
			// Submission rejected by Meta is still a useful 201 with the stored
			// row so the merchant sees approval_status=rejected + reason.
			return writeJSON(k, http.StatusCreated, tmpl)
		}
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusCreated, tmpl)
}

// GET /api/whatsapp/templates — list the merchant's templates.
func (a *App) handleAPITemplates(k *kit.Kit) error {
	principal := auth.FromKit(k)
	templates, err := a.WhatsApp.Templates(k.Request.Context(), principal.User.ShopID)
	if err != nil {
		a.writeAPIError(k, http.StatusInternalServerError, err)
		return nil
	}
	return writeJSON(k, http.StatusOK, templates)
}
