package devices

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/relay/relaytest"
)

func throttled(err error) bool {
	var te *ThrottledError
	return errors.As(err, &te)
}

// A hostile cloud that keeps offering pairings, and frees each slot with a bad reveal, must not
// get more than a handful of tries however long it keeps going.
func TestPairingGrindingIsBounded(t *testing.T) {
	m, c := newManager(t)
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := relay.MarshalSPKI(&priv.PublicKey)

	accepted := 0
	for i := 0; i < 3*3600; i++ { // three hours, one offer per second
		c.advance(time.Second)
		n := make([]byte, 16)
		_, _ = rand.Read(n)
		pair := fmt.Sprintf("atk%013d", i)
		if _, err := m.Offer(pair, relay.PairCommit(spki, n), "Safari on iPhone", "UA"); err != nil {
			if !throttled(err) && !errors.Is(err, ErrTooManyPending) {
				t.Fatalf("offer %d: %v", i, err)
			}
			continue
		}
		accepted++
		// Free the slot with a reveal that fails the commitment check.
		_, _ = m.Reveal(pair, relay.Enc(spki), relay.Enc([]byte("wrong-nonce-0000")))
	}
	// One accepted offer, then the failed reveal pauses pairing for an hour: 3 hours allow 3.
	if accepted > 3 {
		t.Fatalf("a hostile cloud got %d tries in three hours", accepted)
	}
}

