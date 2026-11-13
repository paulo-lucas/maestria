import crypto from 'node:crypto';
import type { LightMyRequestResponse } from 'fastify';
import { beforeAll, describe, expect, it } from 'vitest';
import { defaultConfig, parseConfig, type Config } from '../src/config.ts';
import { openMemoryDb } from '../src/db.ts';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { buildServer, type Server } from '../src/server.ts';
import type { Jwk } from '../src/auth/cloudflare.ts';

const PASSWORD = 'correct horse battery staple';
const HOST = 'localhost:4777';
const ORIGIN = `http://${HOST}`;
let passwordHash = '';
beforeAll(() => {
  passwordHash = hashPassword(PASSWORD);
});

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

interface Ctx {
  srv: Server;
  clock: ReturnType<typeof clock>;
  totpSecret: string;
  config: Config;
}

async function setup(configOver: unknown = {}, extra: { jwksFetcher?: () => Promise<Jwk[]> } = {}): Promise<Ctx> {
  const c = clock();
  const totpSecret = generateSecret();
  const config = parseConfig({ cookieSecure: false, ...(configOver as object) }, '/tmp/unused');
  const srv = await buildServer({
    config,
    db: openMemoryDb(),
    secrets: { passwordHash, totpSecret, createdAt: 'x' },
    now: c.now,
    ...extra,
  });
  return { srv, clock: c, totpSecret, config };
}

const base = { host: HOST };

function cookieHeader(res: LightMyRequestResponse): string {
  return res.cookies.map((c) => `${c.name}=${c.value}`).join('; ');
}

function csrfFrom(html: string): string {
  const m = /name="_csrf" value="([^"]+)"/.exec(html);
  if (!m) throw new Error('no csrf in page');
  return m[1]!;
}

async function attemptLogin(ctx: Ctx, over: { password?: string; code?: string; ip?: string } = {}) {
  const page = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
  const csrf = csrfFrom(page.body);
  const body = new URLSearchParams({
    _csrf: csrf,
    password: over.password ?? PASSWORD,
    code: over.code ?? totp(ctx.totpSecret, ctx.clock.now()),
  }).toString();
  return ctx.srv.app.inject({
    method: 'POST',
    url: '/login',
    headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(page) },
    payload: body,
    remoteAddress: over.ip,
  });
}

async function login(ctx: Ctx) {
  ctx.clock.advance(31_000); // fresh TOTP step each time (codes are single-use)
  const res = await attemptLogin(ctx);
  expect(res.statusCode).toBe(303);
  const session = res.cookies.find((c) => c.name === 'maestria_session');
  expect(session).toBeDefined();
  return { cookie: `maestria_session=${session!.value}`, res };
}

describe('security headers', () => {
  it('are set on every response', async () => {
    const { srv } = await setup();
    for (const url of ['/login', '/static/app.css', '/nope']) {
      const res = await srv.app.inject({ method: 'GET', url, headers: base });
      expect(res.headers['content-security-policy']).toContain("script-src 'self'");
      expect(res.headers['content-security-policy']).not.toContain('unsafe-inline');
      expect(res.headers['content-security-policy']).toContain("frame-ancestors 'none'");
      expect(res.headers['x-frame-options']).toBe('DENY');
      expect(res.headers['x-content-type-options']).toBe('nosniff');
      expect(res.headers['referrer-policy']).toBe('same-origin');
      expect(res.headers['access-control-allow-origin']).toBeUndefined();
    }
  });

  it('adds HSTS only when cookies are Secure', async () => {
    const secure = await setup({ cookieSecure: true });
    const res = await secure.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    expect(res.headers['strict-transport-security']).toBeDefined();
    expect(res.cookies[0]!.name).toBe('__Host-maestria_login_csrf');
    const plain = await setup();
    const res2 = await plain.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    expect(res2.headers['strict-transport-security']).toBeUndefined();
  });

  it('serves no inline scripts or styles in pages', async () => {
    const { srv } = await setup();
    const res = await srv.app.inject({ method: 'GET', url: '/login', headers: base });
    expect(res.body).not.toMatch(/<script/i);
    expect(res.body).not.toMatch(/\sstyle=/i);
    expect(res.body).not.toMatch(/\son\w+=/i);
  });
});

