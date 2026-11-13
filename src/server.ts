import crypto from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import Fastify, { type FastifyInstance, type FastifyReply, type FastifyRequest } from 'fastify';
import cookie from '@fastify/cookie';
import type { Config } from './config.ts';
import type { Db } from './db.ts';
import { Audit } from './audit.ts';
import { AuthService } from './auth/service.ts';
import { SessionStore, type Session } from './auth/sessions.ts';
import { LoginLimiter } from './auth/ratelimit.ts';
import { AccessVerifier, type JwksFetcher, fetchJwks } from './auth/cloudflare.ts';
import type { AuthSecrets } from './auth/secrets.ts';
import { ChatStore } from './chat.ts';
import { ConflictError, InboxStore, ValidationError, type InboxStatus } from './inbox.ts';
import { EventStore } from './events.ts';
import { readWorkspaceRoadmap } from './roadmap.ts';
import { StandbyGuard, type StandbyNotifier } from './standby.ts';
import { TunnelController, type CommandRunner } from './tunnel.ts';
import { UsageService, type FallbackRunner } from './usage.ts';
import { WorkspaceError, WorkspaceRegistry, type WorkspaceProvider } from './workspaces.ts';
import { homePage, loginPage } from './web/html.ts';

declare module 'fastify' {
  interface FastifyRequest {
    session: Session | null;
    clientIp: string;
  }
  interface FastifyContextConfig {
    /** Reachable without a session. */
    public?: boolean;
    /** CSRF token comes from the pre-login cookie instead of the session. */
    loginCsrf?: boolean;
  }
}

export interface ServerDeps {
  config: Config;
  db: Db;
  secrets: AuthSecrets;
  workspaces?: WorkspaceProvider;
  now?: () => number;
  logger?: boolean;
  jwksFetcher?: JwksFetcher;
  fallbackRunner?: FallbackRunner;
  standbyNotifier?: StandbyNotifier;
  tunnelRunner?: CommandRunner;
  enableUsagePoll?: boolean;
}

const PUBLIC_DIR = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'public');
const STATIC_FILES: Record<string, { type: string }> = {
  'app.css': { type: 'text/css; charset=utf-8' },
  'app.js': { type: 'application/javascript; charset=utf-8' },
};

export const CSP = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self' data:",
  "connect-src 'self'",
  "font-src 'self'",
  "form-action 'self'",
  "frame-ancestors 'none'",
  "base-uri 'none'",
].join('; ');

const SAFE_METHODS = new Set(['GET', 'HEAD']);

function safeEqual(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && crypto.timingSafeEqual(ab, bb);
}

function formString(body: unknown, key: string, max: number): string {
  if (typeof body !== 'object' || body === null) return '';
  const v = (body as Record<string, unknown>)[key];
  return typeof v === 'string' ? v.slice(0, max) : '';
}

export interface Server {
  app: FastifyInstance;
  sessions: SessionStore;
  audit: Audit;
  limiter: LoginLimiter;
  workspaces: WorkspaceProvider;
  events: EventStore;
  inbox: InboxStore;
  chat: ChatStore;
  usage: UsageService;
  standby: StandbyGuard;
  tunnel: TunnelController;
}

