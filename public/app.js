'use strict';

(() => {
  const DRAFT_KEY = 'maestria:drafts:v1';
  const POLL_MS = 5000;

  const csrf = document.body.dataset.csrf || '';
  const el = {
    switcher: document.getElementById('ws-switcher'),
    sync: document.getElementById('sync-status'),
    banner: document.getElementById('banner'),
    overviewSection: document.getElementById('overview-section'),
    overviewGrid: document.getElementById('overview-grid'),
    noWorkspaces: document.getElementById('no-workspaces'),
    wsView: document.getElementById('workspace-view'),
    wsTitle: document.getElementById('ws-title'),
    wsMeta: document.getElementById('ws-meta'),
    tasksSection: document.getElementById('tasks-section'),
    tasksCount: document.getElementById('tasks-count'),
    toggleArchived: document.getElementById('toggle-archived'),
    tasksList: document.getElementById('tasks-list'),
    tasksEmpty: document.getElementById('tasks-empty'),
    noticesCount: document.getElementById('notices-count'),
    noticesList: document.getElementById('notices-list'),
    noticesEmpty: document.getElementById('notices-empty'),
  };

  let workspaces = [];
  let currentWsId = null;
  let showArchived = false;
  let boardState = { tasks: [], notices: [], dismissedCount: 0 };
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
    showArchived = false;
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
      openChip.textContent = `${openCount} open task${openCount === 1 ? '' : 's'}`;
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
    wrap.className = `task-item ${task.status}`;
    wrap.dataset.taskId = task.id;

    const row = document.createElement('div');
    row.className = 'task-row';

    const label = document.createElement('label');
    label.className = 'task-check-label';

    const check = document.createElement('input');
    check.type = 'checkbox';
    check.className = 'task-check';
    check.checked = task.status === 'done';
    check.disabled = task.status === 'dismissed';

    const textSpan = document.createElement('span');
    textSpan.className = 'task-text';
    textSpan.innerHTML = task.textHtml;

    label.append(check, textSpan);

    const actionBtn = document.createElement('button');
    actionBtn.type = 'button';
    if (task.status === 'dismissed') {
      actionBtn.className = 'ghost small-btn';
      actionBtn.textContent = 'Restore';
      actionBtn.title = 'Restore task';
    } else {
      actionBtn.className = 'icon-btn';
      actionBtn.textContent = '✕';
      actionBtn.title = 'Dismiss (archive)';
      actionBtn.setAttribute('aria-label', 'Dismiss task');
    }

    row.append(label, actionBtn);
    wrap.appendChild(row);

    const meta = document.createElement('div');
    meta.className = 'task-meta';
    wrap.appendChild(meta);

    if (task.kind === 'question') {
      const answerBox = document.createElement('div');
      answerBox.className = 'task-answer-box';

      const textarea = document.createElement('textarea');
      textarea.placeholder = 'Write your answer…';
      textarea.maxLength = 8000;
      const initialVal = Object.hasOwn(drafts, task.id) ? drafts[task.id] : (task.answer || '');
      textarea.value = initialVal;
      textarea.readOnly = task.status !== 'open';

      const actions = document.createElement('div');
      actions.className = 'task-answer-actions';

      const submitBtn = document.createElement('button');
      submitBtn.type = 'button';
      submitBtn.className = 'small-btn';
      submitBtn.textContent = 'Submit answer';
      submitBtn.hidden = task.status !== 'open';

      const draftHint = document.createElement('span');
      draftHint.className = 'muted small draft-hint';
      draftHint.textContent = Object.hasOwn(drafts, task.id) && task.status === 'open' ? 'Draft saved locally' : '';

      actions.append(submitBtn, draftHint);
      answerBox.append(textarea, actions);
      wrap.appendChild(answerBox);

      textarea.addEventListener('input', () => {
        setDraft(task.id, textarea.value);
        draftHint.textContent = 'Draft saved locally';
      });

      submitBtn.addEventListener('click', () => {
        void submitTaskTransition(task.id, 'done', textarea.value);
      });
    }

    check.addEventListener('change', () => {
      const nextStatus = check.checked ? 'done' : 'open';
      const textarea = wrap.querySelector('textarea');
      const answer = textarea ? textarea.value : undefined;
      void submitTaskTransition(task.id, nextStatus, answer);
    });

    actionBtn.addEventListener('click', () => {
      const nextStatus = task.status === 'dismissed' ? 'open' : 'dismissed';
      void submitTaskTransition(task.id, nextStatus);
    });

    updateTaskElement(wrap, task);
    return wrap;
  }

  function updateTaskElement(wrap, task) {
    wrap.className = `task-item ${task.status}`;

    const check = wrap.querySelector('.task-check');
    if (check) {
      check.checked = task.status === 'done';
      check.disabled = task.status === 'dismissed';
    }

    const textSpan = wrap.querySelector('.task-text');
    if (textSpan && textSpan.dataset.rawText !== task.text) {
      textSpan.innerHTML = task.textHtml;
      textSpan.dataset.rawText = task.text;
    }

    const meta = wrap.querySelector('.task-meta');
    if (meta) {
      const parts = [`by ${task.createdBy}`, fmtTime(task.createdAt)];
      if (task.status === 'done' && task.doneAt) parts.push(`done ${fmtTime(task.doneAt)}`);
      if (task.status === 'dismissed' && task.dismissedAt) parts.push(`archived ${fmtTime(task.dismissedAt)}`);
      meta.textContent = parts.filter(Boolean).join(' · ');
    }

    const row = wrap.querySelector('.task-row');
    const actionBtn = row ? row.querySelector('button') : null;
    if (actionBtn) {
      if (task.status === 'dismissed') {
        actionBtn.className = 'ghost small-btn';
        actionBtn.textContent = 'Restore';
        actionBtn.title = 'Restore task';
      } else {
        actionBtn.className = 'icon-btn';
        actionBtn.textContent = '✕';
        actionBtn.title = 'Dismiss (archive)';
        actionBtn.setAttribute('aria-label', 'Dismiss task');
      }
    }

    const textarea = wrap.querySelector('textarea');
    const submitBtn = wrap.querySelector('.task-answer-actions button');
    const draftHint = wrap.querySelector('.draft-hint');
    if (textarea) {
      textarea.readOnly = task.status !== 'open';
      const isFocused = document.activeElement === textarea;
      const hasDraft = Object.hasOwn(drafts, task.id);
      // Never clobber an active editor or unsaved draft on refresh/poll.
      if (!isFocused && !hasDraft) {
        const serverVal = task.answer || '';
        if (textarea.value !== serverVal) textarea.value = serverVal;
      }
      if (submitBtn) submitBtn.hidden = task.status !== 'open';
      if (draftHint) {
        draftHint.textContent = hasDraft && task.status === 'open' ? 'Draft saved locally' : '';
      }
    }
  }

  function reconcileTasks() {
    const tasks = boardState.tasks;
    const openCount = tasks.filter((t) => t.status === 'open').length;
    const doneCount = tasks.filter((t) => t.status === 'done').length;

    el.tasksSection.classList.toggle('clear', openCount === 0);
    el.tasksCount.textContent = tasks.length ? `${openCount} open · ${doneCount} done` : '';
    if (boardState.dismissedCount > 0 || showArchived) {
      el.toggleArchived.hidden = false;
      el.toggleArchived.textContent = showArchived
        ? 'Hide archived'
        : `Show archived (${boardState.dismissedCount})`;
    } else {
      el.toggleArchived.hidden = true;
    }

    const existingById = new Map();
    for (const child of Array.from(el.tasksList.children)) {
      if (child.dataset.taskId) existingById.set(child.dataset.taskId, child);
    }

    const desiredIds = new Set(tasks.map((t) => t.id));
    for (const [id, node] of existingById) {
      if (!desiredIds.has(id)) {
        node.remove();
      }
    }

    // Preserve focused element across ordering updates by appending/inserting only when out of order.
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

  async function submitTaskTransition(taskId, nextStatus, answer) {
    const wsId = currentWsId;
    if (!wsId) return;
    const prevTasks = boardState.tasks.map((t) => ({ ...t }));
    const prevDismissedCount = boardState.dismissedCount;

    // Optimistic update
    const idx = boardState.tasks.findIndex((t) => t.id === taskId);
    if (idx >= 0) {
      const cur = boardState.tasks[idx];
      if (nextStatus === 'dismissed' && !showArchived) {
        boardState.tasks.splice(idx, 1);
        boardState.dismissedCount += 1;
      } else {
        boardState.tasks[idx] = {
          ...cur,
          status: nextStatus,
          answer: answer !== undefined ? answer : cur.answer,
          doneAt: nextStatus === 'done' ? Date.now() : nextStatus === 'open' ? null : cur.doneAt,
          dismissedAt: nextStatus === 'dismissed' ? Date.now() : null,
        };
      }
      reconcileTasks();
    }

    inFlight += 1;
    try {
      showBanner('');
      const payload = { status: nextStatus };
      if (answer !== undefined) payload.answer = answer;
      const res = await api(`/api/workspaces/${encodeURIComponent(wsId)}/tasks/${encodeURIComponent(taskId)}`, {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      if (nextStatus === 'done') {
        clearDraft(taskId);
      }
      if (currentWsId === wsId) {
        const pos = boardState.tasks.findIndex((t) => t.id === taskId);
        if (res.task.status === 'dismissed' && !showArchived) {
          if (pos >= 0) boardState.tasks.splice(pos, 1);
        } else if (pos >= 0) {
          boardState.tasks[pos] = res.task;
        }
        reconcileTasks();
      }
      void refreshWorkspaces();
    } catch (err) {
      // Revert optimistic UI, keeping any user draft intact in localStorage.
      if (currentWsId === wsId) {
        boardState.tasks = prevTasks;
        boardState.dismissedCount = prevDismissedCount;
        reconcileTasks();
      }
      if (err.message !== 'unauthenticated') {
        showBanner(`Could not update task: ${err.message}`);
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
    const q = showArchived ? '?includeDismissed=1' : '';
    const data = await api(`/api/workspaces/${encodeURIComponent(wsId)}/board${q}`);
    if (currentWsId !== wsId || inFlight > 0) return;
    boardState = {
      tasks: data.tasks || [],
      notices: data.notices || [],
      dismissedCount: data.dismissedCount || 0,
    };
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

  el.toggleArchived.addEventListener('click', () => {
    showArchived = !showArchived;
    void refreshBoard();
  });

  window.addEventListener('hashchange', () => {
    const id = readSelectedWorkspaceFromHash();
    if (id && workspaces.some((w) => w.id === id) && id !== currentWsId) {
      currentWsId = id;
      showArchived = false;
      renderWorkspaceChrome();
      void refreshBoard();
    }
  });

  void poll();
  setInterval(() => {
    if (!document.hidden) void poll();
  }, POLL_MS);
})();
