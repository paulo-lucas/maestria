import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { parseConfig } from '../src/config.ts';
import { openMemoryDb } from '../src/db.ts';
import { EventStore } from '../src/events.ts';
import { buildServer } from '../src/server.ts';
import { StandbyGuard } from '../src/standby.ts';
import { executeProviderFile, mapProviderOutput } from '../src/usage-runner.ts';
import { UsageService } from '../src/usage.ts';
import { parseWorkspaces, StaticWorkspaceProvider } from '../src/workspaces.ts';

function mkProvidersDir(maestriDir: string): string {
  const dir = path.join(maestriDir, 'usage', 'providers');
  fs.mkdirSync(dir, { recursive: true });
  return dir;
}

describe('Maestri usage reader, fallback runner, and standby guard', () => {
  it('reports clear setup errors when maestriDir, usage/providers, or .status.json are missing', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-usage-setup-'));
    const cfgMissingDir = parseConfig({ maestriDir: path.join(tmp, 'nope') }, tmp);
    expect((await new UsageService(cfgMissingDir).read()).status).toBe('missing_maestri_dir');

    const partial = path.join(tmp, 'm1');
    fs.mkdirSync(partial);
    const cfgNoProviders = parseConfig({ maestriDir: partial }, tmp);
    expect((await new UsageService(cfgNoProviders).read()).status).toBe('missing_providers_dir');

    mkProvidersDir(partial);
    const noStatus = await new UsageService(cfgNoProviders).read();
    expect(noStatus.status).toBe('missing_status_file');
    expect(noStatus.setupError).toContain('Agent Usage');
  });

  it('reads .status.json providers verbatim and executes small command/stdio fallback specs when stale', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-usage-fb-'));
    const maestriDir = path.join(tmp, 'maestri');
    const providersDir = mkProvidersDir(maestriDir);
    let nowMs = Date.parse('2026-11-13T02:00:00Z');

    // Write a tiny executable Node command provider and a tiny stdio provider.
    const cmdProv = {
      schemaVersion: 1,
      id: 'custom-cmd',
      revision: 1,
      name: 'Custom Command',
      source: {
        type: 'command',
        executable: process.execPath,
        args: ['-e', 'console.log(JSON.stringify({ status: "ok", week: { percent: 62, resets: 1794600000 } }))'],
        expect: { match: [{ path: '/status', equals: 'ok' }], require: '/week' },
      },
      map: {
        meters: [
          {
            id: 'plan',
            label: { key: 'plan' },
            windows: [
              {
                id: 'week',
                select: '/week',
                used: { path: '/percent', as: 'percent' },
                resetsAt: { path: '/resets', as: 'epochSeconds' },
                duration: { seconds: 604800 },
              },
            ],
          },
        ],
      },
    };
    fs.writeFileSync(path.join(providersDir, 'custom-cmd.json'), JSON.stringify(cmdProv));

    const stdioProv = {
      schemaVersion: 1,
      id: 'custom-stdio',
      revision: 1,
      name: 'Custom Stdio',
      source: {
        type: 'stdio',
        executable: process.execPath,
        args: [
          '-e',
          'process.stdin.on("data", () => { process.stdout.write(JSON.stringify({ type: "resp", usedFrac: 0.25 }) + "\\n"); process.exit(0); });',
        ],
        steps: [
          { write: { type: 'req' } },
          { await: { match: [{ path: '/type', equals: 'resp' }], require: '/usedFrac', capture: 'u' } },
        ],
        output: 'u',
      },
      map: {
        meters: [
          {
            id: 'plan',
            label: { key: 'plan' },
            windows: [
              { id: 'sess', used: { path: '/usedFrac', as: 'fraction' }, duration: { seconds: 18000 } },
            ],
          },
        ],
      },
    };
    const stdioPath = path.join(providersDir, 'custom-stdio.json');
    fs.writeFileSync(stdioPath, JSON.stringify(stdioProv));

    const directStdio = await executeProviderFile(stdioPath, path.join(tmp, 'cache'), () => nowMs);
    expect(directStdio.state).toBe('ready');
    expect(directStdio.meters[0]?.windows[0]).toMatchObject({ id: 'sess', usedPercent: 25, ring: 'session' });

    fs.writeFileSync(
      path.join(providersDir, '.status.json'),
      JSON.stringify({
        updatedAt: '2026-11-13T01:00:00Z',
        providers: [
          {
            id: 'codex',
            name: 'Codex',
            enabled: true,
            state: 'executableNotFound',
            error: 'Missing codex binary',
            meters: [],
          },
          {
            id: 'custom-cmd',
            name: 'Custom Command',
            enabled: true,
            state: 'ready',
            lastSuccessAt: '2026-11-13T01:00:00Z', // 60 minutes old > 15m staleMinutes
            meters: [{ id: 'plan', label: 'Plan limits', windows: [{ id: 'week', title: 'Weekly', usedPercent: 10 }] }],
          },
        ],
      }),
    );

    const cfg = parseConfig({ maestriDir, usage: { staleMinutes: 15, fallbackEnabled: true } }, tmp);
    const svc = new UsageService(cfg, () => nowMs);
    const snap = await svc.read();
    expect(snap.status).toBe('ok');
    expect(snap.providers).toHaveLength(2);
    expect(snap.providers[0]).toMatchObject({ id: 'codex', state: 'executableNotFound', error: 'Missing codex binary' });
    expect(snap.providers[1]).toMatchObject({ id: 'custom-cmd', source: 'fallback', stale: false });
    expect(snap.providers[1]?.meters[0]?.windows[0]?.usedPercent).toBe(62);
  });

  it('maps remainingFraction and windowName durations per AGENTS.md spec', () => {
    const mapped = mapProviderOutput(
      {
        root: '/command/data',
        meters: [
          {
            each: '/groups',
            id: { path: '/name' },
            label: { path: '/name' },
            windows: [
              {
                each: '/buckets',
                id: { path: '/id' },
                used: { path: '/remaining_fraction', as: 'remainingFraction' },
                resetsAt: { path: '/reset_time', as: 'iso8601' },
                duration: { path: '/window', as: 'windowName' },
              },
            ],
          },
        ],
      },
      {
        command: {
          data: {
            groups: [
              {
                name: 'Gemini Models',
                buckets: [{ id: 'g-5h', remaining_fraction: 0.7, reset_time: '2026-11-13T05:00:00Z', window: '5h' }],
              },
            ],
          },
        },
      },
    );
    expect(mapped.meters[0]?.windows[0]?.usedPercent).toBeCloseTo(30, 5);
    expect(mapped.meters[0]?.windows[0]?.ring).toBe('session');
  });

  it('triggers standby and resume transitions with events, audit entries, and HTTP endpoints', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-standby-'));
    const maestriDir = path.join(tmp, 'maestri');
    const providersDir = mkProvidersDir(maestriDir);
    const wsDir = path.join(tmp, 'ws-app');
    fs.mkdirSync(wsDir);
    fs.writeFileSync(path.join(wsDir, 'ROADMAP.md'), '## Done\n| # | Sprint | Result |\n| --- | --- | --- |\n| 1 | Boot | Ready |\n');

    const statusFile = path.join(providersDir, '.status.json');
    const writeStatus = (sessionPct: number) =>
      fs.writeFileSync(
        statusFile,
        JSON.stringify({
          updatedAt: new Date(nowMs).toISOString(),
          providers: [
            {
              id: 'claude',
              name: 'Claude Code',
              enabled: true,
              state: 'ready',
              lastSuccessAt: new Date(nowMs).toISOString(),
              meters: [
                {
                  id: 'plan',
                  label: 'Plan limits',
                  windows: [
                    { id: 'five_hour', title: 'Session', ring: 'session', usedPercent: sessionPct, resetsAt: '2026-11-13T06:00:00Z' },
                  ],
                },
              ],
            },
          ],
        }),
      );

    let nowMs = Date.parse('2026-11-13T02:00:00Z');
    writeStatus(96);

    const cfg = parseConfig(
      {
        cookieSecure: false,
        maestriDir,
        standby: {
          thresholds: { claude: { five_hour: 95 } },
          resumeBelowPercent: 80,
          notifyViaCli: true,
        },
      },
      tmp,
    );

    const db = openMemoryDb();
    const audit = new Audit(db, () => nowMs);
    const events = new EventStore(db, () => nowMs);
    const notifications: string[] = [];
    const wsList = parseWorkspaces([{ id: 'app', name: 'App', path: wsDir, roadmapPath: 'ROADMAP.md' }]);
    const guard = new StandbyGuard(db, audit, cfg, events, () => nowMs, (kind) => {
      notifications.push(kind);
    });
    const usage = new UsageService(cfg, () => nowMs);

    const s1 = await guard.evaluate(await usage.read(), wsList);
    expect(s1.active).toBe(true);
    expect(s1.triggers[0]?.providerId).toBe('claude');
    expect(notifications).toEqual(['standby']);
    expect(events.since('app', 0).map((e) => e.type)).toEqual(['standby']);

    // Usage drops to 85%: still >= resumeBelowPercent (80%), so stay in standby!
    nowMs += 60_000;
    writeStatus(85);
    const s2 = await guard.evaluate(await usage.read(), wsList);
    expect(s2.active).toBe(true);
    expect(notifications).toEqual(['standby']);

    // Usage resets to 10% < 80%: resume!
    nowMs += 60_000;
    writeStatus(10);
    const s3 = await guard.evaluate(await usage.read(), wsList);
    expect(s3.active).toBe(false);
    expect(notifications).toEqual(['standby', 'resume']);
    expect(events.since('app', 0).map((e) => e.type)).toEqual(['standby', 'resume']);

    // Verify API routes /api/usage and /api/workspaces/:id/roadmap require auth and serve data
    const totpSecret = generateSecret();
    const srv = await buildServer({
      config: cfg,
      db,
      secrets: { passwordHash: hashPassword('pw'), totpSecret, createdAt: 'x' },
      workspaces: new StaticWorkspaceProvider(wsList),
      now: () => nowMs,
    });

    const unauth = await srv.app.inject({ method: 'GET', url: '/api/usage', headers: { host: 'localhost:4777' } });
    expect(unauth.statusCode).toBe(401);

    const lp = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'localhost:4777' } });
    const csrf = /name="_csrf" value="([^"]+)"/.exec(lp.body)![1]!;
    const lc = lp.cookies.map((c) => `${c.name}=${c.value}`).join('; ');
    const lr = await srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'content-type': 'application/x-www-form-urlencoded', cookie: lc },
      payload: new URLSearchParams({ _csrf: csrf, password: 'pw', code: totp(totpSecret, nowMs) }).toString(),
    });
    const sc = `maestria_session=${lr.cookies.find((c) => c.name === 'maestria_session')!.value}`;

    const uRes = await srv.app.inject({ method: 'GET', url: '/api/usage', headers: { host: 'localhost:4777', cookie: sc } });
    expect(uRes.statusCode).toBe(200);
    expect(uRes.json().providers[0].id).toBe('claude');

    const rRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces/app/roadmap', headers: { host: 'localhost:4777', cookie: sc } });
    expect(rRes.statusCode).toBe(200);
    expect(rRes.json().roadmap.done[0].title).toBe('Boot');
  });
});
