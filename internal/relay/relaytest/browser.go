// Package relaytest is a fake browser for tests: it holds an ECDSA P-256 key like the WebCrypto
// one, pairs with a connector and signs relayed requests exactly as the real frontend does.
package relaytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/relay"
)

// Browser is a fake browser device.
type Browser struct {
	Key      *ecdsa.PrivateKey
	SPKI     []byte
	DeviceID string
	Label    string
	Now      func() time.Time
}

// NewBrowser creates a browser with a fresh key.
func NewBrowser(t testing.TB) *Browser {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := relay.MarshalSPKI(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &Browser{Key: k, SPKI: spki, DeviceID: relay.DeviceID(spki), Label: "Test browser", Now: time.Now}
}

// Pairing is a browser's half of a pairing.
type Pairing struct {
	Nonce  []byte
	Commit string
}

// StartPairing creates the nonce and the commitment sent before the key.
func (b *Browser) StartPairing(t testing.TB) *Pairing {
	t.Helper()
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		t.Fatal(err)
	}
	return &Pairing{Nonce: n, Commit: relay.PairCommit(b.SPKI, n)}
}

// Code is the code the browser shows once it has the connector's r (b64u).
func (b *Browser) Code(t testing.TB, p *Pairing, r string) string {
	t.Helper()
	rb, err := relay.Dec(r)
	if err != nil {
		t.Fatal(err)
	}
	return relay.PairCode(b.SPKI, p.Nonce, rb)
}

// Request is a signed relayed request ready to send.
type Request struct {
	Method, Path, RawQuery string
	Body                   []byte
	Header                 map[string]string
}

// Sign signs a request for connectorID with a fresh nonce and the browser clock.
func (b *Browser) Sign(t testing.TB, connectorID, method, path, rawQuery string, body []byte) *Request {
	t.Helper()
	return b.SignAt(t, connectorID, method, path, rawQuery, body, b.Now().UnixMilli(), relay.Enc(randBytes(t, 16)))
}

// SignAt signs with an explicit timestamp and nonce (replay and clock tests).
func (b *Browser) SignAt(t testing.TB, connectorID, method, path, rawQuery string, body []byte, ts int64, nonce string) *Request {
	t.Helper()
	hash := relay.BodyHash(body)
	sr := relay.SignedRequest{ConnectorID: connectorID, DeviceID: b.DeviceID, Method: method, Path: path, RawQuery: rawQuery, BodySHA256: hash, TS: ts, Nonce: nonce}
	sig, err := relay.Sign(b.Key, sr.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	return &Request{Method: method, Path: path, RawQuery: rawQuery, Body: body, Header: map[string]string{
		relay.HeaderDevice: b.DeviceID,
		relay.HeaderTS:     itoa(ts),
		relay.HeaderNonce:  nonce,
		relay.HeaderBody:   hash,
		relay.HeaderSig:    sig,
	}}
}

func randBytes(t testing.TB, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
