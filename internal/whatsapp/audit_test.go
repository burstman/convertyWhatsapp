package whatsapp

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The audit stores a digest, not the number. It must not contain the raw digits
// and must be stable, keyed and shop-scoped.
func TestHashRecipientHidesPhone(t *testing.T) {
	const secret = "test-superkit-secret-32-bytes-min"
	shop := uuid.New()
	const phone = "+21624118849"

	got := HashRecipient(secret, shop, phone)
	if got == phone {
		t.Fatal("hash equals the phone number")
	}
	if strings.Contains(got, "21624118849") {
		t.Fatalf("hash leaks the phone digits: %q", got)
	}
	if len(got) != 64 {
		t.Fatalf("expected a sha256 hex digest (64 chars), got %d", len(got))
	}
	if HashRecipient(secret, shop, phone) != got {
		t.Fatal("hash is not stable")
	}
	if HashRecipient(secret, uuid.New(), phone) == got {
		t.Fatal("hash must differ per shop")
	}
	if HashRecipient("another-secret", shop, phone) == got {
		t.Fatal("hash must be keyed by the secret")
	}
}