export async function buildServer(deps: ServerDeps): Promise<Server> {
  const { config, db, secrets } = deps;
  const now = deps.now ?? Date.now;

  const audit = new Audit(db, now);
  const workspaces = deps.workspaces ?? new WorkspaceRegistry(config.configDir);
  const events = new EventStore(db, now);
  const inbox = new InboxStore(db, audit, now, events);
  const chat = new ChatStore(db, audit, events, now);
  const usage = new UsageService(config, now, deps.fallbackRunner);
  const standby = new StandbyGuard(db, audit, config, events, now, deps.standbyNotifier);
  const sessions = new SessionStore(db, { idleMs: config.session.idleMinutes * 60_000, absoluteMs: config.session.absoluteHours * 3_600_000 }, now);
  const tunnel = new TunnelController(db, audit, sessions, config, now, deps.tunnelRunner);
  const limiter = new LoginLimiter(
    {
      maxFailuresPerIp: config.login.maxFailuresPerIp,
      maxFailuresGlobal: config.login.maxFailuresGlobal,
      windowMs: config.login.windowSeconds * 1000,
      lockoutMs: config.login.lockoutSeconds * 1000,
    },
    now,
  );
  const auth = new AuthService({ db, secrets, sessions, limiter, audit, now });
  const access = config.cloudflareAccess ? new AccessVerifier(config.cloudflareAccess, deps.jwksFetcher ?? fetchJwks, now) : null;

  const allowedHosts = new Set<string>([`localhost:${config.port}`, `127.0.0.1:${config.port}`, ...config.allowedHosts]);
  const sessionCookie = config.cookieSecure ? '__Host-maestria_session' : 'maestria_session';
  const loginCsrfCookie = config.cookieSecure ? '__Host-maestria_login_csrf' : 'maestria_login_csrf';
  const cookieOpts = { path: '/', httpOnly: true, secure: config.cookieSecure, sameSite: 'strict' as const };

  const app = Fastify({
    logger: deps.logger ? { level: 'info' } : false,
    bodyLimit: 32 * 1024,
    trustProxy: false,
    routerOptions: { maxParamLength: 100 },
  });

  await app.register(cookie);

  app.addContentTypeParser('application/x-www-form-urlencoded', { parseAs: 'string', bodyLimit: 8 * 1024 }, (_req, body, done) => {
    try {
      done(null, Object.fromEntries(new URLSearchParams(body as string)));
    } catch (err) {
      done(err as Error);
    }
  });

  app.decorateRequest('session', null);
  app.decorateRequest('clientIp', '');

  // --- security headers on every response ---
  app.addHook('onSend', async (req, reply, payload) => {
    reply.header('Content-Security-Policy', CSP);
    reply.header('X-Frame-Options', 'DENY');
    reply.header('X-Content-Type-Options', 'nosniff');
    reply.header('Referrer-Policy', 'same-origin');
    reply.header('Cross-Origin-Opener-Policy', 'same-origin');
    reply.header('Cross-Origin-Resource-Policy', 'same-origin');
    reply.header('Permissions-Policy', 'camera=(), microphone=(), geolocation=(), payment=(), usb=()');
    if (config.cookieSecure) reply.header('Strict-Transport-Security', 'max-age=31536000');
    if (!reply.hasHeader('Cache-Control')) reply.header('Cache-Control', 'no-store');
    return payload;
  });

  function resolveClientIp(req: FastifyRequest): string {
    if (config.trustCfConnectingIp) {
      const h = req.headers['cf-connecting-ip'];
      if (typeof h === 'string' && net.isIP(h.trim())) return h.trim();
    }
    return req.socket.remoteAddress ?? 'unknown';
  }

  function deny(reply: FastifyReply, code: number, message: string): FastifyReply {
    return reply.code(code).type('text/plain; charset=utf-8').send(message);
  }

  // --- gate: host, Cloudflare Access, origin, session ---
  app.addHook('onRequest', async (req, reply) => {
    req.clientIp = resolveClientIp(req);

    // DNS-rebinding defence: only answer to known Host names.
    const host = (req.headers.host ?? '').toLowerCase();
    if (!allowedHosts.has(host)) return deny(reply, 421, 'Misdirected request');

    if (access) {
      const ok = await access.verify(req.headers['cf-access-jwt-assertion'] as string | undefined);
      if (!ok) {
        audit.logThrottled(`cf:${req.clientIp}`, 'access_jwt_reject', { ip: req.clientIp });
        return deny(reply, 403, 'Forbidden');
      }
    }

    // Cross-site write defence (in addition to CSRF tokens): browsers always send Origin on POST.
    // Some browsers send Origin: null under restrictive referrer or privacy modes for same-origin form POSTs,
    // so only allow Origin: null when Sec-Fetch-Site proves the request is same-origin.
    if (!SAFE_METHODS.has(req.method)) {
      const origin = req.headers.origin;
      const fetchSite = req.headers['sec-fetch-site'];
      let sameOrigin = false;
      if (origin === 'null') {
        sameOrigin = fetchSite === 'same-origin';
      } else if (typeof origin === 'string') {
        try {
          sameOrigin = new URL(origin).host.toLowerCase() === host;
        } catch {
          sameOrigin = false;
        }
      }
      if (!sameOrigin || (typeof fetchSite === 'string' && fetchSite !== 'same-origin')) {
        audit.logThrottled(`origin:${req.clientIp}`, 'origin_reject', { ip: req.clientIp, detail: { path: req.url.slice(0, 100) } });
        return deny(reply, 403, 'Forbidden');
      }
    }

    // Session resolution. Routes marked `public` skip the requirement.
    const token = req.cookies[sessionCookie];
    req.session = sessions.validate(token);
    if (!req.session && !req.routeOptions.config?.public) {
      if (req.url.startsWith('/api/')) return reply.code(401).send({ error: 'unauthenticated' });
      if (req.method === 'GET') return reply.redirect('/login', 303);
      return deny(reply, 401, 'Unauthorized');
    }
  });

  // --- CSRF: every state-changing request must carry the matching token ---
  app.addHook('preHandler', async (req, reply) => {
    if (SAFE_METHODS.has(req.method)) return;
    const given = req.headers['x-csrf-token'] ?? formString(req.body, '_csrf', 200);
    const supplied = typeof given === 'string' ? given : '';
    let expected = '';
    if (req.routeOptions.config?.loginCsrf) {
      expected = req.cookies[loginCsrfCookie] ?? '';
    } else if (req.session) {
      expected = req.session.csrfToken;
    }
    if (!expected || !supplied || !safeEqual(expected, supplied)) {
      audit.log('csrf_reject', { ip: req.clientIp, detail: { path: req.url.slice(0, 100) } });
      return deny(reply, 403, 'Invalid CSRF token');
    }
  });

  // --- routes ---
  app.get('/static/:file', { config: { public: true } }, async (req, reply) => {
    const name = (req.params as { file: string }).file;
    const meta = Object.hasOwn(STATIC_FILES, name) ? STATIC_FILES[name] : undefined;
    if (!meta) return deny(reply, 404, 'Not found');
    return reply.header('Cache-Control', 'no-cache').type(meta.type).send(fs.readFileSync(path.join(PUBLIC_DIR, name)));
  });

  app.get('/login', { config: { public: true } }, async (req, reply) => {
    if (req.session) return reply.redirect('/', 303);
    // Reuse a well-formed existing token: browsers often load /login twice (prefetch, reload, two tabs),
    // and rotating it on every GET made the first form's token stale ("Invalid CSRF token").
    const existing = req.cookies[loginCsrfCookie];
    const csrf =
      typeof existing === 'string' && /^[A-Za-z0-9_-]{43}$/.test(existing)
        ? existing
        : crypto.randomBytes(32).toString('base64url');
    reply.setCookie(loginCsrfCookie, csrf, cookieOpts);
    return reply.type('text/html; charset=utf-8').send(loginPage({ csrf }));
  });

  app.post('/login', { config: { public: true, loginCsrf: true } }, async (req, reply) => {
    const csrf = crypto.randomBytes(32).toString('base64url');
    if (tunnel.isKilled()) {
      audit.logThrottled(`killed:${req.clientIp}`, 'login_blocked_killed', { ip: req.clientIp });
      reply.setCookie(loginCsrfCookie, csrf, cookieOpts);
      return reply
        .code(403)
        .type('text/html; charset=utf-8')
        .send(loginPage({ csrf, error: 'Panel access is disabled by the kill switch. Re-enable locally via "maestria tunnel on".' }));
    }
    const result = auth.login({
      ip: req.clientIp,
      userAgent: String(req.headers['user-agent'] ?? ''),
      password: formString(req.body, 'password', 256),
      code: formString(req.body, 'code', 16),
    });
    if (!result.ok) {
      reply.setCookie(loginCsrfCookie, csrf, cookieOpts);
      if (result.reason === 'locked') {
        reply.header('Retry-After', String(result.retryAfterSeconds));
        const minutes = Math.ceil(result.retryAfterSeconds / 60);
        return reply.code(429).type('text/html; charset=utf-8').send(loginPage({ csrf, error: `Too many failed attempts. Try again in ${minutes} minute${minutes === 1 ? '' : 's'}.` }));
      }
      return reply.code(401).type('text/html; charset=utf-8').send(loginPage({ csrf, error: 'Invalid credentials.' }));
    }
    reply.clearCookie(loginCsrfCookie, { path: '/' });
    reply.setCookie(sessionCookie, result.token, cookieOpts);
    return reply.redirect('/', 303);
  });

  app.post('/logout', async (req, reply) => {
    sessions.destroy(req.cookies[sessionCookie]);
    audit.log('logout', { ip: req.clientIp });
    reply.clearCookie(sessionCookie, { path: '/' });
    return reply.redirect('/login', 303);
  });

  app.post('/logout-all', async (req, reply) => {
    const n = sessions.destroyAll();
    audit.log('logout_all', { ip: req.clientIp, detail: { sessions: n } });
    reply.clearCookie(sessionCookie, { path: '/' });
    return reply.redirect('/login', 303);
  });

  app.get('/', async (req, reply) => {
    return reply.type('text/html; charset=utf-8').send(homePage({ csrf: req.session!.csrfToken }));
  });

  app.get('/api/session', async (req) => ({ csrfToken: req.session!.csrfToken, expiresAt: req.session!.expiresAt }));

  app.get('/workspaces.css', async (_req, reply) => {
    const css = workspaces
      .list()
      .filter((w) => /^[a-z0-9][a-z0-9._-]{0,63}$/.test(w.id) && /^#[0-9a-f]{6}$/i.test(w.color))
      .map((w) => `[data-ws="${w.id}"] { --ws-color: ${w.color}; }`)
      .join('\n');
    return reply.header('Cache-Control', 'no-cache').type('text/css; charset=utf-8').send(css + '\n');
  });

  app.get('/api/workspaces', async () => {
    const list = workspaces.list();
    const counts = inbox.countsByWorkspace();
    return {
      workspaces: list.map((ws) => {
        const c = counts.get(ws.id) ?? { needsYou: 0, updates: 0, history: 0 };
        return {
          id: ws.id,
          name: ws.name,
          path: ws.path,
          color: ws.color,
          maestroAgent: ws.maestroAgent,
          hasRoadmap: ws.roadmapPath !== null,
          counts: c,
        };
      }),
    };
  });

  app.get('/api/inbox', async (req, reply) => {
    const q = (req.query ?? {}) as { workspace?: string; status?: string };
    const status = q.status ?? 'open';
    if (!['open', 'history', 'responded', 'dismissed', 'all'].includes(status)) {
      return reply.code(400).send({ error: 'invalid_status' });
    }
    const list = workspaces.list();
    const byId = new Map(list.map((w) => [w.id, w]));
    if (q.workspace !== undefined) {
      if (!byId.has(q.workspace)) return reply.code(404).send({ error: 'workspace_not_found' });
    }

    const counts = inbox.countsByWorkspace();
    const enrich = (item: ReturnType<InboxStore['list']>[number]) => {
      const ws = byId.get(item.workspaceId);
      return ws ? { ...item, workspaceName: ws.name, workspaceColor: ws.color } : null;
    };

    if (status !== 'open') {
      const items = inbox
        .list({ workspaceId: q.workspace, status: status as InboxStatus | 'history' | 'all' })
        .map(enrich)
        .filter((x): x is NonNullable<typeof x> => x !== null);
      return { items };
    }

    const openItems = inbox
      .list({ workspaceId: q.workspace, status: 'open' })
      .map(enrich)
      .filter((x): x is NonNullable<typeof x> => x !== null);

    const needsYou = openItems.filter((i) => i.kind !== 'notice');
    const updates = openItems.filter((i) => i.kind === 'notice');

    let historyCount = 0;
    if (q.workspace) {
      historyCount = counts.get(q.workspace)?.history ?? 0;
    } else {
      for (const ws of list) {
        historyCount += counts.get(ws.id)?.history ?? 0;
      }
    }

    return { needsYou, updates, historyCount };
  });

  app.get('/api/workspaces/:id/inbox', async (req, reply) => {
    const { id } = req.params as { id: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    const status = ((req.query as { status?: string } | undefined)?.status ?? 'open') as string;
    if (!['open', 'history', 'responded', 'dismissed', 'all'].includes(status)) {
      return reply.code(400).send({ error: 'invalid_status' });
    }
    const wsMeta = {
      id: ws.id,
      name: ws.name,
      path: ws.path,
      color: ws.color,
      maestroAgent: ws.maestroAgent,
      hasRoadmap: ws.roadmapPath !== null,
    };
    const enrich = (item: ReturnType<InboxStore['list']>[number]) => ({
      ...item,
      workspaceName: ws.name,
      workspaceColor: ws.color,
    });
    if (status !== 'open') {
      return {
        workspace: wsMeta,
        items: inbox.list({ workspaceId: ws.id, status: status as InboxStatus | 'history' | 'all' }).map(enrich),
      };
    }
    const openItems = inbox.list({ workspaceId: ws.id, status: 'open' }).map(enrich);
    const c = inbox.countsByWorkspace().get(ws.id) ?? { needsYou: 0, updates: 0, history: 0 };
    return {
      workspace: wsMeta,
      needsYou: openItems.filter((i) => i.kind !== 'notice'),
      updates: openItems.filter((i) => i.kind === 'notice'),
      historyCount: c.history,
    };
  });

  app.post('/api/workspaces/:id/inbox/:itemId/respond', async (req, reply) => {
    const { id, itemId } = req.params as { id: string; itemId: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    const body = req.body ?? {};
    if (typeof body !== 'object' || body === null || Array.isArray(body)) {
      return reply.code(400).send({ error: 'invalid_body' });
    }
    const b = body as Record<string, unknown>;
    if (b.decision !== undefined && typeof b.decision !== 'string') {
      return reply.code(400).send({ error: 'invalid_decision' });
    }
    if (b.response !== undefined && typeof b.response !== 'string') {
      return reply.code(400).send({ error: 'invalid_response' });
    }
    const updated = inbox.respond(
      ws.id,
      itemId,
      { decision: b.decision, response: b.response },
      { via: 'web', ip: req.clientIp },
    );
    if (!updated) return reply.code(404).send({ error: 'item_not_found' });
    return { item: { ...updated, workspaceName: ws.name, workspaceColor: ws.color } };
  });

  app.post('/api/workspaces/:id/inbox/:itemId/dismiss', async (req, reply) => {
    const { id, itemId } = req.params as { id: string; itemId: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    const updated = inbox.dismiss(ws.id, itemId, { via: 'web', ip: req.clientIp });
    if (!updated) return reply.code(404).send({ error: 'item_not_found' });
    return { item: { ...updated, workspaceName: ws.name, workspaceColor: ws.color } };
  });

  app.get('/api/workspaces/:id/chat', async (req, reply) => {
    const { id } = req.params as { id: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    return {
      workspace: { id: ws.id, name: ws.name, color: ws.color, maestroAgent: ws.maestroAgent },
      messages: chat.list(ws.id),
    };
  });

  app.post('/api/workspaces/:id/chat', async (req, reply) => {
    const { id } = req.params as { id: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    const body = req.body ?? {};
    if (typeof body !== 'object' || body === null || Array.isArray(body)) {
      return reply.code(400).send({ error: 'invalid_body' });
    }
    const text = (body as Record<string, unknown>).body;
    if (typeof text !== 'string') {
      return reply.code(400).send({ error: 'invalid_message_body' });
    }
    const message = chat.send(ws.id, { sender: 'human', author: 'Human', body: text }, { via: 'web', ip: req.clientIp });
    return { message };
  });

  app.get('/api/workspaces/:id/roadmap', async (req, reply) => {
    const { id } = req.params as { id: string };
    const ws = workspaces.get(id);
    if (!ws) return reply.code(404).send({ error: 'workspace_not_found' });
    return {
      workspace: { id: ws.id, name: ws.name, color: ws.color, roadmapPath: ws.roadmapPath },
      ...readWorkspaceRoadmap(ws),
    };
  });

  app.get('/api/usage', async () => {
    const snap = await usage.read();
    const standbyState = await standby.evaluate(snap, workspaces.list());
    return { ...snap, standby: standbyState };
  });

  app.post('/api/usage/refresh', async () => {
    const snap = await usage.read({ forceFallback: true });
    const standbyState = await standby.evaluate(snap, workspaces.list());
    return { ...snap, standby: standbyState };
  });

  app.get('/api/tunnel', async () => tunnel.status());

  app.post('/api/kill-switch', async (req, reply) => {
    const result = await tunnel.killSwitch({ via: 'web', ip: req.clientIp });
    reply.clearCookie(sessionCookie, { path: '/' });
    return { ok: true, ...result };
  });

  app.get('/api/audit', async (req) => {
    const q = (req.query ?? {}) as { limit?: string };
    const n = q.limit ? Number(q.limit) : 100;
    const limit = Number.isInteger(n) && n >= 1 ? Math.min(n, 200) : 100;
    return { entries: audit.recent(limit) };
  });

  app.setNotFoundHandler(async (_req, reply) => deny(reply, 404, 'Not found'));

  app.setErrorHandler(async (err, req, reply) => {
    if (err instanceof ValidationError) {
      return reply.code(400).send({ error: err.message });
    }
    if (err instanceof ConflictError) {
      return reply.code(409).send({ error: err.message });
    }
    if (err instanceof WorkspaceError) {
      req.log.error(err);
      return reply.code(500).send({ error: 'workspaces_config_error' });
    }
    const status = (err as { statusCode?: number }).statusCode;
    if (status && status >= 400 && status < 500) return deny(reply, status, 'Bad request');
    req.log.error(err);
    return deny(reply, 500, 'Internal error');
  });

  // Expired sessions are also rejected on use; this just keeps the table small.
  const purge = setInterval(() => sessions.purgeExpired(), 10 * 60_000);
  purge.unref();

  let usageTimer: NodeJS.Timeout | null = null;
  if (deps.enableUsagePoll) {
    const checkUsage = async () => {
      try {
        const snap = await usage.read();
        await standby.evaluate(snap, workspaces.list());
      } catch {
        /* keep background timer resilient */
      }
    };
    void checkUsage();
    usageTimer = setInterval(() => {
      void checkUsage();
    }, config.usage.pollSeconds * 1000);
    usageTimer.unref();
  }

  app.addHook('onClose', async () => {
    clearInterval(purge);
    if (usageTimer) clearInterval(usageTimer);
  });

  return { app, sessions, audit, limiter, workspaces, events, inbox, chat, usage, standby, tunnel };
}
