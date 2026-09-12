package crypto

import (
	"bytes"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	ct, err := Encrypt("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "owner-token", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "owner-token", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pt, []byte("s3cret")) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestPurposeSeparation(t *testing.T) {
	ct, _ := Encrypt("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "owner-token", []byte("x"))
	if _, err := Decrypt("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "pms-token", ct); err == nil {
		t.Fatal("cross-purpose decrypt must fail")
	}
	if _, err := Decrypt("wrong-secret-0123456789abcdef012345", "owner-token", ct); err == nil {
		t.Fatal("wrong secret must fail")
	}
}

func TestFingerprintStableAndSecret(t *testing.T) {
	a := Fingerprint("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "tok")
	b := Fingerprint("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "tok")
	if a != b || len(a) != 64 {
		t.Fatal("fingerprint must be stable hex")
	}
	if c := Fingerprint("other-secret-0123456789abcdef0123", "tok"); a == c {
		t.Fatal("fingerprint must depend on secret")
	}
}
