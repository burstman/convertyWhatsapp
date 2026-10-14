package server

import (
	"net/http"
	"testing"

	"github.com/gorilla/sessions"
)

func TestApplySessionOptions(t *testing.T) {
	cases := []struct {
		name   string
		secure bool
	}{
		{"development", false},
		{"production", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &sessions.Session{Options: &sessions.Options{}}
			applySessionOptions(sess, tc.secure)

			if !sess.Options.HttpOnly {
				t.Fatal("session cookie must be HttpOnly")
			}
			if sess.Options.Secure != tc.secure {
				t.Fatalf("Secure = %v, want %v", sess.Options.Secure, tc.secure)
			}
			if sess.Options.SameSite != http.SameSiteLaxMode {
				t.Fatalf("SameSite = %v, want Lax", sess.Options.SameSite)
			}
			if sess.Options.Path != "/" {
				t.Fatalf("Path = %q, want /", sess.Options.Path)
			}
		})
	}
}

// A pre-existing path must be preserved rather than overwritten.
func TestApplySessionOptionsKeepsPath(t *testing.T) {
	sess := &sessions.Session{Options: &sessions.Options{Path: "/app"}}
	applySessionOptions(sess, false)
	if sess.Options.Path != "/app" {
		t.Fatalf("Path = %q, want /app", sess.Options.Path)
	}
}