describe('host validation', () => {
  it('rejects unknown Host headers (DNS rebinding)', async () => {
    const { srv } = await setup();
    const res = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'evil.example.com' } });
    expect(res.statusCode).toBe(421);
  });

  it('accepts configured public hosts', async () => {
    const { srv } = await setup({ allowedHosts: ['panel.example.com'] });
    const res = await srv.app.inject({ method: 'GET', url: '/login', headers: { host: 'panel.example.com' } });
    expect(res.statusCode).toBe(200);
  });
});

describe('authentication', () => {
  it('redirects unauthenticated pages to /login and 401s the API', async () => {
    const { srv } = await setup();
    const page = await srv.app.inject({ method: 'GET', url: '/', headers: base });
    expect(page.statusCode).toBe(303);
    expect(page.headers.location).toBe('/login');
    const api = await srv.app.inject({ method: 'GET', url: '/api/session', headers: base });
    expect(api.statusCode).toBe(401);
  });

  it('logs in with password + TOTP and sets a hardened session cookie', async () => {
    const ctx = await setup();
    const { res, cookie } = await login(ctx);
    const c = res.cookies.find((x) => x.name === 'maestria_session')!;
    expect(c.httpOnly).toBe(true);
    expect(c.sameSite).toBe('Strict');
    expect(c.path).toBe('/');
    const home = await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } });
    expect(home.statusCode).toBe(200);
    expect(home.body).toContain('Workspaces');
  });

  it('marks the session cookie Secure with a __Host- prefix when configured', async () => {
    const ctx = await setup({ cookieSecure: true });
    ctx.clock.advance(31_000);
    const page = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(page) },
      payload: new URLSearchParams({ _csrf: csrfFrom(page.body), password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    const c = res.cookies.find((x) => x.name === '__Host-maestria_session')!;
    expect(c.secure).toBe(true);
    expect(c.httpOnly).toBe(true);
  });

  it('logs in with password + TOTP when browser sends Origin: null and Sec-Fetch-Site: same-origin', async () => {
    const ctx = await setup();
    ctx.clock.advance(31_000);
    const page = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: {
        ...base,
        origin: 'null',
        'sec-fetch-site': 'same-origin',
        'content-type': 'application/x-www-form-urlencoded',
        cookie: cookieHeader(page),
      },
      payload: new URLSearchParams({ _csrf: csrfFrom(page.body), password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    expect(res.statusCode).toBe(303);
    expect(res.cookies.find((x) => x.name === 'maestria_session')).toBeDefined();
  });

  it('rejects a wrong password, a wrong code, and gives the same message for both', async () => {
    const ctx = await setup();
    const badPw = await attemptLogin(ctx, { password: 'wrong password!!' });
    const badCode = await attemptLogin(ctx, { code: '000000' });
    expect(badPw.statusCode).toBe(401);
    expect(badCode.statusCode).toBe(401);
    expect(badPw.body).toContain('Invalid credentials.');
    expect(badCode.body).toContain('Invalid credentials.');
    expect(badPw.cookies.find((c) => c.name === 'maestria_session')).toBeUndefined();
  });

  it('refuses to reuse a TOTP code (replay protection)', async () => {
    const ctx = await setup();
    ctx.clock.advance(31_000);
    const code = totp(ctx.totpSecret, ctx.clock.now());
    expect((await attemptLogin(ctx, { code })).statusCode).toBe(303);
    expect((await attemptLogin(ctx, { code })).statusCode).toBe(401);
  });

  it('writes login events to the audit log without secrets', async () => {
    const ctx = await setup();
    await attemptLogin(ctx, { password: 'wrong password!!' });
    await login(ctx);
    const events = ctx.srv.audit.recent().map((e) => e.event);
    expect(events).toContain('login_fail');
    expect(events).toContain('login_ok');
    expect(JSON.stringify(ctx.srv.audit.recent())).not.toContain(PASSWORD);
  });
});

describe('rate limit and lockout', () => {
  it('locks out after repeated failures, even for correct credentials, then recovers', async () => {
    const ctx = await setup({ login: { maxFailuresPerIp: 3, windowSeconds: 600, lockoutSeconds: 600 } });
    for (let i = 0; i < 3; i++) expect((await attemptLogin(ctx, { password: 'wrong password!!' })).statusCode).toBe(401);
    ctx.clock.advance(31_000);
    const locked = await attemptLogin(ctx);
    expect(locked.statusCode).toBe(429);
    expect(Number(locked.headers['retry-after'])).toBeGreaterThan(0);
    expect(locked.cookies.find((c) => c.name === 'maestria_session')).toBeUndefined();
    expect(ctx.srv.audit.recent().map((e) => e.event)).toContain('login_lockout');

    ctx.clock.advance(601_000);
    expect((await attemptLogin(ctx)).statusCode).toBe(303);
  });
});

describe('CSRF', () => {
  it('rejects a login POST without the CSRF token and re-renders the login form with a fresh token', async () => {
    const ctx = await setup();
    const page = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(page) },
      payload: new URLSearchParams({ password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    expect(res.statusCode).toBe(403);
    expect(res.headers['content-type']).toContain('text/html');
    expect(res.body).toContain('Your login form expired or did not match its security token. Please try again.');
    expect(res.body).not.toBe('Invalid CSRF token');

    // Submitting immediately with the refreshed form token and cookie succeeds.
    ctx.clock.advance(31_000);
    const freshCsrf = csrfFrom(res.body);
    const retry = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(res) },
      payload: new URLSearchParams({ _csrf: freshCsrf, password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    expect(retry.statusCode).toBe(303);
  });

  it('rejects a login POST whose token does not match the cookie and re-renders login cleanly', async () => {
    const ctx = await setup();
    const page = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(page) },
      payload: new URLSearchParams({ _csrf: 'forged', password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    expect(res.statusCode).toBe(403);
    expect(res.headers['content-type']).toContain('text/html');
    expect(res.body).toContain('Please try again.');
  });

  it('reuses a well-formed login CSRF cookie across double GET /login so the first form token still logs in', async () => {
    const ctx = await setup();
    ctx.clock.advance(31_000);
    const first = await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base });
    const firstCsrf = csrfFrom(first.body);
    const second = await ctx.srv.app.inject({
      method: 'GET',
      url: '/login',
      headers: { ...base, cookie: cookieHeader(first) },
    });
    expect(csrfFrom(second.body)).toBe(firstCsrf);

    const post = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: cookieHeader(second) },
      payload: new URLSearchParams({ _csrf: firstCsrf, password: PASSWORD, code: totp(ctx.totpSecret, ctx.clock.now()) }).toString(),
    });
    expect(post.statusCode).not.toBe(403);
    expect(post.statusCode).toBe(303);
    expect(post.cookies.find((x) => x.name === 'maestria_session')).toBeDefined();
  });

  it('requires the session CSRF token on logout', async () => {
    const ctx = await setup();
    const { cookie } = await login(ctx);
    const home = await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } });
    const csrf = csrfFrom(home.body);
    const form = { ...base, origin: ORIGIN, cookie, 'content-type': 'application/x-www-form-urlencoded' };

    const without = await ctx.srv.app.inject({ method: 'POST', url: '/logout', headers: form, payload: '' });
    expect(without.statusCode).toBe(403);
    const wrong = await ctx.srv.app.inject({ method: 'POST', url: '/logout', headers: form, payload: '_csrf=nope' });
    expect(wrong.statusCode).toBe(403);
    // Session still alive after rejected attempts.
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(200);

    const ok = await ctx.srv.app.inject({ method: 'POST', url: '/logout', headers: form, payload: `_csrf=${csrf}` });
    expect(ok.statusCode).toBe(303);
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(303);
  });

  it('accepts the token in the X-CSRF-Token header', async () => {
    const ctx = await setup();
    const { cookie } = await login(ctx);
    const info = await ctx.srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie } });
    const { csrfToken } = info.json<{ csrfToken: string }>();
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/logout',
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken },
    });
    expect(res.statusCode).toBe(303);
  });

  it('rejects cross-origin and origin-less writes even with a valid token', async () => {
    const ctx = await setup();
    const { cookie } = await login(ctx);
    const info = await ctx.srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie } });
    const { csrfToken } = info.json<{ csrfToken: string }>();
    const attempt = (headers: Record<string, string>) =>
      ctx.srv.app.inject({ method: 'POST', url: '/logout', headers: { ...base, cookie, 'x-csrf-token': csrfToken, ...headers } });
    expect((await attempt({ origin: 'https://evil.example.com' })).statusCode).toBe(403);
    expect((await attempt({ origin: 'null' })).statusCode).toBe(403);
    expect((await attempt({ origin: 'null', 'sec-fetch-site': 'cross-site' })).statusCode).toBe(403);
    expect((await attempt({})).statusCode).toBe(403);
    expect((await attempt({ origin: ORIGIN, 'sec-fetch-site': 'cross-site' })).statusCode).toBe(403);
    expect((await attempt({ origin: ORIGIN, 'sec-fetch-site': 'same-origin' })).statusCode).toBe(303);

    // Fresh session for testing Origin: null + Sec-Fetch-Site: same-origin accepted write
    const s2 = await login(ctx);
    const info2 = await ctx.srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie: s2.cookie } });
    const csrf2 = info2.json<{ csrfToken: string }>().csrfToken;
    const nullSameOriginRes = await ctx.srv.app.inject({
      method: 'POST',
      url: '/logout',
      headers: { ...base, cookie: s2.cookie, 'x-csrf-token': csrf2, origin: 'null', 'sec-fetch-site': 'same-origin' },
    });
    expect(nullSameOriginRes.statusCode).toBe(303);
  });
});

