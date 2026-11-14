package cloud

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/maestria-dev/maestria/internal/privfile"
	"github.com/maestria-dev/maestria/internal/relay"
)

// KeyFileName holds the 32-byte server key (0600). It encrypts TOTP secrets at rest.
const KeyFileName = "server.key"

// Sealer encrypts small secrets with AES-256-GCM.
type Sealer struct{ aead cipher.AEAD }

// LoadOrCreateSealer reads the server key from dir, creating it on first run.
func LoadOrCreateSealer(dir string) (*Sealer, error) {
	path := filepath.Join(dir, KeyFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := privfile.EnsureDir(dir); err != nil {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := privfile.WriteAtomic(path, []byte(relay.Enc(key)+"\n")); err != nil {
			return nil, err
		}
		return NewSealer(key)
	}
	if err != nil {
		return nil, err
	}
	if st, serr := os.Stat(path); serr == nil {
		if err := privfile.Check(path, st); err != nil {
			return nil, err
		}
	}
	key, err := relay.Dec(string(trimNL(data)))
	if err != nil || len(key) != 32 {
		return nil, errors.New(KeyFileName + " is malformed")
	}
	return NewSealer(key)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// NewSealer builds a sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext; the result is b64u(nonce || ciphertext).
func (s *Sealer) Seal(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return relay.Enc(s.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Open decrypts a Seal result.
func (s *Sealer) Open(sealed string) (string, error) {
	raw, err := relay.Dec(sealed)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("invalid sealed value")
	}
	n := s.aead.NonceSize()
	out, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("invalid sealed value")
	}
	return string(out), nil
}
