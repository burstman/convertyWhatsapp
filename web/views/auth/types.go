package viewsauth

// LoginValues is bound from the login form via `form` tags.
type LoginValues struct {
	Email    string `form:"email"`
	Password string `form:"password"`
	Redirect string `form:"redirect"`
}

// RegisterValues is bound from the registration form via `form` tags.
type RegisterValues struct {
	Name            string `form:"name"`
	Email           string `form:"email"`
	Password        string `form:"password"`
	ConfirmPassword string `form:"confirm_password"`
}
