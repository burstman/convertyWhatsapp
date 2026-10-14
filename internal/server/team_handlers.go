package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthdm/superkit/kit"
	"github.com/anthdm/superkit/validate"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"whatsappconverty/internal/auth"
	"whatsappconverty/internal/shops"
	viewauth "whatsappconverty/web/views/auth"
	viewshared "whatsappconverty/web/views/components"
	vdashboard "whatsappconverty/web/views/dashboard"
)

var inviteSchema = validate.Schema{
	"name":  validate.Rules(validate.Required),
	"email": validate.Rules(validate.Email),
}

var redeemSchema = validate.Schema{
	"password":        validate.Rules(validate.Required, validate.Min(8)),
	"confirmPassword": validate.Rules(validate.Required),
}

// handleTeam renders the owner-only member management page: the shop's users,
// the invite form, and the one-time invite link (shown right after an invite).
func (a *App) handleTeamGet(k *kit.Kit) error {
	allowed, err := a.requireOwner(k)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	principal := auth.FromKit(k)
	ctx := k.Request.Context()

	shop, err := a.Shops.GetByID(ctx, principal.User.ShopID)
	if err != nil && !errors.Is(err, shops.ErrNotFound) {
		return err
	}

	members, err := a.Auth.UsersByShop(ctx, principal.User.ShopID)
	if err != nil {
		return err
	}

	flash := vdashboard.TeamFlash{}
	switch k.Request.URL.Query().Get("flash") {
	case "":
	case "invited":
		flash.Info = "Invitation created."
		flash.InviteToken = k.Request.URL.Query().Get("token")
		flash.InviteEmail = k.Request.URL.Query().Get("email")
	case "removed":
		flash.Info = "Member removed from the shop."
	case "emailtaken":
		flash.Error = "That email already belongs to an account."
	case "notfound":
		flash.Error = "That member could not be found."
	case "error":
		flash.Error = "Something went wrong. Please try again."
	}

	page := viewshared.Page{
		Title:    "Team",
		Active:   "team",
		ShopName: shop.Name,
		UserName: principal.User.Name,
		IsOwner:  true,
	}
	baseURL := strings.TrimRight(a.Cfg.AppURL, "/")
	return k.Render(vdashboard.TeamPage(page, baseURL, members, flash))
}

// handleTeamInvitePost creates a pending member and redirects to /team where
// the freshly generated one-time invite link is shown once for the owner to
// copy and send.
func (a *App) handleTeamInvitePost(k *kit.Kit) error {
	allowed, err := a.requireOwner(k)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	principal := auth.FromKit(k)

	var values vdashboard.TeamInviteValues
	if _, ok := validate.Request(k.Request, &values, inviteSchema); !ok {
		return k.Redirect(http.StatusSeeOther, "/team?flash=error")
	}

	token, err := a.Auth.InviteMember(k.Request.Context(), principal.User.ShopID, values.Name, strings.TrimSpace(values.Email))
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			return k.Redirect(http.StatusSeeOther, "/team?flash=emailtaken")
		}
		a.Log.Error("team invite failed", "shop_id", principal.User.ShopID, "error", err)
		return k.Redirect(http.StatusSeeOther, "/team?flash=error")
	}

	a.Log.Info("member invited", "shop_id", principal.User.ShopID, "email", values.Email)
	q := url.Values{}
	q.Set("flash", "invited")
	q.Set("token", token)
	q.Set("email", values.Email)
	return k.Redirect(http.StatusSeeOther, "/team?"+q.Encode())
}

// handleTeamDeletePost removes a member account. The owner can never remove
// themselves or another owner (the service only ever deletes 'member' rows).
func (a *App) handleTeamDeletePost(k *kit.Kit) error {
	allowed, err := a.requireOwner(k)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	principal := auth.FromKit(k)

	id, err := uuid.Parse(chi.URLParam(k.Request, "id"))
	if err != nil {
		return k.Redirect(http.StatusSeeOther, "/team?flash=notfound")
	}

	if err := a.Auth.DeleteMember(k.Request.Context(), principal.User.ShopID, id); err != nil {
		if errors.Is(err, auth.ErrNotMember) {
			return k.Redirect(http.StatusSeeOther, "/team?flash=notfound")
		}
		a.Log.Error("team member delete failed", "shop_id", principal.User.ShopID, "user_id", id, "error", err)
		return k.Redirect(http.StatusSeeOther, "/team?flash=error")
	}

	a.Log.Info("member removed", "shop_id", principal.User.ShopID, "user_id", id)
	return k.Redirect(http.StatusSeeOther, "/team?flash=removed")
}

// handleRedeemInviteGet renders the set-password form for a single-use invite
// link. Logged-in users are sent to the dashboard instead.
func (a *App) handleRedeemInviteGet(k *kit.Kit) error {
	if auth.FromKit(k).LoggedIn {
		return k.Redirect(http.StatusSeeOther, "/dashboard")
	}
	token := chi.URLParam(k.Request, "token")
	if token == "" {
		return k.Redirect(http.StatusSeeOther, "/login")
	}
	return k.Render(viewauth.RedeemInvitePage(token))
}

// handleRedeemInvitePost activates the pending member: password set for the
// first time, invite cleared, session created, straight to the dashboard.
func (a *App) handleRedeemInvitePost(k *kit.Kit) error {
	token := chi.URLParam(k.Request, "token")

	var values viewauth.InviteRedeemValues
	formErrs, ok := validate.Request(k.Request, &values, redeemSchema)
	if !ok {
		return k.Render(viewauth.RedeemInviteForm(values, formErrs, token))
	}
	if values.Password != values.ConfirmPassword {
		formErrs.Add("confirmPassword", "passwords do not match")
		return k.Render(viewauth.RedeemInviteForm(values, formErrs, token))
	}

	sessionToken, err := a.Auth.RedeemInvite(k.Request.Context(), token, values.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInviteInvalid) {
			return k.Render(viewauth.RedeemInviteError())
		}
		return err
	}

	sess := k.GetSession(auth.SessionCookieName)
	sess.Values["sessionToken"] = sessionToken
	applySessionOptions(sess, a.Cfg.IsProduction())
	if err := sess.Save(k.Request, k.Response); err != nil {
		return err
	}
	return k.Redirect(http.StatusSeeOther, "/dashboard")
}
