import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import vm from 'node:vm';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { parseConfig } from '../src/config.ts';
import { openMemoryDb } from '../src/db.ts';
import { EventStore } from '../src/events.ts';
import { InboxStore } from '../src/inbox.ts';
import { buildServer } from '../src/server.ts';
import { homePage } from '../src/web/html.ts';
import { parseWorkspaces, StaticWorkspaceProvider } from '../src/workspaces.ts';

class FakeElement {
  id = '';
  tagName: string;
  className = '';
  textContent = '';
  value = '';
  placeholder = '';
  maxLength = 0;
  type = '';
  title = '';
  disabled = false;
  hidden = false;
  max = 100;
  scrollTop = 0;
  scrollHeight = 100;
  dataset: Record<string, string> = {};
  attributes = new Map<string, string>();
  children: FakeElement[] = [];
  parentElement: FakeElement | null = null;
  listeners = new Map<string, Array<(ev: { key?: string; metaKey?: boolean; ctrlKey?: boolean; preventDefault: () => void }) => void>>();

  private _innerHTML = '';

  constructor(tagName: string, private ownerDoc?: FakeDocument) {
    this.tagName = tagName.toUpperCase();
  }

  get innerHTML(): string {
    return this._innerHTML;
  }

  set innerHTML(val: string) {
    this._innerHTML = val;
    this.textContent = val.replace(/<[^>]+>/g, '');
  }

  get firstElementChild(): FakeElement | null {
    return this.children[0] ?? null;
  }

  get nextElementSibling(): FakeElement | null {
    if (!this.parentElement) return null;
    const idx = this.parentElement.children.indexOf(this);
    return idx >= 0 ? (this.parentElement.children[idx + 1] ?? null) : null;
  }

  classList = {
    add: (...tokens: string[]) => {
      const set = new Set(this.className.split(/\s+/).filter(Boolean));
      for (const t of tokens) set.add(t);
      this.className = Array.from(set).join(' ');
    },
    remove: (...tokens: string[]) => {
      const set = new Set(this.className.split(/\s+/).filter(Boolean));
      for (const t of tokens) set.delete(t);
      this.className = Array.from(set).join(' ');
    },
    toggle: (token: string, force?: boolean) => {
      const set = new Set(this.className.split(/\s+/).filter(Boolean));
      const next = force !== undefined ? force : !set.has(token);
      if (next) set.add(token);
      else set.delete(token);
      this.className = Array.from(set).join(' ');
      return next;
    },
    contains: (token: string) => this.className.split(/\s+/).filter(Boolean).includes(token),
  };

  setAttribute(name: string, val: string): void {
    this.attributes.set(name, val);
  }

  getAttribute(name: string): string | null {
    return this.attributes.get(name) ?? null;
  }

  appendChild(child: FakeElement): FakeElement {
    if (child.parentElement) {
      child.parentElement.children = child.parentElement.children.filter((c) => c !== child);
    }
    child.parentElement = this;
    this.children.push(child);
    return child;
  }

  append(...nodes: Array<FakeElement | string>): void {
    for (const n of nodes) {
      if (typeof n === 'string') {
        const span = new FakeElement('#text', this.ownerDoc);
        span.textContent = n;
        this.appendChild(span);
      } else {
        this.appendChild(n);
      }
    }
  }

  insertBefore(newNode: FakeElement, refNode: FakeElement | null): FakeElement {
    if (!refNode) return this.appendChild(newNode);
    if (newNode.parentElement) {
      newNode.parentElement.children = newNode.parentElement.children.filter((c) => c !== newNode);
    }
    newNode.parentElement = this;
    const idx = this.children.indexOf(refNode);
    if (idx === -1) this.children.push(newNode);
    else this.children.splice(idx, 0, newNode);
    return newNode;
  }

  remove(): void {
    if (!this.parentElement) return;
    this.parentElement.children = this.parentElement.children.filter((c) => c !== this);
    this.parentElement = null;
  }

  contains(other: FakeElement | null): boolean {
    if (!other) return false;
    if (other === this) return true;
    return this.children.some((c) => c.contains(other));
  }

  focus(): void {
    if (this.ownerDoc) this.ownerDoc.activeElement = this;
  }

  closest(selector: string): FakeElement | null {
    let cur: FakeElement | null = this;
    while (cur) {
      if (selector.startsWith('.') && cur.classList.contains(selector.slice(1))) return cur;
      cur = cur.parentElement;
    }
    return null;
  }

