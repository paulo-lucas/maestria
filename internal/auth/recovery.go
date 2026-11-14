package auth

import (
	"crypto/rand"
	"database/sql"
	"math/big"
	"strings"
	"time"
)

// RecoveryCodeCount is how many codes `auth setup` issues.
const RecoveryCodeCount = 10

// Lowercase letters and digits without look-alikes (0/o, 1/l/i): 31 symbols, 8 per code (~39 bits).
// Online guessing is bounded by the login limiter; offline guessing by argon2id.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GenerateRecoveryCodes returns n new codes formatted xxxx-xxxx.
func GenerateRecoveryCodes(n int) ([]string, error) {
	out := make([]string, 0, n)
	max := big.NewInt(int64(len(recoveryAlphabet)))
	for len(out) < n {
		var sb strings.Builder
		for i := 0; i < 8; i++ {
			if i == 4 {
				sb.WriteByte('-')
			}
			v, err := rand.Int(rand.Reader, max)
			if err != nil {
				return nil, err
			}
			sb.WriteByte(recoveryAlphabet[v.Int64()])
		}
		out = append(out, sb.String())
	}
	return out, nil
}

// NormalizeRecoveryCode lowercases and strips spaces and dashes, so "K7FQ 2M9X" matches "k7fq-2m9x".
func NormalizeRecoveryCode(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// ReplaceRecoveryCodes deletes every stored code and stores hashes of the given ones.
func ReplaceRecoveryCodes(db *sql.DB, codes []string, now time.Time) error {
	hashes := make([]string, len(codes))
	for i, c := range codes {
		h, err := hashWith(NormalizeRecoveryCode(c), RecoveryParams)
		if err != nil {
			return err
		}
		hashes[i] = h
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM recovery_codes"); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec("INSERT INTO recovery_codes (hash, created_at) VALUES (?, ?)", h, now.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemainingRecoveryCodes counts unused codes.
func RemainingRecoveryCodes(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM recovery_codes WHERE used_at IS NULL").Scan(&n)
	return n, err
}

// ConsumeRecoveryCode marks a matching unused code as used and reports whether one matched.
// All unused codes are checked so timing does not depend on which one matched.
func ConsumeRecoveryCode(db *sql.DB, code string, now time.Time) (bool, error) {
	norm := NormalizeRecoveryCode(code)
	if len(norm) != 8 || len(code) > 32 {
		return false, nil
	}
	rows, err := db.Query("SELECT id, hash FROM recovery_codes WHERE used_at IS NULL")
	if err != nil {
		return false, err
	}
	type row struct {
		id   int64
		hash string
	}
	var candidates []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.hash); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	var match int64 = -1
	for _, r := range candidates {
		if VerifyPassword(norm, r.hash) {
			match = r.id
		}
	}
	if match < 0 {
		return false, nil
	}
	// Conditional update: two concurrent uses of the same code cannot both succeed.
	res, err := db.Exec("UPDATE recovery_codes SET used_at = ? WHERE id = ? AND used_at IS NULL", now.UnixMilli(), match)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
