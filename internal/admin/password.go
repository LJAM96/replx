// Package admin password helpers: Argon2id local administrator credentials.
//
// Records are self-describing PHC strings:
// $argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>
// Verification parses the stored parameters and verifies with THOSE
// values, so a future parameter upgrade never locks out existing
// passwords; successful logins transparently rehash to current parameters.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// argonParams are parsed PHC parameters plus decoded material.
type argonParams struct {
	version uint32
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	hash    []byte
}

// hashPassword hashes with Argon2id and a random salt. Format:
// $argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>
func hashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return formatPHC(argon2.Version, argonMemory, argonTime, argonThreads, salt, hash), nil
}

func formatPHC(version, memory, time, threads uint32, salt, hash []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))
}

// parsePHC validates the full record: algorithm, version, all three cost
// parameters with sane bounds, and decodable salt+hash.
func parsePHC(encoded string) (argonParams, error) {
	var p argonParams
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, fmt.Errorf("not an argon2id PHC record")
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &p.version); err != nil {
		return p, fmt.Errorf("bad argon2 version: %q", parts[2])
	}
	if p.version != argon2.Version {
		return p, fmt.Errorf("unsupported argon2 version %d", p.version)
	}
	params := map[string]uint64{}
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, fmt.Errorf("bad argon2 param: %q", kv)
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, fmt.Errorf("bad argon2 param value: %q", kv)
		}
		params[k] = n
	}
	var ok bool
	var m, t, th uint64
	if m, ok = params["m"]; !ok || m < 8*1024 || m > 1<<22 {
		return p, fmt.Errorf("argon2 memory out of sane bounds")
	}
	if t, ok = params["t"]; !ok || t < 1 || t > 16 {
		return p, fmt.Errorf("argon2 time out of sane bounds")
	}
	if th, ok = params["p"]; !ok || th < 1 || th > 16 {
		return p, fmt.Errorf("argon2 parallelism out of sane bounds")
	}
	p.memory, p.time, p.threads = uint32(m), uint32(t), uint8(th)
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return p, fmt.Errorf("bad argon2 salt")
	}
	if p.hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.hash) < 16 {
		return p, fmt.Errorf("bad argon2 hash")
	}
	return p, nil
}

func verifyPassword(encoded, password string) bool {
	p, err := parsePHC(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.hash)))
	return subtle.ConstantTimeCompare(got, p.hash) == 1
}

// needsRehash reports whether a valid record predates current parameters.
func needsRehash(encoded string) bool {
	p, err := parsePHC(encoded)
	if err != nil {
		return false
	}
	return p.version != argon2.Version || p.memory != argonMemory ||
		p.time != argonTime || p.threads != argonThreads
}
