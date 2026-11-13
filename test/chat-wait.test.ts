import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { ChatStore } from '../src/chat.ts';
import { runSayCommand, runWaitCommand, waitForEvents } from '../src/commands/chat.ts';
import { parseConfig } from '../src/config.ts';
import { openDb, openMemoryDb } from '../src/db.ts';
import { EventStore } from '../src/events.ts';
import { InboxStore, ValidationError } from '../src/inbox.ts';
import { buildServer } from '../src/server.ts';
import { parseWorkspaces, saveWorkspaces, StaticWorkspaceProvider } from '../src/workspaces.ts';

function captureStream() {
  const s = new PassThrough();
  let text = '';
  s.on('data', (c) => (text += c.toString('utf8')));
  return { stream: s, read: () => text };
}

describe('Chat, event stream, maestria say, and maestria wait', () => {
  it('stores sanitized human and maestro messages and emits events on human messages + inbox writes', () => {
    const db = openMemoryDb();
    let t = 1_700_000_000_000;
    const now = () => (t += 1000);
    const audit = new Audit(db, now);
    const events = new EventStore(db, now);
    const chat = new ChatStore(db, audit, events, now);
    const inbox = new InboxStore(db, audit, now, events);

    expect(() => chat.send('app', { sender: 'human', body: '   ' })).toThrow(ValidationError);

    const h1 = chat.send('app', { sender: 'human', author: 'Owner', body: 'Status? <script>xss()</script>' }, { via: 'web' });
    expect(h1.bodyHtml).not.toContain('<script>');
    expect(events.since('app', 0)).toHaveLength(1);
    expect(events.since('app', 0)[0]?.type).toBe('chat');

    // Maestro replies should NOT re-trigger chat events.
    const m1 = chat.send('app', { sender: 'maestro', author: 'Maestro', body: '**All green**' }, { via: 'cli' });
    expect(m1.bodyHtml).toContain('<strong>All green</strong>');
    expect(events.since('app', 0)).toHaveLength(1);

    const item = inbox.post({ workspaceId: 'app', kind: 'approve', body: 'Ship v1?' });
    inbox.respond('app', item.id, { decision: 'approved', response: 'LGTM' });
    const evs = events.since('app', 0);
    expect(evs.map((e) => e.type)).toEqual(['chat', 'inbox_response']);

    expect(chat.list('app').map((m) => m.sender)).toEqual(['human', 'maestro']);
  });

  it('wakes waitForEvents on new arrivals, persists cursors, and honors timeout', async () => {
    const db = openMemoryDb();
    const events = new EventStore(db);

    const timedOut = await waitForEvents({ events, workspaceId: 'app', timeoutSeconds: 0.05, pollIntervalMs: 15 });
    expect(timedOut).toEqual({ events: [], timedOut: true });

    const waiting = waitForEvents({ events, workspaceId: 'app', cursor: 'm1', timeoutSeconds: 2, pollIntervalMs: 15 });
    setTimeout(() => {
      events.append('app', 'chat', { author: 'Human', body: 'Wake up!' });
    }, 35);
    const res = await waiting;
    expect(res.timedOut).toBe(false);
    expect(res.events).toHaveLength(1);
    expect(res.events[0]?.payload.body).toBe('Wake up!');

    // Second call starts after persisted cursor and sees nothing immediately.
    const res2 = await waitForEvents({ events, workspaceId: 'app', cursor: 'm1', timeoutSeconds: 0.04, pollIntervalMs: 15 });
    expect(res2.events).toEqual([]);
    expect(res2.timedOut).toBe(true);
  });

  it('supports CLI maestria say and maestria wait commands', async () => {
    const configDir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-chat-cli-'));
    const wsPath = path.join(configDir, 'ws-app');
    fs.mkdirSync(wsPath);
    saveWorkspaces(configDir, [{ id: 'app', name: 'App', path: wsPath, maestroAgent: 'Klefki' }]);

    const db = openDb(configDir);
    const events = new EventStore(db);
    events.append('app', 'standby', {
      message: 'Usage standby threshold reached; pause agents until resumed.',
      triggers: [
        {
          providerId: 'claude',
          providerName: 'Claude Code',
          windowId: 'five_hour',
          windowTitle: 'Plan limits · Session',
          usedPercent: 96,
          thresholdPercent: 95,
          resetsAt: null,
        },
      ],
    });
    db.close();

    const waitTextOut = captureStream();
    await runWaitCommand(['--workspace', 'app', '--cursor', 'text-view', '--timeout', '1'], {
      configDir,
      stdout: waitTextOut.stream,
      pollIntervalMs: 15,
    });
    expect(waitTextOut.read()).toContain('STANDBY (Claude Code / Plan limits · Session: 96% >= 95%)');

    const waitOut = captureStream();
    await runWaitCommand(['--workspace', 'app', '--json', '--timeout', '1'], {
      configDir,
      stdout: waitOut.stream,
      pollIntervalMs: 15,
    });
    const waitJson = JSON.parse(waitOut.read());
    expect(waitJson.events).toHaveLength(1);
    expect(waitJson.events[0].type).toBe('standby');
    expect(waitJson.events[0].payload.triggers[0].providerId).toBe('claude');

    const sayOut = captureStream();
    runSayCommand(['--workspace', 'app', '--json', 'Standing', 'by!'], {
      configDir,
      stdout: sayOut.stream,
    });
    const sayJson = JSON.parse(sayOut.read());
    expect(sayJson).toMatchObject({ sender: 'maestro', author: 'Klefki', body: 'Standing by!' });
  });

  it('serves authenticated chat GET and CSRF-protected chat POST endpoints', async () => {
    const t = 1_700_000_000_000;
    const totpSecret = generateSecret();
    const config = parseConfig({ cookieSecure: false }, '/tmp/unused');
    const ws = parseWorkspaces([{ id: 'app', name: 'App', path: '/tmp/app', maestroAgent: 'Solosis' }]);
    const srv = await buildServer({
      config,
      db: openMemoryDb(),
      secrets: { passwordHash: hashPassword('pw'), totpSecret, createdAt: 'x' },
      workspaces: new StaticWorkspaceProvider(ws),
      now: () => t,
    });

    const unauth = await srv.app.inject({ method: 'GET', url: '/api/workspaces/app/chat', headers: { host: 'localhost:4777' } });
    expect(unauth.statusCode).toBe(401);

    const lp = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'localhost:4777' } });
    const csrfLogin = /name="_csrf" value="([^"]+)"/.exec(lp.body)![1]!;
    const lc = lp.cookies.map((c) => `${c.name}=${c.value}`).join('; ');
    const lr = await srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'content-type': 'application/x-www-form-urlencoded', cookie: lc },
      payload: new URLSearchParams({ _csrf: csrfLogin, password: 'pw', code: totp(totpSecret, t) }).toString(),
    });
    const sc = `maestria_session=${lr.cookies.find((c) => c.name === 'maestria_session')!.value}`;
    const sessRes = await srv.app.inject({ method: 'GET', url: '/api/session', headers: { host: 'localhost:4777', cookie: sc } });
    const csrf = sessRes.json().csrfToken as string;

    const postRes = await srv.app.inject({
      method: 'POST',
      url: '/api/workspaces/app/chat',
      headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'x-csrf-token': csrf, cookie: sc },
      payload: { body: 'Hello Solosis!' },
    });
    expect(postRes.statusCode).toBe(200);
    expect(postRes.json().message.body).toBe('Hello Solosis!');
    expect(srv.events.since('app', 0).map((e) => e.type)).toEqual(['chat']);

    const listRes = await srv.app.inject({
      method: 'GET',
      url: '/api/workspaces/app/chat',
      headers: { host: 'localhost:4777', cookie: sc },
    });
    expect(listRes.statusCode).toBe(200);
    expect(listRes.json().messages).toHaveLength(1);
  });

  it('keeps the built CLI wait subprocess alive past 2 s and exits with the event once appended', async () => {
    const rootDir = path.resolve(import.meta.dirname, '..');
    const distCli = path.join(rootDir, 'dist', 'cli.js');
    if (!fs.existsSync(distCli)) {
      const tscBin = path.join(rootDir, 'node_modules', '.bin', process.platform === 'win32' ? 'tsc.cmd' : 'tsc');
      execFileSync(tscBin, ['-p', 'tsconfig.build.json'], { cwd: rootDir, stdio: 'pipe' });
    }

    const configDir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-wait-proc-'));
    const wsPath = path.join(configDir, 'ws-app');
    fs.mkdirSync(wsPath);
    saveWorkspaces(configDir, [{ id: 'app', name: 'App', path: wsPath, maestroAgent: 'Klefki' }]);
    const initDb = openDb(configDir);
    initDb.close();

    const child = spawn(process.execPath, [distCli, 'wait', '--workspace', 'app', '--json'], {
      env: { ...process.env, MAESTRIA_CONFIG_DIR: configDir },
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    let stdout = '';
    let stderr = '';
    let exitCode: number | null = null;
    child.stdout.on('data', (chunk) => {
      stdout += chunk.toString('utf8');
    });
    child.stderr.on('data', (chunk) => {
      stderr += chunk.toString('utf8');
    });
    const exitPromise = new Promise<number | null>((resolve, reject) => {
      child.on('error', reject);
      child.on('close', (code) => {
        exitCode = code;
        resolve(code);
      });
    });

    try {
      await new Promise((resolve) => setTimeout(resolve, 2100));
      expect(exitCode).toBeNull();
      expect(child.exitCode).toBeNull();
      expect(stderr).toBe('');

      const db = openDb(configDir);
      try {
        const events = new EventStore(db);
        events.append('app', 'chat', { author: 'Owner', body: 'Still awake!' });
      } finally {
        db.close();
      }

      const code = await Promise.race([
        exitPromise,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('wait subprocess did not exit after event')), 5000)),
      ]);
      expect(code).toBe(0);
      expect(stderr).toBe('');
      const parsed = JSON.parse(stdout);
      expect(parsed.workspaceId).toBe('app');
      expect(parsed.timedOut).toBe(false);
      expect(parsed.events).toHaveLength(1);
      expect(parsed.events[0].type).toBe('chat');
      expect(parsed.events[0].payload.body).toBe('Still awake!');
    } finally {
      if (child.exitCode === null && !child.killed) {
        child.kill('SIGKILL');
      }
      fs.rmSync(configDir, { recursive: true, force: true });
    }
  }, 15_000);
});
