package admin

import (
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct-horse-32")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("PHC shape: %s", hash)
	}
	if !verifyPassword(hash, "correct-horse-32") {
		t.Fatal("valid password must verify")
	}
	if verifyPassword(hash, "wrong-password") {
		t.Fatal("wrong password must not verify")
	}
	if verifyPassword("bogus", "correct-horse-32") {
		t.Fatal("malformed record must not verify")
	}
	if needsRehash(hash) {
		t.Fatal("fresh record must not need rehash")
	}
}

func TestPasswordParametersHonored(t *testing.T) {
	salt := []byte("0123456789abcdef")
	weak := formatPHC(argon2.Version, 8*1024, 1, 1, salt, argon2.IDKey([]byte("pw-12345"), salt, 1, 8*1024, 1, 32))
	if !verifyPassword(weak, "pw-12345") {
		t.Fatal("stored parameters must verify, not current constants")
	}
	if verifyPassword(weak, "nope") {
		t.Fatal("wrong password must not verify")
	}
	if !needsRehash(weak) {
		t.Fatal("older parameters must flag rehash")
	}
	if verifyPassword("$argon2id$v=19$m=1,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "x") {
		t.Fatal("absurd parameters must not verify")
	}
}

func TestPasswordRejectsShort(t *testing.T) {
	if _, err := hashPassword("short"); err == nil {
		t.Fatal("short password must be rejected")
	}
}
