import crypto from 'node:crypto';
import type { Audit } from './audit.ts';
import type { Db } from './db.ts';
import type { EventStore } from './events.ts';
import { renderMarkdown } from './web/markdown.ts';

export type InboxKind = 'notice' | 'approve' | 'input' | 'choice';
export type InboxStatus = 'open' | 'responded' | 'dismissed';

export interface InboxItem {
  id: string;
  workspaceId: string;
  kind: InboxKind;
  body: string;
  bodyHtml: string;
  options: string[];
  status: InboxStatus;
  decision: string | null;
  response: string | null;
  createdBy: string;
  createdAt: number;
  updatedAt: number;
  resolvedAt: number | null;
}

export interface ActorContext {
  via: 'web' | 'cli';
  ip?: string | null;
}

export class ValidationError extends Error {}
export class ConflictError extends Error {}

export const INBOX_KINDS: readonly InboxKind[] = ['notice', 'approve', 'input', 'choice'];
export const MAX_INBOX_BODY = 4000;
export const MAX_INBOX_RESPONSE = 8000;
export const MAX_OPTION_TEXT = 80;
export const MAX_AUTHOR_LEN = 64;

interface InboxRow {
  id: string;
  workspace_id: string;
  kind: InboxKind;
  body: string;
  options: string | null;
  status: InboxStatus;
  decision: string | null;
  response: string | null;
  created_by: string;
  created_at: number;
  updated_at: number;
  resolved_at: number | null;
}