// Without failed reveals the window and the daily budgets still hold (offers left to expire).
func TestPairingOfferBudget(t *testing.T) {
	m, c := newManager(t)
	offer := func(i int) error {
		_, err := m.Offer(fmt.Sprintf("budget-%08d", i), relay.Enc(make([]byte, 32)), "x", "")
		return err
	}
	for i := 0; i < MaxOffersPerWindow; i++ {
		if err := offer(i); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	if err := offer(99); !throttled(err) {
		t.Fatalf("the offer past the window budget must be paused, got %v", err)
	}
	// The same pairing offered again is not a new try.
	if err := offer(0); err != nil {
		t.Fatalf("repeating a known pairing: %v", err)
	}
	c.advance(PairWindow + time.Second)
	if err := offer(100); err != nil {
		t.Fatalf("the window slides: %v", err)
	}

	// Over a day the total is capped as well.
	total := 6
	for h := 0; h < 24 && total < MaxOffersPerDay+5; h++ {
		for i := 0; i < 2; i++ {
			c.advance(PairWindow + time.Second)
			if err := offer(1000 + total); err == nil {
				total++
			}
		}
	}
	if total > MaxOffersPerDay+1 { // +1: the first offer of the next day is allowed
		t.Fatalf("%d offers accepted in about a day", total)
	}
}

// A failed code typed on the machine counts, and past the budget even the right code is refused.
func TestWrongApprovalCodesAreLimited(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	r, err := m.Offer("pair-limit-01", p.Commit, b.Label, "UA")
	if err != nil {
		t.Fatal(err)
	}
	right, err := m.Reveal("pair-limit-01", relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	if right != b.Code(t, p, r) {
		t.Fatal("codes differ")
	}
	wrong := "00000000"
	if wrong == right {
		wrong = "11111111"
	}
	for i := 0; i < MaxBadCodes; i++ {
		if _, err := m.Approve(wrong); !errors.Is(err, ErrNoPairing) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, err := m.Approve(right); !throttled(err) {
		t.Fatalf("after %d wrong codes the right one must be refused too, got %v", MaxBadCodes, err)
	}
	if _, err := m.Lookup(right); !throttled(err) {
		t.Fatalf("lookup is paused as well: %v", err)
	}
	c.advance(PairWindow + time.Second)
	// The pairing itself expired meanwhile: the right code finds nothing, but it is not throttled.
	if _, err := m.Approve(right); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("after the window: %v", err)
	}
}

// Every offer, refusal, failure and approval is written to the audit log.
func TestPairingEventsAreAudited(t *testing.T) {
	m, c := newManager(t)
	log := audit.New(m.db, c.now)
	m.SetAudit(log)

	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	for i := 0; i < MaxOffersPerWindow; i++ {
		_, _ = m.Offer(fmt.Sprintf("extra-%08d", i), relay.Enc(make([]byte, 32)), "x", "")
	}
	_, _ = m.Approve("12345678")

	seen := map[string]int{}
	entries, _ := log.Recent(100)
	for _, e := range entries {
		seen[e.Event]++
	}
	for _, ev := range []string{"pair_offered", "device_approved", "pair_refused", "pair_bad_code"} {
		if seen[ev] == 0 {
			t.Errorf("no %q entry in the audit log (have %v)", ev, seen)
		}
	}
	for _, e := range entries {
		if e.Event == "device_approved" && !strings.Contains(e.Detail, relay.Fingerprint(b.DeviceID)) {
			t.Errorf("the approval does not record the fingerprint: %s", e.Detail)
		}
	}
}

// The pairing shows the same fingerprint the browser computes, once it revealed its key, so the
// person can compare it, and the label the cloud chose cannot carry terminal escape sequences.
func TestPairingShowsFingerprintAndCleanLabel(t *testing.T) {
	m, _ := newManager(t)
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	label := "\x1b]52;c;Y3VybCBldmlsIHwgc2g=\x07\x1b[2K\rSafari on iPhone"
	if _, err := m.Offer("pair-fp-0001", p.Commit, label, "UA\x1b[31m"); err != nil {
		t.Fatal(err)
	}
	list, _ := m.Pending()
	if len(list) != 1 || list[0].Fingerprint != "" {
		t.Fatalf("no fingerprint before the key is revealed: %+v", list)
	}
	for _, s := range []string{list[0].Label, list[0].UA} {
		if strings.ContainsAny(s, "\x1b\x07\r") {
			t.Fatalf("control characters survived: %q", s)
		}
	}
	code, err := m.Reveal("pair-fp-0001", relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Lookup(code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != relay.Fingerprint(b.DeviceID) || got.DeviceID != b.DeviceID {
		t.Fatalf("fingerprint %q, want %q", got.Fingerprint, relay.Fingerprint(b.DeviceID))
	}
	if got.CreatedAt == 0 {
		t.Fatal("the offer time is shown at approval")
	}
}

func TestDefaultPolicyLifetimes(t *testing.T) {
	p := DefaultPolicy()
	if p.Absolute != 0 || p.Idle != 0 { // no limit

		t.Fatalf("unexpected default lifetimes: %+v", p)
	}
	// The config file defaults (what the connector really runs with) are the same numbers.
	cfg, err := config.Defaults(t.TempDir(), config.Env{Getenv: func(string) string { return "" }, GOOS: runtime.GOOS, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Relay.ApprovalAbsoluteDays != DefaultAbsoluteDays || cfg.Relay.IdleDays != DefaultIdleDays {
		t.Fatalf("config defaults %d/%d differ from the policy defaults %d/%d",
			cfg.Relay.ApprovalAbsoluteDays, cfg.Relay.IdleDays, DefaultAbsoluteDays, DefaultIdleDays)
	}
}

// Finished pairings are pruned, so a long flood of offers cannot grow the table and slow every
// call down; live ones and recent ones are kept.
func TestFinishedPairingsArePruned(t *testing.T) {
	m, c := newManager(t)
	count := func() (n int) {
		_ = m.db.QueryRow("SELECT COUNT(*) FROM device_pairings").Scan(&n)
		return n
	}
	for i := 0; i < 2000; i++ { // rows as a flood would leave them: expired, rejected, mismatched
		state := []string{"expired", "rejected", "mismatch", "approved"}[i%4]
		if _, err := m.db.Exec(`INSERT INTO device_pairings (pair_id, commit_hash, label, ua, r, state, created_at, expires_at) VALUES (?, 'x', 'x', '', 'r', ?, ?, ?)`,
			fmt.Sprintf("old-%08d", i), state, c.now().UnixMilli(), c.now().Add(PairingTTL).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	if _, err := m.Offer("live-0001", p.Commit, "x", ""); err != nil {
		t.Fatal(err)
	}
	if count() != 2001 {
		t.Fatalf("setup: %d rows", count())
	}
	// Shortly after expiry the finished rows are still kept (they may not have been reported yet).
	c.advance(PairingTTL + 10*time.Minute)
	if _, err := m.Pending(); err != nil {
		t.Fatal(err)
	}
	if count() != 2001 {
		t.Fatalf("rows pruned too early: %d", count())
	}
	// An hour after expiry they go; the pairing that expired meanwhile goes with them one sweep later.
	c.advance(PairingKeep)
	if _, err := m.Pending(); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d finished pairings were not pruned", n)
	}
	// A pairing still waiting is never pruned.
	if _, err := m.Offer("live-0002", p.Commit, "x", ""); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	_, _ = m.Pending()
	if count() != 1 {
		t.Fatalf("a live pairing was pruned: %d rows", count())
	}
	// And the lookups use the index.
	var plan string
	rows, err := m.db.Query("EXPLAIN QUERY PLAN SELECT COUNT(*) FROM device_pairings WHERE state IN ('pending','revealed')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c2 int
		var detail string
		_ = rows.Scan(&a, &b, &c2, &detail)
		plan += detail + ";"
	}
	if !strings.Contains(plan, "idx_device_pairings_state_expires") {
		t.Fatalf("the live-pairing count does not use the (state, expires_at) index: %s", plan)
	}
}