describe('session lifecycle', () => {
  it('expires after the idle timeout', async () => {
    const ctx = await setup({ session: { idleMinutes: 5, absoluteHours: 12 } });
    const { cookie } = await login(ctx);
    ctx.clock.advance(4 * 60_000);
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(200);
    ctx.clock.advance(6 * 60_000);
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(303);
  });

  it('expires at the absolute limit despite activity', async () => {
    const ctx = await setup({ session: { idleMinutes: 30, absoluteHours: 1 } });
    const { cookie } = await login(ctx);
    for (let i = 0; i < 5; i++) {
      ctx.clock.advance(11 * 60_000);
      expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(200);
    }
    ctx.clock.advance(11 * 60_000); // 66 minutes since login
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } })).statusCode).toBe(303);
  });

  it('logout-all invalidates every session', async () => {
    const ctx = await setup();
    const a = await login(ctx);
    const b = await login(ctx);
    const info = await ctx.srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie: a.cookie } });
    const { csrfToken } = info.json<{ csrfToken: string }>();
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/logout-all',
      headers: { ...base, origin: ORIGIN, cookie: a.cookie, 'x-csrf-token': csrfToken },
    });
    expect(res.statusCode).toBe(303);
    for (const c of [a.cookie, b.cookie]) {
      expect((await ctx.srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie: c } })).statusCode).toBe(401);
    }
    expect(ctx.srv.audit.recent().map((e) => e.event)).toContain('logout_all');
  });
});