function parseOptions(raw: string | null): string[] {
  if (!raw) return [];
  try {
    const v: unknown = JSON.parse(raw);
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

function toItem(row: InboxRow): InboxItem {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    kind: row.kind,
    body: row.body,
    bodyHtml: renderMarkdown(row.body),
    options: parseOptions(row.options),
    status: row.status,
    decision: row.decision,
    response: row.response,
    createdBy: row.created_by,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    resolvedAt: row.resolved_at,
  };
}

export class InboxStore {
  constructor(
    private db: Db,
    private audit?: Audit,
    private now: () => number = Date.now,
    private events?: EventStore,
  ) {}

  post(
    input: { workspaceId: string; kind?: InboxKind; body: string; options?: string[]; createdBy?: string },
    actor: ActorContext = { via: 'cli' },
  ): InboxItem {
    const kind: InboxKind = input.kind ?? 'notice';
    if (!INBOX_KINDS.includes(kind)) {
      throw new ValidationError(`Invalid kind: expected ${INBOX_KINDS.join(', ')}`);
    }
    const body = input.body.trim();
    if (!body || body.length > MAX_INBOX_BODY) {
      throw new ValidationError(`Body must be 1..${MAX_INBOX_BODY} characters`);
    }

    const options: string[] = [];
    if (kind === 'choice') {
      const rawOpts = input.options ?? [];
      if (!Array.isArray(rawOpts) || rawOpts.length < 2 || rawOpts.length > 4) {
        throw new ValidationError('Choice items require 2 to 4 --option values');
      }
      const seen = new Set<string>();
      for (const o of rawOpts) {
        if (typeof o !== 'string') throw new ValidationError('Option must be a string');
        const trimmed = o.trim();
        if (!trimmed || trimmed.length > MAX_OPTION_TEXT) {
          throw new ValidationError(`Each option must be 1..${MAX_OPTION_TEXT} characters`);
        }
        if (seen.has(trimmed)) throw new ValidationError(`Duplicate option "${trimmed}"`);
        seen.add(trimmed);
        options.push(trimmed);
      }
    } else if (input.options && input.options.length > 0) {
      throw new ValidationError(`Options are only supported for --kind choice (got --kind ${kind})`);
    }

    const createdBy = (input.createdBy ?? 'Maestro').trim().slice(0, MAX_AUTHOR_LEN) || 'Maestro';
    const id = crypto.randomBytes(5).toString('hex');
    const t = this.now();

    this.db
      .prepare(
        `INSERT INTO inbox (id, workspace_id, kind, body, options, status, decision, response, created_by, created_at, updated_at, resolved_at)
         VALUES (?, ?, ?, ?, ?, 'open', NULL, NULL, ?, ?, ?, NULL)`,
      )
      .run(id, input.workspaceId, kind, body, options.length ? JSON.stringify(options) : null, createdBy, t, t);

    this.audit?.log('inbox_post', {
      ip: actor.ip ?? null,
      detail: { workspaceId: input.workspaceId, itemId: id, kind, via: actor.via },
    });

    return this.get(input.workspaceId, id)!;
  }

  get(workspaceId: string, id: string): InboxItem | null {
    const row = this.db
      .prepare('SELECT * FROM inbox WHERE workspace_id = ? AND id = ?')
      .get(workspaceId, id) as InboxRow | undefined;
    return row ? toItem(row) : null;
  }

  list(opts: { workspaceId?: string; status?: InboxStatus | 'history' | 'all'; kind?: InboxKind } = {}): InboxItem[] {
    const status = opts.status ?? 'open';
    const where: string[] = [];
    const params: unknown[] = [];

    if (opts.workspaceId) {
      where.push('workspace_id = ?');
      params.push(opts.workspaceId);
    }
    if (opts.kind) {
      where.push('kind = ?');
      params.push(opts.kind);
    }

    if (status === 'history') {
      where.push("status IN ('responded', 'dismissed')");
    } else if (status !== 'all') {
      where.push('status = ?');
      params.push(status);
    }

    const clause = where.length ? `WHERE ${where.join(' AND ')}` : '';
    const order =
      status === 'history' || status === 'responded' || status === 'dismissed'
        ? 'ORDER BY COALESCE(resolved_at, updated_at) DESC, created_at DESC'
        : status === 'all'
          ? "ORDER BY CASE status WHEN 'open' THEN 0 ELSE 1 END, created_at DESC"
          : 'ORDER BY created_at DESC';

    const rows = this.db.prepare(`SELECT * FROM inbox ${clause} ${order}`).all(...params) as InboxRow[];
    return rows.map(toItem);
  }

  answers(workspaceId: string, opts: { includeDismissed?: boolean } = {}): InboxItem[] {
    const sql = opts.includeDismissed
      ? `SELECT * FROM inbox WHERE workspace_id = ? AND status IN ('responded', 'dismissed')
         ORDER BY COALESCE(resolved_at, updated_at) DESC`
      : `SELECT * FROM inbox WHERE workspace_id = ? AND status = 'responded'
         ORDER BY COALESCE(resolved_at, updated_at) DESC`;
    const rows = this.db.prepare(sql).all(workspaceId) as InboxRow[];
    return rows.map(toItem);
  }

  respond(
    workspaceId: string,
    id: string,
    input: { decision?: string; response?: string },
    actor: ActorContext = { via: 'web' },
  ): InboxItem | null {
    const existing = this.get(workspaceId, id);
    if (!existing) return null;
    if (existing.status !== 'open') {
      throw new ConflictError('Item has already been resolved');
    }

    if (input.response !== undefined && typeof input.response !== 'string') {
      throw new ValidationError('Response must be a string');
    }
    if (input.decision !== undefined && typeof input.decision !== 'string') {
      throw new ValidationError('Decision must be a string');
    }

    let nextStatus: InboxStatus = 'responded';
    let decision: string | null = null;
    let response: string | null = null;

    switch (existing.kind) {
      case 'notice': {
        nextStatus = 'dismissed';
        decision = 'ack';
        response = null;
        break;
      }
      case 'approve': {
        if (input.decision !== 'approved' && input.decision !== 'rejected') {
          throw new ValidationError('Approve item requires decision "approved" or "rejected"');
        }
        decision = input.decision;
        if (input.response !== undefined) {
          if (input.response.length > MAX_INBOX_RESPONSE) {
            throw new ValidationError(`Comment must be at most ${MAX_INBOX_RESPONSE} characters`);
          }
          const trimmed = input.response.trim();
          response = trimmed ? trimmed : null;
        }
        break;
      }
      case 'input': {
        const raw = input.response ?? '';
        if (raw.length > MAX_INBOX_RESPONSE) {
          throw new ValidationError(`Response must be at most ${MAX_INBOX_RESPONSE} characters`);
        }
        const trimmed = raw.trim();
        if (!trimmed) {
          throw new ValidationError('Input item requires a non-empty response');
        }
        decision = null;
        response = trimmed;
        break;
      }
      case 'choice': {
        if (!input.decision || !existing.options.includes(input.decision)) {
          throw new ValidationError('Choice item requires decision matching one of the options');
        }
        decision = input.decision;
        response = null;
        break;
      }
    }

    const t = this.now();
    this.db
      .prepare(
        `UPDATE inbox
         SET status = ?, decision = ?, response = ?, updated_at = ?, resolved_at = ?
         WHERE workspace_id = ? AND id = ? AND status = 'open'`,
      )
      .run(nextStatus, decision, response, t, t, workspaceId, id);

    this.audit?.log(nextStatus === 'dismissed' ? 'inbox_dismiss' : 'inbox_respond', {
      ip: actor.ip ?? null,
      detail: { workspaceId, itemId: id, kind: existing.kind, decision, hasResponse: response !== null, via: actor.via },
    });

    const updated = this.get(workspaceId, id)!;
    this.events?.append(workspaceId, nextStatus === 'dismissed' ? 'inbox_dismiss' : 'inbox_response', {
      itemId: id,
      kind: existing.kind,
      decision,
      response,
    });
    return updated;
  }

  dismiss(workspaceId: string, id: string, actor: ActorContext = { via: 'web' }): InboxItem | null {
    const existing = this.get(workspaceId, id);
    if (!existing) return null;
    if (existing.status !== 'open') {
      throw new ConflictError('Item has already been resolved');
    }
    const t = this.now();
    const decision = existing.kind === 'notice' ? 'ack' : null;
    this.db
      .prepare(
        `UPDATE inbox
         SET status = 'dismissed', decision = ?, updated_at = ?, resolved_at = ?
         WHERE workspace_id = ? AND id = ? AND status = 'open'`,
      )
      .run(decision, t, t, workspaceId, id);

    this.audit?.log('inbox_dismiss', {
      ip: actor.ip ?? null,
      detail: { workspaceId, itemId: id, kind: existing.kind, via: actor.via },
    });

    const updated = this.get(workspaceId, id)!;
    this.events?.append(workspaceId, 'inbox_dismiss', {
      itemId: id,
      kind: existing.kind,
      decision,
    });
    return updated;
  }

  countsByWorkspace(): Map<string, { needsYou: number; updates: number; history: number }> {
    const rows = this.db
      .prepare(
        `SELECT workspace_id,
                SUM(CASE WHEN status = 'open' AND kind != 'notice' THEN 1 ELSE 0 END) AS needs_you,
                SUM(CASE WHEN status = 'open' AND kind = 'notice' THEN 1 ELSE 0 END) AS updates,
                SUM(CASE WHEN status IN ('responded', 'dismissed') THEN 1 ELSE 0 END) AS history
         FROM inbox
         GROUP BY workspace_id`,
      )
      .all() as { workspace_id: string; needs_you: number; updates: number; history: number }[];
    const map = new Map<string, { needsYou: number; updates: number; history: number }>();
    for (const r of rows) {
      map.set(r.workspace_id, {
        needsYou: r.needs_you || 0,
        updates: r.updates || 0,
        history: r.history || 0,
      });
    }
    return map;
  }
}