  addEventListener(type: string, fn: (ev: { key?: string; metaKey?: boolean; ctrlKey?: boolean; preventDefault: () => void }) => void): void {
    const arr = this.listeners.get(type) ?? [];
    arr.push(fn);
    this.listeners.set(type, arr);
  }

  dispatchEvent(type: string, extra: { key?: string; metaKey?: boolean; ctrlKey?: boolean } = {}): void {
    for (const fn of this.listeners.get(type) ?? []) {
      fn({ ...extra, preventDefault: () => {} });
    }
  }

  click(): void {
    this.dispatchEvent('click');
  }

  querySelector(selector: string): FakeElement | null {
    return this.querySelectorAll(selector)[0] ?? null;
  }

  querySelectorAll(selector: string): FakeElement[] {
    const out: FakeElement[] = [];
    const match = (el: FakeElement): boolean => {
      if (selector.startsWith('.')) {
        return el.classList.contains(selector.slice(1));
      }
      return el.tagName.toLowerCase() === selector.toLowerCase();
    };
    const walk = (node: FakeElement) => {
      for (const c of node.children) {
        if (match(c)) out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }

  fullText(): string {
    if (this.children.length === 0) return this.textContent;
    return this.textContent + this.children.map((c) => c.fullText()).join(' ');
  }
}

class FakeDocument {
  body = new FakeElement('body', this);
  hidden = false;
  activeElement: FakeElement | null = null;
  byId = new Map<string, FakeElement>();

  constructor(html: string) {
    const csrfMatch = /data-csrf="([^"]+)"/.exec(html);
    if (csrfMatch) this.body.dataset.csrf = csrfMatch[1]!;

    const tagRe = /<([a-zA-Z0-9]+)\s+([^>]*?\bid="([^"]+)"[^>]*)>/g;
    let m: RegExpExecArray | null;
    while ((m = tagRe.exec(html)) !== null) {
      const [, tag, attrs, id] = m;
      const el = new FakeElement(tag!, this);
      el.id = id!;
      const clsMatch = /\bclass="([^"]+)"/.exec(attrs!);
      if (clsMatch) el.className = clsMatch[1]!;
      if (/\bhidden\b/.test(attrs!)) el.hidden = true;
      this.byId.set(id!, el);
    }
  }

  getElementById(id: string): FakeElement | null {
    return this.byId.get(id) ?? null;
  }

  createElement(tag: string): FakeElement {
    return new FakeElement(tag, this);
  }

  createTextNode(text: string): FakeElement {
    const n = new FakeElement('#text', this);
    n.textContent = text;
    return n;
  }
}

