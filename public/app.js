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
    wsTabs: document.getElementById('ws-tabs'),
    tabInbox: document.getElementById('tab-inbox'),
    tabChat: document.getElementById('tab-chat'),
    tabRoadmap: document.getElementById('tab-roadmap'),
    usageSection: document.getElementById('usage-section'),
    standbyPill: document.getElementById('standby-pill'),
    usageUpdated: document.getElementById('usage-updated'),
    refreshUsage: document.getElementById('refresh-usage'),
    usageSetupError: document.getElementById('usage-setup-error'),
    usageStandbyNote: document.getElementById('usage-standby-note'),
    usageGrid: document.getElementById('usage-grid'),
    usageHidden: document.getElementById('usage-hidden'),
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
    roadmapSection: document.getElementById('roadmap-section'),
    roadmapCounts: document.getElementById('roadmap-counts'),
    roadmapUpdated: document.getElementById('roadmap-updated'),
    roadmapBody: document.getElementById('roadmap-body'),
    chatSection: document.getElementById('chat-section'),
    chatAgent: document.getElementById('chat-agent'),
    chatList: document.getElementById('chat-list'),
    chatEmpty: document.getElementById('chat-empty'),
    chatForm: document.getElementById('chat-form'),
    chatInput: document.getElementById('chat-input'),
    chatSend: document.getElementById('chat-send'),
    chatErr: document.getElementById('chat-err'),
    toggleAudit: document.getElementById('toggle-audit'),
    killBtn: document.getElementById('kill-btn'),
    killConfirm: document.getElementById('kill-confirm'),
    killConfirmBtn: document.getElementById('kill-confirm-btn'),
    killCancelBtn: document.getElementById('kill-cancel-btn'),
    auditSection: document.getElementById('audit-section'),
    auditCount: document.getElementById('audit-count'),
    auditRefresh: document.getElementById('audit-refresh'),
    auditClose: document.getElementById('audit-close'),
    auditBody: document.getElementById('audit-body'),
  };

  let workspaces = [];
  const initialRoute = readRouteFromHash();
  let routeWsId = initialRoute.wsId; // null => Home, string => workspace page
  let wsTab = initialRoute.tab; // 'inbox' | 'chat' | 'roadmap'
  let chatMessages = [];
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
      const val = typeof ms === 'string' && /^\d+$/.test(ms) ? Number(ms) : ms;
      return new Date(val).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
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

  function readRouteFromHash() {
    const raw = (window.location.hash || '').replace(/^#/, '');
    if (!raw) return { wsId: null, tab: 'inbox' };
    const params = new URLSearchParams(raw);
    const wsId = params.get('ws') || null;
    const rawTab = params.get('tab');
    const tab = wsId && (rawTab === 'chat' || rawTab === 'roadmap' || rawTab === 'inbox') ? rawTab : 'inbox';
    return { wsId, tab };
  }

  function buildHash(wsId, tab) {
    if (!wsId) return '';
    return tab && tab !== 'inbox'
      ? `ws=${encodeURIComponent(wsId)}&tab=${encodeURIComponent(tab)}`
      : `ws=${encodeURIComponent(wsId)}`;
  }

  function openWorkspace(id, tab = 'inbox') {
    routeWsId = id;
    wsTab = id ? tab : 'inbox';
    showHistory = false;
    el.historySection.hidden = true;
    const nextHash = buildHash(routeWsId, wsTab);
    if ((window.location.hash || '').replace(/^#/, '') !== nextHash) {
      window.location.hash = nextHash;
    }
    renderNavigation();
    if (routeWsId === null) {
      void refreshUsage(false);
      void refreshInbox();
    } else if (wsTab === 'roadmap') {
      void refreshRoadmap();
    } else if (wsTab === 'chat') {
      void refreshChat();
    } else {
      void refreshInbox();
    }
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

    const showInboxSections = routeWsId === null || wsTab === 'inbox';
    el.usageSection.hidden = routeWsId !== null;
    el.wsTabs.hidden = routeWsId === null;
    el.tabInbox.classList.toggle('active', wsTab === 'inbox');
    el.tabChat.classList.toggle('active', wsTab === 'chat');
    el.tabRoadmap.classList.toggle('active', wsTab === 'roadmap');
    el.needsSection.hidden = !showInboxSections;
    el.updatesSection.hidden = !showInboxSections;
    if (!showInboxSections) el.historySection.hidden = true;
    el.chatSection.hidden = routeWsId === null || wsTab !== 'chat';
    el.roadmapSection.hidden = routeWsId === null || wsTab !== 'roadmap';

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

    const activeItem =
      document.activeElement instanceof HTMLTextAreaElement && el.needsList.contains(document.activeElement)
        ? document.activeElement.closest('.inbox-item')
        : null;
    const isTyping = Boolean(activeItem && needsYou.some((i) => i.id === activeItem.dataset.itemId));
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

  function fmtResetTime(iso) {
    if (!iso) return '';
    const ms = Date.parse(iso);
    if (!Number.isFinite(ms)) return '';
    const diffMs = ms - Date.now();
    if (diffMs > 0 && diffMs < 48 * 3600 * 1000) {
      const mins = Math.round(diffMs / 60000);
      if (mins < 60) return `resets in ${mins}m`;
      const hrs = Math.floor(mins / 60);
      const rem = mins % 60;
      return `resets in ${hrs}h${rem ? ` ${rem}m` : ''}`;
    }
    try {
      return 'resets ' + new Date(ms).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    } catch {
      return '';
    }
  }

  function findThreshold(thresholds, providerId, win) {
    if (!thresholds) return null;
    const lower = String(providerId || '').toLowerCase();
    const alias = lower === 'claude-code' ? 'claude' : lower === 'claude' ? 'claude-code' : null;
    const rules = thresholds[lower] || (alias && thresholds[alias]) || thresholds[providerId] || thresholds['*'];
    if (!rules) return null;
    if (typeof rules[win.id] === 'number') return rules[win.id];
    const idLower = String(win.id || '').toLowerCase();
    const ring =
      win.ring ||
      (typeof win.durationSeconds === 'number'
        ? win.durationSeconds <= 86400
          ? 'session'
          : 'weekly'
        : idLower === 'five_hour' || idLower === '5h' || idLower === 'session'
          ? 'session'
          : idLower === 'seven_day' || idLower === '7d' || idLower === 'weekly'
            ? 'weekly'
            : null);
    if (ring && typeof rules[ring] === 'number') return rules[ring];
    if (typeof rules['*'] === 'number') return rules['*'];
    return null;
  }

  function renderUsage(data) {
    el.usageUpdated.textContent = data.updatedAt ? `· updated ${fmtTime(Date.parse(data.updatedAt))}` : '';
    const standby = data.standby || { active: false, triggers: [], thresholds: {} };
    el.standbyPill.hidden = !standby.active;

    if (standby.active && standby.triggers && standby.triggers.length > 0) {
      const summary = standby.triggers
        .map((t) => `${t.providerName} ${t.windowTitle}: ${Math.round(t.usedPercent)}% (limit ${t.thresholdPercent}%)`)
        .join('; ');
      const pausedNames = Array.isArray(standby.workspaces)
        ? standby.workspaces.filter((w) => w.active).map((w) => w.workspaceName || w.workspaceId)
        : [];
      const wsPart = pausedNames.length > 0 ? ` [${pausedNames.join(', ')}]` : '';
      const resumePart =
        typeof standby.resumeBelowPercent === 'number'
          ? `on window reset or below ${standby.resumeBelowPercent}%`
          : 'after reset';
      el.usageStandbyNote.textContent = `Standby active${wsPart} — ${summary}. Work will resume automatically ${resumePart}.`;
      el.usageStandbyNote.hidden = false;
    } else {
      el.usageStandbyNote.hidden = true;
    }

    if (data.status !== 'ok') {
      el.usageSetupError.textContent = data.setupError || 'Usage data unavailable.';
      el.usageSetupError.hidden = false;
      el.usageGrid.textContent = '';
      if (el.usageHidden) {
        el.usageHidden.hidden = true;
        el.usageHidden.textContent = '';
      }
      return;
    }

    el.usageSetupError.hidden = true;
    el.usageGrid.textContent = '';
    if (el.usageHidden) {
      el.usageHidden.hidden = true;
      el.usageHidden.textContent = '';
    }

    const allProviders = data.providers || [];
    if (allProviders.length === 0) {
      const p = document.createElement('p');
      p.className = 'muted small';
      p.textContent = 'No usage providers enabled in Maestri.';
      el.usageGrid.appendChild(p);
      return;
    }

    const providers = allProviders.filter((p) => !p.hidden);
    const hiddenProviders = allProviders.filter((p) => p.hidden);

    if (providers.length === 0) {
      const p = document.createElement('p');
      p.className = 'muted small';
      p.textContent = 'No active usage providers with recent readings.';
      el.usageGrid.appendChild(p);
    }

    for (const prov of providers) {
      const card = document.createElement('div');
      card.className = 'usage-card';

      const head = document.createElement('div');
      head.className = 'usage-card-head';

      const title = document.createElement('div');
      title.className = 'usage-card-title';
      title.textContent = prov.name || prov.id;
      if (prov.plan) {
        const plan = document.createElement('span');
        plan.className = 'kind-tag';
        plan.textContent = prov.plan;
        title.appendChild(plan);
      }

      const badges = document.createElement('div');
      const st = document.createElement('span');
      let stCls = 'state-pill';
      if (prov.stale) stCls += ' warn';
      else if (prov.state === 'ready') stCls += ' ready';
      else if (prov.state === 'failed' || prov.state === 'executableNotFound' || prov.state === 'fileError') stCls += ' err';
      else stCls += ' warn';
      st.className = stCls;
      st.textContent = prov.stale ? 'stale' : prov.state;
      badges.appendChild(st);

      if (prov.source === 'fallback') {
        const fb = document.createElement('span');
        fb.className = 'kind-tag';
        fb.textContent = 'fallback';
        badges.append(' ', fb);
      }

      head.append(title, badges);
      card.appendChild(head);

      if (prov.error) {
        const errP = document.createElement('div');
        errP.className = prov.stale ? 'muted small' : 'inline-err';
        errP.textContent = prov.stale ? `Refresh failed (${prov.error}) — showing last reading.` : prov.error;
        card.appendChild(errP);
      } else if (prov.limitsApply === false) {
        const p = document.createElement('p');
        p.className = 'muted small';
        p.textContent = 'Plan limits do not apply to this account.';
        card.appendChild(p);
      }

      for (const meter of prov.meters || []) {
        if ((prov.meters || []).length > 1 || (meter.label && meter.label !== 'Plan limits')) {
          const ml = document.createElement('div');
          ml.className = 'usage-meter-label';
          ml.textContent = meter.label;
          card.appendChild(ml);
        }
        for (const win of meter.windows || []) {
          const wWrap = document.createElement('div');
          wWrap.className = 'usage-window';

          const row = document.createElement('div');
          row.className = 'usage-win-row';
          const left = document.createElement('span');
          const pct = win.usedPercent !== null ? `${Math.round(win.usedPercent)}%` : '—';
          const guard = findThreshold(standby.thresholds, prov.id, win);
          left.textContent = `${win.title}: ${pct}` + (guard !== null ? ` (guard ${guard}%)` : '');
          const right = document.createElement('span');
          right.className = 'muted small';
          right.textContent = fmtResetTime(win.resetsAt);
          row.append(left, right);

          const bar = document.createElement('progress');
          bar.className = 'usage-bar';
          bar.max = 100;
          bar.value = win.usedPercent !== null ? Math.max(0, Math.min(100, win.usedPercent)) : 0;
          if (guard !== null && win.usedPercent !== null) {
            if (win.usedPercent >= guard) bar.classList.add('over');
            else if (win.usedPercent >= guard * 0.85) bar.classList.add('warn');
          } else if (win.usedPercent !== null && win.usedPercent >= 90) {
            bar.classList.add('over');
          }

          wWrap.append(row, bar);
          card.appendChild(wWrap);
        }
      }

      if (prov.lastSuccessAt) {
        const foot = document.createElement('div');
        foot.className = 'inbox-meta';
        foot.textContent = `Last reading: ${fmtTime(Date.parse(prov.lastSuccessAt))}`;
        card.appendChild(foot);
      }

      el.usageGrid.appendChild(card);
    }

    if (el.usageHidden && hiddenProviders.length > 0) {
      const details = document.createElement('details');
      details.className = 'usage-hidden-note';
      const summary = document.createElement('summary');
      summary.className = 'muted small';
      summary.textContent = `${hiddenProviders.length} provider${hiddenProviders.length === 1 ? '' : 's'} hidden (not installed or failing)`;
      details.appendChild(summary);
      const ul = document.createElement('ul');
      ul.className = 'usage-hidden-list muted small';
      for (const hp of hiddenProviders) {
        const li = document.createElement('li');
        li.textContent = `${hp.name || hp.id}: ${hp.error || hp.state}`;
        ul.appendChild(li);
      }
      details.appendChild(ul);
      el.usageHidden.appendChild(details);
      el.usageHidden.hidden = false;
    }
  }

  async function refreshUsage(force) {
    try {
      const data = force
        ? await api('/api/usage/refresh', { method: 'POST' })
        : await api('/api/usage');
      if (routeWsId === null) {
        renderUsage(data);
      }
    } catch (err) {
      if (err.message !== 'unauthenticated' && force) {
        showBanner(`Could not refresh usage: ${err.message}`);
      }
    }
  }

  function makeTableElement(tbl) {
    const table = document.createElement('table');
    table.className = 'roadmap-table';
    const thead = document.createElement('thead');
    const htr = document.createElement('tr');
    for (const h of tbl.headers || []) {
      const th = document.createElement('th');
      th.textContent = h;
      htr.appendChild(th);
    }
    thead.appendChild(htr);
    table.appendChild(thead);

    const tbody = document.createElement('tbody');
    for (const row of tbl.rows || []) {
      const tr = document.createElement('tr');
      for (const cell of row) {
        const td = document.createElement('td');
        td.textContent = cell;
        tr.appendChild(td);
      }
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    return table;
  }

  function renderRoadmap(data) {
    el.roadmapBody.textContent = '';
    el.roadmapUpdated.textContent = '';
    el.roadmapCounts.textContent = '';

    if (data.status === 'unconfigured') {
      const p = document.createElement('p');
      p.className = 'muted';
      p.textContent = 'No roadmapPath configured for this workspace.';
      el.roadmapBody.appendChild(p);
      return;
    }
    if (data.status !== 'ok' || !data.roadmap) {
      const p = document.createElement('p');
      p.className = 'inline-err';
      p.textContent = data.error || 'Could not load roadmap.';
      el.roadmapBody.appendChild(p);
      return;
    }

    const rm = data.roadmap;
    el.roadmapCounts.textContent = `(${rm.pipeline.length} in pipeline · ${rm.done.length} done)`;
    if (rm.updatedNote) {
      el.roadmapUpdated.textContent = rm.updatedNote;
    }

    const activeSections = [];
    let doneSection = null;
    for (const sec of rm.sections || []) {
      if (sec.heading.toLowerCase() === 'done') {
        doneSection = sec;
      } else if (sec.tables.length > 0 || sec.listItems.length > 0) {
        activeSections.push(sec);
      }
    }

    for (const sec of activeSections) {
      const block = document.createElement('div');
      block.className = 'roadmap-block';
      const h3 = document.createElement('h3');
      h3.textContent = sec.heading;
      block.appendChild(h3);

      for (const tbl of sec.tables) {
        if (tbl.milestone) {
          const h4 = document.createElement('h4');
          h4.textContent = tbl.milestone;
          block.appendChild(h4);
        }
        if (tbl.rows.length > 0) {
          block.appendChild(makeTableElement(tbl));
        } else if (tbl.milestone) {
          const empty = document.createElement('p');
          empty.className = 'muted small';
          empty.textContent = 'Completed.';
          block.appendChild(empty);
        }
      }

      if (sec.listItems.length > 0) {
        const ul = document.createElement('ul');
        for (const item of sec.listItems) {
          const li = document.createElement('li');
          li.textContent = (item.checked === true ? '[done] ' : item.checked === false ? '[ ] ' : '') + item.text;
          ul.appendChild(li);
        }
        block.appendChild(ul);
      }

      el.roadmapBody.appendChild(block);
    }

    if (doneSection && (doneSection.tables.length > 0 || doneSection.listItems.length > 0)) {
      const details = document.createElement('details');
      details.className = 'roadmap-details';
      const summary = document.createElement('summary');
      summary.textContent = `Done (${rm.done.length})`;
      details.appendChild(summary);
      for (const tbl of doneSection.tables) {
        if (tbl.rows.length > 0) details.appendChild(makeTableElement(tbl));
      }
      if (doneSection.listItems.length > 0) {
        const ul = document.createElement('ul');
        for (const item of doneSection.listItems) {
          const li = document.createElement('li');
          li.textContent = item.text;
          ul.appendChild(li);
        }
        details.appendChild(ul);
      }
      el.roadmapBody.appendChild(details);
    }

    if (el.roadmapBody.children.length === 0) {
      const p = document.createElement('p');
      p.className = 'muted';
      p.textContent = 'Roadmap has no tables or list items yet.';
      el.roadmapBody.appendChild(p);
    }
  }

  async function refreshRoadmap() {
    if (!routeWsId) return;
    const target = routeWsId;
    try {
      const data = await api(`/api/workspaces/${encodeURIComponent(target)}/roadmap`);
      if (routeWsId === target && wsTab === 'roadmap') {
        renderRoadmap(data);
      }
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not load roadmap: ${err.message}`);
      }
    }
  }

  function renderChat(messages) {
    chatMessages = messages || [];
    const ws = workspaces.find((w) => w.id === routeWsId);
    el.chatAgent.textContent = ws ? `· ${ws.maestroAgent}` : '';
    el.chatEmpty.hidden = chatMessages.length > 0;

    const draftKey = routeWsId ? `chat:${routeWsId}` : null;
    if (draftKey && document.activeElement !== el.chatInput) {
      el.chatInput.value = Object.hasOwn(drafts, draftKey) ? drafts[draftKey] : '';
    }

    const existing = new Map();
    for (const node of Array.from(el.chatList.children)) {
      if (node.dataset.msgId) existing.set(node.dataset.msgId, node);
    }
    const desiredIds = new Set(chatMessages.map((m) => m.id));
    for (const [id, node] of existing) {
      if (!desiredIds.has(id)) node.remove();
    }

    let appended = false;
    for (const msg of chatMessages) {
      if (existing.has(msg.id)) continue;
      appended = true;
      const box = document.createElement('div');
      box.className = `chat-msg ${msg.sender}`;
      box.dataset.msgId = msg.id;
      box.dataset.ws = msg.workspaceId;

      const head = document.createElement('div');
      head.className = 'chat-msg-head';
      const author = document.createElement('span');
      author.className = 'chat-msg-author';
      author.textContent = msg.author;
      const ts = document.createElement('span');
      ts.textContent = fmtTime(msg.createdAt);
      head.append(author, ts);

      const md = document.createElement('div');
      md.className = 'md';
      md.innerHTML = msg.bodyHtml;

      box.append(head, md);
      el.chatList.appendChild(box);
    }
    if (appended) {
      el.chatList.scrollTop = el.chatList.scrollHeight;
    }
  }

  async function refreshChat() {
    if (!routeWsId) return;
    const target = routeWsId;
    try {
      const data = await api(`/api/workspaces/${encodeURIComponent(target)}/chat`);
      if (routeWsId === target && wsTab === 'chat') {
        renderChat(data.messages || []);
      }
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not load chat: ${err.message}`);
      }
    }
  }

  async function submitChat() {
    if (!routeWsId) return;
    const target = routeWsId;
    const text = el.chatInput.value.trim();
    if (!text) {
      el.chatErr.textContent = 'Please write a message first.';
      el.chatInput.focus();
      return;
    }
    el.chatErr.textContent = '';
    el.chatSend.disabled = true;
    try {
      const res = await api(`/api/workspaces/${encodeURIComponent(target)}/chat`, {
        method: 'POST',
        body: JSON.stringify({ body: el.chatInput.value }),
      });
      clearDraft(`chat:${target}`);
      el.chatInput.value = '';
      if (routeWsId === target) {
        renderChat([...chatMessages, res.message]);
      }
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        el.chatErr.textContent = err.message;
      }
    } finally {
      el.chatSend.disabled = false;
    }
  }

  function fmtAuditDetail(detail) {
    if (detail === null || detail === undefined || detail === '') return '';
    if (typeof detail === 'string') return detail;
    try {
      return JSON.stringify(detail);
    } catch {
      return String(detail);
    }
  }

  async function refreshAudit() {
    try {
      const data = await api('/api/audit?limit=100');
      const entries = Array.isArray(data.entries) ? data.entries : [];
      el.auditCount.textContent = `(${entries.length})`;
      el.auditBody.textContent = '';
      if (entries.length === 0) {
        const p = document.createElement('p');
        p.className = 'muted small';
        p.textContent = 'No audit events recorded yet.';
        el.auditBody.appendChild(p);
        return;
      }
      el.auditBody.appendChild(
        makeTableElement({
          headers: ['Time', 'Event', 'IP', 'Detail'],
          rows: entries.map((e) => [
            fmtTime(e.ts ?? e.createdAt),
            String(e.event ?? ''),
            e.ip || 'local',
            fmtAuditDetail(e.detail),
          ]),
        }),
      );
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not load audit log: ${err.message}`);
      }
    }
  }

  async function triggerKillSwitch() {
    el.killConfirmBtn.disabled = true;
    try {
      await api('/api/kill-switch', { method: 'POST' });
      window.location.href = '/login';
    } catch (err) {
      el.killConfirmBtn.disabled = false;
      if (err.message !== 'unauthenticated') {
        showBanner(`Kill switch failed: ${err.message}`);
      }
    }
  }

  async function poll() {
    try {
      await refreshWorkspaces();
      if (routeWsId === null) {
        await Promise.all([refreshUsage(false), refreshInbox()]);
      } else if (wsTab === 'roadmap') {
        await refreshRoadmap();
      } else if (wsTab === 'chat') {
        await refreshChat();
      } else {
        await refreshInbox();
      }
      if (!el.auditSection.hidden) {
        await refreshAudit();
      }
      el.sync.textContent = `Updated ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`;
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        el.sync.textContent = 'Offline / sync paused';
      }
    }
  }

  function switchTab(targetTab) {
    const targetWs = routeWsId || homeFilterWsId || (workspaces[0] ? workspaces[0].id : null);
    if (!targetWs) return;
    openWorkspace(targetWs, targetTab);
  }

  el.backHome.addEventListener('click', () => openWorkspace(null));
  el.refreshUsage.addEventListener('click', () => void refreshUsage(true));
  el.tabInbox.addEventListener('click', () => switchTab('inbox'));
  el.tabChat.addEventListener('click', () => switchTab('chat'));
  el.tabRoadmap.addEventListener('click', () => switchTab('roadmap'));
  el.chatInput.addEventListener('input', () => {
    el.chatErr.textContent = '';
    if (routeWsId) setDraft(`chat:${routeWsId}`, el.chatInput.value);
  });
  el.chatInput.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      void submitChat();
    }
  });
  el.chatForm.addEventListener('submit', (e) => {
    e.preventDefault();
    void submitChat();
  });

  el.killBtn.addEventListener('click', () => {
    el.killConfirm.hidden = !el.killConfirm.hidden;
  });
  el.killCancelBtn.addEventListener('click', () => {
    el.killConfirm.hidden = true;
  });
  el.killConfirmBtn.addEventListener('click', () => void triggerKillSwitch());

  function setAuditOpen(open) {
    el.auditSection.hidden = !open;
    el.toggleAudit.classList.toggle('active', open);
    el.toggleAudit.setAttribute('aria-expanded', String(open));
    if (open) {
      void refreshAudit();
    }
  }

  el.toggleAudit.addEventListener('click', () => {
    setAuditOpen(el.auditSection.hidden);
  });
  el.auditRefresh.addEventListener('click', () => void refreshAudit());
  el.auditClose.addEventListener('click', () => {
    setAuditOpen(false);
  });

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
    const { wsId, tab } = readRouteFromHash();
    if (wsId !== routeWsId || tab !== wsTab) {
      routeWsId = wsId;
      wsTab = tab;
      showHistory = false;
      el.historySection.hidden = true;
      renderNavigation();
      if (routeWsId === null) {
        void refreshUsage(false);
        void refreshInbox();
      } else if (wsTab === 'roadmap') {
        void refreshRoadmap();
      } else if (wsTab === 'chat') {
        void refreshChat();
      } else {
        void refreshInbox();
      }
    }
  });

  void poll();
  setInterval(() => {
    if (!document.hidden) void poll();
  }, POLL_MS);
})();
