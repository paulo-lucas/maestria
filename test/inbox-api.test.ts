import { beforeAll, describe, expect, it } from 'vitest';
import { parseConfig } from '../src/config.ts';
import { openMemoryDb } from '../src/db.ts';
import { hashPassword } from '../src/auth/password.ts';
import { generateSecret, totp } from '../src/auth/totp.ts';
import { buildServer, type Server } from '../src/server.ts';
import { parseWorkspaces, StaticWorkspaceProvider } from '../src/workspaces.ts';

const PASSWORD = 'correct horse battery staple';
const HOST = 'localhost:4777';
const ORIGIN = `http://${HOST}`;
const base = { host: HOST };

let passwordHash = '';
beforeAll(() => {
  passwordHash = hashPassword(PASSWORD);
});

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

async function setupWithWorkspaces() {
  const c = clock();
  const totpSecret = generateSecret();
  const config = parseConfig({ cookieSecure: false }, '/tmp/unused');
  const workspaces = new StaticWorkspaceProvider(
    parseWorkspaces([
      { id: 'alpha', name: 'Alpha', path: '/tmp/alpha', maestroAgent: 'Bulbasaur', color: '#5b4bd6' },
      { id: 'beta', name: 'Beta', path: '/tmp/beta', color: '#0e7490' },
    ]),
  );
  const srv: Server = await buildServer({
    config,
    db: openMemoryDb(),
    secrets: { passwordHash, totpSecret, createdAt: 'x' },
    workspaces,
    now: c.now,
  });

  c.advance(31_000);
  const loginGet = await srv.app.inject({ method: 'GET', url: '/login', headers: base });
  const csrfMatch = /name="_csrf" value="([^"]+)"/.exec(loginGet.body)!;
  const loginCookie = loginGet.cookies.map((x) => `${x.name}=${x.value}`).join('; ');
  const loginPost = await srv.app.inject({
    method: 'POST',
    url: '/login',
    headers: { ...base, origin: ORIGIN, 'content-type': 'application/x-www-form-urlencoded', cookie: loginCookie },
    payload: new URLSearchParams({
      _csrf: csrfMatch[1]!,
      password: PASSWORD,
      code: totp(totpSecret, c.now()),
    }).toString(),
  });
  expect(loginPost.statusCode).toBe(303);
  const sessionCookie = loginPost.cookies.find((x) => x.name === 'maestria_session')!.value;
  const cookie = `maestria_session=${sessionCookie}`;
  const sessionInfo = await srv.app.inject({ method: 'GET', url: '/api/session', headers: { ...base, cookie } });
  const { csrfToken } = sessionInfo.json<{ csrfToken: string }>();

  return { srv, cookie, csrfToken, clock: c };
}

