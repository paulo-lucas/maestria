import crypto from 'node:crypto';
import type { Audit } from './audit.ts';
import type { Db } from './db.ts';
import { renderInlineMarkdown } from './web/markdown.ts';

export type TaskType = 'approve' | 'ack' | 'input' | 'choice';
export type TaskStatus = 'open' | 'responded' | 'dismissed';

export interface Task {
  id: string;
  workspaceId: string;
  type: TaskType;
  text: string;
  textHtml: string;
  options: string[];
  status: TaskStatus;
  decision: string | null;
  response: string | null;
  createdBy: string;
  createdAt: number;
  updatedAt: number;
  respondedAt: number | null;
  dismissedAt: number | null;
}

export interface ActorContext {
  via: 'web' | 'cli';
  ip?: string | null;
}

export class ValidationError extends Error {}
export class ConflictError extends Error {}

export const TASK_TYPES: readonly TaskType[] = ['approve', 'ack', 'input', 'choice'];
export const MAX_TASK_TEXT = 2000;
export const MAX_TASK_RESPONSE = 8000;
export const MAX_OPTION_TEXT = 80;
export const MAX_AUTHOR_LEN = 64;

interface TaskRow {
  id: string;
  workspace_id: string;
  type: TaskType;
  text: string;
  options: string | null;
  status: TaskStatus;
  decision: string | null;
  response: string | null;
  created_by: string;
  created_at: number;
  updated_at: number;
  responded_at: number | null;
  dismissed_at: number | null;
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

function toTask(row: TaskRow): Task {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    type: row.type,
    text: row.text,
    textHtml: renderInlineMarkdown(row.text),
    options: parseOptions(row.options),
    status: row.status,
    decision: row.decision,
    response: row.response,
    createdBy: row.created_by,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    respondedAt: row.responded_at,
    dismissedAt: row.dismissed_at,
  };
}

export class TaskStore {
  constructor(
    private db: Db,
    private audit?: Audit,
    private now: () => number = Date.now,
  ) {}

