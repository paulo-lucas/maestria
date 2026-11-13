import type { Db } from './db.ts';

const MAX_DETAIL = 1000;

export interface AuditEntry {
  id: number;
  ts: number;
  event: string;
  ip: string | null;
  detail: string | null;
}

/**
 * Append-only audit trail. Never pass secrets (passwords, codes, tokens) as detail.
 * Rows are never updated or deleted by the application.
 */
export class Audit {
  private insert;
  constructor(
    private db: Db,
    private now: () => number = Date.now,
  ) {
    this.insert = db.prepare('INSERT INTO audit (ts, event, ip, detail) VALUES (?, ?, ?, ?)');
  }

  log(event: string, opts: { ip?: string | null; detail?: unknown } = {}): void {
    let detail: string | null = null;
    if (opts.detail !== undefined) {
      detail = typeof opts.detail === 'string' ? opts.detail : JSON.stringify(opts.detail);
      if (detail.length > MAX_DETAIL) detail = detail.slice(0, MAX_DETAIL) + '…';
    }
    this.insert.run(this.now(), event, opts.ip ?? null, detail);
  }

  private throttle = new Map<string, number>();

  /** Like log(), but at most once per `intervalMs` per key; keeps floods of rejected requests from filling the table. */
  logThrottled(key: string, event: string, opts: { ip?: string | null; detail?: unknown } = {}, intervalMs = 60_000): void {
    const t = this.now();
    const last = this.throttle.get(key);
    if (last !== undefined && t - last < intervalMs) return;
    if (this.throttle.size > 1000) this.throttle.clear();
    this.throttle.set(key, t);
    this.log(event, opts);
  }

  recent(limit = 100): AuditEntry[] {
    return this.db.prepare('SELECT id, ts, event, ip, detail FROM audit ORDER BY id DESC LIMIT ?').all(limit) as AuditEntry[];
  }
}
