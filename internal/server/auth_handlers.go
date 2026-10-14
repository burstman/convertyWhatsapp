package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/anthdm/superkit/kit"
	"github.com/anthdm/superkit/validate"

	"whatsappconverty/internal/auth"
	viewauth "whatsappconverty/web/views/auth"
	errortpl "whatsappconverty/web/views/errors"
)

var loginSchema = validate.Schema{
	"email":    validate.Rules(validate.Email),
	"password": validate.Rules(validate.Required),
}

var registerSchema = validate.Schema{
	// Keys are matched against Go struct fields by name, so they must be
	// camelCase (the validate package normalises only the first character).
	"shopName":        []validate.RuleSet{validate.Required.Message("give your shop a name")},
	"name":            validate.Rules(validate.Required),
	"email":           validate.Rules(validate.Email),
	"password":        validate.Rules(validate.Required, validate.Min(8)),
	"confirmPassword": validate.Rules(validate.Required),
}

func (a *App) handleLoginGet(k *kit.Kit) error {
	if auth.FromKit(k).LoggedIn {
		return k.Redirect(http.StatusSeeOther, "/dashboard")
	}
	return k.Render(viewauth.LoginPage())
}

func (a *App) handleLoginPost(k *kit.Kit) error {
	var values viewauth.LoginValues
	formErrs, ok := validate.Request(k.Request, &values, loginSchema)
	if !ok {
		return k.Render(viewauth.LoginForm(values, formErrs))
	}

	token, err := a.Auth.Login(k.Request.Context(), values.Email, values.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			formErrs.Add("email", "incorrect email or password")
			formErrs.Add("password", "")
			return k.Render(viewauth.LoginForm(values, formErrs))
		}
		return err
	}

	sess := k.GetSession(auth.SessionCookieName)
	sess.Values["sessionToken"] = token
	applySessionOptions(sess, a.Cfg.IsProduction())
	if err := sess.Save(k.Request, k.Response); err != nil {
		return err
	}

	redirect := values.Redirect
	if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/dashboard"
	}
	return k.Redirect(http.StatusSeeOther, redirect)
}

func (a *App) handleRegisterGet(k *kit.Kit) error {
	if auth.FromKit(k).LoggedIn {
		return k.Redirect(http.StatusSeeOther, "/dashboard")
	}
	return k.Render(viewauth.RegisterPage())
}

func (a *App) handleRegisterPost(k *kit.Kit) error {
	var values viewauth.RegisterValues
	formErrs, ok := validate.Request(k.Request, &values, registerSchema)
	if !ok {
		return k.Render(viewauth.RegisterForm(values, formErrs))
	}
	if values.Password != values.ConfirmPassword {
		formErrs.Add("confirm_password", "passwords do not match")
		return k.Render(viewauth.RegisterForm(values, formErrs))
	}

	token, err := a.Auth.Register(k.Request.Context(), auth.RegisterInput{
		ShopName: values.ShopName,
		UserName: values.Name,
		Email:    strings.ToLower(strings.TrimSpace(values.Email)),
		Password: values.Password,
	})
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			formErrs.Add("email", "an account with this email already exists")
			return k.Render(viewauth.RegisterForm(values, formErrs))
		}
		return err
	}

	sess := k.GetSession(auth.SessionCookieName)
	sess.Values["sessionToken"] = token
	applySessionOptions(sess, a.Cfg.IsProduction())
	if err := sess.Save(k.Request, k.Response); err != nil {
		return err
	}
	return k.Redirect(http.StatusSeeOther, "/dashboard")
}

func (a *App) handleLogout(k *kit.Kit) error {
	sess := k.GetSession(auth.SessionCookieName)
	if token, ok := sess.Values["sessionToken"].(string); ok && token != "" {
		_ = a.Auth.Logout(k.Request.Context(), token)
	}
	sess.Options.MaxAge = -1
	applySessionOptions(sess, a.Cfg.IsProduction())
	_ = sess.Save(k.Request, k.Response)
	return k.Redirect(http.StatusSeeOther, "/login")
}

func (a *App) handleNotFound(k *kit.Kit) error {
	return k.Render(errortpl.NotFoundPage())
}
