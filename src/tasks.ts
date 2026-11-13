import crypto from 'node:crypto';
import type { Audit } from './audit.ts';
import type { Db } from './db.ts';
import { renderInlineMarkdown } from './web/markdown.ts';

export type TaskKind = 'task' | 'question';
export type TaskStatus = 'open' | 'done' | 'dismissed';

export interface Task {
  id: string;
  workspaceId: string;
  kind: TaskKind;
  text: string;
  textHtml: string;
  status: TaskStatus;
  answer: string | null;
  createdBy: string;
  createdAt: number;
  updatedAt: number;
  doneAt: number | null;
  dismissedAt: number | null;
}

export interface ActorContext {
  via: 'web' | 'cli';
  ip?: string | null;
}

export class ValidationError extends Error {}

export const MAX_TASK_TEXT = 2000;
export const MAX_TASK_ANSWER = 8000;
export const MAX_AUTHOR_LEN = 64;

interface TaskRow {
  id: string;
  workspace_id: string;
  kind: TaskKind;
  text: string;
  status: TaskStatus;
  answer: string | null;
  created_by: string;
  created_at: number;
  updated_at: number;
  done_at: number | null;
  dismissed_at: number | null;
}

function toTask(row: TaskRow): Task {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    kind: row.kind,
    text: row.text,
    textHtml: renderInlineMarkdown(row.text),
    status: row.status,
    answer: row.answer,
    createdBy: row.created_by,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    doneAt: row.done_at,
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
    input: { workspaceId: string; kind?: TaskKind; text: string; createdBy?: string },
    actor: ActorContext = { via: 'cli' },
  ): Task {
    const kind: TaskKind = input.kind ?? 'task';
    if (kind !== 'task' && kind !== 'question') {
      throw new ValidationError('Invalid task kind');
    }
    const text = input.text.trim();
    if (!text || text.length > MAX_TASK_TEXT) {
      throw new ValidationError(`Task text must be 1..${MAX_TASK_TEXT} characters`);
    }
    const createdBy = (input.createdBy ?? 'Maestro').trim().slice(0, MAX_AUTHOR_LEN) || 'Maestro';
    const id = crypto.randomBytes(5).toString('hex');
    const t = this.now();

    this.db
      .prepare(
        `INSERT INTO tasks (id, workspace_id, kind, text, status, answer, created_by, created_at, updated_at, done_at, dismissed_at)
         VALUES (?, ?, ?, ?, 'open', NULL, ?, ?, ?, NULL, NULL)`,
      )
      .run(id, input.workspaceId, kind, text, createdBy, t, t);

    this.audit?.log('task_add', {
      ip: actor.ip ?? null,
      detail: { workspaceId: input.workspaceId, taskId: id, kind, via: actor.via },
    });

    return this.get(input.workspaceId, id)!;
  }

  get(workspaceId: string, id: string): Task | null {
    const row = this.db
      .prepare('SELECT * FROM tasks WHERE workspace_id = ? AND id = ?')
      .get(workspaceId, id) as TaskRow | undefined;
    return row ? toTask(row) : null;
  }

  list(workspaceId: string, opts: { status?: TaskStatus | 'board' | 'all' } = {}): Task[] {
    const status = opts.status ?? 'board';
    let rows: TaskRow[];
    if (status === 'all') {
      rows = this.db
        .prepare(
          `SELECT * FROM tasks WHERE workspace_id = ?
           ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'done' THEN 1 ELSE 2 END,
                    COALESCE(done_at, dismissed_at, created_at) DESC, created_at ASC`,
        )
        .all(workspaceId) as TaskRow[];
    } else if (status === 'board') {
      rows = this.db
        .prepare(
          `SELECT * FROM tasks WHERE workspace_id = ? AND status IN ('open', 'done')
           ORDER BY CASE status WHEN 'open' THEN 0 ELSE 1 END,
                    CASE WHEN status = 'open' THEN created_at ELSE -COALESCE(done_at, created_at) END ASC`,
        )
        .all(workspaceId) as TaskRow[];
    } else {
      rows = this.db
        .prepare('SELECT * FROM tasks WHERE workspace_id = ? AND status = ? ORDER BY created_at ASC')
        .all(workspaceId, status) as TaskRow[];
    }
    return rows.map(toTask);
  }

  answers(workspaceId: string, opts: { includeDismissed?: boolean } = {}): Task[] {
    const sql = opts.includeDismissed
      ? `SELECT * FROM tasks WHERE workspace_id = ? AND (status = 'done' OR (status = 'dismissed' AND answer IS NOT NULL AND answer != ''))
         ORDER BY COALESCE(done_at, dismissed_at, updated_at) DESC`
      : `SELECT * FROM tasks WHERE workspace_id = ? AND status = 'done'
         ORDER BY COALESCE(done_at, updated_at) DESC`;
    const rows = this.db.prepare(sql).all(workspaceId) as TaskRow[];
    return rows.map(toTask);
  }

  updateState(
    workspaceId: string,
    id: string,
    update: { status: TaskStatus; answer?: string },
    actor: ActorContext = { via: 'web' },
  ): Task | null {
    if (update.status !== 'open' && update.status !== 'done' && update.status !== 'dismissed') {
      throw new ValidationError('Invalid task status');
    }
    if (update.answer !== undefined) {
      if (typeof update.answer !== 'string' || update.answer.length > MAX_TASK_ANSWER) {
        throw new ValidationError(`Answer must be at most ${MAX_TASK_ANSWER} characters`);
      }
    }

    const existing = this.get(workspaceId, id);
    if (!existing) return null;

    const t = this.now();
    const nextAnswer = update.answer !== undefined ? update.answer : existing.answer;
    const doneAt = update.status === 'done' ? (existing.doneAt ?? t) : update.status === 'open' ? null : existing.doneAt;
    const dismissedAt = update.status === 'dismissed' ? (existing.dismissedAt ?? t) : null;

    this.db
      .prepare(
        `UPDATE tasks
         SET status = ?, answer = ?, updated_at = ?, done_at = ?, dismissed_at = ?
         WHERE workspace_id = ? AND id = ?`,
      )
      .run(update.status, nextAnswer, t, doneAt, dismissedAt, workspaceId, id);

    this.audit?.log(update.status === 'dismissed' ? 'task_dismiss' : 'task_update', {
      ip: actor.ip ?? null,
      detail: { workspaceId, taskId: id, status: update.status, hasAnswer: Boolean(nextAnswer), via: actor.via },
    });

    return this.get(workspaceId, id)!;
  }

  countsByWorkspace(): Map<string, { open: number; done: number; dismissed: number }> {
    const rows = this.db
      .prepare('SELECT workspace_id, status, COUNT(*) AS n FROM tasks GROUP BY workspace_id, status')
      .all() as { workspace_id: string; status: TaskStatus; n: number }[];
    const map = new Map<string, { open: number; done: number; dismissed: number }>();
    for (const r of rows) {
      let entry = map.get(r.workspace_id);
      if (!entry) {
        entry = { open: 0, done: 0, dismissed: 0 };
        map.set(r.workspace_id, entry);
      }
      entry[r.status] = r.n;
    }
    return map;
  }
}
