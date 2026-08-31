package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// crypto.go — password hashing, hand-rolled on the standard library (invariant #2: no new Go
// modules — no golang.org/x/crypto, no bcrypt, no JWT/DB drivers). PBKDF2-HMAC-SHA256 with a
// per-user random salt; the encoded string carries its own params so the iteration count can be
// raised later without breaking existing hashes.
//
//	encoded = "pbkdf2$sha256$<iters>$<saltb64>$<hashb64>"

const (
	pbkdf2Iters = 210_000 // OWASP floor for PBKDF2-HMAC-SHA256
	saltLen     = 16      // bytes, from crypto/rand
	keyLen      = 32      // derived-key length (SHA-256 block)
)

// pbkdf2SHA256 is the PBKDF2 key-derivation function (RFC 8018) over HMAC-SHA256. Kept small and
// dependency-free; only the single-block path is exercised (keyLen == hLen) but it handles any len.
func pbkdf2SHA256(password, salt []byte, iter, dkLen int) []byte {
	hLen := sha256.Size
	blocks := (dkLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		mac.Write(buf)
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 2; n <= iter; n++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:dkLen]
}

// dummyHash is a valid encoded hash of a throwaway secret. Login verifies against it when the email
// doesn't exist, so a missing account costs roughly the same time as a wrong password — closing a
// timing side-channel for user enumeration. Computed once at startup.
var dummyHash, _ = hashPassword("nvda-platform-nonexistent-account-sentinel")

// hashPassword derives an encoded PBKDF2 hash with a fresh random salt.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iters, keyLen)
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", pbkdf2Iters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// verifyPassword recomputes the hash with the stored params/salt and compares in constant time.
func verifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}
