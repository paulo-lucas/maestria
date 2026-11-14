package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/maestria-dev/maestria/internal/privfile"
)

// FileName is the credentials file in the config directory.
const FileName = "wire.json"

// DefaultHost is where Maestri Wire listens.
const DefaultHost = "localhost:7434"

// Credential is one Wire device token. It never prints its token.
type Credential struct {
	Token    string `json:"token"`
	DeviceID string `json:"deviceId"`
}

func (c Credential) String() string             { return "wire.Credential{redacted}" }
func (c Credential) GoString() string           { return c.String() }
func (c Credential) LogValue() slog.Value       { return slog.StringValue("[redacted]") }
func (c Credential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// Credentials is wire.json: the host, its pinned key and the two device tokens.
type Credentials struct {
	Version    int         `json:"version"`
	Host       string      `json:"host"`
	Pin        string      `json:"pin"` // lowercase hex SHA-256 of the SPKI
	DeviceName string      `json:"deviceName,omitempty"`
	Guest      Credential  `json:"guest"`
	Owner      *Credential `json:"owner,omitempty"`
	PairedAt   int64       `json:"pairedAt,omitempty"`
}

// Path returns where wire.json lives inside configDir.
func Path(configDir string) string { return filepath.Join(configDir, FileName) }

// LoadCredentials reads wire.json. A missing file is ErrNotPaired. A file readable by other users
// is refused, like the other secrets.
func LoadCredentials(path string) (*Credentials, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, err
	}
	if err := privfile.Check(path, info); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("wire: %s is not valid JSON", FileName)
	}
	if c.Host == "" || c.Guest.Token == "" || c.Guest.DeviceID == "" {
		return nil, fmt.Errorf("wire: %s is incomplete; run maestria wire pair", FileName)
	}
	if _, err := ParsePin(c.Pin); err != nil {
		return nil, fmt.Errorf("wire: %s has an invalid pin; run maestria wire pair", FileName)
	}
	if c.Owner != nil && (c.Owner.Token == "" || c.Owner.DeviceID == "") {
		c.Owner = nil
	}
	return &c, nil
}

// Save writes wire.json (0600) atomically.
func (c *Credentials) Save(path string) error {
	c.Version = 1
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return privfile.WriteAtomic(path, append(data, '\n'))
}

// PinValue returns the parsed pin.
func (c *Credentials) PinValue() (Pin, error) { return ParsePin(c.Pin) }

// Clients builds the guest client and, when an owner token is held, the owner client.
func (c *Credentials) Clients() (guest, owner *Client, err error) {
	pin, err := c.PinValue()
	if err != nil {
		return nil, nil, err
	}
	guest = NewClient(c.Host, pin, ScopeRead, c.Guest.Token, c.Guest.DeviceID)
	if c.Owner != nil {
		owner = NewClient(c.Host, pin, ScopeWrite, c.Owner.Token, c.Owner.DeviceID)
	}
	return guest, owner, nil
}

// Secrets lists the tokens, for scrubbing them out of error text.
func (c *Credentials) Secrets() []string {
	s := []string{c.Guest.Token}
	if c.Owner != nil {
		s = append(s, c.Owner.Token)
	}
	return s
}

// NormalizeHost accepts "host", "host:port" or a URL and returns host:port (default port 7434).
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "wss://")
	h = strings.TrimSuffix(h, "/")
	if h == "" {
		return DefaultHost, nil
	}
	if strings.ContainsAny(h, "/?# \t") {
		return "", errors.New("the host must be a name or address, optionally with a port (for example localhost:7434)")
	}
	if i := strings.LastIndex(h, ":"); i < 0 || strings.HasSuffix(h, "]") {
		h += ":7434"
	}
	return h, nil
}
