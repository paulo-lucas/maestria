// Package connect is the connector's side of hosted mode: the saved link to a cloud, the HTTP
// calls that create it, the outbound WebSocket, and the gate that serves relayed requests.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/privfile"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/termsafe"
)

// LinkFileName holds the cloud link (0600) in the config dir.
const LinkFileName = "cloud.json"

// Link is what `maestria connect` keeps about its cloud.
type Link struct {
	Cloud       string `json:"cloud"`
	ConnectorID string `json:"connectorId"`
	Credential  string `json:"credential"`
	Name        string `json:"name"`
	LinkedAt    string `json:"linkedAt"`
}

// LinkPath returns the link file path in configDir.
func LinkPath(configDir string) string { return filepath.Join(configDir, LinkFileName) }

// LoadLink reads the link. It returns (nil, nil) when there is none, and refuses loose
// permissions on POSIX.
func LoadLink(configDir string) (*Link, error) {
	path := LinkPath(configDir)
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
	var l Link
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	if _, _, ok := relay.ParseCredential(l.Credential); !ok || l.Cloud == "" || l.ConnectorID == "" {
		return nil, fmt.Errorf("%s is malformed; unlink and link again", path)
	}
	return &l, nil
}

// SaveLink writes the link atomically with 0600 permissions.
func SaveLink(configDir string, l *Link) error {
	if err := privfile.EnsureDir(configDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return privfile.WriteAtomic(LinkPath(configDir), append(data, '\n'))
}

// DeleteLink removes the saved link.
func DeleteLink(configDir string) error {
	err := os.Remove(LinkPath(configDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// API calls the cloud's link endpoints.
type API struct {
	Cloud   string
	Machine relay.Machine
	HTTP    *http.Client
}

// APIError is a refusal from the cloud.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	switch e.Code {
	case "invalid_code":
		return "that code is not valid, was already used, or expired"
	case "rate_limited":
		return "too many attempts; wait a few minutes and try again"
	case "":
		return fmt.Sprintf("the cloud answered %d", e.Status)
	}
	return fmt.Sprintf("the cloud refused (%s)", termsafe.Clean(e.Code, 60))
}

func (a *API) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (a *API) do(ctx context.Context, method, path string, in, out any, bearer string) error {
	u, err := relay.APIURL(a.Cloud, path)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "maestria-connector/"+a.Machine.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var eb relay.ErrorBody
		_ = json.Unmarshal(data, &eb)
		return &APIError{Status: res.StatusCode, Code: eb.Error}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected answer from the cloud")
		}
	}
	return nil
}

// Redeem starts the web-code flow: it validates the code and returns the account hint.
func (a *API) Redeem(ctx context.Context, code string) (*relay.RedeemResponse, error) {
	var out relay.RedeemResponse
	err := a.do(ctx, "POST", "/api/link/redeem", relay.RedeemRequest{Code: code, Machine: a.Machine}, &out, "")
	out.AccountHint, out.DefaultName = termsafe.Clean(out.AccountHint, 120), termsafe.Clean(out.DefaultName, 60)
	return &out, err
}

// Confirm finishes the web-code flow after the person confirmed on the machine.
func (a *API) Confirm(ctx context.Context, code, token, name string) (*relay.Linked, error) {
	var out relay.Linked
	err := a.do(ctx, "POST", "/api/link/confirm", relay.ConfirmRequest{Code: code, Token: token, Name: name}, &out, "")
	out.Name = termsafe.Clean(out.Name, 60)
	return &out, err
}

// Start begins the CLI-code flow.
func (a *API) Start(ctx context.Context) (*relay.StartResponse, error) {
	var out relay.StartResponse
	err := a.do(ctx, "POST", "/api/link/start", relay.StartRequest{Machine: a.Machine}, &out, "")
	out.Code, out.URL = termsafe.Clean(out.Code, 40), termsafe.Clean(out.URL, 300)
	return &out, err
}

// Poll asks whether the code was claimed in the web, and confirms when confirm is set.
func (a *API) Poll(ctx context.Context, pollToken string, confirm bool, name string) (*relay.PollResponse, error) {
	var out relay.PollResponse
	err := a.do(ctx, "POST", "/api/link/poll", relay.PollRequest{PollToken: pollToken, Confirm: confirm, Name: name}, &out, "")
	out.AccountHint, out.DefaultName = termsafe.Clean(out.AccountHint, 120), termsafe.Clean(out.DefaultName, 60)
	if out.Linked != nil {
		out.Linked.Name = termsafe.Clean(out.Linked.Name, 60)
	}
	return &out, err
}

// Unlink tells the cloud to revoke this connector's credential.
func (a *API) Unlink(ctx context.Context, credential string) error {
	return a.do(ctx, "DELETE", "/api/connector", nil, nil, credential)
}

// CleanCloudURL trims a cloud URL to scheme and host (and path prefix), without a trailing slash.
func CleanCloudURL(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
