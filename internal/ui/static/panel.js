(() => {
  const body = document.body;
  if (!body || !body.dataset.csrf) return;

  const csrf = body.dataset.csrf;
  const DRAFT_PREFIX = 'maestria.draft.';
  let pendingRefresh = false;
  let refreshTimer = null;
  let refreshing = false;
  let overviewFilter = 'all';
  let agentsFilter = 'all';

  function draftStorageKey(el) {
    const k = el.getAttribute('data-draft-key');
    return k ? DRAFT_PREFIX + k : '';
  }

  function saveDraft(el) {
    const key = draftStorageKey(el);
    if (!key) return;
    try {
      if (el.value) localStorage.setItem(key, el.value);
      else localStorage.removeItem(key);
    } catch {}
  }

  function restoreDrafts(root = document) {
    root.querySelectorAll('[data-draft-key]').forEach((el) => {
      const key = draftStorageKey(el);
      if (!key) return;
      try {
        const saved = localStorage.getItem(key);
        if (saved !== null && saved !== el.value) {
          el.value = saved;
          if (el.tagName === 'TEXTAREA' && el.id === 'msg') {
            resizeChatInput(el);
          }
        }
      } catch {}
    });
  }

  function clearDraft(el) {
    if (!el) return;
    const key = draftStorageKey(el);
    if (!key) return;
    try {
      localStorage.removeItem(key);
    } catch {}
  }

  function resizeChatInput(el) {
    const lines = Math.max(1, el.value.split('\n').length);
    el.rows = Math.min(6, lines);
  }

  function isUserBusy() {
    const active = document.activeElement;
    if (active && active.closest('#main')) {
      const tag = active.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') {
        return true;
      }
    }
    if (document.querySelector('#main details.menu[open]')) {
      return true;
    }
    if (document.querySelector('dialog[open]')) {
      return true;
    }
    return false;
  }

  function applyOverviewFilter(root = document) {
    const row = root.querySelector('.chip-row[data-filter="needs-list"]');
    const list = root.querySelector('#needs-list');
    if (!row || !list) return;

    const chips = [...row.querySelectorAll('.chip')];
    const hasMatch = chips.some((c) => c.dataset.value === overviewFilter);
    if (!hasMatch) overviewFilter = 'all';

    chips.forEach((c) => {
      c.setAttribute('aria-pressed', c.dataset.value === overviewFilter ? 'true' : 'false');
    });
    list.querySelectorAll('.task').forEach((item) => {
      item.hidden = overviewFilter !== 'all' && item.dataset.key !== overviewFilter;
    });
  }

  function applyAgentsFilter(root = document) {
    const row = root.querySelector('.chip-row[data-filter="agents"]');
    const wrap = root.querySelector('#agents');
    if (!row || !wrap) return;
    const chips = [...row.querySelectorAll('.chip')];
    if (!chips.some((c) => c.dataset.value === agentsFilter)) agentsFilter = 'all';
    chips.forEach((c) => {
      c.setAttribute('aria-pressed', c.dataset.value === agentsFilter ? 'true' : 'false');
    });
    wrap.querySelectorAll('li[data-key]').forEach((item) => {
      item.hidden = agentsFilter !== 'all' && item.dataset.key !== agentsFilter;
    });
    // Hide a floor card whose agents are all filtered out.
    wrap.querySelectorAll('section.card').forEach((card) => {
      const items = [...card.querySelectorAll('li[data-key]')];
      card.hidden = items.length > 0 && items.every((i) => i.hidden);
    });
  }

  function showError(el, message) {
    if (!el) return;
    el.textContent = message || '';
    el.hidden = !message;
  }

  function syncDocsNavPicker(root = document) {
    if (window.matchMedia('(max-width: 1023px)').matches) {
      root.querySelectorAll('.docs-nav[open]').forEach((d) => {
        d.removeAttribute('open');
      });
    }
  }

  function captureOpenDisclosures() {
    const open = new Set();
    document.querySelectorAll('details[data-disclosure-id][open]').forEach((d) => {
      open.add(d.getAttribute('data-disclosure-id'));
    });
    return open;
  }

  function restoreOpenDisclosures(open, root = document) {
    if (!open || open.size === 0) return;
    root.querySelectorAll('details[data-disclosure-id]').forEach((d) => {
      if (open.has(d.getAttribute('data-disclosure-id'))) {
        d.setAttribute('open', '');
      }
    });
  }

  // In hosted mode the page sets window.MaestriaHost: it signs requests for a machine and builds
  // the page from several machines. Local mode never sets it and nothing below changes.
  const host = window.MaestriaHost || null;

  async function api(url, opts = {}) {
    if (host) return host.api(url, opts);
    const headers = {
      'X-CSRF-Token': csrf,
      'Accept': 'application/json',
      ...(opts.headers || {}),
    };
    if (opts.body !== undefined && typeof opts.body !== 'string') {
      headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(opts.body);
    }
    const res = await fetch(url, { credentials: 'same-origin', ...opts, headers });
    let data = null;
    try {
      data = await res.json();
    } catch {}
    if (!res.ok) {
      const msg = (data && data.error) || `Request failed (${res.status})`;
      throw new Error(msg);
    }
    return data;
  }

  async function refreshNow(force = false) {
    if (!force && isUserBusy()) {
      pendingRefresh = true;
      return;
    }
    if (refreshing) {
      pendingRefresh = true;
      return;
    }
    refreshing = true;
    pendingRefresh = false;

    try {
      let html;
      if (host) {
        html = await host.page();
        if (html == null) return;
        if (!force && isUserBusy()) {
          pendingRefresh = true;
          return;
        }
      } else {
        const res = await fetch(location.href, {
          credentials: 'same-origin',
          headers: { 'X-Requested-With': 'maestria-live' },
        });
        if (!res.ok) {
          if (res.status === 401 || res.redirected) {
            location.href = res.url || '/login';
          }
          return;
        }
        if (!force && isUserBusy()) {
          pendingRefresh = true;
          return;
        }
        html = await res.text();
      }
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const openDisclosures = captureOpenDisclosures();

      const curChat = document.getElementById('chat-log');
      const prevKeys = chatKeys(curChat);
      const prevScrollY = window.scrollY;

      const swap = (sel) => {
        const cur = document.querySelector(sel);
        const next = doc.querySelector(sel);
        if (cur && next) cur.replaceWith(next);
      };
      swap('aside.sidebar .nav');
      swap('header.topbar .scope');
      swap('nav.bottomnav');
      swap('main#main');

      const newMain = document.getElementById('main');
      restoreOpenDisclosures(openDisclosures, newMain);
      restoreDrafts(newMain);
      applyOverviewFilter(newMain);
      applyAgentsFilter(newMain);
      syncDocsNavPicker(newMain);

      const newChat = document.getElementById('chat-log');
      if (newChat && (!curChat || chatPinned)) {
        // Opening the chat, or following it: stay on the newest message.
        if (!curChat) chatUnseen = 0;
        scrollChatToLatest();
      } else {
        if (newChat) {
          // Reading older messages: keep the place and count what arrived below.
          chatUnseen += countNewKeys(prevKeys, chatKeys(newChat));
        }
        window.scrollTo(0, prevScrollY);
      }
      syncChatJump();
    } catch {
      // Transient error; next SSE event will retry.
    } finally {
      refreshing = false;
      if (pendingRefresh && !isUserBusy()) {
        refreshNow(false);
      }
    }
  }

  // ---- Chat scroll: follow the newest message until the reader scrolls up ----
  // Pinned (default): the view stays on the newest message as new ones arrive.
  // Scrolling up unpins it; a "Latest" button appears and counts what arrived below.
  // Reaching the bottom again (button or manual scroll) pins it again.
  let chatPinned = true;
  let chatUnseen = 0;
  const CHAT_BOTTOM_SLACK = 48;

  function chatAtBottom() {
    return window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - CHAT_BOTTOM_SLACK;
  }

  function chatKeys(log) {
    if (!log) return [];
    return Array.from(log.querySelectorAll('[data-msg-key], [data-msg-id]'), (el) => el.dataset.msgKey || el.dataset.msgId);
  }

  function countNewKeys(before, after) {
    const seen = new Set(before);
    return after.filter((k) => !seen.has(k)).length;
  }

  function scrollChatToLatest() {
    chatPinned = true;
    chatUnseen = 0;
    window.scrollTo(0, document.documentElement.scrollHeight);
  }

  function syncChatJump() {
    const btn = document.querySelector('[data-chat-jump]');
    if (!btn) return;
    const show = !chatPinned && document.getElementById('chat-log') != null;
    btn.hidden = !show;
    const label = btn.querySelector('[data-chat-jump-label]');
    if (chatUnseen > 0) {
      btn.dataset.new = '';
      if (label) label.textContent = chatUnseen === 1 ? '1 new message' : `${chatUnseen} new messages`;
    } else {
      delete btn.dataset.new;
      if (label) label.textContent = 'Latest';
    }
  }

  window.addEventListener('scroll', () => {
    if (!document.getElementById('chat-log')) return;
    const atBottom = chatAtBottom();
    if (atBottom !== chatPinned || (atBottom && chatUnseen)) {
      chatPinned = atBottom;
      if (atBottom) chatUnseen = 0;
      syncChatJump();
    }
  }, { passive: true });

  document.addEventListener('click', (e) => {
    if (e.target.closest && e.target.closest('[data-chat-jump]')) {
      e.preventDefault();
      scrollChatToLatest();
      syncChatJump();
    }
  });

  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => refreshNow(false), 90);
  }

  function setBusy(el, busy) {
    if (!el) return;
    el.querySelectorAll('button, input, textarea, select').forEach((ctrl) => {
      if (busy) ctrl.dataset.wasDisabled = ctrl.disabled ? '1' : '0';
      ctrl.disabled = busy ? true : ctrl.dataset.wasDisabled === '1';
    });
  }

  function syncWindowControls(setting) {
    const toggle = setting.querySelector('[data-sb-toggle]');
    if (!toggle) return;
    const enabled = toggle.checked;
    setting.querySelectorAll('[data-sb-threshold], [data-sb-action], [data-sb-wake]').forEach((el) => {
      el.disabled = !enabled;
    });
  }

  async function saveStandby(card, statusEl) {
    if (statusEl) statusEl.textContent = 'Saving…';
    const current = await api('/api/usage', { method: 'GET' });
    const cfg = (current && current.standbyConfig) || {};
    const thresholds = structuredClone(cfg.thresholds || {});
    const actions = structuredClone(cfg.actions || {});
    let wakeOnReset = !!cfg.wakeOnReset;

    // Expand any provider-level defaults onto the provider's visible accounts before editing them,
    // so disabling one account/window does not fall back to a hidden provider rule.
    const visibleProviders = new Set();
    const accountsByProvider = new Map();
    document.querySelectorAll('[data-usage-card]').forEach((c) => {
      const ak = c.dataset.accountKey;
      const prov = c.dataset.provider;
      if (!ak || !prov) return;
      visibleProviders.add(prov);
      if (!accountsByProvider.has(prov)) accountsByProvider.set(prov, new Set());
      accountsByProvider.get(prov).add(ak);
    });
    visibleProviders.forEach((prov) => {
      const pTh = thresholds[prov];
      const pAct = actions[prov];
      const accts = accountsByProvider.get(prov) || new Set();
      if (pTh) {
        accts.forEach((ak) => {
          thresholds[ak] = { ...pTh, ...(thresholds[ak] || {}) };
        });
        delete thresholds[prov];
      }
      if (pAct) {
        accts.forEach((ak) => {
          actions[ak] = { ...pAct, ...(actions[ak] || {}) };
        });
        delete actions[prov];
      }
    });

    document.querySelectorAll('[data-window-setting]').forEach((setting) => {
      const ak = setting.dataset.accountKey;
      const wid = setting.dataset.windowId;
      if (!ak || !wid) return;

      const toggle = setting.querySelector('[data-sb-toggle]');
      const select = setting.querySelector('[data-sb-threshold]');
      const actionRadio = setting.querySelector('[data-sb-action]:checked');
      const wakeEl = setting.querySelector('[data-sb-wake]');
      if (wakeEl && toggle && toggle.checked) {
        wakeOnReset = wakeEl.checked;
      }

      if (toggle && toggle.checked) {
        if (!thresholds[ak]) thresholds[ak] = {};
        if (!actions[ak]) actions[ak] = {};
        thresholds[ak][wid] = parseInt((select && select.value) || '90', 10);
        actions[ak][wid] = (actionRadio && actionRadio.value) || 'tell';
      } else {
        if (thresholds[ak]) {
          delete thresholds[ak][wid];
          delete thresholds[ak]['*'];
          if (Object.keys(thresholds[ak]).length === 0) delete thresholds[ak];
        }
        if (actions[ak]) {
          delete actions[ak][wid];
          delete actions[ak]['*'];
          if (Object.keys(actions[ak]).length === 0) delete actions[ak];
        }
      }
    });

    // If every visible guard is off, allow saving wakeOnReset from the clicked card's wake box.
    const firstWake = card && card.querySelector('[data-sb-wake]');
    if (firstWake && Object.keys(thresholds).length === 0) {
      wakeOnReset = firstWake.checked;
    }

    await api('/api/standby/config', {
      method: 'POST',
      body: { thresholds, actions, wakeOnReset },
    });
    if (statusEl) statusEl.textContent = 'Saved';
    await refreshNow(true);
  }

  document.addEventListener('input', (e) => {
    const target = e.target;
    if (target.matches && target.matches('[data-draft-key]')) {
      saveDraft(target);
      if (target.tagName === 'TEXTAREA' && target.id === 'msg') {
        resizeChatInput(target);
      }
    }
  });

  document.addEventListener('change', (e) => {
    const target = e.target;
    if (target.matches('[data-sb-toggle]')) {
      const setting = target.closest('[data-window-setting]');
      if (setting) syncWindowControls(setting);
    } else if (target.matches('[data-sb-wake]')) {
      const checked = target.checked;
      document.querySelectorAll('[data-sb-wake]').forEach((cb) => {
        cb.checked = checked;
      });
    }
  });

  document.addEventListener('focusout', () => {
    setTimeout(() => {
      if (pendingRefresh && !isUserBusy()) refreshNow(false);
    }, 60);
  });

  document.addEventListener('click', async (e) => {
    const chip = e.target.closest('.chip-row[data-filter="needs-list"] .chip');
    if (chip) {
      overviewFilter = chip.dataset.value || 'all';
      applyOverviewFilter(document);
      return;
    }

    const agentChip = e.target.closest('.chip-row[data-filter="agents"] .chip');
    if (agentChip) {
      agentsFilter = agentChip.dataset.value || 'all';
      applyAgentsFilter(document);
      return;
    }

    const copyBtn = e.target.closest('[data-copy]');
    if (copyBtn) {
      const code = copyBtn.parentElement && copyBtn.parentElement.querySelector('code');
      if (code && navigator.clipboard) {
        navigator.clipboard.writeText(code.textContent).then(() => {
          copyBtn.setAttribute('title', 'Copied');
          setTimeout(() => copyBtn.setAttribute('title', 'Copy command'), 1500);
        }).catch(() => {});
      }
      return;
    }

    const btn = e.target.closest('[data-action]');
    if (!btn || btn.tagName === 'FORM') return;
    const action = btn.dataset.action;

    if (action === 'agent-answer') {
      const agent = btn.closest('.agent');
      if (!agent) return;
      const wsId = agent.dataset.wsId;
      const term = agent.dataset.terminalId;
      const errEl = agent.querySelector('[data-agent-error]');
      showError(errEl, '');
      setBusy(agent, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/agents/${encodeURIComponent(term)}/${btn.dataset.answer === 'approve' ? 'approve' : 'reject'}`, {
          method: 'POST',
          body: { promptHash: btn.dataset.promptHash || '' },
        });
        await refreshNow(true);
      } catch (err) {
        setBusy(agent, false);
        showError(errEl, err.message);
        await refreshNow(true);
      }
      return;
    }

    if (action === 'respond') {
      const task = btn.closest('.task');
      if (!task) return;
      const wsId = task.dataset.wsId;
      const itemId = task.dataset.itemId;
      const value = btn.dataset.value;
      const commentEl = task.querySelector('textarea[data-draft-key]');
      const comment = commentEl ? commentEl.value.trim() : '';
      setBusy(task, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/${encodeURIComponent(itemId)}/respond`, {
          method: 'POST',
          body: { response: value, comment },
        });
        clearDraft(commentEl);
        await refreshNow(true);
      } catch (err) {
        setBusy(task, false);
        showError(task.querySelector('[data-task-error]'), err.message);
      }
      return;
    }

    if (action === 'postpone') {
      const task = btn.closest('.task');
      if (!task) return;
      const menu = btn.closest('details.menu');
      if (menu) menu.removeAttribute('open');
      const wsId = task.dataset.wsId;
      const itemId = task.dataset.itemId;
      const preset = btn.dataset.preset || '1h';
      setBusy(task, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/${encodeURIComponent(itemId)}/postpone`, {
          method: 'POST',
          body: { preset },
        });
        await refreshNow(true);
      } catch {
        setBusy(task, false);
      }
      return;
    }

    if (action === 'dismiss') {
      const task = btn.closest('.task');
      if (!task) return;
      const wsId = task.dataset.wsId;
      const itemId = task.dataset.itemId;
      setBusy(task, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/${encodeURIComponent(itemId)}/dismiss`, {
          method: 'POST',
          body: {},
        });
        await refreshNow(true);
      } catch {
        setBusy(task, false);
      }
      return;
    }

    if (action === 'dismiss-all') {
      const wsId = btn.dataset.wsId;
      btn.disabled = true;
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/dismiss-notices`, {
          method: 'POST',
          body: {},
        });
        await refreshNow(true);
      } catch {
        btn.disabled = false;
      }
      return;
    }

    if (action === 'save-standby') {
      const card = btn.closest('[data-usage-card]');
      const statusEl = card ? card.querySelector('[data-sb-status]') : null;
      btn.disabled = true;
      try {
        await saveStandby(card, statusEl);
      } catch (err) {
        if (statusEl) statusEl.textContent = err.message || 'Save failed';
      } finally {
        btn.disabled = false;
      }
      return;
    }

    if (action === 'resume-standby') {
      const guardKey = btn.dataset.guardKey || '';
      btn.disabled = true;
      try {
        await api('/api/standby/resume', {
          method: 'POST',
          body: { guardKey },
        });
        await refreshNow(true);
      } catch {
        btn.disabled = false;
      }
    }
  });

  document.addEventListener('submit', async (e) => {
    const form = e.target;

    if (form.matches('[data-action="respond-input"]')) {
      e.preventDefault();
      const task = form.closest('.task');
      const input = form.querySelector('input');
      if (!task || !input) return;
      const val = input.value.trim();
      if (!val) {
        input.focus();
        return;
      }
      const wsId = task.dataset.wsId;
      const itemId = task.dataset.itemId;
      setBusy(task, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/${encodeURIComponent(itemId)}/respond`, {
          method: 'POST',
          body: { response: val },
        });
        clearDraft(input);
        await refreshNow(true);
      } catch {
        setBusy(task, false);
      }
      return;
    }

    if (form.matches('[data-action="agent-prompt"]')) {
      e.preventDefault();
      const agent = form.closest('.agent');
      const input = form.querySelector('input');
      if (!agent || !input) return;
      const text = input.value.trim();
      if (!text) {
        input.focus();
        return;
      }
      const errEl = agent.querySelector('[data-agent-error]');
      showError(errEl, '');
      setBusy(form, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(agent.dataset.wsId)}/agents/${encodeURIComponent(agent.dataset.terminalId)}/prompt`, {
          method: 'POST',
          body: { body: text },
        });
        input.value = '';
        clearDraft(input);
        showError(errEl, 'Sent.');
      } catch (err) {
        showError(errEl, err.message);
      } finally {
        setBusy(form, false);
      }
      return;
    }

    if (form.matches('[data-chat-form]')) {
      e.preventDefault();
      const wsId = form.dataset.wsId;
      const ta = form.querySelector('textarea');
      if (!ta || !wsId) return;
      const bodyText = ta.value.trim();
      if (!bodyText) {
        ta.focus();
        return;
      }
      const chatErr = document.querySelector('[data-chat-error]');
      showError(chatErr, '');
      setBusy(form, true);
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/chat`, {
          method: 'POST',
          body: { body: bodyText },
        });
        ta.value = '';
        ta.rows = 1;
        clearDraft(ta);
        await refreshNow(true);
        scrollChatToLatest();
        syncChatJump();
      } catch (err) {
        // The text stays in the box: when delivery is unconfirmed the person decides whether to resend.
        showError(chatErr, err.message);
      } finally {
        setBusy(form, false);
        ta.focus();
      }
    }
  });

  document.addEventListener('keydown', (e) => {
    const target = e.target;
    if (target && target.id === 'msg' && e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      const form = target.closest('[data-chat-form]');
      if (form) form.requestSubmit();
    }
  });

  document.addEventListener('toggle', (e) => {
    if (e.target.matches('details.menu') && !e.target.open && pendingRefresh && !isUserBusy()) {
      refreshNow(false);
    }
  }, true);

  restoreDrafts(document);
  applyOverviewFilter(document);
  applyAgentsFilter(document);
  syncDocsNavPicker(document);
  if (document.getElementById('chat-log')) {
    // Open the chat on the newest message (again after layout settles, for images/fonts).
    scrollChatToLatest();
    requestAnimationFrame(() => { if (chatPinned) scrollChatToLatest(); syncChatJump(); });
  }

  window.MaestriaPanel = { refresh: scheduleRefresh, refreshNow, applyFilters: (root) => { applyOverviewFilter(root); applyAgentsFilter(root); restoreDrafts(root); } };

  if (typeof EventSource === 'function' && !host) {
    const es = new EventSource('/api/stream');
    ['update', 'refresh', 'inbox', 'chat', 'standby', 'usage', 'workspaces', 'floors'].forEach((ev) => {
      es.addEventListener(ev, scheduleRefresh);
    });
    es.onmessage = scheduleRefresh;
  }
})();