  add(
    input: { workspaceId: string; type?: TaskType; text: string; options?: string[]; createdBy?: string },
    actor: ActorContext = { via: 'cli' },
  ): Task {
    const type: TaskType = input.type ?? 'ack';
    if (!TASK_TYPES.includes(type)) {
      throw new ValidationError(`Invalid task type: expected ${TASK_TYPES.join(', ')}`);
    }
    const text = input.text.trim();
    if (!text || text.length > MAX_TASK_TEXT) {
      throw new ValidationError(`Task text must be 1..${MAX_TASK_TEXT} characters`);
    }

    let options: string[] = [];
    if (type === 'choice') {
      const rawOpts = input.options ?? [];
      if (!Array.isArray(rawOpts) || rawOpts.length < 2 || rawOpts.length > 4) {
        throw new ValidationError('Choice tasks require 2 to 4 --option values');
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
      throw new ValidationError(`Options are only supported for --type choice (got --type ${type})`);
    }

    const createdBy = (input.createdBy ?? 'Maestro').trim().slice(0, MAX_AUTHOR_LEN) || 'Maestro';
    const id = crypto.randomBytes(5).toString('hex');
    const t = this.now();

    this.db
      .prepare(
        `INSERT INTO tasks (id, workspace_id, type, text, options, status, decision, response, created_by, created_at, updated_at, responded_at, dismissed_at)
         VALUES (?, ?, ?, ?, ?, 'open', NULL, NULL, ?, ?, ?, NULL, NULL)`,
      )
      .run(id, input.workspaceId, type, text, options.length ? JSON.stringify(options) : null, createdBy, t, t);

    this.audit?.log('task_add', {
      ip: actor.ip ?? null,
      detail: { workspaceId: input.workspaceId, taskId: id, type, via: actor.via },
    });

    return this.get(input.workspaceId, id)!;
  }

  get(workspaceId: string, id: string): Task | null {
    const row = this.db
      .prepare('SELECT * FROM tasks WHERE workspace_id = ? AND id = ?')
      .get(workspaceId, id) as TaskRow | undefined;
    return row ? toTask(row) : null;
  }

  list(workspaceId: string, opts: { status?: TaskStatus | 'history' | 'all' } = {}): Task[] {
    const status = opts.status ?? 'open';
    let rows: TaskRow[];
    if (status === 'all') {
      rows = this.db
        .prepare(
          `SELECT * FROM tasks WHERE workspace_id = ?
           ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'responded' THEN 1 ELSE 2 END,
                    COALESCE(responded_at, dismissed_at, created_at) DESC, created_at ASC`,
        )
        .all(workspaceId) as TaskRow[];
    } else if (status === 'history') {
      rows = this.db
        .prepare(
          `SELECT * FROM tasks WHERE workspace_id = ? AND status IN ('responded', 'dismissed')
           ORDER BY COALESCE(responded_at, dismissed_at, updated_at) DESC`,
        )
        .all(workspaceId) as TaskRow[];
    } else if (status === 'open') {
      rows = this.db
        .prepare(`SELECT * FROM tasks WHERE workspace_id = ? AND status = 'open' ORDER BY created_at ASC`)
        .all(workspaceId) as TaskRow[];
    } else {
      rows = this.db
        .prepare(`SELECT * FROM tasks WHERE workspace_id = ? AND status = ? ORDER BY COALESCE(responded_at, dismissed_at, updated_at) DESC`)
        .all(workspaceId, status) as TaskRow[];
    }
    return rows.map(toTask);
  }

  answers(workspaceId: string, opts: { includeDismissed?: boolean } = {}): Task[] {
    const sql = opts.includeDismissed
      ? `SELECT * FROM tasks WHERE workspace_id = ? AND status IN ('responded', 'dismissed')
         ORDER BY COALESCE(responded_at, dismissed_at, updated_at) DESC`
      : `SELECT * FROM tasks WHERE workspace_id = ? AND status = 'responded'
         ORDER BY COALESCE(responded_at, updated_at) DESC`;
    const rows = this.db.prepare(sql).all(workspaceId) as TaskRow[];
    return rows.map(toTask);
  }

  respond(
    workspaceId: string,
    id: string,
    input: { decision?: string; response?: string },
    actor: ActorContext = { via: 'web' },
  ): Task | null {
    const existing = this.get(workspaceId, id);
    if (!existing) return null;
    if (existing.status !== 'open') {
      throw new ConflictError('Task has already been resolved');
    }

    if (input.response !== undefined && typeof input.response !== 'string') {
      throw new ValidationError('Response must be a string');
    }
    if (input.decision !== undefined && typeof input.decision !== 'string') {
      throw new ValidationError('Decision must be a string');
    }

    let decision: string | null = null;
    let response: string | null = null;

    switch (existing.type) {
      case 'ack': {
        decision = 'ack';
        response = null;
        break;
      }
      case 'approve': {
        if (input.decision !== 'approved' && input.decision !== 'rejected') {
          throw new ValidationError('Approve task requires decision "approved" or "rejected"');
        }
        decision = input.decision;
        if (input.response !== undefined) {
          if (input.response.length > MAX_TASK_RESPONSE) {
            throw new ValidationError(`Comment must be at most ${MAX_TASK_RESPONSE} characters`);
          }
          const trimmed = input.response.trim();
          response = trimmed ? trimmed : null;
        }
        break;
      }
      case 'input': {
        const raw = input.response ?? '';
        if (raw.length > MAX_TASK_RESPONSE) {
          throw new ValidationError(`Response must be at most ${MAX_TASK_RESPONSE} characters`);
        }
        const trimmed = raw.trim();
        if (!trimmed) {
          throw new ValidationError('Input task requires a non-empty response');
        }
        decision = null;
        response = trimmed;
        break;
      }
      case 'choice': {
        if (!input.decision || !existing.options.includes(input.decision)) {
          throw new ValidationError('Choice task requires decision matching one of the task options');
        }
        decision = input.decision;
        response = null;
        break;
      }
    }

    const t = this.now();
    this.db
      .prepare(
        `UPDATE tasks
         SET status = 'responded', decision = ?, response = ?, updated_at = ?, responded_at = ?
         WHERE workspace_id = ? AND id = ? AND status = 'open'`,
      )
      .run(decision, response, t, t, workspaceId, id);

    this.audit?.log('task_respond', {
      ip: actor.ip ?? null,
      detail: { workspaceId, taskId: id, type: existing.type, decision, hasResponse: response !== null, via: actor.via },
    });

    return this.get(workspaceId, id)!;
  }

  dismiss(workspaceId: string, id: string, actor: ActorContext = { via: 'web' }): Task | null {
    const existing = this.get(workspaceId, id);
    if (!existing) return null;
    if (existing.status !== 'open') {
      throw new ConflictError('Task has already been resolved');
    }
    const t = this.now();
    this.db
      .prepare(
        `UPDATE tasks
         SET status = 'dismissed', updated_at = ?, dismissed_at = ?
         WHERE workspace_id = ? AND id = ? AND status = 'open'`,
      )
      .run(t, t, workspaceId, id);

    this.audit?.log('task_dismiss', {
      ip: actor.ip ?? null,
      detail: { workspaceId, taskId: id, type: existing.type, via: actor.via },
    });

    return this.get(workspaceId, id)!;
  }

  countsByWorkspace(): Map<string, { open: number; responded: number; dismissed: number }> {
    const rows = this.db
      .prepare('SELECT workspace_id, status, COUNT(*) AS n FROM tasks GROUP BY workspace_id, status')
      .all() as { workspace_id: string; status: TaskStatus; n: number }[];
    const map = new Map<string, { open: number; responded: number; dismissed: number }>();
    for (const r of rows) {
      let entry = map.get(r.workspace_id);
      if (!entry) {
        entry = { open: 0, responded: 0, dismissed: 0 };
        map.set(r.workspace_id, entry);
      }
      entry[r.status] = r.n;
    }
    return map;
  }
}
