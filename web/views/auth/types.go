package viewsauth

// LoginValues is bound from the login form via `form` tags.
type LoginValues struct {
	Email    string `form:"email"`
	Password string `form:"password"`
	Redirect string `form:"redirect"`
}

// RegisterValues is bound from the registration form via `form` tags.
type RegisterValues struct {
	ShopName        string `form:"shop_name"`
	Name            string `form:"name"`
	Email           string `form:"email"`
	Password        string `form:"password"`
	ConfirmPassword string `form:"confirm_password"`
}

// InviteRedeemValues is bound from the "set your password" form on the
// /invite/{token} page, where a pending member activates their account.
type InviteRedeemValues struct {
	Password        string `form:"password"`
	ConfirmPassword string `form:"confirm_password"`
}