describe('unified inbox and workspaces API', () => {
  it('requires authentication for inbox endpoints and workspaces.css', async () => {
    const { srv } = await setupWithWorkspaces();
    expect((await srv.app.inject({ method: 'GET', url: '/api/workspaces', headers: base })).statusCode).toBe(401);
    expect((await srv.app.inject({ method: 'GET', url: '/api/inbox', headers: base })).statusCode).toBe(401);
    expect((await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/inbox', headers: base })).statusCode).toBe(401);
    expect((await srv.app.inject({ method: 'GET', url: '/workspaces.css', headers: base })).statusCode).toBe(303);
  });

  it('serves workspaces.css, app.js, and keeps panel HTML free of inline scripts/styles/handlers', async () => {
    const { srv, cookie } = await setupWithWorkspaces();
    const home = await srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } });
    expect(home.statusCode).toBe(200);
    expect(home.body).toContain('<link rel="stylesheet" href="/workspaces.css">');
    expect(home.body).toContain('<script src="/static/app.js" defer></script>');
    expect(home.body).not.toMatch(/<script(?![^>]*\bsrc="\/static\/)/i);
    expect(home.body).not.toMatch(/\sstyle=/i);
    expect(home.body).not.toMatch(/\son\w+=/i);

    const wsCss = await srv.app.inject({ method: 'GET', url: '/workspaces.css', headers: { ...base, cookie } });
    expect(wsCss.statusCode).toBe(200);
    expect(wsCss.headers['content-type']).toContain('text/css');
    expect(wsCss.body).toContain('[data-ws="alpha"] { --ws-color: #5b4bd6; }');
    expect(wsCss.body).toContain('[data-ws="beta"] { --ws-color: #0e7490; }');

    const js = await srv.app.inject({ method: 'GET', url: '/static/app.js', headers: base });
    expect(js.statusCode).toBe(200);
    expect(js.headers['content-type']).toContain('javascript');
  });

  it('provides global unified Home inbox across workspaces with workspace tags, newest first, and filtering', async () => {
    const { srv, cookie, clock: c } = await setupWithWorkspaces();
    const aNotice = srv.inbox.post({ workspaceId: 'alpha', kind: 'notice', body: 'Alpha <script>x</script> **update**' });
    c.advance(100);
    const aApprove = srv.inbox.post({ workspaceId: 'alpha', kind: 'approve', body: 'Ship alpha?' });
    c.advance(100);
    const bInput = srv.inbox.post({ workspaceId: 'beta', kind: 'input', body: 'Beta domain?' });
    c.advance(100);
    const bNotice = srv.inbox.post({ workspaceId: 'beta', kind: 'notice', body: 'Beta update' });

    const homeRes = await srv.app.inject({ method: 'GET', url: '/api/inbox', headers: { ...base, cookie } });
    expect(homeRes.statusCode).toBe(200);
    const home = homeRes.json<{
      needsYou: { id: string; workspaceId: string; workspaceName: string; workspaceColor: string }[];
      updates: { id: string; bodyHtml: string; workspaceId: string }[];
      historyCount: number;
    }>();

    // Newest first in both Needs you and Updates
    expect(home.needsYou.map((i) => i.id)).toEqual([bInput.id, aApprove.id]);
    expect(home.needsYou[0]).toMatchObject({ workspaceId: 'beta', workspaceName: 'Beta', workspaceColor: '#0e7490' });
    expect(home.updates.map((i) => i.id)).toEqual([bNotice.id, aNotice.id]);
    expect(home.updates[1]!.bodyHtml).toContain('<strong>update</strong>');
    expect(home.updates[1]!.bodyHtml).not.toContain('<script>');

    // Filter Home by workspace
    const filtered = await srv.app.inject({ method: 'GET', url: '/api/inbox?workspace=alpha', headers: { ...base, cookie } });
    expect(filtered.statusCode).toBe(200);
    expect(filtered.json<{ needsYou: { id: string }[] }>().needsYou.map((i) => i.id)).toEqual([aApprove.id]);
  });

  it('handles responses and dismissals with CSRF, moves items to history, and updates counts', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const iApprove = srv.inbox.post({ workspaceId: 'alpha', kind: 'approve', body: 'Ship `v1`?' });
    const iChoice = srv.inbox.post({ workspaceId: 'alpha', kind: 'choice', body: 'Target env', options: ['Staging', 'Prod'] });
    const iNotice = srv.inbox.post({ workspaceId: 'alpha', kind: 'notice', body: 'QA finished' });

    const wsRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces', headers: { ...base, cookie } });
    expect(wsRes.json<{ workspaces: { counts: { needsYou: number; updates: number } }[] }>().workspaces[0]!.counts).toMatchObject({
      needsYou: 2,
      updates: 1,
    });

    // Missing CSRF is rejected
    const noCsrf = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${iApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'approved' }),
    });
    expect(noCsrf.statusCode).toBe(403);

    // Approve with optional comment
    const approveRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${iApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'approved', response: 'Looks great.' }),
    });
    expect(approveRes.statusCode).toBe(200);
    expect(approveRes.json<{ item: { status: string; decision: string; response: string } }>().item).toMatchObject({
      status: 'responded',
      decision: 'approved',
      response: 'Looks great.',
    });

    // Double-responding returns 409 Conflict
    const repeat = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${iApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'rejected' }),
    });
    expect(repeat.statusCode).toBe(409);

    // Notice dismiss via respond ('Got it')
    const ackRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${iNotice.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'ack' }),
    });
    expect(ackRes.statusCode).toBe(200);
    expect(ackRes.json<{ item: { status: string; decision: string } }>().item).toMatchObject({
      status: 'dismissed',
      decision: 'ack',
    });

    // Dismiss choice without responding
    const dismissRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${iChoice.id}/dismiss`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken },
    });
    expect(dismissRes.statusCode).toBe(200);

    // Workspace inbox now has 0 open items and historyCount=3
    const wsInbox = await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/inbox', headers: { ...base, cookie } });
    expect(wsInbox.json<{ needsYou: unknown[]; updates: unknown[]; historyCount: number }>()).toMatchObject({
      needsYou: [],
      updates: [],
      historyCount: 3,
    });

    const hist = await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/inbox?status=history', headers: { ...base, cookie } });
    expect(hist.json<{ items: unknown[] }>().items).toHaveLength(3);
  });

  it('returns 404 for unknown workspace/item and 400 for invalid payloads', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const item = srv.inbox.post({ workspaceId: 'alpha', kind: 'input', body: 'Say hi' });

    const cross = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/beta/inbox/${item.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ response: 'hi' }),
    });
    expect(cross.statusCode).toBe(404);

    const blank = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/inbox/${item.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ response: '   ' }),
    });
    expect(blank.statusCode).toBe(400);
  });
});
