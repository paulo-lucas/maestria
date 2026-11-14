package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/maestria-dev/maestria/internal/privfile"
)

// SecretsFileName holds the v2 password hash and TOTP secret (0600). V1's auth.json is left alone.
const SecretsFileName = "auth-v2.json"

// Secrets are the local owner's credentials.
type Secrets struct {
	PasswordHash string `json:"passwordHash"`
	TOTPSecret   string `json:"totpSecret"`
	CreatedAt    string `json:"createdAt"`
}

// SecretsPath returns the secrets file path in configDir.
func SecretsPath(configDir string) string { return filepath.Join(configDir, SecretsFileName) }

// SaveSecrets writes the secrets file atomically with 0600 permissions.
func SaveSecrets(configDir string, s *Secrets) error {
	if err := privfile.EnsureDir(configDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return privfile.WriteAtomic(SecretsPath(configDir), append(data, '\n'))
}

// LoadSecrets reads the secrets file. It returns (nil, nil) when absent, and refuses loose
// permissions on POSIX and malformed contents.
func LoadSecrets(configDir string) (*Secrets, error) {
	path := SecretsPath(configDir)
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := privfile.Check(path, st); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Secrets
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	if _, ok := parsePHC(s.PasswordHash); !ok {
		return nil, fmt.Errorf("%s is malformed; run \"maestria auth setup --force\"", path)
	}
	if _, err := DecodeTOTPSecret(s.TOTPSecret); err != nil || s.CreatedAt == "" {
		return nil, fmt.Errorf("%s is malformed; run \"maestria auth setup --force\"", path)
	}
	return &s, nil
}