describe('Panel UI views & client/API integration (public/app.js + public/app.css + src/web/html.ts)', () => {
  it('enforces [hidden] { display: none !important; } in public/app.css and places #audit-section drawer at top of panel', () => {
    const css = fs.readFileSync(path.resolve(import.meta.dirname, '..', 'public', 'app.css'), 'utf8');
    expect(css).toMatch(/\[hidden\]\s*\{\s*display:\s*none\s*!important;\s*\}/);

    const html = homePage({ csrf: 'test-csrf' });
    const auditIdx = html.indexOf('id="audit-section"');
    const viewHeaderIdx = html.indexOf('id="view-header"');
    expect(auditIdx).toBeGreaterThan(0);
    expect(viewHeaderIdx).toBeGreaterThan(auditIdx);
    expect(html).toContain('id="usage-hidden"');
  });

  it('exercises Home, Usage (ready + <24h stale + hidden error toggle), all 4 Inbox kinds, History, Workspace tabs (Inbox/Chat/Roadmap), Audit drawer, and Kill switch confirm', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-ui-int-'));
    const maestriDir = path.join(tmp, 'maestri');
    const providersDir = path.join(maestriDir, 'usage', 'providers');
    const wsDir = path.join(tmp, 'chapter');
    fs.mkdirSync(providersDir, { recursive: true });
    fs.mkdirSync(path.join(wsDir, 'docs'), { recursive: true });

    const nowMs = Date.parse('2026-11-13T12:00:00Z');
    const twoHoursAgoIso = new Date(nowMs - 2 * 3600_000).toISOString();

    fs.writeFileSync(
      path.join(providersDir, '.status.json'),
      JSON.stringify({
        updatedAt: new Date(nowMs).toISOString(),
        providers: [
          {
            id: 'claude-code',
            name: 'Claude Code',
            enabled: true,
            state: 'ready',
            plan: 'Max',
            limitsApply: true,
            lastSuccessAt: new Date(nowMs).toISOString(),
            meters: [{ id: 'plan', label: 'Plan limits', windows: [{ id: '5h', title: '5h session', usedPercent: 45, ring: 'session' }] }],
          },
          {
            id: 'gemini',
            name: 'Gemini CLI',
            enabled: true,
            state: 'failed',
            error: 'Probe timeout',
            plan: 'Pro',
            limitsApply: true,
            lastSuccessAt: twoHoursAgoIso,
            meters: [{ id: 'plan', label: 'Plan limits', windows: [{ id: 'daily', title: 'Daily', usedPercent: 72, ring: 'weekly' }] }],
          },
          {
            id: 'codex',
            name: 'Codex',
            enabled: true,
            state: 'executableNotFound',
            error: "The executable wasn't found on PATH",
            lastSuccessAt: null,
            meters: [],
          },
        ],
      }),
    );

    const fixtureRoadmap = fs.readFileSync(path.resolve(import.meta.dirname, 'fixtures', 'chapter-roadmap.md'), 'utf8');
    fs.writeFileSync(path.join(wsDir, 'docs', 'roadmap.md'), fixtureRoadmap);

    const cfg = parseConfig({ cookieSecure: false, maestriDir, usage: { fallbackEnabled: false } }, tmp);
    const db = openMemoryDb();
    const audit = new Audit(db, () => nowMs);
    const events = new EventStore(db, () => nowMs);
    const inbox = new InboxStore(db, audit, () => nowMs, events);
    const wsList = parseWorkspaces([
      { id: 'chapter', name: 'Chapter', path: wsDir, roadmapPath: 'docs/roadmap.md', maestroAgent: 'Maestro' },
    ]);

    inbox.post({ workspaceId: 'chapter', kind: 'notice', body: 'Build succeeded.' });
    inbox.post({ workspaceId: 'chapter', kind: 'approve', body: 'Approve release?' });
    inbox.post({ workspaceId: 'chapter', kind: 'input', body: 'Release tag?' });
    inbox.post({ workspaceId: 'chapter', kind: 'choice', body: 'Target env', options: ['staging', 'prod'] });
    inbox.post({ workspaceId: 'chapter', kind: 'approve', body: 'Dismissable item' });

    const totpSecret = generateSecret();
    const srv = await buildServer({
      config: cfg,
      db,
      secrets: { passwordHash: hashPassword('correct-horse-battery'), totpSecret, createdAt: '2026-11-13T00:00:00Z' },
      workspaces: new StaticWorkspaceProvider(wsList),
      now: () => nowMs,
    });

    // Authenticate
    const loginGet = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'localhost:4777' } });
    const loginCsrf = /name="_csrf" value="([^"]+)"/.exec(loginGet.body)![1]!;
    const loginCookie = loginGet.cookies.map((c) => `${c.name}=${c.value}`).join('; ');
    const loginPost = await srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: {
        host: 'localhost:4777',
        origin: 'http://localhost:4777',
        'content-type': 'application/x-www-form-urlencoded',
        cookie: loginCookie,
      },
      payload: new URLSearchParams({
        _csrf: loginCsrf,
        password: 'correct-horse-battery',
        code: totp(totpSecret, nowMs),
      }).toString(),
    });
    expect(loginPost.statusCode).toBe(303);
    const sessionCookie = `maestria_session=${loginPost.cookies.find((c) => c.name === 'maestria_session')!.value}`;

    const homeRes = await srv.app.inject({ method: 'GET', url: '/', headers: { host: 'localhost:4777', cookie: sessionCookie } });
    expect(homeRes.statusCode).toBe(200);

    const doc = new FakeDocument(homeRes.body);
    const winListeners = new Map<string, Array<() => void>>();
    let currentHash = '';
    const windowObj = {
      location: {
        href: 'http://localhost:4777/',
        get hash() {
          return currentHash;
        },
        set hash(v: string) {
          const norm = v ? (v.startsWith('#') ? v : `#${v}`) : '';
          if (norm !== currentHash) {
            currentHash = norm;
            for (const fn of winListeners.get('hashchange') ?? []) fn();
          }
        },
      },
      addEventListener: (ev: string, fn: () => void) => {
        const arr = winListeners.get(ev) ?? [];
        arr.push(fn);
        winListeners.set(ev, arr);
      },
    };

    let pendingRequests = 0;
    const flushAsync = async () => {
      for (let i = 0; i < 15; i++) {
        await new Promise((r) => setTimeout(r, 5));
        if (pendingRequests === 0) break;
      }
      await new Promise((r) => setTimeout(r, 10));
    };

    const fetchFn = async (url: string, opts: { method?: string; headers?: Record<string, string>; body?: string } = {}) => {
      pendingRequests++;
      try {
        const method = (opts.method ?? 'GET') as 'GET' | 'POST';
        const headers: Record<string, string> = {
          host: 'localhost:4777',
          cookie: sessionCookie,
          ...(opts.headers ?? {}),
        };
        if (method !== 'GET') headers.origin = 'http://localhost:4777';
        const res = await srv.app.inject({
          method,
          url,
          headers,
          payload: opts.body,
        });
        return {
          ok: res.statusCode >= 200 && res.statusCode < 300,
          status: res.statusCode,
          json: async () => res.json(),
        };
      } finally {
        pendingRequests--;
      }
    };

    const storage = new Map<string, string>();
    const appJs = fs.readFileSync(path.resolve(import.meta.dirname, '..', 'public', 'app.js'), 'utf8');
    vm.runInNewContext(appJs, {
      window: windowObj,
      document: doc,
      localStorage: {
        getItem: (k: string) => storage.get(k) ?? null,
        setItem: (k: string, v: string) => storage.set(k, v),
      },
      fetch: fetchFn,
      URLSearchParams,
      HTMLTextAreaElement: FakeElement,
      setInterval: () => 0,
      console,
    });

    await flushAsync();

    // 1. Home state & Usage cards (workspace tabs hidden on Home, static assets use no-store + default-hidden CSS)
    expect(doc.getElementById('ws-tabs')!.hidden).toBe(true);
    const cssRes = await srv.app.inject({ method: 'GET', url: '/static/app.css', headers: { host: 'localhost:4777' } });
    expect(cssRes.headers['cache-control']).toBe('no-store');
    expect(cssRes.body).toMatch(/\.ws-tabs\s*\{[^}]*display:\s*none;/);
    expect(cssRes.body).toMatch(/\.ws-tabs:not\(\[hidden\]\)\s*\{[^}]*display:\s*flex;/);
    expect(doc.getElementById('usage-section')!.hidden).toBe(false);
    const usageCards = doc.getElementById('usage-grid')!.children;
    expect(usageCards).toHaveLength(2);
    expect(usageCards[0]!.fullText()).toContain('Claude Code');
    expect(usageCards[0]!.fullText()).toContain('ready');
    expect(usageCards[1]!.fullText()).toContain('Gemini CLI');
    expect(usageCards[1]!.fullText()).toContain('stale');
    expect(usageCards[1]!.fullText()).toContain('72%');
    expect(doc.getElementById('usage-hidden')!.hidden).toBe(false);
    expect(doc.getElementById('usage-hidden')!.fullText()).toContain('1 provider hidden (not installed or failing)');
    expect(doc.getElementById('usage-hidden')!.fullText()).toContain("Codex: The executable wasn't found on PATH");

    // 2. Inbox kinds (notice, approve, input, choice) + dismiss + History
    expect(doc.getElementById('updates-list')!.children).toHaveLength(1);
    expect(doc.getElementById('needs-list')!.children).toHaveLength(4);

    // Ack notice
    const noticeAckBtn = doc.getElementById('updates-list')!.children[0]!.querySelector('.inbox-actions')!.children[0]!;
    noticeAckBtn.click();
    await flushAsync();
    expect(doc.getElementById('updates-list')!.children).toHaveLength(0);

    // Approve item
    const approveItem = doc.getElementById('needs-list')!.children.find((c) => c.fullText().includes('Approve release?'))!;
    approveItem.querySelector('.btn-ok')!.click();
    await flushAsync();

    // Input item
    const inputItem = doc.getElementById('needs-list')!.children.find((c) => c.fullText().includes('Release tag?'))!;
    const inputTa = inputItem.querySelector('textarea')!;
    inputTa.value = 'v1.2.0';
    inputTa.dispatchEvent('input');
    inputItem.querySelector('.inbox-actions')!.children[0]!.click();
    await flushAsync();

    // Choice item
    const choiceItem = doc.getElementById('needs-list')!.children.find((c) => c.fullText().includes('Target env'))!;
    const prodBtn = choiceItem.querySelectorAll('.btn-choice').find((b) => b.textContent === 'prod')!;
    prodBtn.click();
    await flushAsync();

    // Dismiss item
    const dismissItem = doc.getElementById('needs-list')!.children[0]!;
    dismissItem.querySelector('.icon-btn')!.click();
    await flushAsync();
    expect(doc.getElementById('needs-list')!.children).toHaveLength(0);
    expect(doc.getElementById('needs-badge')!.hidden).toBe(true);

    // Toggle History
    doc.getElementById('toggle-history')!.click();
    await flushAsync();
    expect(doc.getElementById('history-section')!.hidden).toBe(false);
    expect(doc.getElementById('history-list')!.children).toHaveLength(5);

    // 3. Open Chapter workspace & test Chat + Roadmap tabs
    const chapterPill = doc.getElementById('ws-switcher')!.children.find((b) => b.fullText().includes('Chapter'))!;
    chapterPill.click();
    await flushAsync();
    expect(windowObj.location.hash).toBe('#ws=chapter');
    expect(doc.getElementById('ws-tabs')!.hidden).toBe(false);

    // Chat tab
    doc.getElementById('tab-chat')!.click();
    await flushAsync();
    expect(windowObj.location.hash).toBe('#ws=chapter&tab=chat');
    expect(doc.getElementById('chat-section')!.hidden).toBe(false);
    doc.getElementById('chat-input')!.value = 'Ping Maestro';
    doc.getElementById('chat-form')!.dispatchEvent('submit');
    await flushAsync();
    expect(doc.getElementById('chat-list')!.children).toHaveLength(1);
    expect(doc.getElementById('chat-list')!.fullText()).toContain('Ping Maestro');

    // Roadmap tab with full GFM fixture
    doc.getElementById('tab-roadmap')!.click();
    await flushAsync();
    expect(windowObj.location.hash).toBe('#ws=chapter&tab=roadmap');
    expect(doc.getElementById('roadmap-section')!.hidden).toBe(false);
    expect(doc.getElementById('roadmap-counts')!.textContent).toBe('(7 in pipeline · 19 done)');
    const rmHtml = doc.getElementById('roadmap-body')!
      .querySelectorAll('.md')
      .map((m) => m.innerHTML)
      .join('\n');
    expect(rmHtml).toContain('<strong>one at a time</strong>');
    expect(rmHtml).toContain('<strong>Profile customization</strong>');
    expect(rmHtml).toContain('<code>/u/[username]</code>');
    expect(doc.getElementById('roadmap-body')!.fullText()).toContain('Known gaps carried along');
    expect(doc.getElementById('roadmap-body')!.fullText()).toContain('Done (19)');

    // Returning Home hides workspace tabs again
    doc.getElementById('back-home')!.click();
    await flushAsync();
    expect(windowObj.location.hash).toBe('');
    expect(doc.getElementById('ws-tabs')!.hidden).toBe(true);

    // 4. Audit log drawer
    doc.getElementById('toggle-audit')!.click();
    await flushAsync();
    expect(doc.getElementById('audit-section')!.hidden).toBe(false);
    expect(doc.getElementById('toggle-audit')!.classList.contains('active')).toBe(true);
    expect(doc.getElementById('audit-body')!.fullText()).toContain('login_ok');
    expect(doc.getElementById('audit-body')!.fullText()).toContain('chat_send');

    doc.getElementById('audit-close')!.click();
    expect(doc.getElementById('audit-section')!.hidden).toBe(true);
    expect(doc.getElementById('toggle-audit')!.classList.contains('active')).toBe(false);

    // 5. Kill switch confirm dialog
    doc.getElementById('kill-btn')!.click();
    expect(doc.getElementById('kill-confirm')!.hidden).toBe(false);
    doc.getElementById('kill-cancel-btn')!.click();
    expect(doc.getElementById('kill-confirm')!.hidden).toBe(true);

    await srv.app.close();
  });
});
