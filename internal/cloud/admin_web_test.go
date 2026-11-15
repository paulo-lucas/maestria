package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// adminBed has a signed-in administrator (ann), a signed-in ordinary user (bob), and invite mode on.
func adminBed(t *testing.T, mut ...func(*Config)) (b *bed, admin, user *browser) {
	b = newBed(t, append([]func(*Config){func(c *Config) { c.DevEmail = "stdout" }}, mut...)...) // the bed has its own mailer; this only says one is configured
	admin = b.browser("10.0.0.1")
	admin.signup("ann@example.com", testPassword)
	user = b.browser("10.0.0.2")
	user.signup("bob@example.com", testPassword)
	if _, err := b.srv.acc.SetAdmin("ann@example.com", true); err != nil {
		t.Fatal(err)
	}
	b.srv.cfg.Signup = SignupInvite
	return b, admin, user
}

func TestAdminRoutesAreInvisibleToEveryoneElse(t *testing.T) {
	b, _, user := adminBed(t)
	inv, _ := b.invite(InviteOptions{})
	anon := b.browser("10.0.0.3")
	for name, br := range map[string]*browser{"user": user, "anonymous": anon} {
		for _, path := range []string{"/admin/invites"} {
			want := 404
			if name == "anonymous" {
				want = 303 // to the login page
			}
			if r := br.get(path); r.StatusCode != want {
				t.Errorf("%s GET %s: %d, want %d", name, path, r.StatusCode, want)
			}
		}
		if name != "user" {
			continue
		}
		if r := br.api("GET", "/admin/invites", nil); r.StatusCode != 404 {
			t.Errorf("user fragment: %d", r.StatusCode)
		}
		if r := br.api("POST", "/api/invites", map[string]any{"note": "x"}); r.StatusCode != 404 {
			t.Errorf("user create: %d", r.StatusCode)
		}
		if r := br.api("POST", "/api/invites/"+inv.ID+"/revoke", nil); r.StatusCode != 404 {
			t.Errorf("user revoke: %d", r.StatusCode)
		}
	}
	if r := anon.api("POST", "/api/invites", map[string]any{}); r.StatusCode != 401 && r.StatusCode != 403 {
		t.Errorf("anonymous create: %d", r.StatusCode)
	}
	// Nothing was created or revoked by them.
	list, _ := b.srv.acc.ListInvites()
	if len(list) != 1 || list[0].RevokedAt != 0 {
		t.Fatalf("invitations changed: %+v", list)
	}
	// The menu shows the page only to administrators.
	if r := user.get("/"); strings.Contains(r.body, "/admin/invites") {
		t.Fatal("an ordinary user must not see the link")
	}
}

func TestAdminCreatesListsAndRevokes(t *testing.T) {
	b, admin, _ := adminBed(t)
	if r := admin.get("/"); !strings.Contains(r.body, `href="/admin/invites"`) {
		t.Fatal("an administrator sees the link")
	}
	page := admin.get("/admin/invites")
	if page.StatusCode != 200 || !strings.Contains(page.body, "Invite someone") || !strings.Contains(page.body, "invites.js") {
		t.Fatalf("page: %d", page.StatusCode)
	}

	r := admin.api("POST", "/api/invites", map[string]any{"note": "for carol", "email": "Carol@Example.com", "expires": "3d"})
	if r.StatusCode != 200 {
		t.Fatalf("create: %d %s", r.StatusCode, r.body)
	}
	var out struct {
		ID, Link string
		Emailed  bool
	}
	decode(t, r.body, &out)
	want := b.cfg.PublicURL + "/signup?invite="
	if !strings.HasPrefix(out.Link, want) || out.Emailed {
		t.Fatalf("link %q must start with the configured public URL %q", out.Link, want)
	}
	tok := strings.TrimPrefix(out.Link, want)

	// The list never shows the token and masks the address.
	list := admin.get("/admin/invites")
	if strings.Contains(list.body, tok) || strings.Contains(list.body, "carol@example.com") || !strings.Contains(list.body, "c***@example.com") || !strings.Contains(list.body, out.ID) || !strings.Contains(list.body, "for carol") {
		t.Fatal("the list shows the id, the note and a masked address, never the token")
	}
	// The link works for the bound address only.
	if rr := b.browser("10.0.0.7").signupWith(tok, "mallory@example.com"); rr.StatusCode != 403 {
		t.Fatalf("mismatch: %d", rr.StatusCode)
	}
	if rr := b.browser("10.0.0.8").signupWith(tok, "carol@example.com"); rr.StatusCode != 200 {
		t.Fatalf("carol: %d", rr.StatusCode)
	}
	if got := admin.get("/admin/invites").body; !strings.Contains(got, "Used") || !strings.Contains(got, "c***@example.com") {
		t.Fatal("the list says it was used")
	}

	// Revoke twice: both fine, and audited once with the admin and the invite id.
	r2 := admin.api("POST", "/api/invites", map[string]any{})
	var second struct{ ID string }
	decode(t, r2.body, &second)
	for i := 0; i < 2; i++ {
		if rr := admin.api("POST", "/api/invites/"+second.ID+"/revoke", nil); rr.StatusCode != 200 {
			t.Fatalf("revoke %d: %d", i, rr.StatusCode)
		}
	}
	if rr := admin.api("POST", "/api/invites/inv_nope/revoke", nil); rr.StatusCode != 404 {
		t.Fatalf("unknown: %d", rr.StatusCode)
	}
	var n int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM audit WHERE event = 'invite_revoked' AND detail LIKE ?", "%"+second.ID+"%").Scan(&n)
	if n != 1 {
		t.Fatalf("revoke audited %d times", n)
	}
}

