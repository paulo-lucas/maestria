/*
 * Maestria hosted app. Vanilla JS, no inline handlers (strict CSP).
 *
 * The cloud only serves this shell and a few account pages. Everything about workspaces comes
 * from the user's machines: each request is signed here with a non-extractable device key and
 * relayed by the cloud, and each connector checks the signature before it answers. This file
 * builds the aggregated Overview from every online machine, shows the other pages from a
 * machine's own HTML views, and keeps them live with one signed event stream per machine.
 */
(function () {
  'use strict';

  var body = document.body;
  if (!body || body.dataset.app !== 'hosted') return;
  var K = window.MaestriaKeys;
  var csrf = body.dataset.csrf;
  var email = body.dataset.email || '';

  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var $$ = function (sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); };
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function icon(id, cls) { return '<svg class="icon' + (cls ? ' ' + cls : '') + '" aria-hidden="true"><use href="#' + id + '"/></svg>'; }
  function plural(n, one, many) { return n === 1 ? one : many; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // ---- state ----

  var state = {
    connectors: [],      // from the cloud: id, name, online, killSwitch, ...
    access: {},          // connector id -> { state: ok|pair|revoked|killed|disabled|offline|clock|error, reason }
    data: {},            // connector id -> { at, doc, workspaces: [...] } (parsed overview)
    streams: {},         // connector id -> AbortController
    key: null,
    loaded: false,
    route: null,
    busy: false
  };

  // ---- cloud (cookie session) ----

  async function cloud(method, path, payload) {
    var headers = { 'Accept': 'application/json', 'X-CSRF-Token': csrf };
    var opts = { method: method, credentials: 'same-origin', headers: headers };
    if (payload !== undefined) { headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(payload); }
    var res = await fetch(path, opts);
    if (res.status === 401) { location.href = '/login'; throw new Error('unauthenticated'); }
    var data = null;
    try { data = await res.json(); } catch (e) { /* no body */ }
    if (!res.ok) throw new Error((data && data.error) || 'Request failed (' + res.status + ')');
    return data;
  }

  // ---- relay (signed) ----

  function RelayError(code, status) { this.name = 'RelayError'; this.code = code; this.status = status; this.message = code; }
  RelayError.prototype = Object.create(Error.prototype);

  var ERROR_TEXT = {
    unknown_device: 'This browser is not approved on that machine yet.',
    approval_expired: 'This browser’s approval on that machine expired. Approve it again.',
    idle_expired: 'This browser was not used for a while, so that machine asks you to approve it again.',
    device_revoked: 'This browser was signed out of that machine.',
    kill_switch: 'The kill switch is on for that machine.',
    relay_disabled: 'Remote access is turned off on that machine.',
    stale_timestamp: 'This device’s clock is off by more than a minute. Fix the time and try again.',
    rate_limited: 'Too many requests. Wait a moment.',
    connector_offline: 'That machine is offline.',
    connector_busy: 'That machine is busy. Try again in a moment.',
    gateway_timeout: 'That machine did not answer in time.',
    route_forbidden: 'That machine does not allow this action from here.',
    unsigned: 'The request was not signed.',
    bad_signature: 'The machine did not accept this browser’s signature.'
  };

  function classify(code) {
    switch (code) {
      case 'unknown_device': case 'approval_expired': case 'idle_expired': return 'pair';
      case 'device_revoked': return 'revoked';
      case 'kill_switch': return 'killed';
      case 'relay_disabled': return 'disabled';
      case 'connector_offline': return 'offline';
      case 'stale_timestamp': return 'clock';
      default: return 'error';
    }
  }

  function noteAccess(cid, code) {
    if (!code) { state.access[cid] = { state: 'ok' }; return; }
    state.access[cid] = { state: classify(code), reason: code };
  }

  // rfetch signs and sends one request to a machine. It throws a RelayError for any refusal.
  async function rfetch(cid, method, path, o) {
    o = o || {};
    var rec = state.key || (state.key = await K.load());
    if (!rec) { noteAccess(cid, 'unknown_device'); throw new RelayError('unknown_device', 403); }
    var query = o.query || '';
    var bytes = new Uint8Array(0);
    if (o.body !== undefined && o.body !== null) bytes = new TextEncoder().encode(typeof o.body === 'string' ? o.body : JSON.stringify(o.body));
    var headers = await K.sign(rec, cid, method, path, query, bytes);
    headers['Accept'] = o.accept || 'application/json';
    if (method === 'POST') { headers['Content-Type'] = 'application/json'; headers['X-CSRF-Token'] = csrf; }
    var res = await fetch('/c/' + cid + path + (query ? '?' + query : ''), {
      method: method, headers: headers, body: method === 'POST' ? bytes : undefined, credentials: 'same-origin', signal: o.signal
    });
    if (!res.ok) {
      var code = 'error';
      try { var j = await res.clone().json(); if (j && j.error) code = j.error; } catch (e) { /* not json */ }
      if (res.status === 401) { location.href = '/login'; throw new RelayError('unauthenticated', 401); }
      if (code === 'workspace_not_found' || res.status === 404) throw new RelayError(code === 'error' ? 'not_found' : code, res.status);
      noteAccess(cid, code);
      throw new RelayError(code, res.status);
    }
    if (!o.stream && !(state.access[cid] && state.access[cid].state === 'ok')) noteAccess(cid, null);
    return res;
  }

  async function rjson(cid, method, path, o) {
    var res = await rfetch(cid, method, path, o);
    return res.json();
  }

  async function rhtml(cid, path, query) {
    var res = await rfetch(cid, 'GET', path, { query: query, accept: 'text/html' });
    return res.text();
  }

  // ---- connectors ----

  function connector(cid) { return state.connectors.filter(function (c) { return c.id === cid; })[0]; }
  function onlineConnectors() { return state.connectors.filter(function (c) { return c.online; }); }
  function readyConnectors() { return onlineConnectors().filter(function (c) { return (state.access[c.id] || {}).state === 'ok' && state.data[c.id]; }); }

  function applyConnectors(list) {
    var prev = {};
    state.connectors.forEach(function (c) { prev[c.id] = c; });
    state.connectors = list;
    var ids = {};
    list.forEach(function (c) {
      ids[c.id] = true;
      if (!c.online) {
        // Live mirror: an offline machine's data is gone from every view.
        delete state.data[c.id];
        state.access[c.id] = { state: 'offline' };
        stopStream(c.id);
      } else if (c.killSwitch) {
        delete state.data[c.id];
        state.access[c.id] = { state: 'killed', reason: 'kill_switch' };
        stopStream(c.id);
      } else if (!prev[c.id] || !prev[c.id].online || prev[c.id].killSwitch || (state.access[c.id] || {}).state !== 'ok') {
        probe(c.id);
      }
    });
    Object.keys(state.data).forEach(function (cid) { if (!ids[cid]) { delete state.data[cid]; stopStream(cid); } });
    renderShell();
    softRender();
  }

  async function loadConnectors() {
    var r = await cloud('GET', '/api/connectors');
    applyConnectors(r.connectors || []);
  }

  // probe fetches a machine's overview page: one signed request that doubles as the access test.
  async function probe(cid) {
    var c = connector(cid);
    if (!c || !c.online || c.killSwitch) return;
    try {
      var html = await rhtml(cid, '/');
      var doc = new DOMParser().parseFromString(html, 'text/html');
      state.data[cid] = { at: Date.now(), doc: doc, workspaces: parseWorkspaces(doc, cid) };
      noteAccess(cid, null);
      startStream(cid);
    } catch (e) {
      delete state.data[cid];
      if (!(e instanceof RelayError)) state.access[cid] = { state: 'error', reason: String(e.message || e) };
      stopStream(cid);
    }
    renderShell();
    softRender();
  }

  function parseWorkspaces(doc, cid) {
    return $$('.ws-strip .ws-card', doc).map(function (a) {
      var m = (a.getAttribute('href') || '').match(/^\/workspaces\/([^/]+)\//);
      var stats = $$('.ws-card__stats > span', a);
      var num = function (el) { var s = el && $('strong', el); return s ? parseInt(s.textContent, 10) || 0 : 0; };
      return {
        cid: cid, id: m ? decodeURIComponent(m[1]) : '', name: ($('.ws-card__name', a) || {}).textContent || '',
        tone: a.getAttribute('data-ws') || 'blue', needs: num(stats[0]), updates: num(stats[1])
      };
    }).filter(function (w) { return w.id; });
  }

  function allWorkspaces() {
    var out = [];
    readyConnectors().forEach(function (c) { state.data[c.id].workspaces.forEach(function (w) { out.push(Object.assign({ machine: c.name }, w)); }); });
    return out;
  }

  // ---- live streams (one signed SSE per machine) ----

  var refreshTimer = null;
  function scheduleRefresh(cid) {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(function () { refreshData(cid); }, 200);
  }

  async function refreshData(cid) {
    var cur = state.route;
    var targets = readyConnectors().map(function (c) { return c.id; });
    if (cid && targets.indexOf(cid) < 0) return;
    await Promise.all(targets.filter(function (id) { return !cid || id === cid; }).map(async function (id) {
      try {
        var html = await rhtml(id, '/');
        var doc = new DOMParser().parseFromString(html, 'text/html');
        state.data[id] = { at: Date.now(), doc: doc, workspaces: parseWorkspaces(doc, id) };
      } catch (e) { /* the next probe or event retries */ }
    }));
    renderShell();
    if (state.route === cur && window.MaestriaPanel) window.MaestriaPanel.refresh();
  }

  function stopStream(cid) {
    var s = state.streams[cid];
    if (s) { s.abort(); delete state.streams[cid]; }
  }

  async function startStream(cid) {
    if (state.streams[cid]) return;
    var ctl = new AbortController();
    state.streams[cid] = ctl;
    var delay = 1000;
    while (!ctl.signal.aborted) {
      try {
        var res = await rfetch(cid, 'GET', '/api/stream', { signal: ctl.signal, accept: 'text/event-stream', stream: true });
        delay = 1000;
        var reader = res.body.getReader();
        var dec = new TextDecoder();
        var buf = '';
        for (;;) {
          var chunk = await reader.read();
          if (chunk.done) break;
          buf += dec.decode(chunk.value, { stream: true });
          var idx;
          while ((idx = buf.indexOf('\n\n')) >= 0) {
            var ev = buf.slice(0, idx);
            buf = buf.slice(idx + 2);
            if (/^event: update/m.test(ev)) scheduleRefresh(cid);
          }
        }
      } catch (e) {
        if (ctl.signal.aborted) return;
        if (e instanceof RelayError && ['pair', 'revoked', 'killed', 'disabled', 'offline', 'clock'].indexOf(classify(e.code)) >= 0) {
          delete state.streams[cid];
          delete state.data[cid];
          renderShell();
          softRender();
          return;
        }
      }
      if (ctl.signal.aborted) return;
      await sleep(delay);
      delay = Math.min(delay * 2, 15000);
    }
  }

  // ---- routing ----

  function parseRoute(url) {
    var u = new URL(url, location.origin);
    var p = u.pathname;
    var m;
    if (p === '/') return { name: 'overview' };
    if (p === '/connectors') return { name: 'connectors', server: true };
    if (p === '/connectors/link') return { name: 'link', server: true };
    if (p === '/security') return { name: 'security' };
    if (p === '/usage') return { name: 'usage' };
    if ((m = p.match(/^\/c\/(cn_[0-9a-f]{32})\/usage$/))) return { name: 'usage', cid: m[1] };
    if ((m = p.match(/^\/c\/(cn_[0-9a-f]{32})\/workspaces\/([^/]+)\/(tasks|chat|activity|floors|docs)$/))) {
      return { name: 'workspace', cid: m[1], ws: decodeURIComponent(m[2]), tab: m[3], query: u.search.slice(1) };
    }
    return { name: 'overview' };
  }

  function wsHref(cid, ws, tab) { return '/c/' + cid + '/workspaces/' + encodeURIComponent(ws) + '/' + tab; }

  function setActive() {
    var r = state.route || {};
    var nav = r.name === 'workspace' ? 'workspace' : r.name === 'link' ? 'connectors' : r.name;
    $$('[data-nav]').forEach(function (a) {
      if (a.getAttribute('data-nav') === nav) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
  }

  function navigate(url, push) {
    var next = parseRoute(url);
    if (push) history.pushState({}, '', url);
    state.route = next;
    setActive();
    renderShell();
    renderCurrent(true);
    window.scrollTo(0, 0);
  }

  document.addEventListener('click', function (ev) {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    var a = ev.target.closest('a[href]');
    if (!a || a.target || a.hasAttribute('download')) return;
    var href = a.getAttribute('href');
    if (!href || href[0] !== '/' || href.indexOf('//') === 0) return;
    if (/^\/(static|api|logout|login|signup|reset|verify|pair)/.test(href)) return;
    ev.preventDefault();
    var menu = a.closest('details.menu');
    if (menu) menu.removeAttribute('open');
    navigate(href, true);
  });

  window.addEventListener('popstate', function () { navigate(location.pathname + location.search, false); });

  // ---- shell ----

  function relTime(ms) {
    var d = Date.now() - ms;
    if (!ms || d < 0) return '';
    if (d < 3600e3) return Math.max(1, Math.round(d / 60e3)) + ' min';
    if (d < 48 * 3600e3) return Math.round(d / 3600e3) + ' hours';
    return Math.round(d / 86400e3) + ' days';
  }

  function renderShell() {
    var wss = allWorkspaces();
    var needs = wss.reduce(function (n, w) { return n + w.needs; }, 0);
    var r = state.route || {};
    var cur = r.name === 'workspace' ? wss.filter(function (w) { return w.cid === r.cid && w.id === r.ws; })[0] : null;

    var navNeeds = $('#nav-needs');
    if (navNeeds) { navNeeds.textContent = needs; navNeeds.hidden = !needs; }

    var html = '';
    if (!wss.length) {
      html = '<p class="nav__empty">' + (onlineConnectors().length ? 'No workspaces yet' : 'No machine online') + '</p>';
    }
    wss.forEach(function (w) {
      var href = wsHref(w.cid, w.id, 'tasks');
      var active = cur && cur.cid === w.cid && cur.id === w.id;
      html += '<a class="nav__item" href="' + esc(href) + '" data-ws="' + esc(w.tone) + '"><span class="swatch"></span> ' + esc(w.name) + (w.needs ? ' <span class="count">' + w.needs + '</span>' : '') + '</a>';
      if (active) {
        var tab = function (t, ic, label, extra) {
          return '<a class="nav__item" href="' + esc(wsHref(w.cid, w.id, t)) + '"' + (r.tab === t ? ' aria-current="page"' : '') + '>' + icon(ic) + ' ' + label + (extra || '') + '</a>';
        };
        html += '<div class="nav__sub">' + tab('activity', 'i-activity', 'Activity') +
          tab('tasks', 'i-inbox', 'Human tasks', w.needs ? ' <span class="count" data-status="needs">' + w.needs + '</span>' : '') +
          tab('chat', 'i-chat', 'Chat') + tab('floors', 'i-floors', 'Floors') + tab('docs', 'i-docs', 'Docs') + '</div>';
      }
    });
    var holder = $('#nav-workspaces');
    if (holder) holder.innerHTML = html;

    // Scope menu (small screens).
    var label = $('#scope-label');
    if (label) label.textContent = cur ? cur.name : 'All workspaces';
    var panel = $('#scope-panel');
    if (panel) {
      var ph = '<a class="menu__item" href="/"' + (!cur ? ' aria-current="page"' : '') + '>' + icon('i-overview') + ' All workspaces' + (needs ? ' <span class="count" data-status="needs">' + needs + '</span>' : '') + '</a>';
      if (wss.length) ph += '<div class="menu__sep"></div>';
      wss.forEach(function (w) {
        ph += '<a class="menu__item" href="' + esc(wsHref(w.cid, w.id, 'tasks')) + '" data-ws="' + esc(w.tone) + '"' + (cur && cur.cid === w.cid && cur.id === w.id ? ' aria-current="page"' : '') + '><span class="swatch swatch--lg"></span> ' + esc(w.name) + (w.needs ? ' <span class="count">' + w.needs + '</span>' : '') + '</a>';
      });
      panel.innerHTML = ph;
    }

    // Bottom nav: the workspace's tabs inside a workspace, the main sections elsewhere.
    var bn = $('#bottomnav');
    if (bn) {
      if (cur) {
        var bt = function (t, ic, l, c) { return '<a class="bottomnav__item" href="' + esc(wsHref(cur.cid, cur.id, t)) + '"' + (r.tab === t ? ' aria-current="page"' : '') + '>' + icon(ic) + l + (c || '') + '</a>'; };
        bn.innerHTML = bt('activity', 'i-activity', 'Activity') + bt('tasks', 'i-inbox', 'Tasks', cur.needs ? ' <span class="count" data-status="needs">' + cur.needs + '</span>' : '') + bt('chat', 'i-chat', 'Chat') + bt('floors', 'i-floors', 'Floors') + bt('docs', 'i-docs', 'Docs');
      } else {
        var cur2 = function (n) { return r.name === n || (n === 'connectors' && r.name === 'link') ? ' aria-current="page"' : ''; };
        bn.innerHTML = '<a class="bottomnav__item" href="/"' + cur2('overview') + '>' + icon('i-overview') + 'Overview' + (needs ? ' <span class="count" data-status="needs">' + needs + '</span>' : '') + '</a>' +
          '<a class="bottomnav__item" href="/usage"' + cur2('usage') + '>' + icon('i-gauge') + 'Usage</a>' +
          '<a class="bottomnav__item" href="/connectors"' + cur2('connectors') + '>' + icon('i-plug') + 'Connectors</a>' +
          '<a class="bottomnav__item" href="/security"' + cur2('security') + '>' + icon('i-shield') + 'Security</a>';
      }
    }

    // Machines card and dot.
    var online = onlineConnectors();
    var offline = state.connectors.filter(function (c) { return !c.online; });
    var name = $('#conn-card-name'), meta = $('#conn-card-meta'), dot = $('#conn-card-dot'), cdot = $('#conn-dot');
    var txt = !online.length ? 'No machine online' : online.length + ' ' + plural(online.length, 'machine', 'machines') + ' online';
    if (name) name.textContent = txt;
    if (meta) {
      meta.textContent = offline.length ? offline[0].name + ' offline' + (offline[0].lastSeenAt ? ' · ' + relTime(offline[0].lastSeenAt) : '') + (offline.length > 1 ? ' · +' + (offline.length - 1) : '') : (state.connectors.length ? '' : 'Link your first machine');
    }
    var needPair = online.filter(function (c) { return ['pair', 'revoked'].indexOf((state.access[c.id] || {}).state) >= 0; });
    var tone = !online.length ? 'offline' : (needPair.length || online.some(function (c) { return c.killSwitch; })) ? 'warn' : 'ok';
    if (dot) dot.setAttribute('data-status', tone);
    if (cdot) { cdot.setAttribute('aria-label', 'Machines: ' + txt); cdot.setAttribute('title', 'Machines: ' + txt); }
    var tabTitle = ({ tasks: 'Human tasks', chat: 'Chat', activity: 'Activity', floors: 'Floors', docs: 'Docs' })[r.tab];
    document.title = (cur ? cur.name + ' · ' + (tabTitle || 'Workspace') : ({ overview: 'Overview', security: 'Devices & security', usage: 'Usage & standby', connectors: 'Connectors', link: 'Link a machine' })[r.name] || 'Maestria') + ' · Maestria';
  }

  // ---- page content ----

  var main = function () { return $('#main'); };

  function setMain(html) {
    var m = main();
    m.innerHTML = html;
    afterRender(m);
  }

  function afterRender(root) {
    if (window.MaestriaPanel) window.MaestriaPanel.applyFilters(root);
    $$('time[datetime]', root).forEach(function () { /* times are already relative text */ });
  }

  function banner(tone, ic, title, text, extra) {
    return '<div class="banner" data-status="' + tone + '" role="status">' + icon(ic) + '<div class="banner__body"><p class="banner__title">' + esc(title) + '</p>' +
      (text ? '<p class="banner__text">' + text + '</p>' : '') + '</div>' + (extra ? '<div class="banner__actions">' + extra + '</div>' : '') + '</div>';
  }

  function machineList(list) {
    return '<ul class="list machine-list">' + list.map(function (c) {
      return '<li class="device"><span class="device__icon">' + icon('i-laptop') + '</span><span class="device__name">' + esc(c.name) + ' <span class="status" data-status="' + (c.online ? 'ok' : 'offline') + '">' + (c.online ? 'Online' : 'Offline') + '</span></span>' +
        '<span class="device__meta">' + (c.online ? 'Connected' : (c.lastSeenAt ? 'Last seen ' + new Date(c.lastSeenAt).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : 'Never connected')) + '</span></li>';
    }).join('') + '</ul>';
  }

  function startConnectorCard() {
    return '<section class="section" aria-labelledby="start-title"><h2 class="section__title" id="start-title">Start the connector</h2><ol class="card checklist">' +
      '<li><span class="checklist__title">Wake the machine</span><span class="checklist__text">A sleeping or disconnected machine can’t reach Maestria. Check its network.</span></li>' +
      '<li><span class="checklist__title">Run the connector</span><span class="checklist__text">In a terminal on that machine:</span><div class="code-inline"><code>maestria connect</code><button class="btn btn--ghost btn--icon btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">' + icon('i-copy') + '</button></div></li>' +
      '<li><span class="checklist__title">Keep it running</span><span class="checklist__text">Install it as a service so it starts with your session:</span><div class="code-inline"><code>maestria service install</code><button class="btn btn--ghost btn--icon btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">' + icon('i-copy') + '</button></div></li>' +
      '</ol><p class="field__hint">Manage or revoke machines in <a href="/connectors">Connectors</a>.</p></section>';
  }

  function stateOnboarding(step) {
    var linked = state.connectors.length > 0;
    var online = onlineConnectors();
    return '<div class="page page--narrow"><header class="page-head"><div class="page-head__title"><h1>Welcome to Maestria</h1><p class="page-head__sub">' + (linked ? 'One more step and your Maestros show up here.' : 'Two more steps and your Maestros show up here.') + '</p></div></header>' +
      '<ol class="card checklist"><li class="is-done"><span class="checklist__title">Create your account</span><span class="checklist__text">Password and two-factor are set.</span></li>' +
      '<li class="' + (linked ? 'is-done' : '') + '"' + (linked ? '' : ' aria-current="step"') + '><span class="checklist__title">Link a machine</span><span class="checklist__text">' + (linked ? '<span class="status" data-status="ok">' + esc(online[0] ? online[0].name + ' is online' : 'Linked') + '</span> · <a href="/connectors/link">Link another</a>' : 'The connector runs next to Maestri and opens an outbound connection here.') + '</span>' + (linked ? '' : '<div class="btn-row"><a class="btn btn--primary btn--sm" href="/connectors/link">Link a machine</a></div>') + '</li>' +
      '<li' + (linked ? ' aria-current="step"' : '') + '><span class="checklist__title">Add a workspace</span><span class="checklist__text">In Maestri, open a workspace and run this in its folder. It installs the Maestria skill, so the workspace’s Maestro can post tasks and chat here.</span><div class="code-inline"><code>maestria init</code><button class="btn btn--ghost btn--icon btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">' + icon('i-copy') + '</button></div>' + (linked ? '<p class="status status--quiet"><span class="spinner"></span>Looking for workspaces on ' + esc(online[0] ? online[0].name : 'your machine') + '…</p>' : '') + '</li></ol>' +
      banner('info', 'i-lock', 'What leaves your machine', 'To show them here, the connector sends chats, project docs, agent briefs and the last lines of agent terminals. They pass through Maestria encrypted and are never stored there. Code, secrets and Maestri tokens never leave the machine. Prefer to keep everything local? Run <code>maestria serve</code> instead.') + '</div>';
  }

  function stateNoMachine() {
    return '<div class="page page--narrow"><header class="page-head"><div class="page-head__title"><h1>Overview</h1></div></header>' +
      '<div class="card"><div class="empty"><span class="empty__icon">' + icon('i-offline', 'icon--lg') + '</span><p class="empty__title">No machine online</p>' +
      '<p class="empty__text">Maestria mirrors Maestri live from your machines, so there is nothing to show until one of them is back. Workspaces, tasks and chats reappear as soon as a connector reconnects.</p></div>' + machineList(state.connectors) + '</div>' + startConnectorCard() + '</div>';
  }

  function stateNeedsApproval(list) {
    return '<div class="page page--narrow"><header class="page-head"><div class="page-head__title"><h1>Overview</h1></div></header>' +
      '<div class="card"><div class="empty"><span class="empty__icon">' + icon('i-lock', 'icon--lg') + '</span><p class="empty__title">Approve this browser</p>' +
      '<p class="empty__text">Your machines only accept commands from browsers you approved on the machine itself. This happens once per browser and machine.</p>' +
      '<a class="btn btn--primary btn--sm" href="/pair?new=' + (list.some(function (c) { return (state.access[c.id] || {}).state === 'revoked'; }) ? '1' : '0') + '">Approve this browser</a></div>' +
      '<ul class="list machine-list">' + list.map(function (c) {
        var a = state.access[c.id] || {};
        return '<li class="device"><span class="device__icon">' + icon('i-laptop') + '</span><span class="device__name">' + esc(c.name) + ' <span class="status" data-status="' + (a.state === 'killed' ? 'danger' : 'warn') + '">' + (a.state === 'killed' ? 'Kill switch on' : 'Needs approval') + '</span></span><span class="device__meta">' + esc(ERROR_TEXT[a.reason] || '') + '</span></li>';
      }).join('') + '</ul></div></div>';
  }

  function stateMachineGone(c) {
    return '<div class="page page--narrow"><div class="card"><div class="empty"><span class="empty__icon">' + icon('i-offline', 'icon--lg') + '</span><p class="empty__title">' + esc(c ? c.name : 'This machine') + ' went offline</p>' +
      '<p class="empty__text">This workspace runs on ' + esc(c ? c.name : 'a machine that is offline') + ', so it is hidden until that machine reconnects. Nothing you send is kept for later. Your other workspaces are still live.</p>' +
      '<div class="btn-row"><a class="btn btn--primary btn--sm" href="/">Go to overview</a><a class="btn btn--sm" href="/connectors">Connectors</a></div></div></div></div>';
  }

  function stateKilled(names) {
    return banner('danger', 'i-power', 'Kill switch is on', esc(names.join(', ')) + ' ' + plural(names.length, 'rejects', 'reject') + ' every command from this site. Turn it off on ' + plural(names.length, 'that machine', 'each machine') + ' with <code>maestria tunnel on</code>.');
  }

  // ---- aggregated overview ----

  function uniquify(node, prefix) {
    $$('[id]', node).forEach(function (el) { el.id = prefix + el.id; });
    $$('[for]', node).forEach(function (el) { el.setAttribute('for', prefix + el.getAttribute('for')); });
    $$('[aria-describedby]', node).forEach(function (el) { el.setAttribute('aria-describedby', prefix + el.getAttribute('aria-describedby')); });
    $$('[data-draft-key]', node).forEach(function (el) { el.setAttribute('data-draft-key', prefix + el.getAttribute('data-draft-key')); });
  }

  function rewriteLinks(node, cid) {
    $$('a[href^="/workspaces/"]', node).forEach(function (a) { a.setAttribute('href', '/c/' + cid + a.getAttribute('href')); });
    $$('a[href="/usage"]', node).forEach(function (a) { a.setAttribute('href', '/c/' + cid + '/usage'); });
    $$('a[href="/security"]', node).forEach(function (a) { a.setAttribute('href', '/security'); });
  }

  function timeOf(li) { var t = $('time[datetime]', li); return t ? Date.parse(t.getAttribute('datetime')) || 0 : 0; }

  function renderOverview() {
    var ready = readyConnectors();
    var wss = allWorkspaces();
    var needsTotal = wss.reduce(function (n, w) { return n + w.needs; }, 0);
    var banners = '';
    var killed = state.connectors.filter(function (c) { return c.online && c.killSwitch; }).map(function (c) { return c.name; });
    if (killed.length) banners += stateKilled(killed);
    var unapproved = onlineConnectors().filter(function (c) { return ['pair', 'revoked'].indexOf((state.access[c.id] || {}).state) >= 0; });
    if (unapproved.length) {
      banners += banner('info', 'i-lock', 'Approve this browser on ' + unapproved.map(function (c) { return c.name; }).join(', '),
        'Its workspaces appear here once you approve this browser on that machine.', '<a class="btn btn--sm" href="/pair?new=0">Approve</a>');
    }
    var clockIssue = onlineConnectors().filter(function (c) { return (state.access[c.id] || {}).state === 'clock'; });
    if (clockIssue.length) banners += banner('warn', 'i-clock', 'Check this device’s clock', esc(ERROR_TEXT.stale_timestamp));

    var machines = state.connectors.length;
    var sub = wss.length + ' ' + plural(wss.length, 'workspace', 'workspaces') + ' on ' + ready.length + ' ' + plural(ready.length, 'machine', 'machines') + ' · ' + (needsTotal ? needsTotal + ' waiting on you' : 'nothing waiting on you');

    // Workspaces strip.
    var strip = '';
    if (wss.length) {
      strip = '<div class="ws-strip">' + wss.map(function (w) {
        return '<a class="ws-card" href="' + esc(wsHref(w.cid, w.id, 'tasks')) + '" data-ws="' + esc(w.tone) + '"><div class="ws-card__head"><span class="ws-card__name">' + esc(w.name) + '</span><span class="status status--quiet" data-status="ok">Online</span></div>' +
          '<div class="ws-card__stats"><span' + (w.needs ? ' data-status="needs"' : ' class="is-zero"') + '><strong>' + w.needs + '</strong>' + plural(w.needs, 'needs you', 'need you') + '</span><span' + (w.updates ? '' : ' class="is-zero"') + '><strong>' + w.updates + '</strong>' + plural(w.updates, 'update', 'updates') + '</span></div>' +
          '<div class="ws-card__meta">' + esc(w.machine) + '</div></a>';
      }).join('') + '</div>';
    } else {
      strip = '<div class="card empty empty--compact"><span class="empty__icon">' + icon('i-folder') + '</span><p class="empty__title">No workspaces registered yet</p><p class="empty__text">Run <code>maestria init</code> inside a Maestri workspace on a linked machine.</p></div>';
    }

    // Needs you: the connectors' own task markup, merged and sorted.
    var tasks = [];
    ready.forEach(function (c) {
      $$('#needs-list > li.task', state.data[c.id].doc).forEach(function (li) {
        var clone = li.cloneNode(true);
        var ws = li.getAttribute('data-ws-id') || '';
        var comp = c.id + ':' + ws;
        clone.setAttribute('data-ws-id', comp);
        clone.setAttribute('data-key', comp);
        clone.setAttribute('data-cid', c.id);
        uniquify(clone, c.id.slice(3, 11) + '-');
        rewriteLinks(clone, c.id);
        tasks.push({ at: timeOf(li), html: clone.outerHTML });
      });
    });
    tasks.sort(function (a, b) { return b.at - a.at; });
    var chips = wss.filter(function (w) { return w.needs; }).map(function (w) {
      return '<button class="chip" type="button" aria-pressed="false" data-value="' + esc(w.cid + ':' + w.id) + '" data-ws="' + esc(w.tone) + '"><span class="swatch"></span>' + esc(w.name) + ' <span class="count">' + w.needs + '</span></button>';
    }).join('');
    var needsPanel = '<section class="card panel" aria-labelledby="needs-title"><header class="panel__head"><h2 class="panel__title" id="needs-title">' + icon('i-inbox') + ' Needs you <span class="count" data-status="needs">' + needsTotal + '</span></h2><div class="panel__aside">Newest first</div></header>';
    if (tasks.length) {
      needsPanel += '<div class="panel__tools"><div class="chip-row" data-filter="needs-list" role="group" aria-label="Filter by workspace"><button class="chip" type="button" aria-pressed="true" data-value="all">All <span class="count">' + needsTotal + '</span></button>' + chips + '</div></div>' +
        '<ul class="list" id="needs-list">' + tasks.map(function (t) { return t.html; }).join('') + '</ul>';
    } else {
      needsPanel += '<div class="empty empty--compact"><span class="empty__icon">' + icon('i-check') + '</span><p class="empty__title">Nothing needs you right now</p><p class="empty__text">Approvals, choices, and questions from your Maestros will appear here live.</p></div>';
    }
    needsPanel += '</section>';

    // Agents at work.
    var agents = [], agentNeeds = 0, wireEmpty = null;
    ready.forEach(function (c) {
      var sec = $('section[aria-labelledby="agents-title"]', state.data[c.id].doc);
      if (!sec) return;
      var items = $$('ul.list > li', sec);
      items.forEach(function (li) {
        var clone = li.cloneNode(true);
        rewriteLinks(clone, c.id);
        agents.push(clone.outerHTML);
      });
      var w = $('.status[data-status="needs"]', sec);
      if (w) agentNeeds += parseInt(w.textContent, 10) || 0;
      if (!items.length && !wireEmpty) { var em = $('.empty', sec); if (em) wireEmpty = em.outerHTML; }
    });
    var agentsPanel = '<section class="card panel span-all" aria-labelledby="agents-title"><header class="panel__head"><h2 class="panel__title" id="agents-title">' + icon('i-activity') + ' Agents at work' + (agents.length ? ' <span class="count">' + agents.length + '</span>' : '') + '</h2>' + (agentNeeds ? '<div class="panel__aside"><span class="status" data-status="needs">' + agentNeeds + ' waiting</span></div>' : '') + '</header>' +
      (agents.length ? '<ul class="list">' + agents.join('') + '</ul>' : (wireEmpty || '<div class="empty empty--compact"><p class="empty__title">No agents at work</p><p class="empty__text">Agents that are working or need you will appear here.</p></div>')) + '</section>';

    // Usage grouped by provider account across machines.
    var providers = {}, order = [];
    ready.forEach(function (c) {
      $$('section[aria-labelledby="usage-title"] .usage-provider', state.data[c.id].doc).forEach(function (blk) {
        var name = ($('.usage-provider__name', blk) || {}).textContent || '';
        var acct = ($('.usage-provider__head .subtle', blk) || {}).textContent || '';
        if (!providers[name]) { providers[name] = {}; order.push(name); }
        if (!providers[name][acct]) providers[name][acct] = { blk: blk, machines: [] };
        providers[name][acct].machines.push(c.name);
      });
    });
    var usageBody = '';
    order.forEach(function (name) {
      var accts = Object.keys(providers[name]);
      accts.forEach(function (a) {
        var g = providers[name][a];
        var clone = g.blk.cloneNode(true);
        if (accts.length > 1) {
          // Different accounts for the same provider: say which machine each one is on.
          var head = $('.usage-provider__head', clone);
          var tag = document.createElement('span');
          tag.className = 'subtle';
          tag.textContent = g.machines.join(', ');
          if (head) { var sub = $('.subtle', head); if (sub) sub.textContent = (a ? a + ' · ' : '') + g.machines.join(', '); else head.appendChild(tag); }
        }
        usageBody += clone.outerHTML;
      });
    });
    var usageIssue = '';
    if (!usageBody) {
      var issue = null;
      ready.forEach(function (c) { var e = $('section[aria-labelledby="usage-title"] .empty', state.data[c.id].doc); if (e && !issue) issue = e.outerHTML; });
      usageIssue = issue || '<div class="empty empty--compact"><p class="empty__title">Usage data unavailable</p><p class="empty__text">Check Maestri’s usage directory in Settings.</p></div>';
    }
    var firstReady = ready[0];
    var usagePanel = '<section class="card panel" aria-labelledby="usage-title"><header class="panel__head"><h2 class="panel__title" id="usage-title">' + icon('i-gauge') + ' Usage</h2><div class="panel__aside"><a href="/usage">Standby settings</a></div></header>' +
      (usageBody ? '<div class="panel__body usage-list">' + usageBody + '</div>' : usageIssue) + '</section>';

    // Active floors.
    var floors = [];
    ready.forEach(function (c) {
      $$('section[aria-labelledby="floors-title"] ul.list > li', state.data[c.id].doc).forEach(function (li) { floors.push(li.outerHTML); });
    });
    var floorsPanel = '<section class="card panel" aria-labelledby="floors-title"><header class="panel__head"><h2 class="panel__title" id="floors-title">' + icon('i-floors') + ' Active floors</h2></header>' +
      (floors.length ? '<ul class="list">' + floors.join('') + '</ul>' : '<div class="empty empty--compact"><p class="empty__title">No active floors</p><p class="empty__text">Floors started by your Maestros will appear here.</p></div>') + '</section>';

    return '<div class="page" data-live-root="overview"><header class="page-head"><div class="page-head__title"><h1>Overview</h1><p class="page-head__sub">' + esc(sub) + ' · updated <time datetime="' + new Date().toISOString() + '">' + new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + '</time></p></div></header>' +
      banners + '<section class="section" aria-labelledby="ws-title"><h2 class="sr-only" id="ws-title">Workspaces</h2>' + strip + '</section>' +
      '<div class="overview-cols">' + needsPanel + '<div class="overview-side">' + agentsPanel + usagePanel + floorsPanel + '</div></div></div>' + (firstReady ? '' : '');
  }

  // ---- workspace and usage views (a machine's own HTML) ----

  async function fetchMachineView(cid, path, query) {
    var html = await rhtml(cid, path, query);
    var doc = new DOMParser().parseFromString(html, 'text/html');
    var m = $('main#main', doc);
    if (!m) return '<div class="page page--narrow"><div class="card"><div class="empty"><p class="empty__title">Nothing to show</p></div></div></div>';
    rewriteLinks(m, cid);
    // Forms and "Kill switch" dialogs of the local shell do not belong here.
    $$('dialog', m).forEach(function (d) { d.remove(); });
    var cn = connector(cid);
    $$('.ws-eyebrow .subtle', m).forEach(function (el) { el.textContent = '\u00b7 ' + (cn ? cn.name : 'machine'); });
    await hydrateImages(m, cid);
    return m.innerHTML;
  }

  async function hydrateImages(root, cid) {
    var imgs = $$('img[src*="/docs/asset"]', root);
    await Promise.all(imgs.map(async function (img) {
      try {
        var u = new URL(img.getAttribute('src'), location.origin);
        var res = await rfetch(cid, 'GET', u.pathname, { query: u.search.slice(1), accept: 'image/*' });
        var blob = await res.blob();
        var data = await new Promise(function (resolve, reject) { var fr = new FileReader(); fr.onload = function () { resolve(fr.result); }; fr.onerror = reject; fr.readAsDataURL(blob); });
        img.setAttribute('src', data);
      } catch (e) { img.removeAttribute('src'); }
    }));
  }

  function machineBanner(cid) {
    var c = connector(cid), a = state.access[cid] || {};
    if (!c || !c.online) return stateMachineGone(c);
    if (a.state === 'killed' || c.killSwitch) return '<div class="page page--narrow">' + stateKilled([c.name]) + '</div>';
    if (a.state === 'pair' || a.state === 'revoked') return stateNeedsApproval([c]);
    if (a.state === 'clock') return '<div class="page page--narrow">' + banner('warn', 'i-clock', 'Check this device’s clock', esc(ERROR_TEXT.stale_timestamp)) + '</div>';
    return null;
  }

  async function workspaceHtml(r) {
    var gone = machineBanner(r.cid);
    if (gone) return gone;
    try {
      var path = '/workspaces/' + encodeURIComponent(r.ws) + '/' + r.tab;
      var html = await fetchMachineView(r.cid, path, r.query || '');
      return html;
    } catch (e) {
      if (e instanceof RelayError && e.status === 404) return '<div class="page page--narrow"><div class="card"><div class="empty"><span class="empty__icon">' + icon('i-folder', 'icon--lg') + '</span><p class="empty__title">Workspace not found</p><p class="empty__text">It may have been removed on that machine.</p><a class="btn btn--primary btn--sm" href="/">Go to overview</a></div></div></div>';
      return machineBanner(r.cid) || errorCard(e);
    }
  }

  function errorCard(e) {
    var code = e && e.code;
    return '<div class="page page--narrow"><div class="card"><div class="empty"><span class="empty__icon">' + icon('i-alert', 'icon--lg') + '</span><p class="empty__title">Could not load this page</p><p class="empty__text">' + esc(ERROR_TEXT[code] || (e && e.message) || 'Something went wrong.') + '</p><a class="btn btn--sm" href="/">Go to overview</a></div></div></div>';
  }

  async function usageHtml(r) {
    var ready = onlineConnectors();
    var cid = r.cid || (ready[0] && ready[0].id);
    if (!cid) return stateNoMachine();
    var switcher = '';
    if (ready.length > 1) {
      switcher = '<div class="chip-row" role="group" aria-label="Machine" style="">' + '</div>';
      switcher = '<nav class="chip-row" aria-label="Machine">' + ready.map(function (c) {
        return '<a class="chip" href="/c/' + c.id + '/usage" aria-pressed="' + (c.id === cid ? 'true' : 'false') + '">' + esc(c.name) + '</a>';
      }).join('') + '</nav>';
    }
    var gone = machineBanner(cid);
    if (gone) return gone;
    try {
      var inner = await fetchMachineView(cid, '/usage', '');
      return switcher ? '<div class="page">' + switcher + '</div>' + inner : inner;
    } catch (e) { return machineBanner(cid) || errorCard(e); }
  }

  // ---- security page ----

  function describeDevice(label) { return label || 'Browser'; }

  async function securityHtml() {
    var ready = readyConnectors();
    var infos = {}, policies = {}, errors = [];
    await Promise.all(ready.map(async function (c) {
      try {
        var d = await rjson(c.id, 'GET', '/api/devices');
        var i = await rjson(c.id, 'GET', '/api/relay/info');
        infos[c.id] = d; policies[c.id] = i;
      } catch (e) { errors.push(c); }
    }));
    var me = state.key ? state.key.id : (await K.load() || {}).id;
    var byDev = {};
    ready.forEach(function (c) {
      var d = infos[c.id];
      if (!d) return;
      (d.devices || []).forEach(function (dev) {
        var g = byDev[dev.id] || (byDev[dev.id] = { id: dev.id, label: dev.label, machines: [] });
        g.machines.push({ cid: c.id, name: c.name, state: dev.state, approvedAt: dev.approvedAt, lastUsedAt: dev.lastUsedAt });
      });
    });
    var groups = Object.keys(byDev).map(function (k) { return byDev[k]; }).filter(function (g) { return g.machines.some(function (m) { return m.state !== 'revoked'; }); });
    groups.sort(function (a, b) { return (a.id === me ? -1 : 0) - (b.id === me ? -1 : 0); });

    var firstPol = null;
    ready.forEach(function (c) { if (!firstPol && policies[c.id] && policies[c.id].policy) firstPol = { name: c.name, p: policies[c.id].policy }; });

    var rows = groups.map(function (g) {
      var active = g.machines.filter(function (m) { return m.state === 'active'; });
      var expired = !active.length;
      var pol = firstPol ? firstPol.p : null;
      var meta = g.machines.map(function (m) { return m.name; }).join(', ');
      var last = Math.max.apply(null, g.machines.map(function (m) { return m.lastUsedAt || 0; }));
      if (active.length && pol) {
        var oldest = Math.min.apply(null, active.map(function (m) { return m.approvedAt; }));
        var left = oldest + pol.approvalAbsoluteDays * 86400e3 - Date.now();
        meta += ' · ' + (g.id === me ? 'active now' : 'active ' + (relTime(last) || 'recently') + ' ago') + ' · approval ends in ' + Math.max(0, Math.round(left / 86400e3)) + ' days';
      } else if (expired) {
        meta += ' · last active ' + (last ? new Date(last).toLocaleDateString([], { month: 'short', day: 'numeric' }) : 'never');
      }
      return '<li class="device"><span class="device__icon">' + icon(g.id === me ? 'i-laptop' : 'i-device') + '</span><span class="device__name">' + esc(describeDevice(g.label)) +
        (g.id === me ? ' <span class="status status--badge" data-status="info">This device</span>' : '') + (expired ? ' <span class="status" data-status="offline">Session expired</span>' : '') + '</span>' +
        '<span class="device__meta">' + esc(meta) + '</span><button class="btn btn--sm" type="button" data-action="revoke-device" data-device="' + esc(g.id) + '">' + (g.id === me ? 'Sign out' : 'Revoke') + '</button></li>';
    }).join('');

    var needApproval = onlineConnectors().filter(function (c) { return ['pair', 'revoked'].indexOf((state.access[c.id] || {}).state) >= 0; });
    var offline = state.connectors.filter(function (c) { return !c.online; });

    var polHtml = '';
    if (firstPol) {
      var p = firstPol.p;
      var same = ready.filter(function (c) { var q = policies[c.id] && policies[c.id].policy; return q && JSON.stringify(q) === JSON.stringify(p); }).map(function (c) { return c.name; });
      var diff = ready.filter(function (c) { var q = policies[c.id] && policies[c.id].policy; return q && JSON.stringify(q) !== JSON.stringify(p); });
      polHtml = '<div class="card"><div class="setting"><dl class="kv"><dt>Approval length</dt><dd>' + p.approvalAbsoluteDays + ' ' + plural(p.approvalAbsoluteDays, 'day', 'days') + '</dd><dt>Idle sign-out</dt><dd>after ' + p.idleDays + ' ' + plural(p.idleDays, 'day', 'days') + '</dd>' +
        '<dt>Allowed commands</dt><dd>prompt the Maestro, answer tasks and Y/n, pause and resume, standby settings, revoke devices</dd><dt>Rate limit</dt><dd>' + p.ratePerMinute + ' requests per minute per device</dd></dl></div>' +
        '<div class="setting"><p class="setting__desc">These are set on each machine, not here, so a compromised website can’t loosen them. To change them, edit the <code>relay</code> section of <code>config.json</code> on the machine.</p>' +
        '<div class="code-inline"><code>maestria devices list</code><button class="btn btn--ghost btn--icon btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">' + icon('i-copy') + '</button></div>' +
        '<p class="field__hint">Showing ' + esc(firstPol.name) + '.' + (same.length > 1 ? ' ' + esc(same.filter(function (n) { return n !== firstPol.name; }).join(', ')) + ' ' + plural(same.length - 1, 'reports', 'report') + ' the same policy.' : '') + (diff.length ? ' ' + esc(diff.map(function (c) { return c.name; }).join(', ')) + ' ' + plural(diff.length, 'has', 'have') + ' a different policy.' : '') + '</p></div></div>';
    } else {
      polHtml = '<div class="card"><div class="setting"><p class="setting__desc">No machine is reachable with this browser yet, so there is no policy to show.</p></div></div>';
    }

    var killed = state.connectors.filter(function (c) { return c.online && c.killSwitch; });
    var killBlock = killed.length
      ? banner('danger', 'i-power', 'Kill switch is on', esc(killed.map(function (c) { return c.name; }).join(', ')) + ' ' + plural(killed.length, 'rejects', 'reject') + ' every command from this site. Only you, on each machine, can turn it off again.')
      : '<div class="banner" data-status="danger">' + icon('i-power') + '<div class="banner__body"><p class="banner__title">Stop all remote control now</p><p class="banner__text">Every connector stops accepting commands from this site right away. Agents keep running on your machines. Only you, on each machine, can turn it off again.</p></div><div class="banner__actions"><button class="btn btn--danger btn--sm" type="button" data-open-dialog="kill-dialog"' + (ready.length ? '' : ' disabled') + '>Turn on</button></div></div>';

    var names = ready.map(function (c) { return c.name; });
    return '<div class="page page--narrow"><header class="page-head"><div class="page-head__title"><h1>Devices &amp; security</h1><p class="page-head__sub">Your machines are the authority: they decide which devices may send commands and for how long.</p></div></header>' +
      (needApproval.length ? banner('info', 'i-lock', 'This browser is not approved on ' + needApproval.map(function (c) { return c.name; }).join(', '), 'Approve it on the machine to see its workspaces here.', '<a class="btn btn--sm" href="/pair?new=0">Approve</a>') : '') +
      '<section class="section" aria-labelledby="dev-title"><div class="section__head"><h2 class="section__title" id="dev-title">Paired devices</h2><span class="count">' + groups.length + '</span></div>' +
      (groups.length ? '<ul class="card list" id="device-list">' + rows + '</ul>' : '<div class="card"><div class="empty empty--compact"><p class="empty__title">No device is approved yet</p><p class="empty__text">Approve this browser to see your machines.</p><a class="btn btn--primary btn--sm" href="/pair?new=0">Approve this browser</a></div></div>') +
      (offline.length ? '<p class="field__hint">' + esc(offline.map(function (c) { return c.name; }).join(', ')) + ' ' + plural(offline.length, 'is', 'are') + ' offline, so ' + plural(offline.length, 'its devices are', 'their devices are') + ' not shown. Devices are managed on each machine.</p>' : '') +
      '</section>' +
      '<section class="section" aria-labelledby="pol-title"><div class="section__head"><h2 class="section__title" id="pol-title">Session policy</h2><span class="label">' + icon('i-lock') + 'Enforced by the connector</span></div>' + polHtml + '</section>' +
      '<section class="section" aria-labelledby="kill-title"><div class="section__head"><h2 class="section__title" id="kill-title">Kill switch</h2><span class="status" data-status="' + (killed.length ? 'danger' : 'ok') + '">' + (killed.length ? 'On' : 'Off') + '</span></div>' + killBlock + '</section>' +
      '<dialog class="dialog" id="kill-dialog" aria-labelledby="kill-d-title"><form method="dialog" id="kill-form"><div class="dialog__body" data-status="danger"><span class="dialog__icon">' + icon('i-power', 'icon--lg') + '</span><h2 class="dialog__title" id="kill-d-title">Turn on the kill switch?</h2>' +
      '<p class="dialog__text">' + esc(names.join(' and ') || 'Your machines') + ' will reject every command from Maestria right away. Agents keep running. It can only be turned off on each machine: run <code>maestria tunnel on</code>.</p>' +
      '<label class="check"><input type="checkbox" id="kill-confirm"> I understand this can’t be undone from the web</label><p class="field__error" id="kill-error" hidden></p></div>' +
      '<div class="dialog__foot"><button class="btn" type="button" data-close-dialog>Cancel</button><button class="btn btn--danger" type="submit" data-requires="kill-confirm">' + icon('i-power') + 'Turn on kill switch</button></div></form></dialog></div>';
  }

  document.addEventListener('click', async function (ev) {
    var b = ev.target.closest('[data-action="revoke-device"]');
    if (b) {
      var dev = b.getAttribute('data-device');
      b.disabled = true;
      var rec = state.key || await K.load();
      var mine = rec && rec.id === dev;
      var targets = readyConnectors();
      // Revoke on every machine, signing this browser out last so each request still verifies.
      for (var i = 0; i < targets.length; i++) {
        try { await rjson(targets[i].id, 'POST', '/api/devices/' + encodeURIComponent(dev) + '/revoke', { body: {} }); } catch (e) { /* not on that machine */ }
      }
      if (mine) { await K.forget(); state.key = null; location.href = '/pair?new=1'; return; }
      renderCurrent(false);
      return;
    }
    var copy = ev.target.closest('[data-copy]');
    if (copy && !copy.closest('#main [data-panel-copy]')) {
      // panel.js also handles [data-copy]; nothing to do here.
    }
  });

  document.addEventListener('submit', async function (ev) {
    var f = ev.target;
    if (f.id === 'kill-form') {
      ev.preventDefault();
      var err = $('#kill-error');
      var failed = [];
      for (var c of readyConnectors()) {
        try { await rjson(c.id, 'POST', '/api/kill-switch', { body: {} }); } catch (e) { failed.push(c.name); }
      }
      var d = $('#kill-dialog');
      if (d && d.close) d.close();
      if (failed.length && err) { err.hidden = false; err.textContent = 'Could not reach ' + failed.join(', ') + '.'; }
      await loadConnectors();
    }
    if (f.id === 'rename-form') {
      ev.preventDefault();
      var name = $('#rename-input').value.trim();
      var id = f.getAttribute('data-id');
      var rerr = $('#rename-error');
      if (!name) return;
      try { await cloud('POST', '/api/connectors/' + id + '/rename', { name: name }); $('#rename-dialog').close(); await loadConnectors(); }
      catch (e) { rerr.hidden = false; rerr.textContent = e.message; }
    }
    if (f.id === 'revoke-form') {
      ev.preventDefault();
      var rid = f.getAttribute('data-id');
      var verr = $('#revoke-error');
      try { await cloud('POST', '/api/connectors/' + rid + '/revoke'); $('#revoke-dialog').close(); await loadConnectors(); navigate('/connectors', true); }
      catch (e) { verr.hidden = false; verr.textContent = e.message; }
    }
    if (f.id === 'claim-form') {
      ev.preventDefault();
      var hint = $('#claim-hint');
      var code = $('#cli-code').value.trim();
      if (!code) return;
      try { await cloud('POST', '/api/link/claim', { code: code }); hint.textContent = 'Code accepted. Confirm on the machine.'; hint.removeAttribute('class'); hint.className = 'field__hint'; }
      catch (e) { hint.textContent = e.message === 'invalid_code' ? 'That code is not valid, was already used, or expired.' : e.message; hint.className = 'field__error'; }
    }
  });

  // Connectors page: menus and dialogs.
  document.addEventListener('click', function (ev) {
    var r = ev.target.closest('[data-action="rename-connector"]');
    if (r) {
      var menu = r.closest('details.menu'); if (menu) menu.removeAttribute('open');
      $('#rename-form').setAttribute('data-id', r.getAttribute('data-id'));
      $('#rename-input').value = r.getAttribute('data-name') || '';
      $('#rename-error').hidden = true;
      $('#rename-dialog').showModal();
      return;
    }
    var v = ev.target.closest('[data-action="revoke-connector"]');
    if (v) {
      var menu2 = v.closest('details.menu'); if (menu2) menu2.removeAttribute('open');
      $('#revoke-form').setAttribute('data-id', v.getAttribute('data-id'));
      $('#revoke-name').textContent = v.getAttribute('data-name') || 'this machine';
      $('#revoke-error').hidden = true;
      $('#revoke-dialog').showModal();
    }
  });

  // ---- link page ----

  var linkTimer = null;
  async function startLinkPage() {
    clearInterval(linkTimer);
    var codeEl = $('#link-code');
    if (!codeEl) return;
    try {
      var c = await cloud('POST', '/api/link/codes');
      codeEl.textContent = c.code;
      var cmd = $('#link-command');
      if (cmd) cmd.textContent = 'maestria connect --link ' + c.code + ' --cloud ' + location.origin;
      var status = $('#link-status');
      var left = $('[data-countdown]');
      var tick = function () {
        var s = Math.max(0, Math.round((c.expiresAt - Date.now()) / 1000));
        if (left) left.textContent = Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
        if (!s) { clearInterval(linkTimer); if (status) status.innerHTML = 'The code expired. <a href="/connectors/link">Get a new one</a>.'; }
      };
      tick();
      linkTimer = setInterval(async function () {
        tick();
        if (!$('#link-code')) { clearInterval(linkTimer); return; }
        try {
          var st = await cloud('GET', '/api/link/codes/' + c.id);
          if (st.state === 'linked') {
            clearInterval(linkTimer);
            if (status) status.innerHTML = '<span class="status" data-status="ok">' + esc(st.name || 'The machine') + ' is linked.</span> <a href="/connectors">Open connectors</a>';
          }
        } catch (e) { /* retry */ }
      }, 2000);
    } catch (e) {
      codeEl.textContent = '––––-––––';
      var s2 = $('#link-status'); if (s2) s2.textContent = e.message;
    }
  }

  async function hydrateConnectors() {
    // Workspaces and Maestri Wire status of each online machine, fetched live through the relay.
    await Promise.all($$('[data-connector]').map(async function (card) {
      var cid = card.getAttribute('data-connector');
      var c = connector(cid);
      var ws = $('[data-slot="workspaces"]', card), wire = $('[data-slot="wire"]', card);
      if (!c || !c.online || !ws) return;
      var a = state.access[cid] || {};
      if (a.state !== 'ok' && !state.data[cid]) {
        var msg = a.state === 'killed' ? 'Hidden: kill switch is on' : 'Approve this browser to see it';
        ws.innerHTML = '<span class="subtle">' + esc(msg) + '</span>';
        if (wire) wire.innerHTML = '<span class="subtle">' + esc(msg) + '</span>';
        return;
      }
      var list = (state.data[cid] || { workspaces: [] }).workspaces;
      ws.innerHTML = list.length ? '<span class="btn-row">' + list.map(function (w) { return '<span class="ws" data-ws="' + esc(w.tone) + '">' + esc(w.name) + '</span>'; }).join('') + '</span>' : '<span class="subtle">None yet</span>';
      if (wire) {
        try {
          var w = await rjson(cid, 'GET', '/api/wire');
          var linked = w && (w.state === 'live' || w.state === 'connected' || w.connected);
          wire.innerHTML = '<span class="status" data-status="' + (linked ? 'ok' : 'offline') + '">' + (linked ? 'Connected' : esc((w && w.state) ? String(w.state).replace(/_/g, ' ') : 'Not connected')) + '</span>';
        } catch (e) { wire.innerHTML = '<span class="subtle">Unknown</span>'; }
      }
    }));
  }

  // ---- rendering the current route ----

  var renderSeq = 0;

  // softRender refreshes the page after the world changed (a machine came or went, a probe
  // finished). Data pages go through panel.js so drafts, open menus and scroll are kept.
  function softRender() {
    var r = state.route;
    if (!r || !state.loaded) return;
    if (r.server) { renderCurrent(false); return; }
    if (window.MaestriaPanel) window.MaestriaPanel.refresh(); else renderCurrent(true);
  }

  // routeHtml builds the main content for a route. Server-owned pages are fetched as fragments.
  async function routeHtml(r) {
    switch (r.name) {
      case 'overview': {
        if (!state.connectors.length) return stateOnboarding();
        if (!onlineConnectors().length) return stateNoMachine();
        var ready = readyConnectors();
        if (!ready.length) {
          var waiting = onlineConnectors().filter(function (c) { return !state.access[c.id] || state.access[c.id].state === 'ok'; });
          if (waiting.length && !state.loaded) return '<div class="page"><p class="status status--quiet"><span class="spinner"></span>Connecting to your machines…</p></div>';
          var live = onlineConnectors();
          if (live.every(function (c) { return (state.access[c.id] || {}).state === 'killed'; })) {
            return '<div class="page page--narrow"><header class="page-head"><div class="page-head__title"><h1>Overview</h1></div></header>' +
              stateKilled(live.map(function (c) { return c.name; })) + '<div class="card">' + machineList(state.connectors) + '</div></div>';
          }
          return stateNeedsApproval(live);
        }
        if (!allWorkspaces().length) return stateOnboarding();
        return renderOverview();
      }
      case 'workspace': return workspaceHtml(r);
      case 'usage': return usageHtml(r);
      case 'security': return securityHtml();
      default: {
        var res = await fetch(location.pathname, { headers: { 'X-Fragment': '1' }, credentials: 'same-origin' });
        if (res.status === 401) { location.href = '/login'; return ''; }
        return res.text();
      }
    }
  }

  async function renderCurrent(full) {
    var r = state.route;
    if (!r) return;
    var seq = ++renderSeq;
    if (!state.loaded && !r.server) { /* wait for the first connector list */ }
    if (r.server) {
      if (full) {
        var html = await routeHtml(r);
        if (seq !== renderSeq) return;
        setMain(html);
        if (r.name === 'link') startLinkPage(); else clearInterval(linkTimer);
        if (r.name === 'connectors') hydrateConnectors();
      } else if (r.name === 'connectors') {
        // Presence changed: refresh the server-rendered list in place unless a dialog is open.
        if (!$('dialog[open]') && !$('details.menu[open]')) {
          var h2 = await routeHtml(r);
          if (seq !== renderSeq) return;
          setMain(h2);
          hydrateConnectors();
        }
      }
      return;
    }
    if (!full) {
      // Live updates of data pages go through panel.js (see MaestriaHost.page); the security
      // page is not live, so it is rebuilt unless a dialog is open.
      if (r.name === 'security' && !$('dialog[open]')) { var h3 = await routeHtml(r); if (seq === renderSeq) setMain(h3); }
      return;
    }
    if (!state.loaded) {
      setMain('<div class="page"><p class="status status--quiet"><span class="spinner"></span>Loading…</p></div>');
      return;
    }
    var html2 = await routeHtml(r);
    if (seq !== renderSeq) return;
    setMain(html2);
  }

  // ---- the hook panel.js uses in hosted mode ----

  function compositeSplit(seg) {
    var s = decodeURIComponent(seg);
    var m = s.match(/^(cn_[0-9a-f]{32}):(.+)$/);
    return m ? { cid: m[1], ws: m[2] } : null;
  }

  window.MaestriaHost = {
    // api mirrors panel.js's local api(): JSON in, JSON out, but signed and relayed.
    api: async function (url, opts) {
      opts = opts || {};
      var u = new URL(url, location.origin);
      var path = u.pathname, cid = null;
      var m = path.match(/^\/api\/workspaces\/([^/]+)(\/.*)?$/);
      if (m) {
        var comp = compositeSplit(m[1]);
        if (comp) { cid = comp.cid; path = '/api/workspaces/' + encodeURIComponent(comp.ws) + (m[2] || ''); }
      }
      if (!cid) cid = (state.route && state.route.cid) || (readyConnectors()[0] || {}).id;
      if (!cid) throw new Error('No machine is available.');
      var method = (opts.method || 'GET').toUpperCase();
      var body = opts.body;
      try {
        var res = await rfetch(cid, method, path, { query: u.search.slice(1), body: body, accept: 'application/json' });
        var text = await res.text();
        return text ? JSON.parse(text) : null;
      } catch (e) {
        if (e instanceof RelayError) {
          // Machine answered with a plain API error (not a gate refusal): surface its message.
          throw new Error(ERROR_TEXT[e.code] || e.code);
        }
        throw e;
      }
    },
    // page returns the new main#main for the current route, for panel.js to swap in.
    page: async function () {
      var r = state.route;
      if (!r || !state.loaded) return null;
      if (r.server) return null;
      var html = await routeHtml(r);
      return '<!doctype html><html><body><main class="main" id="main">' + html + '</main></body></html>';
    }
  };

  // ---- boot ----

  async function boot() {
    state.route = parseRoute(location.pathname + location.search);
    setActive();
    try { state.key = await K.load(); } catch (e) { state.key = null; }
    var pre = $('#main [data-view]');
    if (state.route.server) {
      if (state.route.name === 'link') startLinkPage();
    }
    try {
      var r = await cloud('GET', '/api/connectors');
      state.loaded = true;
      applyConnectors(r.connectors || []);
    } catch (e) {
      state.loaded = true;
      renderShell();
    }
    renderCurrent(true);
    if (typeof EventSource === 'function') {
      var es = new EventSource('/api/presence');
      es.addEventListener('connectors', function (ev) {
        try { applyConnectors(JSON.parse(ev.data).connectors || []); } catch (e) { /* ignore */ }
      });
    }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) refreshData(); });
    void pre;
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot); else boot();
})();
