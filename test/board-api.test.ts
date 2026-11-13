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

  it('lists workspaces with counts and handles task lifecycle with CSRF', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const t1 = srv.tasks.add({ workspaceId: 'alpha', kind: 'question', text: 'Ship `v1`?' });
    const t2 = srv.tasks.add({ workspaceId: 'alpha', kind: 'task', text: 'Check logs' });
    srv.notices.add({ workspaceId: 'alpha', body: 'Hello <script>x</script> **world**' });

    const wsRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces', headers: { ...base, cookie } });
    expect(wsRes.statusCode).toBe(200);
    const wsJson = wsRes.json<{ workspaces: { id: string; counts: { openTasks: number; activeNotices: number } }[] }>();
    expect(wsJson.workspaces[0]!.counts).toMatchObject({ openTasks: 2, activeNotices: 1 });

    // Missing CSRF is rejected
    const noCsrf = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${t1.id}`,
      headers: { ...base, origin: ORIGIN, cookie, 'content-type': 'application/json' },
      payload: JSON.stringify({ status: 'done', answer: 'Yes' }),
    });
    expect(noCsrf.statusCode).toBe(403);

    // Valid answer + done transition
    const doneRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${t1.id}`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ status: 'done', answer: 'Yes, ship it.' }),
    });
    expect(doneRes.statusCode).toBe(200);
    expect(doneRes.json<{ task: { status: string; answer: string } }>().task).toMatchObject({
      status: 'done',
      answer: 'Yes, ship it.',
    });

    // Dismiss (archive) t2
    const dismissRes = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${t2.id}`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ status: 'dismissed' }),
    });
    expect(dismissRes.statusCode).toBe(200);

    // Board excludes dismissed by default, shows dismissedCount=1, and sanitized notice HTML
    const boardRes = await srv.app.inject({ method: 'GET', url: '/api/workspaces/alpha/board', headers: { ...base, cookie } });
    const board = boardRes.json<{
      tasks: { id: string }[];
      dismissedCount: number;
      notices: { bodyHtml: string }[];
    }>();
    expect(board.tasks.map((t) => t.id)).toEqual([t1.id]);
    expect(board.dismissedCount).toBe(1);
    expect(board.notices[0]!.bodyHtml).toContain('<strong>world</strong>');
    expect(board.notices[0]!.bodyHtml).not.toContain('<script>');

    // Archived task is still available via includeDismissed=1
    const withArchived = await srv.app.inject({
      method: 'GET',
      url: '/api/workspaces/alpha/board?includeDismissed=1',
      headers: { ...base, cookie },
    });
    expect(withArchived.json<{ tasks: unknown[] }>().tasks).toHaveLength(2);
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

  it('returns 404 for unknown workspace, task, or notice, and 400 for invalid payloads', async () => {
    const { srv, cookie, csrfToken } = await setupWithWorkspaces();
    const t = srv.tasks.add({ workspaceId: 'alpha', text: 'Alpha only' });

    // Cross-workspace isolation: task in alpha cannot be modified via beta
    const cross = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/beta/tasks/${t.id}`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ status: 'done' }),
    });
    expect(cross.statusCode).toBe(404);

    const badStatus = await srv.app.inject({
      method: 'POST',
      url: `/api/workspaces/alpha/tasks/${t.id}`,
      headers: { ...base, origin: ORIGIN, cookie, 'x-csrf-token': csrfToken, 'content-type': 'application/json' },
      payload: JSON.stringify({ status: 'deleted' }),
    });
    expect(badStatus.statusCode).toBe(400);
  });
});
