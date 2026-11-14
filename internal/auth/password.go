package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are argon2id cost parameters. They are stored in the PHC string so they can be
// raised later without breaking existing hashes.
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	KeyLen      uint32
}

// PasswordParams follow the OWASP recommendation (m=64 MiB, t=3, p=4), like V1.
var PasswordParams = Argon2Params{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 4, KeyLen: 32}

// RecoveryParams are lighter: every unused recovery code is checked on each attempt.
var RecoveryParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, KeyLen: 32}

const (
	// MinPasswordLength is the shortest password `auth setup` accepts.
	MinPasswordLength = 12
	// MaxPasswordLength bounds the input hashed on login.
	MaxPasswordLength = 256
)

var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id PHC string ($argon2id$v=19$m=..,t=..,p=..$salt$hash).
func HashPassword(password string) (string, error) {
	return hashWith(password, PasswordParams)
}

func hashWith(secret string, p Argon2Params) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(secret), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var phcRE = regexp.MustCompile(`^\$argon2id\$v=19\$m=(\d{1,8}),t=(\d{1,3}),p=(\d{1,3})\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)

type parsedHash struct {
	p    Argon2Params
	salt []byte
	key  []byte
}

func parsePHC(hash string) (*parsedHash, bool) {
	m := phcRE.FindStringSubmatch(hash)
	if m == nil {
		return nil, false
	}
	mem, _ := strconv.Atoi(m[1])
	t, _ := strconv.Atoi(m[2])
	par, _ := strconv.Atoi(m[3])
	// Refuse absurd parameters from a tampered file (memory or CPU exhaustion).
	if mem < 8 || mem > 1024*1024 || t < 1 || t > 20 || par < 1 || par > 16 {
		return nil, false
	}
	salt, err1 := b64.DecodeString(m[4])
	key, err2 := b64.DecodeString(m[5])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(key) < 16 || len(key) > 64 {
		return nil, false
	}
	return &parsedHash{p: Argon2Params{MemoryKiB: uint32(mem), Iterations: uint32(t), Parallelism: uint8(par), KeyLen: uint32(len(key))}, salt: salt, key: key}, true
}

var verifyCalls atomic.Int64

// VerifyCalls reports how many argon2 verifications ran in this process. Tests use it to prove
// that the login does the same work whatever the password was.
func VerifyCalls() int64 { return verifyCalls.Load() }

// VerifyPassword checks password against a PHC hash in constant time. Malformed hashes never match.
func VerifyPassword(password, hash string) bool {
	ph, ok := parsePHC(hash)
	if !ok {
		return false
	}
	verifyCalls.Add(1)
	got := argon2.IDKey([]byte(password), ph.salt, ph.p.Iterations, ph.p.MemoryKiB, ph.p.Parallelism, ph.p.KeyLen)
	return subtle.ConstantTimeCompare(got, ph.key) == 1
}

// dummyHash is a well-formed hash of a random value, verified when no real hash applies so that
// response timing does not reveal which check failed.
var dummyHash = sync.OnceValue(func() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	h, err := HashPassword(b64.EncodeToString(buf))
	if err != nil {
		panic(err)
	}
	return h
})

// ValidateNewPassword enforces the length policy for a new password.
func ValidateNewPassword(pw string) error {
	n := len([]rune(pw))
	if n < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters (entered %d)", MinPasswordLength, n)
	}
	if len(pw) > MaxPasswordLength {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	}
	return nil
}
