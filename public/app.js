'use strict';

(() => {
  const DRAFT_KEY = 'maestria:drafts:v2';
  const POLL_MS = 5000;

  const csrf = document.body.dataset.csrf || '';
  const el = {
    switcher: document.getElementById('ws-switcher'),
    sync: document.getElementById('sync-status'),
    banner: document.getElementById('banner'),
    overviewGrid: document.getElementById('overview-grid'),
    noWorkspaces: document.getElementById('no-workspaces'),
    wsView: document.getElementById('workspace-view'),
    wsTitle: document.getElementById('ws-title'),
    wsMeta: document.getElementById('ws-meta'),
    tasksSection: document.getElementById('tasks-section'),
    tasksCount: document.getElementById('tasks-count'),
    toggleHistory: document.getElementById('toggle-history'),
    tasksList: document.getElementById('tasks-list'),
    tasksEmpty: document.getElementById('tasks-empty'),
    historyPanel: document.getElementById('history-panel'),
    historyList: document.getElementById('history-list'),
    historyEmpty: document.getElementById('history-empty'),
    noticesCount: document.getElementById('notices-count'),
    noticesList: document.getElementById('notices-list'),
    noticesEmpty: document.getElementById('notices-empty'),
  };

  let workspaces = [];
  let currentWsId = null;
  let showHistory = false;
  let boardState = { tasks: [], notices: [], historyCount: 0, history: [] };
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
      /* ignore storage quota errors */
    }
  }

  function setDraft(taskId, value) {
    if (value === '') {
      clearDraft(taskId);
      return;
    }
    drafts[taskId] = value;
    saveDrafts();
  }

  function clearDraft(taskId) {
    if (Object.hasOwn(drafts, taskId)) {
      delete drafts[taskId];
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

  function readSelectedWorkspaceFromHash() {
    const h = window.location.hash || '';
    const m = /^#ws=([^&]+)$/.exec(h);
    return m ? decodeURIComponent(m[1]) : null;
  }

  function selectWorkspace(id) {
    currentWsId = id;
    showHistory = false;
    el.historyPanel.hidden = true;
    window.location.hash = id ? `ws=${encodeURIComponent(id)}` : '';
    renderWorkspaceChrome();
    if (id) {
      void refreshBoard();
    }
  }

  function renderWorkspaceChrome() {
    el.switcher.textContent = '';
    for (const ws of workspaces) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'ws-pill' + (ws.id === currentWsId ? ' active' : '');
      btn.textContent = ws.name;
      const pending = (ws.counts?.openTasks || 0) + (ws.counts?.activeNotices || 0);
      if (pending > 0) {
        const badge = document.createElement('span');
        badge.className = 'pill-badge';
        badge.textContent = String(pending);
        btn.appendChild(badge);
      }
      btn.addEventListener('click', () => selectWorkspace(ws.id));
      el.switcher.appendChild(btn);
    }

    el.overviewGrid.textContent = '';
    el.noWorkspaces.hidden = workspaces.length > 0;
    for (const ws of workspaces) {
      const card = document.createElement('button');
      card.type = 'button';
      card.className = 'ws-card' + (ws.id === currentWsId ? ' selected' : '');
      card.addEventListener('click', () => selectWorkspace(ws.id));

      const title = document.createElement('div');
      title.className = 'ws-card-title';
      title.textContent = ws.name;

      const pathEl = document.createElement('div');
      pathEl.className = 'ws-card-path';
      pathEl.textContent = ws.path;

      const stats = document.createElement('div');
      stats.className = 'ws-card-stats';

      const openChip = document.createElement('span');
      const openCount = ws.counts?.openTasks || 0;
      openChip.className = 'stat-chip' + (openCount > 0 ? ' warn' : '');
      openChip.textContent = `${openCount} pending`;
      stats.appendChild(openChip);

      const noticeChip = document.createElement('span');
      const noticeCount = ws.counts?.activeNotices || 0;
      noticeChip.className = 'stat-chip';
      noticeChip.textContent = `${noticeCount} notice${noticeCount === 1 ? '' : 's'}`;
      stats.appendChild(noticeChip);

      const maestroChip = document.createElement('span');
      maestroChip.className = 'stat-chip ok';
      maestroChip.textContent = ws.maestroAgent;
      stats.appendChild(maestroChip);

      card.append(title, pathEl, stats);
      el.overviewGrid.appendChild(card);
    }

    const activeWs = workspaces.find((w) => w.id === currentWsId) || null;
    if (!activeWs) {
      el.wsView.hidden = true;
      return;
    }
    el.wsView.hidden = false;
    el.wsTitle.textContent = activeWs.name;
    el.wsMeta.textContent = `${activeWs.path} · Maestro: ${activeWs.maestroAgent}`;
  }

  function createTaskElement(task) {
    const wrap = document.createElement('div');
    wrap.className = 'task-item';
    wrap.dataset.taskId = task.id;
    wrap.dataset.taskType = task.type;

    const head = document.createElement('div');
    head.className = 'task-head';

    const body = document.createElement('div');
    body.className = 'task-body';

    const textWrap = document.createElement('div');
    const typeTag = document.createElement('span');
    typeTag.className = 'task-type-tag';
    typeTag.textContent = task.type;

    const textSpan = document.createElement('span');
    textSpan.className = 'task-text';
    textSpan.innerHTML = task.textHtml;
    textSpan.dataset.rawText = task.text;

    textWrap.append(typeTag, textSpan);

    const meta = document.createElement('div');
    meta.className = 'task-meta';
    meta.textContent = `by ${task.createdBy} · ${fmtTime(task.createdAt)}`;

    body.append(textWrap, meta);

    const dismissBtn = document.createElement('button');
    dismissBtn.type = 'button';
    dismissBtn.className = 'icon-btn';
    dismissBtn.textContent = '✕';
    dismissBtn.title = 'Dismiss without responding';
    dismissBtn.setAttribute('aria-label', 'Dismiss task');
    dismissBtn.addEventListener('click', () => {
      void dismissTask(task.id);
    });

    head.append(body, dismissBtn);
    wrap.appendChild(head);

    let textarea = null;
    if (task.type === 'approve' || task.type === 'input') {
      textarea = document.createElement('textarea');
      textarea.placeholder = task.type === 'approve' ? 'Optional comment…' : 'Write your response…';
      textarea.maxLength = 8000;
      if (Object.hasOwn(drafts, task.id)) {
        textarea.value = drafts[task.id];
      }
      textarea.addEventListener('input', () => {
        setDraft(task.id, textarea.value);
        const hint = wrap.querySelector('.draft-hint');
        if (hint) hint.textContent = textarea.value ? 'Draft saved' : '';
        const inlineErr = wrap.querySelector('.task-inline-err');
        if (inlineErr) inlineErr.textContent = '';
      });
      wrap.appendChild(textarea);
    }

    const actions = document.createElement('div');
    actions.className = 'task-actions';

    if (task.type === 'ack') {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'small-btn';
      btn.textContent = 'Got it';
      btn.addEventListener('click', () => void respondToTask(task.id, { decision: 'ack' }));
      actions.appendChild(btn);
    } else if (task.type === 'approve') {
      const approveBtn = document.createElement('button');
      approveBtn.type = 'button';
      approveBtn.className = 'small-btn btn-ok';
      approveBtn.textContent = 'Approve';
      approveBtn.addEventListener('click', () =>
        void respondToTask(task.id, { decision: 'approved', response: textarea ? textarea.value : '' }),
      );

      const rejectBtn = document.createElement('button');
      rejectBtn.type = 'button';
      rejectBtn.className = 'small-btn btn-danger';
      rejectBtn.textContent = 'Reject';
      rejectBtn.addEventListener('click', () =>
        void respondToTask(task.id, { decision: 'rejected', response: textarea ? textarea.value : '' }),
      );

      const hint = document.createElement('span');
      hint.className = 'muted small draft-hint';
      hint.textContent = Object.hasOwn(drafts, task.id) ? 'Draft saved' : '';

      actions.append(approveBtn, rejectBtn, hint);
    } else if (task.type === 'input') {
      const sendBtn = document.createElement('button');
      sendBtn.type = 'button';
      sendBtn.className = 'small-btn';
      sendBtn.textContent = 'Send';

      const inlineErr = document.createElement('span');
      inlineErr.className = 'task-inline-err';

      const hint = document.createElement('span');
      hint.className = 'muted small draft-hint';
      hint.textContent = Object.hasOwn(drafts, task.id) ? 'Draft saved' : '';

      sendBtn.addEventListener('click', () => {
        const val = textarea ? textarea.value.trim() : '';
        if (!val) {
          inlineErr.textContent = 'Please write a response first.';
          if (textarea) textarea.focus();
          return;
        }
        void respondToTask(task.id, { response: textarea.value });
      });

      actions.append(sendBtn, hint, inlineErr);
    } else if (task.type === 'choice') {
      for (const opt of task.options || []) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'small-btn btn-choice';
        btn.textContent = opt;
        btn.addEventListener('click', () => void respondToTask(task.id, { decision: opt }));
        actions.appendChild(btn);
      }
    }

    wrap.appendChild(actions);
    return wrap;
  }

  function updateTaskElement(wrap, task) {
    const textSpan = wrap.querySelector('.task-text');
    if (textSpan && textSpan.dataset.rawText !== task.text) {
      textSpan.innerHTML = task.textHtml;
      textSpan.dataset.rawText = task.text;
    }
    const meta = wrap.querySelector('.task-meta');
    if (meta) {
      meta.textContent = `by ${task.createdBy} · ${fmtTime(task.createdAt)}`;
    }
  }

  function reconcileTasks() {
    const tasks = boardState.tasks;
    el.tasksSection.classList.toggle('clear', tasks.length === 0);
    el.tasksCount.textContent = tasks.length ? `${tasks.length} pending` : '';

    if (boardState.historyCount > 0 || showHistory) {
      el.toggleHistory.hidden = false;
      el.toggleHistory.textContent = showHistory
        ? 'Hide history'
        : `History (${boardState.historyCount})`;
    } else {
      el.toggleHistory.hidden = true;
    }

    const existingById = new Map();
    for (const child of Array.from(el.tasksList.children)) {
      if (child.dataset.taskId) existingById.set(child.dataset.taskId, child);
    }
    const desiredIds = new Set(tasks.map((t) => t.id));
    for (const [id, node] of existingById) {
      if (!desiredIds.has(id)) node.remove();
    }

    let cursor = el.tasksList.firstElementChild;
    for (const task of tasks) {
      let node = existingById.get(task.id);
      if (!node) {
        node = createTaskElement(task);
        el.tasksList.insertBefore(node, cursor);
      } else {
        updateTaskElement(node, task);
        if (node !== cursor) {
          el.tasksList.insertBefore(node, cursor);
        } else {
          cursor = cursor.nextElementSibling;
        }
      }
    }

    el.tasksEmpty.hidden = tasks.length > 0;
  }

  function renderHistory() {
    el.historyPanel.hidden = !showHistory;
    if (!showHistory) return;

    const items = boardState.history || [];
    el.historyList.textContent = '';
    el.historyEmpty.hidden = items.length > 0;

    for (const t of items) {
      const row = document.createElement('div');
      row.className = 'history-item';

      const top = document.createElement('div');
      top.className = 'history-row';

      const prompt = document.createElement('div');
      prompt.className = 'task-text';
      prompt.innerHTML = t.textHtml;

      const right = document.createElement('div');
      const badge = document.createElement('span');
      if (t.status === 'dismissed') {
        badge.className = 'decision-badge';
        badge.textContent = 'Dismissed';
      } else if (t.type === 'approve') {
        badge.className = `decision-badge ${t.decision === 'approved' ? 'approved' : 'rejected'}`;
        badge.textContent = t.decision === 'approved' ? 'Approved' : 'Rejected';
      } else if (t.type === 'ack') {
        badge.className = 'decision-badge ack';
        badge.textContent = 'Got it';
      } else if (t.type === 'choice') {
        badge.className = 'decision-badge ack';
        badge.textContent = t.decision || 'Chosen';
      } else {
        badge.className = 'decision-badge ack';
        badge.textContent = 'Replied';
      }

      const ts = document.createElement('span');
      ts.className = 'muted small';
      ts.textContent = ' ' + fmtTime(t.respondedAt || t.dismissedAt);

      right.append(badge, ts);
      top.append(prompt, right);
      row.appendChild(top);

      if (t.response) {
        const resp = document.createElement('div');
        resp.className = 'history-response';
        resp.textContent = t.response;
        row.appendChild(resp);
      }

      el.historyList.appendChild(row);
    }
  }

  function reconcileNotices() {
    const notices = boardState.notices;
    el.noticesCount.textContent = notices.length ? String(notices.length) : '';
    el.noticesEmpty.hidden = notices.length > 0;

    const existingById = new Map();
    for (const child of Array.from(el.noticesList.children)) {
      if (child.dataset.noticeId) existingById.set(child.dataset.noticeId, child);
    }
    const desiredIds = new Set(notices.map((n) => n.id));
    for (const [id, node] of existingById) {
      if (!desiredIds.has(id)) node.remove();
    }

    let cursor = el.noticesList.firstElementChild;
    for (const notice of notices) {
      let node = existingById.get(notice.id);
      if (!node) {
        node = document.createElement('div');
        node.className = 'notice-item';
        node.dataset.noticeId = notice.id;

        const main = document.createElement('div');
        main.className = 'notice-main';

        const body = document.createElement('div');
        body.className = 'md';
        body.innerHTML = notice.bodyHtml;
        body.dataset.rawBody = notice.body;

        const meta = document.createElement('div');
        meta.className = 'notice-meta';
        meta.textContent = `by ${notice.createdBy} · ${fmtTime(notice.createdAt)}`;

        main.append(body, meta);

        const dismissBtn = document.createElement('button');
        dismissBtn.type = 'button';
        dismissBtn.className = 'icon-btn';
        dismissBtn.textContent = '✕';
        dismissBtn.title = 'Dismiss notice';
        dismissBtn.setAttribute('aria-label', 'Dismiss notice');
        dismissBtn.addEventListener('click', () => {
          void dismissNotice(notice.id);
        });

        node.append(main, dismissBtn);
        el.noticesList.insertBefore(node, cursor);
      } else {
        const body = node.querySelector('.md');
        if (body && body.dataset.rawBody !== notice.body) {
          body.innerHTML = notice.bodyHtml;
          body.dataset.rawBody = notice.body;
        }
        if (node !== cursor) {
          el.noticesList.insertBefore(node, cursor);
        } else {
          cursor = cursor.nextElementSibling;
        }
      }
    }
  }

  async function respondToTask(taskId, payload) {
    const wsId = currentWsId;
    if (!wsId) return;
    const prevTasks = [...boardState.tasks];
    const prevHistoryCount = boardState.historyCount;

    // Optimistic: remove task from the board immediately
    const target = boardState.tasks.find((t) => t.id === taskId);
    boardState.tasks = boardState.tasks.filter((t) => t.id !== taskId);
    boardState.historyCount += 1;
    if (showHistory && target) {
      boardState.history = [
        {
          ...target,
          status: 'responded',
          decision: payload.decision || null,
          response: payload.response ? payload.response.trim() : null,
          respondedAt: Date.now(),
        },
        ...boardState.history,
      ];
      renderHistory();
    }
    reconcileTasks();

    inFlight += 1;
    try {
      showBanner('');
      const res = await api(`/api/workspaces/${encodeURIComponent(wsId)}/tasks/${encodeURIComponent(taskId)}/respond`, {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      clearDraft(taskId);
      if (currentWsId === wsId && showHistory) {
        boardState.history = [res.task, ...boardState.history.filter((h) => h.id !== taskId)];
        renderHistory();
      }
      void refreshWorkspaces();
    } catch (err) {
      if (currentWsId === wsId) {
        boardState.tasks = prevTasks;
        boardState.historyCount = prevHistoryCount;
        reconcileTasks();
        if (showHistory) renderHistory();
      }
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not submit response: ${err.message}`);
      }
    } finally {
      inFlight -= 1;
    }
  }

  async function dismissTask(taskId) {
    const wsId = currentWsId;
    if (!wsId) return;
    const prevTasks = [...boardState.tasks];
    const prevHistoryCount = boardState.historyCount;

    boardState.tasks = boardState.tasks.filter((t) => t.id !== taskId);
    boardState.historyCount += 1;
    reconcileTasks();

    inFlight += 1;
    try {
      showBanner('');
      const res = await api(`/api/workspaces/${encodeURIComponent(wsId)}/tasks/${encodeURIComponent(taskId)}/dismiss`, {
        method: 'POST',
      });
      clearDraft(taskId);
      if (currentWsId === wsId && showHistory) {
        boardState.history = [res.task, ...boardState.history.filter((h) => h.id !== taskId)];
        renderHistory();
      }
      void refreshWorkspaces();
    } catch (err) {
      if (currentWsId === wsId) {
        boardState.tasks = prevTasks;
        boardState.historyCount = prevHistoryCount;
        reconcileTasks();
      }
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not dismiss task: ${err.message}`);
      }
    } finally {
      inFlight -= 1;
    }
  }

  async function dismissNotice(noticeId) {
    const wsId = currentWsId;
    if (!wsId) return;
    const prevNotices = [...boardState.notices];
    boardState.notices = boardState.notices.filter((n) => n.id !== noticeId);
    reconcileNotices();

    inFlight += 1;
    try {
      showBanner('');
      await api(`/api/workspaces/${encodeURIComponent(wsId)}/notices/${encodeURIComponent(noticeId)}/dismiss`, {
        method: 'POST',
      });
      void refreshWorkspaces();
    } catch (err) {
      if (currentWsId === wsId) {
        boardState.notices = prevNotices;
        reconcileNotices();
      }
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not dismiss notice: ${err.message}`);
      }
    } finally {
      inFlight -= 1;
    }
  }

  async function refreshWorkspaces() {
    const data = await api('/api/workspaces');
    workspaces = data.workspaces || [];
    const fromHash = readSelectedWorkspaceFromHash();
    if (fromHash && workspaces.some((w) => w.id === fromHash)) {
      currentWsId = fromHash;
    } else if (!currentWsId || !workspaces.some((w) => w.id === currentWsId)) {
      currentWsId = workspaces.length > 0 ? workspaces[0].id : null;
    }
    renderWorkspaceChrome();
  }

  async function refreshBoard() {
    if (!currentWsId) return;
    const wsId = currentWsId;
    const data = await api(`/api/workspaces/${encodeURIComponent(wsId)}/board`);
    if (currentWsId !== wsId || inFlight > 0) return;

    boardState.tasks = data.tasks || [];
    boardState.notices = data.notices || [];
    boardState.historyCount = data.historyCount || 0;

    if (showHistory) {
      const h = await api(`/api/workspaces/${encodeURIComponent(wsId)}/tasks?status=history`);
      if (currentWsId === wsId && inFlight === 0) {
        boardState.history = h.tasks || [];
        renderHistory();
      }
    }

    const isTyping = document.activeElement instanceof HTMLTextAreaElement && el.tasksList.contains(document.activeElement);
    if (!isTyping) {
      reconcileTasks();
    }
    reconcileNotices();
  }

  async function poll() {
    try {
      await refreshWorkspaces();
      if (currentWsId) await refreshBoard();
      el.sync.textContent = `Updated ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`;
    } catch (err) {
      if (err.message !== 'unauthenticated') {
        el.sync.textContent = 'Offline / sync paused';
      }
    }
  }

  el.toggleHistory.addEventListener('click', () => {
    showHistory = !showHistory;
    reconcileTasks();
    if (showHistory) {
      void refreshBoard();
    } else {
      renderHistory();
    }
  });

  window.addEventListener('hashchange', () => {
    const id = readSelectedWorkspaceFromHash();
    if (id && workspaces.some((w) => w.id === id) && id !== currentWsId) {
      currentWsId = id;
      showHistory = false;
      el.historyPanel.hidden = true;
      renderWorkspaceChrome();
      void refreshBoard();
    }
  });

  void poll();
  setInterval(() => {
    if (!document.hidden) void poll();
  }, POLL_MS);
})();
