/*
 * Maestria hosted mode: the device key and everything that touches it.
 *
 * The key pair is ECDSA P-256 made with WebCrypto as NON-EXTRACTABLE and kept in IndexedDB, so
 * script cannot read the private key out; it can only ask the browser to sign. Every request to a
 * machine is signed (docs/hosted-protocol.md, section 7). Pairing uses commit-then-reveal so a
 * hostile cloud cannot swap the key (section 6). Everything here must produce byte-identical
 * results to internal/relay (a Go test runs this file under node to check that).
 */
(function (root) {
  'use strict';

  var enc = new TextEncoder();
  var SIGNING_VERSION = 'maestria-relay-v1';
  var COMMIT_TAG = 'maestria-pair-commit-v1';
  var SAS_TAG = 'maestria-pair-sas-v1';
  var DB_NAME = 'maestria-keys';
  var STORE = 'keys';
  var KEY_ID = 'device';

  function subtle() {
    var c = root.crypto;
    if (!c || !c.subtle) throw new Error('This browser has no WebCrypto (needs https or localhost).');
    return c.subtle;
  }

  // ---- encodings ----

  function toB64u(bytes) {
    var u8 = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
    var s = '';
    for (var i = 0; i < u8.length; i++) s += String.fromCharCode(u8[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function fromB64u(str) {
    var s = str.replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) s += '=';
    var bin = atob(s);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function toHex(bytes) {
    var u8 = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
    var s = '';
    for (var i = 0; i < u8.length; i++) s += (u8[i] < 16 ? '0' : '') + u8[i].toString(16);
    return s;
  }

  function concat() {
    var parts = Array.prototype.slice.call(arguments);
    var n = 0;
    parts.forEach(function (p) { n += p.length; });
    var out = new Uint8Array(n);
    var o = 0;
    parts.forEach(function (p) { out.set(p, o); o += p.length; });
    return out;
  }

  function randomBytes(n) {
    var b = new Uint8Array(n);
    root.crypto.getRandomValues(b);
    return b;
  }

  async function sha256(bytes) {
    return new Uint8Array(await subtle().digest('SHA-256', bytes));
  }

  // ---- hashing and canonical strings (must match internal/relay) ----

  async function bodyHash(bytes) { return toHex(await sha256(bytes)); }

  function canonical(r) {
    return [SIGNING_VERSION, r.connectorId, r.deviceId, r.method.toUpperCase(), r.path, r.rawQuery || '', r.bodySha256, String(r.ts), r.nonce].join('\n');
  }

  async function deviceIdFor(spki) { return toHex(await sha256(spki)).slice(0, 32); }

  async function pairCommit(spki, nonce) { return toB64u(await sha256(concat(enc.encode(COMMIT_TAG), spki, nonce))); }

  async function pairCode(spki, nonce, r) {
    var d = await sha256(concat(enc.encode(SAS_TAG), spki, nonce, r));
    var n = ((d[0] << 24) | (d[1] << 16) | (d[2] << 8) | d[3]) >>> 0;
    return String(n % 1000000).padStart(6, '0');
  }

  function formatCode(code) {
    return code.length === 6 ? code.slice(0, 2) + ' · ' + code.slice(2, 4) + ' · ' + code.slice(4, 6) : code;
  }

  // ---- IndexedDB ----

  function openDB() {
    return new Promise(function (resolve, reject) {
      var req = root.indexedDB.open(DB_NAME, 1);
      req.onupgradeneeded = function () { req.result.createObjectStore(STORE); };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error); };
    });
  }

  function tx(mode, fn) {
    return openDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var t = db.transaction(STORE, mode);
        var out = fn(t.objectStore(STORE));
        t.oncomplete = function () { db.close(); resolve(out && out.result !== undefined ? out.result : undefined); };
        t.onerror = function () { db.close(); reject(t.error); };
        t.onabort = function () { db.close(); reject(t.error); };
      });
    });
  }

  var cached = null;

  // load returns the stored device key or null.
  async function load() {
    if (cached) return cached;
    var rec = await tx('readonly', function (s) { return s.get(KEY_ID); });
    if (rec && rec.privateKey && rec.spki) cached = rec;
    return cached;
  }

  // create makes a fresh non-extractable key pair and stores it, replacing any old one.
  async function create() {
    var pair = await subtle().generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    var spki = new Uint8Array(await subtle().exportKey('spki', pair.publicKey));
    var rec = { privateKey: pair.privateKey, publicKey: pair.publicKey, spki: spki, id: await deviceIdFor(spki), createdAt: Date.now() };
    await tx('readwrite', function (s) { return s.put(rec, KEY_ID); });
    cached = rec;
    return rec;
  }

  async function ensure() { return (await load()) || create(); }

  // forget deletes the key (sign out of this device, or after a revocation).
  async function forget() {
    cached = null;
    await tx('readwrite', function (s) { return s.delete(KEY_ID); });
  }

  // ---- signing ----

  // sign returns the headers for one request. body is a Uint8Array (empty for none).
  async function sign(rec, connectorId, method, path, rawQuery, body) {
    var req = {
      connectorId: connectorId, deviceId: rec.id, method: method, path: path, rawQuery: rawQuery || '',
      bodySha256: await bodyHash(body), ts: Date.now(), nonce: toB64u(randomBytes(16))
    };
    var sig = new Uint8Array(await subtle().sign({ name: 'ECDSA', hash: 'SHA-256' }, rec.privateKey, enc.encode(canonical(req))));
    return {
      'X-Maestria-Device': rec.id,
      'X-Maestria-Ts': String(req.ts),
      'X-Maestria-Nonce': req.nonce,
      'X-Maestria-Body': req.bodySha256,
      'X-Maestria-Sig': toB64u(sig)
    };
  }

  var api = {
    toB64u: toB64u, fromB64u: fromB64u, toHex: toHex, bodyHash: bodyHash, canonical: canonical, deviceIdFor: deviceIdFor,
    pairCommit: pairCommit, pairCode: pairCode, formatCode: formatCode, randomBytes: randomBytes,
    load: load, create: create, ensure: ensure, forget: forget, sign: sign
  };
  root.MaestriaKeys = api;
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
})(typeof window !== 'undefined' ? window : globalThis);
