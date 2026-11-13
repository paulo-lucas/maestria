'use strict';

(() => {
  const DRAFT_KEY = 'maestria:drafts:v3';
  const POLL_MS = 5000;

  const csrf = document.body.dataset.csrf || '';
  const el = {
    switcher: document.getElementById('ws-switcher'),
    sync: document.getElementById('sync-status'),
    banner: document.getElementById('banner'),
    viewTitle: document.getElementById('view-title'),
    viewMeta: document.getElementById('view-meta'),
    backHome: document.getElementById('back-home'),
    filterBar: document.getElementById('filter-bar'),
    noWorkspaces: document.getElementById('no-workspaces'),
    needsSection: document.getElementById('needs-section'),
    needsBadge: document.getElementById('needs-badge'),
    toggleHistory: document.getElementById('toggle-history'),
    needsList: document.getElementById('needs-list'),
    needsEmpty: document.getElementById('needs-empty'),
    updatesSection: document.getElementById('updates-section'),
    updatesBadge: document.getElementById('updates-badge'),
    updatesList: document.getElementById('updates-list'),
    updatesEmpty: document.getElementById('updates-empty'),
    historySection: document.getElementById('history-section'),
    historyList: document.getElementById('history-list'),
    historyEmpty: document.getElementById('history-empty'),
  };

  let workspaces = [];
  let routeWsId = readWorkspaceFromHash(); // null => Home, string => workspace page
  let homeFilterWsId = null; // null => All workspaces on Home
  let showHistory = false;
  let state = { needsYou: [], updates: [], historyCount: 0, history: [] };
  let drafts = loadDrafts();
  let inFlight = 0;

  function loadDrafts() {
    try {
      const parsed = JSON.parse(localStorage.getItem(DRAFT_KEY) || '{}');
      return parsed && typeof parsed === 'object' ? parsed : {};
    } catch {
      return {};
    }
  }

  function saveDrafts() {
    try {
      localStorage.setItem(DRAFT_KEY, JSON.stringify(drafts));
    } catch {
      /* ignore quota errors */
    }
  }

  function setDraft(itemId, value) {
    if (value === '') {
      clearDraft(itemId);
      return;
    }
    drafts[itemId] = value;
    saveDrafts();
  }

  function clearDraft(itemId) {
    if (Object.hasOwn(drafts, itemId)) {
      delete drafts[itemId];
      saveDrafts();
    }
  }

  function showBanner(msg) {
    if (!msg) {
      el.banner.hidden = true;
      el.banner.textContent = '';
      return;
    }
    el.banner.textContent = msg;
    el.banner.hidden = false;
  }

  function fmtTime(ms) {
    if (!ms) return '';
    try {
      return new Date(ms).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    } catch {
      return '';
    }
  }

  async function api(url, opts = {}) {
    const headers = { ...(opts.headers || {}) };
    if (opts.method && opts.method !== 'GET') {
      headers['X-CSRF-Token'] = csrf;
      if (opts.body !== undefined) {
        headers['Content-Type'] = 'application/json';
      }
    }
    const res = await fetch(url, { ...opts, headers, cache: 'no-store' });
    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('unauthenticated');
    }
    if (!res.ok) {
      let msg = `Request failed (${res.status})`;
      try {
        const data = await res.json();
        if (data && typeof data.error === 'string') msg = data.error;
      } catch {
        /* ignore */
      }
      throw new Error(msg);
    }
    return res.json();
  }

  function readWorkspaceFromHash() {
    const h = window.location.hash || '';
    const m = /^#ws=([^&]+)$/.exec(h);
    return m ? decodeURIComponent(m[1]) : null;
  }

  function openWorkspace(id) {
    routeWsId = id;
    showHistory = false;
    el.historySection.hidden = true;
    window.location.hash = id ? `ws=${encodeURIComponent(id)}` : '';
    renderNavigation();
    void refreshInbox();
  }

  function makeWsDot(wsId) {
    const dot = document.createElement('span');
    dot.className = 'ws-dot';
    dot.dataset.ws = wsId;
    return dot;
  }

  function makeWsTag(item) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ws-tag';
    btn.dataset.ws = item.workspaceId;
    btn.append(makeWsDot(item.workspaceId), document.createTextNode(item.workspaceName || item.workspaceId));
    btn.title = `Open ${item.workspaceName || item.workspaceId}`;
    btn.addEventListener('click', () => openWorkspace(item.workspaceId));
    return btn;
  }

  function renderNavigation() {
    // Top switcher: Home + workspaces
    el.switcher.textContent = '';

    const homeBtn = document.createElement('button');
    homeBtn.type = 'button';
    homeBtn.className = 'ws-pill' + (routeWsId === null ? ' active' : '');
    homeBtn.textContent = 'Home';
    const totalNeeds = workspaces.reduce((sum, w) => sum + (w.counts?.needsYou || 0), 0);
    if (totalNeeds > 0) {
      const b = document.createElement('span');
      b.className = 'pill-badge';
      b.textContent = String(totalNeeds);
      homeBtn.appendChild(b);
    }
    homeBtn.addEventListener('click', () => openWorkspace(null));
    el.switcher.appendChild(homeBtn);

    for (const ws of workspaces) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'ws-pill' + (ws.id === routeWsId ? ' active' : '');
      btn.append(makeWsDot(ws.id), document.createTextNode(ws.name));
      const count = ws.counts?.needsYou || 0;
      if (count > 0) {
        const b = document.createElement('span');
        b.className = 'pill-badge';
        b.textContent = String(count);
        btn.appendChild(b);
      }
      btn.addEventListener('click', () => openWorkspace(ws.id));
      el.switcher.appendChild(btn);
    }

    el.noWorkspaces.hidden = workspaces.length > 0;

    if (routeWsId === null) {
      el.viewTitle.textContent = 'Home';
      el.viewMeta.textContent = workspaces.length
        ? `Unified inbox across ${workspaces.length} workspace${workspaces.length === 1 ? '' : 's'}`
        : '';
      el.backHome.hidden = true;

      // Filter chips on Home
      el.filterBar.textContent = '';
      if (workspaces.length > 1) {
        el.filterBar.hidden = false;
        const allChip = document.createElement('button');
        allChip.type = 'button';
        allChip.className = 'filter-chip' + (homeFilterWsId === null ? ' active' : '');
        allChip.textContent = 'All workspaces';
        allChip.addEventListener('click', () => {
          homeFilterWsId = null;
          renderNavigation();
          void refreshInbox();
        });
        el.filterBar.appendChild(allChip);

        for (const ws of workspaces) {
          const chip = document.createElement('button');
          chip.type = 'button';
          chip.className = 'filter-chip' + (homeFilterWsId === ws.id ? ' active' : '');
          chip.dataset.ws = ws.id;
          const pending = (ws.counts?.needsYou || 0) + (ws.counts?.updates || 0);
          const label = pending > 0 ? `${ws.name} (${pending})` : ws.name;
          chip.append(makeWsDot(ws.id), document.createTextNode(label));
          chip.addEventListener('click', () => {
            homeFilterWsId = homeFilterWsId === ws.id ? null : ws.id;
            renderNavigation();
            void refreshInbox();
          });
          el.filterBar.appendChild(chip);
        }
      } else {
        el.filterBar.hidden = true;
      }
    } else {
      const ws = workspaces.find((w) => w.id === routeWsId);
      el.filterBar.hidden = true;
      el.backHome.hidden = false;
      if (ws) {
        el.viewTitle.textContent = ws.name;
        el.viewMeta.textContent = `${ws.path} · Maestro: ${ws.maestroAgent}`;
      } else {
        el.viewTitle.textContent = routeWsId;
        el.viewMeta.textContent = '';
      }
    }
  }

  function createInboxElement(item) {
    const wrap = document.createElement('div');
    wrap.className = 'inbox-item';
    wrap.dataset.itemId = item.id;
    wrap.dataset.ws = item.workspaceId;

    const head = document.createElement('div');
    head.className = 'inbox-head';

    const bodyCol = document.createElement('div');
    bodyCol.className = 'inbox-body';

    const tags = document.createElement('div');
    tags.className = 'inbox-tags';
    if (routeWsId === null) {
      tags.appendChild(makeWsTag(item));
    }
    const kindTag = document.createElement('span');
    kindTag.className = 'kind-tag';
    kindTag.textContent = item.kind;
    tags.appendChild(kindTag);

    const md = document.createElement('div');
    md.className = 'md';
    md.innerHTML = item.bodyHtml;
    md.dataset.rawBody = item.body;

    const meta = document.createElement('div');
    meta.className = 'inbox-meta';
    meta.textContent = `by ${item.createdBy} · ${fmtTime(item.createdAt)}`;

    bodyCol.append(tags, md, meta);
    head.appendChild(bodyCol);

    if (item.kind !== 'notice') {
      const dismissBtn = document.createElement('button');
      dismissBtn.type = 'button';
      dismissBtn.className = 'icon-btn';
      dismissBtn.textContent = '✕';
      dismissBtn.title = 'Dismiss without responding';
      dismissBtn.setAttribute('aria-label', 'Dismiss item');
      dismissBtn.addEventListener('click', () => void dismissItem(item));
      head.appendChild(dismissBtn);
    }

    wrap.appendChild(head);

    let textarea = null;
    if (item.kind === 'approve' || item.kind === 'input') {
      textarea = document.createElement('textarea');
      textarea.placeholder = item.kind === 'approve' ? 'Optional comment…' : 'Write your response…';
      textarea.maxLength = 8000;
      if (Object.hasOwn(drafts, item.id)) {
        textarea.value = drafts[item.id];
      }
      textarea.addEventListener('input', () => {
        setDraft(item.id, textarea.value);
        const hint = wrap.querySelector('.draft-hint');
        if (hint) hint.textContent = textarea.value ? 'Draft saved' : '';
        const err = wrap.querySelector('.inline-err');
        if (err) err.textContent = '';
      });
      wrap.appendChild(textarea);
    }

    const actions = document.createElement('div');
    actions.className = 'inbox-actions';

    if (item.kind === 'notice') {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'ghost small-btn';
      btn.textContent = 'Got it';
      btn.addEventListener('click', () => void respondToItem(item, { decision: 'ack' }));
      actions.appendChild(btn);
    } else if (item.kind === 'approve') {
      const approveBtn = document.createElement('button');
      approveBtn.type = 'button';
      approveBtn.className = 'small-btn btn-ok';
      approveBtn.textContent = 'Approve';
      approveBtn.addEventListener('click', () =>
        void respondToItem(item, { decision: 'approved', response: textarea ? textarea.value : '' }),
      );

      const rejectBtn = document.createElement('button');
      rejectBtn.type = 'button';
      rejectBtn.className = 'small-btn btn-danger';
      rejectBtn.textContent = 'Reject';
      rejectBtn.addEventListener('click', () =>
        void respondToItem(item, { decision: 'rejected', response: textarea ? textarea.value : '' }),
      );

      const hint = document.createElement('span');
      hint.className = 'muted small draft-hint';
      hint.textContent = Object.hasOwn(drafts, item.id) ? 'Draft saved' : '';

      actions.append(approveBtn, rejectBtn, hint);
    } else if (item.kind === 'input') {
      const sendBtn = document.createElement('button');
      sendBtn.type = 'button';
      sendBtn.className = 'small-btn';
      sendBtn.textContent = 'Send';

      const inlineErr = document.createElement('span');
      inlineErr.className = 'inline-err';

      const hint = document.createElement('span');
      hint.className = 'muted small draft-hint';
      hint.textContent = Object.hasOwn(drafts, item.id) ? 'Draft saved' : '';

      sendBtn.addEventListener('click', () => {
        const val = textarea ? textarea.value.trim() : '';
        if (!val) {
          inlineErr.textContent = 'Please write a response first.';
          if (textarea) textarea.focus();
          return;
        }
        void respondToItem(item, { response: textarea.value });
      });

      actions.append(sendBtn, hint, inlineErr);
    } else if (item.kind === 'choice') {
      for (const opt of item.options || []) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'small-btn btn-choice';
        btn.textContent = opt;
        btn.addEventListener('click', () => void respondToItem(item, { decision: opt }));
        actions.appendChild(btn);
      }
    }

    wrap.appendChild(actions);
    return wrap;
  }

  function updateInboxElement(wrap, item) {
    wrap.dataset.ws = item.workspaceId;
    const md = wrap.querySelector('.md');
    if (md && md.dataset.rawBody !== item.body) {
      md.innerHTML = item.bodyHtml;
      md.dataset.rawBody = item.body;
    }
    const meta = wrap.querySelector('.inbox-meta');
    if (meta) {
      meta.textContent = `by ${item.createdBy} · ${fmtTime(item.createdAt)}`;
    }
  }

  function reconcileList(container, items) {
    const existingById = new Map();
    for (const child of Array.from(container.children)) {
      if (child.dataset.itemId) existingById.set(child.dataset.itemId, child);
    }
    const desiredIds = new Set(items.map((i) => i.id));
    for (const [id, node] of existingById) {
      if (!desiredIds.has(id)) node.remove();
    }

    let cursor = container.firstElementChild;
    for (const item of items) {
      let node = existingById.get(item.id);
      if (!node) {
        node = createInboxElement(item);
        container.insertBefore(node, cursor);
      } else {
        updateInboxElement(node, item);
        if (node !== cursor) {
          container.insertBefore(node, cursor);
        } else {
          cursor = cursor.nextElementSibling;
        }
      }
    }
  }

  function renderInbox() {
    const { needsYou, updates, historyCount } = state;

    el.needsSection.classList.toggle('clear', needsYou.length === 0);
    el.needsBadge.hidden = needsYou.length === 0;
    el.needsBadge.textContent = String(needsYou.length);
    el.needsEmpty.hidden = needsYou.length > 0;

    el.updatesBadge.textContent = updates.length ? `(${updates.length})` : '';
    el.updatesEmpty.hidden = updates.length > 0;

    if (historyCount > 0 || showHistory) {
      el.toggleHistory.hidden = false;
      el.toggleHistory.textContent = showHistory ? 'Hide history' : `History (${historyCount})`;
    } else {
      el.toggleHistory.hidden = true;
    }

    const isTyping = document.activeElement instanceof HTMLTextAreaElement && el.needsList.contains(document.activeElement);
    if (!isTyping) {
      reconcileList(el.needsList, needsYou);
    }
    reconcileList(el.updatesList, updates);
  }

  function renderHistory() {
    el.historySection.hidden = !showHistory;
    if (!showHistory) return;

    const items = state.history || [];
    el.historyList.textContent = '';
    el.historyEmpty.hidden = items.length > 0;

    for (const item of items) {
      const row = document.createElement('div');
      row.className = 'history-item';
      row.dataset.ws = item.workspaceId;

      const top = document.createElement('div');
      top.className = 'history-row';

      const left = document.createElement('div');
      if (routeWsId === null) {
        const tags = document.createElement('div');
        tags.className = 'inbox-tags';
        tags.appendChild(makeWsTag(item));
        left.appendChild(tags);
      }
      const prompt = document.createElement('div');
      prompt.className = 'md';
      prompt.innerHTML = item.bodyHtml;
      left.appendChild(prompt);

      const right = document.createElement('div');
      const badge = document.createElement('span');
      if (item.status === 'dismissed') {
        badge.className = 'decision-badge' + (item.decision === 'ack' ? ' ok' : '');
        badge.textContent = item.decision === 'ack' ? 'Got it' : 'Dismissed';
      } else if (item.kind === 'approve') {
        badge.className = `decision-badge ${item.decision === 'approved' ? 'approved' : 'rejected'}`;
        badge.textContent = item.decision === 'approved' ? 'Approved' : 'Rejected';
      } else if (item.kind === 'choice') {
        badge.className = 'decision-badge ok';
        badge.textContent = item.decision || 'Chosen';
      } else {
        badge.className = 'decision-badge ok';
        badge.textContent = 'Replied';
      }

      const ts = document.createElement('span');
      ts.className = 'muted small';
      ts.textContent = ' ' + fmtTime(item.resolvedAt);

      right.append(badge, ts);
      top.append(left, right);
      row.appendChild(top);

      if (item.response) {
        const resp = document.createElement('div');
        resp.className = 'history-response';
        resp.textContent = item.response;
        row.appendChild(resp);
      }

      el.historyList.appendChild(row);
    }
  }

  async function respondToItem(item, payload) {
    const prevNeeds = [...state.needsYou];
    const prevUpdates = [...state.updates];
    const prevHistoryCount = state.historyCount;

    state.needsYou = state.needsYou.filter((i) => i.id !== item.id);
    state.updates = state.updates.filter((i) => i.id !== item.id);
    state.historyCount += 1;
    renderInbox();

    inFlight += 1;
    try {
      showBanner('');
      const res = await api(
        `/api/workspaces/${encodeURIComponent(item.workspaceId)}/inbox/${encodeURIComponent(item.id)}/respond`,
        { method: 'POST', body: JSON.stringify(payload) },
      );
      clearDraft(item.id);
      if (showHistory) {
        state.history = [res.item, ...state.history.filter((h) => h.id !== item.id)];
        renderHistory();
      }
      void refreshWorkspaces();
    } catch (err) {
      state.needsYou = prevNeeds;
      state.updates = prevUpdates;
      state.historyCount = prevHistoryCount;
      renderInbox();
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not submit response: ${err.message}`);
      }
    } finally {
      inFlight -= 1;
    }
  }

  async function dismissItem(item) {
    const prevNeeds = [...state.needsYou];
    const prevUpdates = [...state.updates];
    const prevHistoryCount = state.historyCount;

    state.needsYou = state.needsYou.filter((i) => i.id !== item.id);
    state.updates = state.updates.filter((i) => i.id !== item.id);
    state.historyCount += 1;
    renderInbox();

    inFlight += 1;
    try {
      showBanner('');
      const res = await api(
        `/api/workspaces/${encodeURIComponent(item.workspaceId)}/inbox/${encodeURIComponent(item.id)}/dismiss`,
        { method: 'POST' },
      );
      clearDraft(item.id);
      if (showHistory) {
        state.history = [res.item, ...state.history.filter((h) => h.id !== item.id)];
        renderHistory();
      }
      void refreshWorkspaces();
    } catch (err) {
      state.needsYou = prevNeeds;
      state.updates = prevUpdates;
      state.historyCount = prevHistoryCount;
      renderInbox();
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not dismiss item: ${err.message}`);
      }
    } finally {
      inFlight -= 1;
    }
  }

  async function refreshWorkspaces() {
    const data = await api('/api/workspaces');
    workspaces = data.workspaces || [];
    if (routeWsId !== null && !workspaces.some((w) => w.id === routeWsId)) {
      routeWsId = null;
    }
    if (homeFilterWsId !== null && !workspaces.some((w) => w.id === homeFilterWsId)) {
      homeFilterWsId = null;
    }
    renderNavigation();
  }

  async function refreshInbox() {
    const targetRoute = routeWsId;
    const targetFilter = homeFilterWsId;

    let openUrl;
    let histUrl;
    if (targetRoute !== null) {
      openUrl = `/api/workspaces/${encodeURIComponent(targetRoute)}/inbox`;
      histUrl = `/api/workspaces/${encodeURIComponent(targetRoute)}/inbox?status=history`;
    } else if (targetFilter !== null) {
      openUrl = `/api/inbox?workspace=${encodeURIComponent(targetFilter)}`;
      histUrl = `/api/inbox?status=history&workspace=${encodeURIComponent(targetFilter)}`;
    } else {
      openUrl = '/api/inbox';
      histUrl = '/api/inbox?status=history';
    }

    const data = await api(openUrl);
    if (routeWsId !== targetRoute || homeFilterWsId !== targetFilter || inFlight > 0) return;

    state.needsYou = data.needsYou || [];
    state.updates = data.updates || [];
    state.historyCount = data.historyCount || 0;

    if (showHistory) {
      const h = await api(histUrl);
      if (routeWsId === targetRoute && homeFilterWsId === targetFilter && inFlight === 0) {
        state.history = h.items || [];
        renderHistory();
      }
    }

    renderInbox();
  }

  async function poll() {
    try {
      await refreshWorkspaces();
      await refreshInbox();
      el.sync.textContent = `Updated ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`;
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        el.sync.textContent = 'Offline / sync paused';
      }
    }
  }

  el.backHome.addEventListener('click', () => openWorkspace(null));

  el.toggleHistory.addEventListener('click', () => {
    showHistory = !showHistory;
    renderInbox();
    if (showHistory) {
      void refreshInbox();
    } else {
      renderHistory();
    }
  });

  window.addEventListener('hashchange', () => {
    const id = readWorkspaceFromHash();
    if (id !== routeWsId) {
      routeWsId = id;
      showHistory = false;
      el.historySection.hidden = true;
      renderNavigation();
      void refreshInbox();
    }
  });

  void poll();
  setInterval(() => {
    if (!document.hidden) void poll();
  }, POLL_MS);
})();
