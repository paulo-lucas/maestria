import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { runTunnelCommand } from '../src/commands/tunnel.ts';
import { parseConfig, saveRawConfig } from '../src/config.ts';
import { openDb, openMemoryDb } from '../src/db.ts';
import { buildServer } from '../src/server.ts';

function capture() {
  const s = new PassThrough();
  let text = '';
  s.on('data', (c) => (text += c.toString('utf8')));
  return { stream: s, read: () => text };
}

describe('Kill switch, tunnel control, and audit log', () => {
  it('stops tunnel, invalidates all sessions, blocks remote logins until local tunnel on, and logs audit events', async () => {
    let t = 1_700_000_000_000;
    const totpSecret = generateSecret();
    const calls: string[][] = [];
    const config = parseConfig(
      {
        cookieSecure: false,
        tunnel: {
          stopCommand: ['pkill', '-x', 'cloudflared'],
          startCommand: ['launchctl', 'bootstrap', 'gui/501'],
          statusCommand: ['pgrep', '-x', 'cloudflared'],
        },
      },
      '/tmp/unused',
    );
    const db = openMemoryDb();
    const srv = await buildServer({
      config,
      db,
      secrets: { passwordHash: hashPassword('pw'), totpSecret, createdAt: 'x' },
      now: () => t,
      tunnelRunner: async (argv) => {
        calls.push([...argv]);
        return { ok: true, code: 0, stderr: '' };
      },
    });

    const doLogin = async () => {
      t += 31_000;
      const lp = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'localhost:4777' } });
      const csrfLogin = /name="_csrf" value="([^"]+)"/.exec(lp.body)![1]!;
      const lc = lp.cookies.map((c) => `${c.name}=${c.value}`).join('; ');
      return srv.app.inject({
        method: 'POST',
        url: '/login',
        headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'content-type': 'application/x-www-form-urlencoded', cookie: lc },
        payload: new URLSearchParams({ _csrf: csrfLogin, password: 'pw', code: totp(totpSecret, t) }).toString(),
      });
    };

    const r1 = await doLogin();
    const r2 = await doLogin();
    const sc1 = `maestria_session=${r1.cookies.find((c) => c.name === 'maestria_session')!.value}`;
    const sc2 = `maestria_session=${r2.cookies.find((c) => c.name === 'maestria_session')!.value}`;
    const csrf1 = (await srv.app.inject({ method: 'GET', url: '/api/session', headers: { host: 'localhost:4777', cookie: sc1 } })).json().csrfToken as string;

    const stRes = await srv.app.inject({ method: 'GET', url: '/api/tunnel', headers: { host: 'localhost:4777', cookie: sc1 } });
    expect(stRes.statusCode).toBe(200);
    expect(stRes.json()).toMatchObject({ killed: false, stopConfigured: true, startConfigured: true, running: true });

    // Kill switch requires CSRF
    const badCsrf = await srv.app.inject({
      method: 'POST',
      url: '/api/kill-switch',
      headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'x-csrf-token': 'wrong', cookie: sc1 },
    });
    expect(badCsrf.statusCode).toBe(403);

    // Valid kill switch triggers stopCommand and invalidates BOTH sessions
    const killRes = await srv.app.inject({
      method: 'POST',
      url: '/api/kill-switch',
      headers: { host: 'localhost:4777', origin: 'http://localhost:4777', 'x-csrf-token': csrf1, cookie: sc1 },
    });
    expect(killRes.statusCode).toBe(200);
    expect(killRes.json()).toMatchObject({ ok: true, sessionsInvalidated: 2 });
    expect(calls).toContainEqual(['pkill', '-x', 'cloudflared']);

    expect((await srv.app.inject({ method: 'GET', url: '/api/session', headers: { host: 'localhost:4777', cookie: sc1 } })).statusCode).toBe(401);
    expect((await srv.app.inject({ method: 'GET', url: '/api/session', headers: { host: 'localhost:4777', cookie: sc2 } })).statusCode).toBe(401);

    // Logins are refused while kill switch is active even with valid password + fresh TOTP
    const lockedLogin = await doLogin();
    expect(lockedLogin.statusCode).toBe(403);
    expect(lockedLogin.body).toContain('maestria tunnel on');

    // Only local tunnel enable unlocks it
    const enabled = await srv.tunnel.enableLocal();
    expect(enabled.wasKilled).toBe(true);
    expect(calls).toContainEqual(['launchctl', 'bootstrap', 'gui/501']);

    const afterUnlock = await doLogin();
    expect(afterUnlock.statusCode).toBe(303);
    const sc3 = `maestria_session=${afterUnlock.cookies.find((c) => c.name === 'maestria_session')!.value}`;

    const auditRes = await srv.app.inject({ method: 'GET', url: '/api/audit?limit=50', headers: { host: 'localhost:4777', cookie: sc3 } });
    expect(auditRes.statusCode).toBe(200);
    const eventNames = (auditRes.json().entries as { event: string }[]).map((e) => e.event);
    expect(eventNames).toContain('kill_switch');
    expect(eventNames).toContain('login_blocked_killed');
    expect(eventNames).toContain('tunnel_on');
  });

  it('supports CLI maestria tunnel status|off|on', async () => {
    const configDir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-tunnel-cli-'));
    saveRawConfig(configDir, {
      tunnel: {
        stopCommand: ['stop-tunnel'],
        startCommand: ['start-tunnel'],
      },
    });
    // Ensure DB exists cleanly
    openDb(configDir).close();

    const invoked: string[][] = [];
    const runner = async (argv: readonly string[]) => {
      invoked.push([...argv]);
      return { ok: true, code: 0, stderr: '' };
    };

    const offOut = capture();
    await runTunnelCommand(['off', '--json'], { configDir, stdout: offOut.stream, runner });
    expect(JSON.parse(offOut.read())).toMatchObject({ ok: true });
    expect(invoked).toEqual([['stop-tunnel']]);

    const stOut = capture();
    await runTunnelCommand(['status', '--json'], { configDir, stdout: stOut.stream, runner });
    expect(JSON.parse(stOut.read())).toMatchObject({ killed: true });

    const onOut = capture();
    await runTunnelCommand(['on', '--json'], { configDir, stdout: onOut.stream, runner });
    expect(JSON.parse(onOut.read())).toMatchObject({ ok: true, wasKilled: true });
    expect(invoked).toEqual([['stop-tunnel'], ['start-tunnel']]);
  });
});