func TestAdminWritesNeedCSRFAndOrigin(t *testing.T) {
	_, admin, _ := adminBed(t)
	admin.api("GET", "/api/me", nil)
	for name, mod := range map[string]func(*http.Request){
		"no token":     func(r *http.Request) { r.Header.Del("X-CSRF-Token") },
		"wrong token":  func(r *http.Request) { r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43)) },
		"no origin":    func(r *http.Request) { r.Header.Del("Origin") },
		"other origin": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
	} {
		req, _ := http.NewRequest("POST", admin.b.ts.URL+"/api/invites", strings.NewReader(`{"note":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", admin.b.ts.URL)
		req.Header.Set("X-CSRF-Token", admin.csrf)
		mod(req)
		if r := admin.do(req); r.StatusCode != 403 {
			t.Errorf("%s: %d, want 403", name, r.StatusCode)
		}
	}
	list, _ := admin.b.srv.acc.ListInvites()
	if len(list) != 0 {
		t.Fatalf("a refused request created %d invitations", len(list))
	}
}

func TestDemoteTakesEffectOnTheNextRequest(t *testing.T) {
	b, admin, _ := adminBed(t)
	if r := admin.get("/admin/invites"); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if _, err := b.srv.acc.SetAdmin("ann@example.com", false); err != nil {
		t.Fatal(err)
	}
	if r := admin.get("/admin/invites"); r.StatusCode != 404 {
		t.Fatalf("after demote: %d", r.StatusCode)
	}
	if r := admin.api("POST", "/api/invites", map[string]any{}); r.StatusCode != 404 {
		t.Fatalf("create after demote: %d", r.StatusCode)
	}
}

// The web cannot make anyone an administrator, whatever it sends.
func TestNothingOnTheWebSetsAdmin(t *testing.T) {
	b, admin, user := adminBed(t)
	for _, br := range []*browser{admin, user} {
		for _, path := range []string{"/api/invites", "/api/me", "/api/connectors/x/rename"} {
			br.api("POST", path, map[string]any{"isAdmin": true, "is_admin": true})
		}
	}
	_, tok := b.invite(InviteOptions{})
	br := b.browser("10.0.0.9")
	br.get("/signup?invite=" + tok)
	br.form("/signup", url.Values{"email": {"eve@example.com"}, "password": {testPassword}, "invite": {tok}, "is_admin": {"1"}, "isAdmin": {"true"}})
	var n int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM users WHERE is_admin = 1").Scan(&n)
	if n != 1 {
		t.Fatalf("exactly ann is an administrator, got %d", n)
	}
	if r := admin.api("POST", "/api/invites", map[string]any{"isAdmin": true}); r.StatusCode != 400 {
		t.Fatalf("unknown fields are refused: %d", r.StatusCode)
	}
}

func TestAdminInputIsValidatedServerSide(t *testing.T) {
	_, admin, _ := adminBed(t)
	for name, body := range map[string]map[string]any{
		"zero":         {"expires": "0"},
		"negative":     {"expires": "-7d"},
		"too long":     {"expires": "31d"},
		"too short":    {"expires": "10m"},
		"words":        {"expires": "forever"},
		"bad email":    {"email": "nope"},
		"long note":    {"note": strings.Repeat("n", 201)},
		"send no addr": {"sendEmail": true},
	} {
		if r := admin.api("POST", "/api/invites", body); r.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, r.StatusCode, r.body)
		}
	}
}

func TestInviteEmailIsFixedTextFromThePublicURL(t *testing.T) {
	b, admin, _ := adminBed(t)
	note := "hi\nBcc: spy@example.com\nSubject: free money"
	r := admin.api("POST", "/api/invites", map[string]any{"email": "Carol@Example.com", "note": note, "sendEmail": true})
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	m := b.waitMail("carol@example.com", "invited")
	if m.Subject != "You are invited to Maestria" {
		t.Fatalf("the subject is fixed text: %q", m.Subject)
	}
	if !strings.Contains(m.Body, b.cfg.PublicURL+"/signup?invite=") || !strings.Contains(m.Body, "not by Maestria") {
		t.Fatalf("body: %q", m.Body)
	}
	if strings.Contains(m.Body, "\nBcc:") || strings.Contains(m.Body, "\nSubject:") {
		t.Fatalf("the note's line breaks must not become headers or lines of their own: %q", m.Body)
	}
	// Counted against the same daily cap as the other mail.
	var sent int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM audit WHERE event = 'mail_sent'").Scan(&sent)
	if sent < 3 { // two verification mails during setup + the invitation
		t.Fatalf("mail_sent entries: %d", sent)
	}
}

func TestInviteMailLimits(t *testing.T) {
	// One recipient: 3 a day however many administrators or minutes between.
	b, admin, _ := adminBed(t)
	for i := 0; i < 4; i++ {
		b.clock.advance(2 * time.Minute) // past the per-address cooldown
		r := admin.api("POST", "/api/invites", map[string]any{"email": "carol@example.com", "sendEmail": true})
		want := 200
		if i == 3 {
			want = 429
		}
		if r.StatusCode != want {
			t.Fatalf("invitation email %d to one recipient: %d %s", i, r.StatusCode, r.body)
		}
	}
	// A refused email leaves no invitation behind.
	list, _ := b.srv.acc.ListInvites()
	if len(list) != 3 {
		t.Fatalf("invitations: %d", len(list))
	}

	// The daily cap is shared with every other email.
	b2, admin2, _ := adminBed(t, func(c *Config) { c.MailDailyLimit = 2 })
	r := admin2.api("POST", "/api/invites", map[string]any{"email": "x@example.com", "sendEmail": true})
	if r.StatusCode != 429 {
		t.Fatalf("the daily cap already reached by the sign-ups: %d", r.StatusCode)
	}
	if got, _ := b2.srv.acc.ListInvites(); len(got) != 0 {
		t.Fatal("no invitation when the email is refused")
	}
}

func TestInviteCreationIsRateLimitedPerAdmin(t *testing.T) {
	_, admin, _ := adminBed(t)
	got429 := false
	for i := 0; i < inviteCreatePerHour+3; i++ {
		r := admin.api("POST", "/api/invites", map[string]any{})
		if r.StatusCode == 429 {
			got429 = true
			if i < inviteCreatePerHour {
				t.Fatalf("limited too early at %d", i)
			}
		} else if r.StatusCode != 200 {
			t.Fatal(r.StatusCode, r.body)
		}
	}
	if !got429 {
		t.Fatal("creating invitations must be rate limited per administrator")
	}
}

func TestInvitePageWarnsInOpenMode(t *testing.T) {
	b, admin, _ := adminBed(t)
	b.srv.cfg.Signup = SignupOpen
	if r := admin.get("/admin/invites"); !strings.Contains(r.body, "Sign-up is open") {
		t.Fatal("the page says sign-up is open")
	}
	_ = fmt.Sprint()
}

func decode(t *testing.T, body string, v any) {
	t.Helper()
	if err := jsonUnmarshalLoose(body, v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

func jsonUnmarshalLoose(s string, v any) error { return json.Unmarshal([]byte(s), v) }
