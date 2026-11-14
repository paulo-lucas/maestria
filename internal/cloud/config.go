// Package cloud is the multi-tenant hosted server: accounts, connector linking, the relay between
// browsers and connectors, and device pairing. It stores no workspace data. See
// docs/hosted-protocol.md.
package cloud

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/paulo-lucas/maestria/internal/privfile"
	"github.com/paulo-lucas/maestria/internal/relay"
)

// SMTP is the outgoing mail server.
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	// TLS uses implicit TLS (port 465). Otherwise STARTTLS is required when the server offers it.
	TLS bool `json:"tls"`
}

// Limits are the failed-login limits.
type Limits struct {
	MaxFailuresPerIP      int `json:"maxFailuresPerIp"`
	MaxFailuresPerAccount int `json:"maxFailuresPerAccount"`
	MaxFailuresGlobal     int `json:"maxFailuresGlobal"`
	WindowSeconds         int `json:"windowSeconds"`
	LockoutSeconds        int `json:"lockoutSeconds"`
}

// Config is the server configuration (config.json in the data dir, overridden by environment).
type Config struct {
	DataDir string `json:"-"`
	// Listen is the address to bind. Default 127.0.0.1:8080. Put a reverse proxy or tunnel in front.
	Listen string `json:"listen"`
	// PublicURL is the https URL users reach the cloud at. It sets the Host allowlist, cookie
	// prefix and the links in emails.
	PublicURL    string   `json:"publicUrl"`
	AllowedHosts []string `json:"allowedHosts"`
	// TrustProxy is "", "xff" (X-Forwarded-For) or "cf" (CF-Connecting-IP). ProxyHops is how many
	// trusted proxies sit in front (xff only; the client address is counted from the right).
	TrustProxy string `json:"trustProxy"`
	ProxyHops  int    `json:"proxyHops"`

	SessionIdleMinutes   int `json:"sessionIdleMinutes"`
	SessionAbsoluteHours int `json:"sessionAbsoluteHours"`

	Login Limits `json:"login"`
	SMTP  SMTP   `json:"smtp"`
	// DevEmail writes emails instead of sending them: "stdout" or "file:<path>".
	DevEmail string `json:"devEmail"`
}

// Defaults returns the default configuration.
func Defaults() *Config {
	return &Config{
		Listen:               "127.0.0.1:8080",
		ProxyHops:            1,
		SessionIdleMinutes:   12 * 60,
		SessionAbsoluteHours: 24 * 7,
		Login:                Limits{MaxFailuresPerIP: 5, MaxFailuresPerAccount: 8, MaxFailuresGlobal: 100, WindowSeconds: 900, LockoutSeconds: 900},
		SMTP:                 SMTP{Port: 587},
	}
}

// DefaultDataDir is $MAESTRIA_CLOUD_DIR, %AppData%\maestria-cloud on Windows, or ~/.config/maestria-cloud.
func DefaultDataDir(getenv func(string) string, home string) string {
	if v := strings.TrimSpace(getenv("MAESTRIA_CLOUD_DIR")); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := getenv("APPDATA"); v != "" {
			return filepath.Join(v, "maestria-cloud")
		}
		return filepath.Join(home, "AppData", "Roaming", "maestria-cloud")
	}
	return filepath.Join(home, ".config", "maestria-cloud")
}

// LoadConfig reads config.json from dir (missing is fine), applies the environment and validates.
func LoadConfig(dir string, getenv func(string) string) (*Config, error) {
	cfg := Defaults()
	cfg.DataDir = dir
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if st, serr := os.Stat(path); serr == nil {
			if err := privfile.Check(path, st); err != nil {
				return nil, err
			}
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if getenv != nil {
		env := func(k string) string { return strings.TrimSpace(getenv("MAESTRIA_CLOUD_" + k)) }
		set := func(dst *string, k string) {
			if v := env(k); v != "" {
				*dst = v
			}
		}
		set(&cfg.Listen, "LISTEN")
		set(&cfg.PublicURL, "PUBLIC_URL")
		set(&cfg.TrustProxy, "TRUST_PROXY")
		set(&cfg.DevEmail, "DEV_EMAIL")
		set(&cfg.SMTP.Host, "SMTP_HOST")
		set(&cfg.SMTP.Username, "SMTP_USER")
		set(&cfg.SMTP.Password, "SMTP_PASSWORD")
		set(&cfg.SMTP.From, "SMTP_FROM")
		if v := env("SMTP_PORT"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, errors.New("MAESTRIA_CLOUD_SMTP_PORT must be a number")
			}
			cfg.SMTP.Port = n
		}
		if v := env("SMTP_TLS"); v != "" {
			cfg.SMTP.TLS = v == "1" || strings.EqualFold(v, "true")
		}
	}
	return cfg, cfg.Validate()
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q must be host:port", c.Listen)
	}
	switch c.TrustProxy {
	case "", "xff", "cf":
	default:
		return errors.New(`trustProxy must be "", "xff" or "cf"`)
	}
	if c.ProxyHops < 1 || c.ProxyHops > 10 {
		return errors.New("proxyHops must be between 1 and 10")
	}
	if c.SessionIdleMinutes < 1 || c.SessionAbsoluteHours < 1 {
		return errors.New("session limits must be positive")
	}
	l := c.Login
	if l.MaxFailuresPerIP < 1 || l.MaxFailuresPerAccount < 1 || l.MaxFailuresGlobal < 1 || l.WindowSeconds < 1 || l.LockoutSeconds < 1 {
		return errors.New("login limits must be positive")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return errors.New("publicUrl must be a plain URL like https://maestria.example")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && relay.IsLoopbackHost(u.Hostname())) {
			return errors.New("publicUrl must be https (plain http is only allowed for localhost)")
		}
	}
	if c.DevEmail != "" && c.DevEmail != "stdout" && !strings.HasPrefix(c.DevEmail, "file:") {
		return errors.New(`devEmail must be "stdout" or "file:<path>"`)
	}
	return nil
}

// Secure reports whether cookies are Secure (the public URL is https).
func (c *Config) Secure() bool { return strings.HasPrefix(c.PublicURL, "https://") }

// Hosts is the Host allowlist.
func (c *Config) Hosts() map[string]bool {
	out := map[string]bool{}
	if u, err := url.Parse(c.PublicURL); err == nil && u.Host != "" {
		out[strings.ToLower(u.Host)] = true
	}
	for _, h := range c.AllowedHosts {
		out[strings.ToLower(h)] = true
	}
	return out
}

// Mailer reports whether outgoing mail is configured (SMTP or dev mode).
func (c *Config) mailConfigured() bool { return c.DevEmail != "" || c.SMTP.Host != "" }
