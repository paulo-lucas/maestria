package auth

// Helpers for callers that keep users in their own tables (the hosted server): the primitives
// stay here, so there is one implementation of hashing, TOTP and recovery codes.

// DummyHash is a well-formed hash verified when no real hash applies, so that response timing
// does not reveal which check failed.
func DummyHash() string { return dummyHash() }

// HashRecoveryCode returns the argon2id hash stored for a recovery code (normalized first).
func HashRecoveryCode(code string) (string, error) {
	return hashWith(NormalizeRecoveryCode(code), RecoveryParams)
}

// VerifyRecoveryCode checks a typed recovery code against a stored hash. Codes that cannot be
// well formed still cost one verification, so length is not a timing signal.
func VerifyRecoveryCode(code, hash string) bool {
	norm := NormalizeRecoveryCode(code)
	valid := len(norm) == 8 && len(code) <= 32
	if !valid {
		norm = "00000000"
	}
	return VerifyPassword(norm, hash) && valid
}
