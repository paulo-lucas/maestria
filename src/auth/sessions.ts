import crypto from 'node:crypto';
import type { Db } from '../db.ts';

export interface Session {
  id: string;
  csrfToken: string;
  createdAt: number;
  lastSeenAt: number;
  expiresAt: number;
}

interface Row {
  id: string;
  csrf_token: string;
  created_at: number;
  last_seen_at: number;
  expires_at: number;
}

export interface SessionOptions {
  idleMs: number;
  absoluteMs: number;
}

const TOUCH_INTERVAL_MS = 30_000;

export function hashToken(token: string): string {
  return crypto.createHash('sha256').update(token).digest('hex');
}

export function randomToken(): string {
  return crypto.randomBytes(32).toString('base64url');
}

/** Server-side sessions: an opaque random token in the cookie, only its hash stored in SQLite. */
export class SessionStore {
  constructor(
    private db: Db,
    private opts: SessionOptions,
    private now: () => number = Date.now,
  ) {}

  create(ip: string, userAgent: string): { token: string; session: Session } {
    const token = randomToken();
    const t = this.now();
    const session: Session = {
      id: hashToken(token),
      csrfToken: randomToken(),
      createdAt: t,
      lastSeenAt: t,
      expiresAt: t + this.opts.absoluteMs,
    };
    this.db
      .prepare('INSERT INTO sessions (id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)')
      .run(session.id, session.csrfToken, t, t, session.expiresAt, ip.slice(0, 64), userAgent.slice(0, 200));
    return { token, session };
  }

  /** Return the live session for a cookie token, enforcing idle and absolute expiry. Expired sessions are deleted. */
  validate(token: string | undefined): Session | null {
    if (!token || token.length > 128) return null;
    const id = hashToken(token);
    const row = this.db.prepare('SELECT id, csrf_token, created_at, last_seen_at, expires_at FROM sessions WHERE id = ?').get(id) as Row | undefined;
    if (!row) return null;
    const t = this.now();
    if (t >= row.expires_at || t - row.last_seen_at >= this.opts.idleMs) {
      this.db.prepare('DELETE FROM sessions WHERE id = ?').run(id);
      return null;
    }
    if (t - row.last_seen_at >= TOUCH_INTERVAL_MS) {
      this.db.prepare('UPDATE sessions SET last_seen_at = ? WHERE id = ?').run(t, id);
      row.last_seen_at = t;
    }
    return {
      id: row.id,
      csrfToken: row.csrf_token,
      createdAt: row.created_at,
      lastSeenAt: row.last_seen_at,
      expiresAt: row.expires_at,
    };
  }

  destroy(token: string | undefined): void {
    if (!token) return;
    this.db.prepare('DELETE FROM sessions WHERE id = ?').run(hashToken(token));
  }

  /** Invalidate every session ("logout everywhere", kill switch). Returns how many were removed. */
  destroyAll(): number {
    return this.db.prepare('DELETE FROM sessions').run().changes;
  }

  purgeExpired(): number {
    const t = this.now();
    return this.db.prepare('DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?').run(t, t - this.opts.idleMs).changes;
  }

  count(): number {
    return (this.db.prepare('SELECT COUNT(*) AS n FROM sessions').get() as { n: number }).n;
  }
}
