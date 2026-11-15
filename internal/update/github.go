package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIBase is GitHub's public API. No token is ever sent.
const DefaultAPIBase = "https://api.github.com"

// Channel selects which releases are considered.
type Channel string

const (
	ChannelStable     Channel = "stable"
	ChannelPrerelease Channel = "prerelease"
)

// ParseChannel validates a --channel value ("" means stable).
func ParseChannel(s string) (Channel, error) {
	switch Channel(s) {
	case "", ChannelStable:
		return ChannelStable, nil
	case ChannelPrerelease:
		return ChannelPrerelease, nil
	}
	return "", fmt.Errorf("unknown channel %q (use stable or prerelease)", s)
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

// Release is a GitHub release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Version returns the release's parsed version.
func (r Release) Version() (Version, error) { return ParseVersion(r.Tag) }

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// RateLimitError means GitHub refused the request because of its rate limit.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "GitHub's rate limit was reached; try again later"
	}
	return "GitHub's rate limit was reached; try again after " + e.Reset.Local().Format("15:04")
}

// ErrNoRelease means no matching release exists.
var ErrNoRelease = errors.New("no release found")

const (
	maxAPIBytes  = 4 << 20
	apiTimeout   = 20 * time.Second
	listPageSize = 30
)

func (u *Updater) apiBase() string {
	if u.APIBase != "" {
		return strings.TrimRight(u.APIBase, "/")
	}
	return DefaultAPIBase
}

func (u *Updater) repo() string {
	if u.Repo != "" {
		return u.Repo
	}
	return Repo
}

func (u *Updater) httpClient() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return defaultHTTP
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Minute}

// allowURL enforces https for everything fetched (http only when Insecure, for tests).
func (u *Updater) allowURL(raw string) error {
	p, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad URL %q", raw)
	}
	if p.Scheme == "https" || (u.Insecure && p.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("refusing non-https URL %q", raw)
}

func (u *Updater) get(ctx context.Context, raw string) (*http.Response, error) {
	if err := u.allowURL(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "maestria-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	c := *u.httpClient()
	prev := c.CheckRedirect
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if err := u.allowURL(r.URL.String()); err != nil {
			return err
		}
		if prev != nil {
			return prev(r, via)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func rateLimited(resp *http.Response) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") != "0" && resp.Header.Get("Retry-After") == "" {
		return nil
	}
	e := &RateLimitError{}
	if s := resp.Header.Get("X-RateLimit-Reset"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			e.Reset = time.Unix(n, 0)
		}
	} else if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			e.Reset = time.Now().Add(time.Duration(n) * time.Second)
		}
	}
	return e
}

func (u *Updater) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	resp, err := u.get(ctx, u.apiBase()+path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if rl := rateLimited(resp); rl != nil {
		return rl
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %s for %s", resp.Status, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxAPIBytes {
		return errors.New("GitHub's answer is too large")
	}
	return json.Unmarshal(body, out)
}

// Latest returns the newest release of the channel. Stable uses releases/latest (GitHub excludes
// drafts and prereleases); prerelease picks the highest version among all published releases.
func (u *Updater) Latest(ctx context.Context, ch Channel) (Release, error) {
	if ch != ChannelPrerelease {
		var r Release
		if err := u.getJSON(ctx, "/repos/"+u.repo()+"/releases/latest", &r); err != nil {
			return Release{}, err
		}
		if r.Draft || r.Tag == "" {
			return Release{}, ErrNoRelease
		}
		return r, nil
	}
	var list []Release
	if err := u.getJSON(ctx, fmt.Sprintf("/repos/%s/releases?per_page=%d", u.repo(), listPageSize), &list); err != nil {
		return Release{}, err
	}
	var best Release
	var bestV Version
	found := false
	for _, r := range list {
		if r.Draft {
			continue
		}
		v, err := r.Version()
		if err != nil {
			continue
		}
		if !found || v.Compare(bestV) > 0 {
			best, bestV, found = r, v, true
		}
	}
	if !found {
		return Release{}, ErrNoRelease
	}
	return best, nil
}

// ByTag returns the release published under tag.
func (u *Updater) ByTag(ctx context.Context, tag string) (Release, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.ContainsAny(tag, "/?#") {
		return Release{}, fmt.Errorf("invalid tag %q", tag)
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	var r Release
	if err := u.getJSON(ctx, "/repos/"+u.repo()+"/releases/tags/"+url.PathEscape(tag), &r); err != nil {
		if errors.Is(err, ErrNoRelease) {
			return Release{}, fmt.Errorf("no release tagged %s", tag)
		}
		return Release{}, err
	}
	if r.Draft {
		return Release{}, fmt.Errorf("no release tagged %s", tag)
	}
	return r, nil
}
