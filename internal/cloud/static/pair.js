/*
 * Approve this browser (docs/hosted-protocol.md, section 6). The browser makes a non-extractable
 * key, commits to it, and reveals it to each machine only after that machine answered with a
 * random value. The code on screen is computed here from the machine's value and our own key, so
 * the cloud cannot show a code that matches a key it swapped in. The person approves on the
 * machine by typing the code this page shows.
 */
(function () {
  'use strict';
  var body = document.body;
  if (!body || body.dataset.app !== 'pair') return;
  var K = window.MaestriaKeys;
  var csrf = body.dataset.csrf;
  var root = document.getElementById('pair-body');
  var card = document.getElementById('pair-root');

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function icon(id) { return '<svg class="icon" aria-hidden="true"><use href="#' + id + '"/></svg>'; }

  async function cloud(method, path, payload) {
    var headers = { 'Accept': 'application/json', 'X-CSRF-Token': csrf };
    var opts = { method: method, credentials: 'same-origin', headers: headers };
    if (payload !== undefined) { headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(payload); }
    var res = await fetch(path, opts);
    if (res.status === 401) { location.href = '/login'; throw new Error('unauthenticated'); }
    var data = null;
    try { data = await res.json(); } catch (e) { /* none */ }
    if (!res.ok) throw new Error((data && data.error) || 'Request failed (' + res.status + ')');
    return data;
  }

  function browserLabel() {
    var ua = navigator.userAgent;
    var b = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
    var os = /iPhone/.test(ua) ? 'iPhone' : /iPad/.test(ua) ? 'iPad' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
    return os ? b + ' on ' + os : b;
  }

  function fmt(ms) {
    var s = Math.max(0, Math.round(ms / 1000));
    return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
  }

  var label = browserLabel();
  var pairing = null, expires = 0, revealed = {}, codes = {}, nonce = null, rec = null, finished = false, lastHTML = '';

  function row(m) {
    var meta, right = '';
    if (!m.online) {
      meta = 'Approve it later, when it is back.';
      return '<li class="device"><span class="device__icon">' + icon('i-laptop') + '</span><span class="device__name">' + esc(m.name) + ' <span class="status" data-status="offline">Offline</span></span><span class="device__meta">' + meta + '</span></li>';
    }
    var code = codes[m.connectorId];
    switch (m.state) {
      case 'approved':
        meta = '<span class="status" data-status="ok">Approved</span>'; right = '';
        break;
      case 'rejected':
        meta = m.reason === 'kill_switch' ? 'The kill switch is on for this machine.' : m.reason === 'too_many_pending' ? 'This machine has too many pairings waiting. Approve or reject one first.' : m.reason === 'pairing_paused' ? 'This machine paused pairing after too many tries. Wait a while, then start again.' : 'Rejected on the machine.';
        right = '';
        break;
      case 'expired': meta = 'The code expired. Start again.'; break;
      case 'mismatch': meta = 'The key did not match what this machine received. Do not approve it. Start again.'; break;
      default:
        meta = (code ? 'Waiting for approval… code <strong class="nowrap">' + esc(code) + '</strong>' : 'Waiting for the machine…');
        right = '<span class="spinner"></span>';
    }
    return '<li class="device"><span class="device__icon">' + icon('i-laptop') + '</span><span class="device__name">' + esc(m.name) + '</span><span class="device__meta">' + meta + '</span>' + right + '</li>';
  }

  function render(view) {
    var online = view.machines.filter(function (m) { return m.online; });
    var pending = online.filter(function (m) { return m.state !== 'approved' && m.state !== 'rejected' && m.state !== 'expired' && m.state !== 'mismatch'; });
    var approved = online.filter(function (m) { return m.state === 'approved'; });
    var left = Math.max(0, expires - Date.now());
    var big = '';
    if (pending.length === 1 && codes[pending[0].connectorId]) {
      big = '<div class="field"><span class="field__label">Check that your machine shows the same code</span><div class="code-display" aria-label="Pairing code">' + esc(codes[pending[0].connectorId]) + '</div></div>';
    } else if (pending.length > 1) {
      big = '<div class="field"><span class="field__label">Each machine shows its own code. Check it matches the one next to its name.</span></div>';
    }
    // The command to run on each machine, with that machine's own code inside it, so one click
    // copies the whole line. The machine still looks the pairing up by this code and shows the
    // key fingerprint and asks for a typed word, so pasting it does not skip any check: a pairing
    // whose key was swapped on the way has a different code and is not found.
    var commands = pending.filter(function (m) { return codes[m.connectorId]; }).map(function (m) {
      var digits = codes[m.connectorId].replace(/\D/g, '');
      return '<div class="field"><span class="field__label">' + (pending.length > 1 ? 'On ' + esc(m.name) + ', in a terminal you type in' : 'On the machine, in a terminal you type in') + '</span>' +
        window.MaestriaCmd.html('maestria devices approve ' + digits) + '</div>';
    }).join('');
    var html = big +
      '<dl class="kv"><dt>This device</dt><dd>' + esc(label) + '</dd><dt>Device key</dt><dd><code>' + esc(K.fingerprint(rec.id)) + '</code> (the machine shows the same fingerprint when you approve)</dd></dl>' +
      commands +
      '<ul class="card list">' + view.machines.map(row).join('') + '</ul>';
    if (!online.length) {
      html += '<div class="banner" data-status="info">' + icon('i-offline') + '<div class="banner__body"><p class="banner__title">No machine is online</p><p class="banner__text">Start the connector on a machine, then come back:</p>' + window.MaestriaCmd.html('maestria connect') + '</div></div>';
    }
    if (approved.length && !pending.length) {
      html += '<a class="btn btn--primary btn--block" href="/">Continue to Maestria</a>';
    }
    html += '<p class="auth-card__alt">' + (left > 0 ? 'The code expires in <span class="num" data-left></span>. ' : 'The code expired. <a href="/pair?new=0">Start again</a>. ') + '<a href="/">Cancel</a></p>';
    // The page is drawn again on every poll. Only touch it when something but the countdown
    // changed, so a Copy button keeps its "Copied" feedback and a selection is not lost.
    if (html !== lastHTML) { root.innerHTML = html; lastHTML = html; }
    var leftEl = root.querySelector('[data-left]');
    if (leftEl) leftEl.textContent = fmt(left);
  }

  async function start() {
    var force = /[?&]new=1/.test(location.search);
    try {
      rec = force ? await K.create() : await K.ensure();
    } catch (e) {
      root.innerHTML = '<div class="banner" data-status="danger">' + icon('i-alert') + '<div class="banner__body"><p class="banner__title">This browser cannot make a device key</p><p class="banner__text">' + esc(e.message) + '</p></div></div>';
      return;
    }
    nonce = K.randomBytes(16);
    var commit = await K.pairCommit(rec.spki, nonce);
    try {
      pairing = await cloud('POST', '/api/pair', { commit: commit, label: label });
    } catch (e) {
      root.innerHTML = '<div class="banner" data-status="danger">' + icon('i-alert') + '<div class="banner__body"><p class="banner__title">Could not start</p><p class="banner__text">' + esc(e.message === 'rate_limited' ? 'Too many tries. Wait a few minutes.' : e.message) + '</p></div></div>';
      return;
    }
    expires = pairing.expiresAt;
    render(pairing);
    poll();
  }

  async function poll() {
    var misses = 0;
    for (;;) {
      await new Promise(function (r) { setTimeout(r, 1000); });
      if (finished) return;
      var view;
      try { view = await cloud('GET', '/api/pair/' + pairing.id); misses = 0; }
      catch (e) { if (++misses > 5) { root.innerHTML = '<div class="banner" data-status="danger">' + icon('i-alert') + '<div class="banner__body"><p class="banner__title">The pairing ended</p><p class="banner__text"><a href="/pair?new=0">Start again</a></p></div></div>'; return; } continue; }
      for (var i = 0; i < view.machines.length; i++) {
        var m = view.machines[i];
        if (m.online && m.r && !revealed[m.connectorId] && m.state === 'pending') {
          revealed[m.connectorId] = true;
          // The machine committed to r before seeing our key; only now do we reveal it.
          codes[m.connectorId] = K.formatCode(await K.pairCode(rec.spki, nonce, K.fromB64u(m.r)));
          try { await cloud('POST', '/api/pair/' + pairing.id + '/reveal', { connectorId: m.connectorId, spki: K.toB64u(rec.spki), nonce: K.toB64u(nonce) }); }
          catch (e) { revealed[m.connectorId] = false; }
        } else if (m.r && codes[m.connectorId] === undefined && m.state === 'revealed') {
          codes[m.connectorId] = K.formatCode(await K.pairCode(rec.spki, nonce, K.fromB64u(m.r)));
        }
      }
      render(view);
      var online = view.machines.filter(function (m) { return m.online; });
      if (online.length && online.every(function (m) { return m.state === 'approved'; })) {
        finished = true;
        setTimeout(function () { location.href = '/'; }, 1200);
        return;
      }
    }
  }

  start();
})();
