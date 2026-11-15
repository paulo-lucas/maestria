(() => {
  const body = document.body;
  if (!body || !body.dataset.csrf) return;

  const csrf = body.dataset.csrf;
  const DRAFT_PREFIX = 'maestria.draft.';
  let pendingRefresh = false;
  let focusAfterRedraw = null; // { id, until }: see settleFocus
  let refreshTimer = null;
  let refreshing = false;
  // The workspace chosen in each overview list's filter chips ("needs-list", "updates-list").
  const overviewFilters = { 'needs-list': 'all', 'updates-list': 'all' };
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
    if (document.querySelector('#main details.menu[open], #main [data-menu] > .menu__panel:popover-open')) {
      return true;
    }
    if (document.querySelector('dialog[open]')) {
      return true;
    }
    return false;
  }

  function applyOverviewFilter(root = document) {
    Object.keys(overviewFilters).forEach((name) => {
      const row = root.querySelector('.chip-row[data-filter="' + name + '"]');
      const list = root.querySelector('#' + name);
      if (!row || !list) return;
      const chips = [...row.querySelectorAll('.chip')];
      if (!chips.some((c) => c.dataset.value === overviewFilters[name])) overviewFilters[name] = 'all';
      const cur = overviewFilters[name];
      chips.forEach((c) => {
        c.setAttribute('aria-pressed', c.dataset.value === cur ? 'true' : 'false');
      });
      list.querySelectorAll('.task').forEach((item) => {
        item.hidden = cur !== 'all' && item.dataset.key !== cur;
      });
      if (name === 'updates-list') {
        // The button acts on what the filter shows: its label says so.
        const btn = root.querySelector('[data-action="dismiss-updates"]');
        const label = btn && btn.querySelector('[data-dismiss-label]');
        if (label) {
          const chip = chips.find((c) => c.dataset.value === cur);
          // The chip's own text is the workspace name (its swatch and count are elements).
          const ws = cur === 'all' || !chip ? '' : [...chip.childNodes].filter((n) => n.nodeType === 3).map((n) => n.textContent).join('').trim();
          label.textContent = ws ? 'Mark ' + ws + ' as read' : 'Mark all as read';
          btn.setAttribute('aria-label', label.textContent);
        }
      }
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
      const prevChatScroll = chatScroller() ? chatScroller().scrollTop : 0;
      // Where the reader is, by message. A log without messages (still connecting, say) keeps
      // the last known place instead of forgetting it.
      if (curChat && !chatPinned && prevKeys.length) chatAnchor = captureChatAnchor(curChat) || chatAnchor;

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
      reapplyBusy(newMain);
      syncDocsNavPicker(newMain);
      applyFocusAfterRedraw();

      const newChat = document.getElementById('chat-log');
      if (newChat && (!curChat || chatPinned)) {
        // Opening the chat, or following it: stay on the newest message.
        if (!curChat) chatUnseen = 0;
        scrollChatToLatest();
      } else {
        if (newChat) {
          // Reading older messages: keep the place and count what arrived below. Pixels are not
          // enough: the log may have been empty for a moment (reconnecting after the tab was in
          // the background), and its scroll position then reads 0, the very first message.
          const newKeys = chatKeys(newChat);
          if (prevKeys.length) chatUnseen += countNewKeys(prevKeys, newKeys);
          if (newKeys.length && !restoreChatAnchor(newChat, chatAnchor) && prevKeys.length) newChat.scrollTop = prevChatScroll;
        } else {
          window.scrollTo(0, prevScrollY);
        }
      }
      const localLog = document.querySelector('.chat-page > .chat-local');
      if (localLog && !curChat) localLog.scrollTop = localLog.scrollHeight;
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

  // The message list scrolls on its own (header and composer stay put), so all of this works on
  // the #chat-log element, not the window.
  function chatScroller() {
    return document.getElementById('chat-log');
  }

  function chatAtBottom() {
    const el = chatScroller();
    if (!el) return true;
    return el.scrollTop + el.clientHeight >= el.scrollHeight - CHAT_BOTTOM_SLACK;
  }

  function chatKeys(log) {
    if (!log) return [];
    return Array.from(log.querySelectorAll('[data-msg-key], [data-msg-id]'), (el) => el.dataset.msgKey || el.dataset.msgId);
  }

  function countNewKeys(before, after) {
    const seen = new Set(before);
    return after.filter((k) => !seen.has(k)).length;
  }

  // chatAnchor is the message at the top of the reader's view and how far below the log's top
  // edge it sits, kept while the chat is unpinned so a redraw can put the reader back on it.
  let chatAnchor = null;

  function captureChatAnchor(log) {
    if (!log) return null;
    const top = log.getBoundingClientRect().top;
    for (const el of log.querySelectorAll('[data-msg-key], [data-msg-id]')) {
      const r = el.getBoundingClientRect();
      if (r.bottom > top) return { key: el.dataset.msgKey || el.dataset.msgId, offset: r.top - top };
    }
    return null;
  }

  // restoreChatAnchor scrolls log so the anchored message sits where it was. It returns false
  // when that message is not in the log.
  function restoreChatAnchor(log, anchor) {
    if (!log || !anchor) return false;
    const el = Array.from(log.querySelectorAll('[data-msg-key], [data-msg-id]'))
      .find((m) => (m.dataset.msgKey || m.dataset.msgId) === anchor.key);
    if (!el) return false;
    log.scrollTop += el.getBoundingClientRect().top - log.getBoundingClientRect().top - anchor.offset;
    return true;
  }

  function scrollChatToLatest() {
    chatPinned = true;
    chatUnseen = 0;
    chatAnchor = null;
    const el = chatScroller();
    if (el) el.scrollTop = el.scrollHeight;
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

  // Scroll events don't bubble: listen in the capture phase so a swapped-in #chat-log is covered.
  document.addEventListener('scroll', (e) => {
    if (!e.target || e.target.id !== 'chat-log') return;
    const atBottom = chatAtBottom();
    if (!atBottom && chatKeys(e.target).length) chatAnchor = captureChatAnchor(e.target);
    if (atBottom !== chatPinned || (atBottom && chatUnseen)) {
      chatPinned = atBottom;
      if (atBottom) chatUnseen = 0;
      syncChatJump();
    }
  }, { passive: true, capture: true });

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

  // The button that shows the spinner for a click: the clicked button, or, for an item of a menu,
  // the button that opens that menu.
  function busyButtonOf(clicked) {
    if (!clicked || !clicked.closest) return null;
    const panel = clicked.closest('.menu__panel');
    if (panel && panel.parentElement) {
      return panel.parentElement.querySelector('[data-menu-toggle]') || null;
    }
    return clicked.tagName === 'BUTTON' ? clicked : null;
  }

  // Puts `el` (an item, a form) in a loading state: every control inside is disabled, `el` gets
  // aria-busy, and the clicked button shows a spinner. Returns false, doing nothing, when `el` is
  // already busy, so a second click or Enter while a request is in flight is dropped.
  function setBusy(el, busy, clicked) {
    if (!el) return false;
    const isBusy = el.getAttribute('aria-busy') === 'true';
    if (busy && isBusy) return false;
    if (!busy && !isBusy) return true;
    el.querySelectorAll('button, input, textarea, select').forEach((ctrl) => {
      if (busy) ctrl.dataset.wasDisabled = ctrl.disabled ? '1' : '0';
      ctrl.disabled = busy ? true : ctrl.dataset.wasDisabled === '1';
    });
    if (busy) el.setAttribute('aria-busy', 'true');
    else el.removeAttribute('aria-busy');
    const target = busyButtonOf(clicked);
    if (target) {
      if (busy) {
        target.setAttribute('aria-busy', 'true');
        if (!target.querySelector(':scope > .spinner')) {
          const spin = document.createElement('span');
          spin.className = 'spinner';
          spin.setAttribute('aria-hidden', 'true');
          target.prepend(spin);
        }
      } else {
        target.removeAttribute('aria-busy');
        const spin = target.querySelector(':scope > .spinner');
        if (spin) spin.remove();
      }
    }
    return true;
  }

  const ERROR_TEXT = {
    item_already_closed: 'This was already answered or dismissed.',
    item_not_found: 'This item no longer exists.',
    workspace_not_found: 'This workspace is no longer registered.',
    internal: 'The panel hit an error. Try again.',
  };

  function friendlyError(err) {
    const raw = (err && err.message) || '';
    return ERROR_TEXT[raw] || raw || 'Something went wrong. Try again.';
  }

  // The error line of an item: created on first use, so every kind of item can show one.
  function taskErrorEl(task) {
    let el = task.querySelector('[data-task-error]');
    if (!el) {
      el = document.createElement('p');
      el.className = 'field__error';
      el.hidden = true;
      el.setAttribute('data-task-error', '');
      task.append(el);
    }
    // The line may come from the template (no role) or be new: either way it is announced.
    el.setAttribute('role', 'alert');
    return el;
  }

  // Items with a write in flight: "workspace:item" -> how to find the clicked button again. A live
  // refresh can replace the item's markup before the server answers; the new copy is put back in
  // its loading state (see reapplyBusy) so it cannot be clicked twice.
  const inflight = new Map();

  function taskKey(task) {
    return task.dataset.wsId + ':' + task.dataset.itemId;
  }

  function findTask(key) {
    for (const t of document.querySelectorAll('.task[data-item-id]')) {
      if (taskKey(t) === key) return t;
    }
    return null;
  }

  function selectorFor(btn) {
    if (!btn) return '';
    if (btn.dataset.action === 'postpone') return `[data-action="postpone"][data-preset="${btn.dataset.preset || ''}"]`;
    if (btn.dataset.action) return `[data-action="${btn.dataset.action}"]${btn.dataset.value ? `[data-value="${CSS.escape(btn.dataset.value)}"]` : ''}`;
    return 'button[type="submit"]';
  }

  function reapplyBusy(root) {
    inflight.forEach((sel, key) => {
      const task = findTask(key);
      if (!task || !root.contains(task) || task.getAttribute('aria-busy') === 'true') return;
      let btn = null;
      try { btn = sel ? task.querySelector(sel) : null; } catch {}
      setBusy(task, true, btn);
    });
  }

  // After a redraw removed the control that had focus, keyboard focus falls to <body>: put it on
  // the heading of the list the item left, so the next Tab continues from there.
  // The refresh that removes the item may still be in flight (the server's own event starts one),
  // so the request is remembered for a few seconds and applied after each redraw, as long as
  // nothing else has taken focus meanwhile.
  function settleFocus(headingId) {
    focusAfterRedraw = { id: headingId, until: Date.now() + 4000 };
    applyFocusAfterRedraw();
  }
  function applyFocusAfterRedraw() {
    if (!focusAfterRedraw) return;
    if (Date.now() > focusAfterRedraw.until) { focusAfterRedraw = null; return; }
    const active = document.activeElement;
    if (active && active !== document.body && active.isConnected) return;
    const h = document.getElementById(focusAfterRedraw.id);
    if (!h) return;
    if (!h.hasAttribute('tabindex')) h.setAttribute('tabindex', '-1');
    h.focus({ preventScroll: true });
  }

  // One write on an inbox item (respond, postpone, dismiss), single-flight: the item is busy
  // until the server answers. On success the page is refreshed (the item leaves or changes); on
  // error everything is enabled again with the reason shown on the item.
  async function taskRequest(task, clicked, action, body, onDone) {
    const wsId = task.dataset.wsId;
    const itemId = task.dataset.itemId;
    const key = taskKey(task);
    if (inflight.has(key) || !setBusy(task, true, clicked)) return;
    inflight.set(key, selectorFor(clicked));
    showError(taskErrorEl(task), '');
    let live = null;
    try {
      await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/${encodeURIComponent(itemId)}/${action}`, {
        method: 'POST',
        body,
      });
      if (onDone) onDone();
      inflight.delete(key);
      const heading = task.closest('#updates-list, [aria-labelledby="updates-title"]') ? 'updates-title' : 'needs-title';
      await refreshNow(true);
      settleFocus(heading);
      // The refresh normally replaces the item. If it is still here (the refresh failed or is
      // queued), do not leave it greyed out for ever.
      setTimeout(() => {
        const t = findTask(key);
        if (t && !inflight.has(key)) setBusy(t, false, t.querySelector('[aria-busy="true"]'));
      }, 8000);
    } catch (err) {
      inflight.delete(key);
      live = findTask(key) || task;
      const spun = live.querySelector('button[aria-busy="true"]');
      setBusy(live, false, spun);
      showError(taskErrorEl(live), friendlyError(err));
      const back = spun || live.querySelector('button:not([disabled])');
      if (back && (!document.activeElement || document.activeElement === document.body)) back.focus();
      // Closed from elsewhere (another tab, the Maestro): show what the item is now.
      if (err && err.message === 'item_already_closed') refreshNow(true);
    }
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
    const chip = e.target.closest('.chip-row[data-filter$="-list"] .chip');
    if (chip) {
      const name = chip.closest('.chip-row').dataset.filter;
      if (name in overviewFilters) {
        overviewFilters[name] = chip.dataset.value || 'all';
        applyOverviewFilter(document);
      }
      return;
    }

    const agentChip = e.target.closest('.chip-row[data-filter="agents"] .chip');
    if (agentChip) {
      agentsFilter = agentChip.dataset.value || 'all';
      applyAgentsFilter(document);
      return;
    }

    // Copy buttons ([data-copy]) are handled by copy.js, for every page that shows a command.

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
      if (!setBusy(agent, true, btn)) return;
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/agents/${encodeURIComponent(term)}/${btn.dataset.answer === 'approve' ? 'approve' : 'reject'}`, {
          method: 'POST',
          body: { promptHash: btn.dataset.promptHash || '' },
        });
        await refreshNow(true);
      } catch (err) {
        setBusy(agent, false, btn);
        showError(errEl, err.message);
        await refreshNow(true);
      }
      return;
    }

    if (action === 'respond') {
      const task = btn.closest('.task');
      if (!task) return;
      const commentEl = task.querySelector('textarea[data-draft-key]');
      const comment = commentEl ? commentEl.value.trim() : '';
      await taskRequest(task, btn, 'respond', { response: btn.dataset.value, comment }, () => clearDraft(commentEl));
      return;
    }

    if (action === 'postpone') {
      const task = btn.closest('.task');
      if (!task) return;
      const menu = btn.closest('details.menu');
      if (menu) menu.removeAttribute('open');
      const panel = btn.closest('.menu__panel');
      if (panel && panel.matches(':popover-open')) panel.hidePopover();
      await taskRequest(task, btn, 'postpone', { preset: btn.dataset.preset || '1h' });
      return;
    }

    if (action === 'dismiss') {
      const task = btn.closest('.task');
      if (!task) return;
      await taskRequest(task, btn, 'dismiss', {});
      return;
    }

    if (action === 'dismiss-all') {
      const wsId = btn.dataset.wsId;
      if (btn.getAttribute('aria-busy') === 'true') return;
      btn.disabled = true;
      btn.setAttribute('aria-busy', 'true');
      const spin = document.createElement('span');
      spin.className = 'spinner';
      spin.setAttribute('aria-hidden', 'true');
      btn.prepend(spin);
      const stop = () => { btn.disabled = false; btn.removeAttribute('aria-busy'); spin.remove(); };
      try {
        await api(`/api/workspaces/${encodeURIComponent(wsId)}/inbox/dismiss-notices`, {
          method: 'POST',
          body: {},
        });
        await refreshNow(true);
        settleFocus('updates-title');
        setTimeout(() => { if (btn.isConnected) stop(); }, 8000);
      } catch (err) {
        stop();
        const head = btn.closest('.section__head, .panel__head');
        const host = head && head.parentElement;
        if (host) showError(taskErrorEl(host), friendlyError(err));
      }
      return;
    }

    if (action === 'dismiss-updates') {
      // Mark every update as read: all workspaces, or just the one chosen in the filter.
      if (btn.getAttribute('aria-busy') === 'true') return;
      const row = document.querySelector('.chip-row[data-filter="updates-list"]');
      const cur = overviewFilters['updates-list'];
      const targets = cur !== 'all' ? [cur] : [...(row ? row.querySelectorAll('.chip') : [])].map((c) => c.dataset.value).filter((v) => v && v !== 'all');
      if (!targets.length) return;
      btn.disabled = true;
      btn.setAttribute('aria-busy', 'true');
      const spin = document.createElement('span');
      spin.className = 'spinner';
      spin.setAttribute('aria-hidden', 'true');
      btn.prepend(spin);
      const stop = () => { btn.disabled = false; btn.removeAttribute('aria-busy'); spin.remove(); };
      const failures = [];
      for (const ws of targets) {
        try {
          await api(`/api/workspaces/${encodeURIComponent(ws)}/inbox/dismiss-notices`, { method: 'POST', body: {} });
        } catch (err) {
          failures.push(friendlyError(err));
        }
      }
      if (failures.length) stop();
      await refreshNow(true);
      if (!failures.length) settleFocus('updates-title');
      if (failures.length) {
        // The page was just redrawn: put the reason on the new panel.
        const panel = document.getElementById('updates-title');
        const host = panel && panel.closest('.panel');
        if (host) showError(taskErrorEl(host), failures[0]);
      }
      setTimeout(() => { if (btn.isConnected && btn.getAttribute('aria-busy') === 'true') stop(); }, 8000);
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
      await taskRequest(task, e.submitter || form.querySelector('button[type="submit"]'), 'respond', { response: val }, () => clearDraft(input));
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
          // The thread on screen: the server refuses the send if Maestri has moved to another one.
          body: form.dataset.thread ? { body: bodyText, thread: form.dataset.thread } : { body: bodyText },
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
    const closedMenu = e.target.matches('details.menu') ? !e.target.open : e.target.matches('[data-menu] > .menu__panel') && e.newState === 'closed';
    if (closedMenu && pendingRefresh && !isUserBusy()) {
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
