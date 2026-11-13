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
      { id: 'alpha', name: 'Alpha', path: '/tmp/alpha', maestroAgent: 'Bulbasaur' },
      { id: 'beta', name: 'Beta', path: '/tmp/beta' },
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

  return { srv, cookie, csrfToken };
}

describe('workspaces, tasks, and notices API', () => {
  it('requires authentication for board endpoints', async () => {
    const { srv } = await setupWithWorkspaces();
    expect((await srv.app.inject({ method: 'GET', url: '/api/workspaces', headers: base })).statusCode).toBe(401);
    expect((await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/board', headers: base })).statusCode).toBe(401);
  });

  it('serves app.js and keeps panel HTML free of inline scripts/styles/handlers', async () => {
    const { srv, cookie } = await setupWithWorkspaces();
    const home = await srv.app.inject({ method: 'GET', url: '/', headers: { ...base, cookie } });
    expect(home.statusCode).toBe(200);
    expect(home.body).toContain('<script src="/static/app.js" defer></script>');
    expect(home.body).not.toMatch(/<script(?![^>]*\bsrc="\/static\/)/i);
    expect(home.body).not.toMatch(/\sstyle=/i);
    expect(home.body).not.toMatch(/\son\w+=/i);

    const js = await srv.app.inject({ method: 'GET', url: '/static/app.js', headers: base });
    expect(js.statusCode).toBe(200);
    expect(js.headers['content-type']).toContain('javascript');
  });

  it('handles typed task responses, moves them off the board into history, and enforces CSRF', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const tApprove = srv.tasks.add({ workspaceId: 'alpha', type: 'approve', text: 'Ship `v1`?' });
    const tChoice = srv.tasks.add({ workspaceId: 'alpha', type: 'choice', text: 'Target env', options: ['Staging', 'Prod'] });
    const tAck = srv.tasks.add({ workspaceId: 'alpha', type: 'ack', text: 'Read notes' });
    srv.notices.add({ workspaceId: 'alpha', body: 'Hello <script>x</script> **world**' });

    const wsRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces', headers: { ...base, cookie } });
    expect(wsRes.statusCode).toBe(200);
    const wsJson = wsRes.json<{ workspaces: { id: string; counts: { openTasks: number; activeNotices: number } }[] }>();
    expect(wsJson.workspaces[0]!.counts).toMatchObject({ openTasks: 3, activeNotices: 1 });

    // Missing CSRF is rejected
    const noCsrf = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${tApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'approved', response: 'Yes' }),
    });
    expect(noCsrf.statusCode).toBe(403);

    // Approve with optional comment
    const approveRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${tApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'approved', response: 'Looks great.' }),
    });
    expect(approveRes.statusCode).toBe(200);
    expect(approveRes.json<{ task: { status: string; decision: string; response: string } }>().task).toMatchObject({
      status: 'responded',
      decision: 'approved',
      response: 'Looks great.',
    });

    // Double-responding returns 409 Conflict
    const repeat = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${tApprove.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'rejected' }),
    });
    expect(repeat.statusCode).toBe(409);

    // Choice selection
    const choiceRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${tChoice.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ decision: 'Staging' }),
    });
    expect(choiceRes.statusCode).toBe(200);

    // Dismiss tAck without responding
    const dismissRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${tAck.id}/dismiss`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken },
    });
    expect(dismissRes.statusCode).toBe(200);

    // Board now has 0 open tasks, historyCount=3, and sanitized notice HTML
    const boardRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/board', headers: { ...base, cookie } });
    const board = boardRes.json<{
      tasks: unknown[];
      historyCount: number;
      notices: { bodyHtml: string }[];
    }>();
    expect(board.tasks).toHaveLength(0);
    expect(board.historyCount).toBe(3);
    expect(board.notices[0]!.bodyHtml).toContain('<strong>world</strong>');
    expect(board.notices[0]!.bodyHtml).not.toContain('<script>');

    // History endpoint returns all 3 resolved tasks
    const histRes = await srv.app.inject({
      method: 'GET',
      url: '/api/workspaces/alpha/tasks?status=history',
      headers: { ...base, cookie },
    });
    expect(histRes.json<{ tasks: unknown[] }>().tasks).toHaveLength(3);
  });

  it('dismisses notices by stable ID so concurrent/multiple dismissals never hit the wrong notice', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const n1 = srv.notices.add({ workspaceId: 'alpha', body: 'First notice' });
    const n2 = srv.notices.add({ workspaceId: 'alpha', body: 'Second notice' });

    const res = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/notices/${n1.id}/dismiss`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken },
    });
    expect(res.statusCode).toBe(200);

    const active = await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/notices', headers: { ...base, cookie } });
    expect(active.json<{ notices: { id: string }[] }>().notices.map((n) => n.id)).toEqual([n2.id]);
    expect(srv.audit.recent().map((e) => e.event)).toContain('notice_dismiss');
  });

  it('returns 404 for unknown workspace/task/notice and 400 for invalid payloads', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const t = srv.tasks.add({ workspaceId: 'alpha', type: 'input', text: 'Say hi' });

    // Cross-workspace isolation
    const cross = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/beta/tasks/${t.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ response: 'hi' }),
    });
    expect(cross.statusCode).toBe(404);

    // Blank input response rejected
    const blank = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${t.id}/respond`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ response: '   ' }),
    });
    expect(blank.statusCode).toBe(400);
  });
});