describe('Cloudflare Access gate', () => {
  const TEAM = 'team.cloudflareaccess.com';
  const AUD = 'c'.repeat(64);

  it('rejects requests without a valid Access JWT, accepts with one', async () => {
    const { publicKey, privateKey } = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 });
    const jwk = { ...publicKey.export({ format: 'jwk' }), kid: 'k' } as Jwk;
    const ctx = await setup({ cloudflareAccess: { teamDomain: TEAM, audience: AUD } }, { jwksFetcher: async () => [jwk] });
    const enc = (o: unknown) => Buffer.from(JSON.stringify(o)).toString('base64url');
    const input = `${enc({ alg: 'RS256', kid: 'k' })}.${enc({ iss: `https://${TEAM}`, aud: [AUD], exp: Math.floor(ctx.clock.now() / 1000) + 300 })}`;
    const jwt = `${input}.${crypto.sign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url')}`;

    expect((await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: base })).statusCode).toBe(403);
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: { ...base, 'cf-access-jwt-assertion': 'x.y.z' } })).statusCode).toBe(403);
    expect((await ctx.srv.app.inject({ method: 'GET', url: '/login', headers: { ...base, 'cf-access-jwt-assertion': jwt } })).statusCode).toBe(200);
  });
});

describe('input limits', () => {
  it('rejects oversized bodies', async () => {
    const ctx = await setup();
    const res = await ctx.srv.app.inject({
      method: 'POST',
      url: '/login',
      headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded' },
      payload: 'password=' + 'a'.repeat(20_000),
    });
    expect(res.statusCode).toBe(413);
  });

  it('never exposes files outside the static whitelist', async () => {
    const ctx = await setup();
    for (const url of ['/static/..%2fpackage.json', '/static/server.ts', '/static/__proto__', '/static/constructor']) {
      const res = await ctx.srv.app.inject({ method: 'GET', url, headers: base });
      expect(res.statusCode).toBe(404);
    }
  });
});

void defaultConfig;
