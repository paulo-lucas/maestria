package cloud

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/maestria-dev/maestria/internal/relay"
)

// The browser code and the Go code must agree byte for byte: the canonical string, the P1363
// signature, the device id and the pairing commitment and code. This runs the real
// hosted-keys.js under node (WebCrypto) and checks its output with internal/relay.
func TestBrowserKeysAgreeWithGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := filepath.Join(t.TempDir(), "check.cjs")
	src := `
const K = require(process.argv[2]);
(async () => {
  const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
  const rec = { privateKey: pair.privateKey, spki, id: await K.deviceIdFor(spki) };
  const body = new TextEncoder().encode('{"body":"héllo ✓"}');
  const hdr = await K.sign(rec, 'cn_0123456789abcdef0123456789abcdef', 'post', '/api/workspaces/a%20b/chat', 'x=1&y=%C3%A9', body);
  const nonce = new Uint8Array(16).map((_, i) => i + 1);
  const r = new Uint8Array(16).map((_, i) => 200 - i);
  console.log(JSON.stringify({
    spki: K.toB64u(spki), id: rec.id, headers: hdr,
    commit: await K.pairCommit(spki, nonce), code: await K.pairCode(spki, nonce, r),
    nonce: K.toB64u(nonce), r: K.toB64u(r), formatted: K.formatCode('471286'),
    bodyHash: await K.bodyHash(body),
  }));
})();
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repository's package.json makes .js files ES modules for node; the browser file is a
	// classic script, so run a copy as CommonJS.
	code, err := os.ReadFile("static/hosted-keys.js")
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(filepath.Dir(script), "hosted-keys.cjs")
	if err := os.WriteFile(copyPath, code, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, copyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		SPKI, ID, Commit, Code, Nonce, R, Formatted, BodyHash string
		Headers                                              map[string]string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	spki, _ := relay.Dec(got.SPKI)
	if relay.DeviceID(spki) != got.ID {
		t.Fatalf("device id: go %s, js %s", relay.DeviceID(spki), got.ID)
	}
	pub, err := relay.ParseSPKI(spki)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"body":"héllo ✓"}`)
	if relay.BodyHash(body) != got.BodyHash || got.Headers[relay.HeaderBody] != got.BodyHash {
		t.Fatal("body hash differs")
	}
	ts, _ := strconv.ParseInt(got.Headers[relay.HeaderTS], 10, 64)
	sr := relay.SignedRequest{ConnectorID: "cn_0123456789abcdef0123456789abcdef", DeviceID: got.ID, Method: "POST", Path: "/api/workspaces/a%20b/chat",
		RawQuery: "x=1&y=%C3%A9", BodySHA256: got.BodyHash, TS: ts, Nonce: got.Headers[relay.HeaderNonce]}
	if err := relay.ValidateSigned(sr); err != nil {
		t.Fatalf("the browser's request does not pass the connector's shape check: %v", err)
	}
	if !relay.VerifySignature(pub, sr.Canonical(), got.Headers[relay.HeaderSig]) {
		t.Fatal("a signature made by WebCrypto does not verify in Go (canonical string or P1363 encoding differs)")
	}
	nonce, _ := relay.Dec(got.Nonce)
	r, _ := relay.Dec(got.R)
	if relay.PairCommit(spki, nonce) != got.Commit {
		t.Fatalf("pair commit: go %s, js %s", relay.PairCommit(spki, nonce), got.Commit)
	}
	if relay.PairCode(spki, nonce, r) != got.Code {
		t.Fatalf("pair code: go %s, js %s", relay.PairCode(spki, nonce, r), got.Code)
	}
	if relay.FormatPairCode("471286") != got.Formatted {
		t.Fatalf("format: %q vs %q", relay.FormatPairCode("471286"), got.Formatted)
	}
}
