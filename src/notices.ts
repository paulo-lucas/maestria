import crypto from 'node:crypto';
import type { Audit } from './audit.ts';
import type { Db } from './db.ts';
import { MAX_AUTHOR_LEN, ValidationError, type ActorContext } from './tasks.ts';
import { renderMarkdown } from './web/markdown.ts';

export type NoticeStatus = 'active' | 'dismissed';

export interface Notice {
  id: string;
  workspaceId: string;
  body: string;
  bodyHtml: string;
  status: NoticeStatus;
  createdBy: string;
  createdAt: number;
  dismissedAt: number | null;
}

export const MAX_NOTICE_BODY = 4000;

interface NoticeRow {
  id: string;
  workspace_id: string;
  body: string;
  status: NoticeStatus;
  created_by: string;
  created_at: number;
  dismissed_at: number | null;
}

function toNotice(row: NoticeRow): Notice {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    body: row.body,
    bodyHtml: renderMarkdown(row.body),
    status: row.status,
    createdBy: row.created_by,
    createdAt: row.created_at,
    dismissedAt: row.dismissed_at,
  };
}

export class NoticeStore {
  constructor(
    private db: Db,
    private audit?: Audit,
    private now: () => number = Date.now,
  ) {}

  add(
    input: { workspaceId: string; body: string; createdBy?: string },
    actor: ActorContext = { via: 'cli' },
  ): Notice {
    const body = input.body.trim();
    if (!body || body.length > MAX_NOTICE_BODY) {
      throw new ValidationError(`Notice body must be 1..${MAX_NOTICE_BODY} characters`);
    }
    const createdBy = (input.createdBy ?? 'Maestro').trim().slice(0, MAX_AUTHOR_LEN) || 'Maestro';
    const id = crypto.randomBytes(5).toString('hex');
    const t = this.now();

    this.db
      .prepare(
        `INSERT INTO notices (id, workspace_id, body, status, created_by, created_at, dismissed_at)
         VALUES (?, ?, ?, 'active', ?, ?, NULL)`,
      )
      .run(id, input.workspaceId, body, createdBy, t);

    this.audit?.log('notice_add', {
      ip: actor.ip ?? null,
      detail: { workspaceId: input.workspaceId, noticeId: id, via: actor.via },
    });

    return this.get(input.workspaceId, id)!;
  }

  get(workspaceId: string, id: string): Notice | null {
    const row = this.db
      .prepare('SELECT * FROM notices WHERE workspace_id = ? AND id = ?')
      .get(workspaceId, id) as NoticeRow | undefined;
    return row ? toNotice(row) : null;
  }

  list(workspaceId: string, opts: { status?: NoticeStatus | 'all' } = {}): Notice[] {
    const status = opts.status ?? 'active';
    const rows =
      status === 'all'
        ? (this.db.prepare('SELECT * FROM notices WHERE workspace_id = ? ORDER BY created_at DESC').all(workspaceId) as NoticeRow[])
        : (this.db
            .prepare('SELECT * FROM notices WHERE workspace_id = ? AND status = ? ORDER BY created_at DESC')
            .all(workspaceId, status) as NoticeRow[]);
    return rows.map(toNotice);
  }

  dismiss(workspaceId: string, id: string, actor: ActorContext = { via: 'web' }): Notice | null {
    const existing = this.get(workspaceId, id);
    if (!existing) return null;
    const t = existing.dismissedAt ?? this.now();
    this.db
      .prepare(`UPDATE notices SET status = 'dismissed', dismissed_at = ? WHERE workspace_id = ? AND id = ?`)
      .run(t, workspaceId, id);

    this.audit?.log('notice_dismiss', {
      ip: actor.ip ?? null,
      detail: { workspaceId, noticeId: id, via: actor.via },
    });

    return this.get(workspaceId, id)!;
  }

  countsByWorkspace(): Map<string, { active: number; dismissed: number }> {
    const rows = this.db
      .prepare('SELECT workspace_id, status, COUNT(*) AS n FROM notices GROUP BY workspace_id, status')
      .all() as { workspace_id: string; status: NoticeStatus; n: number }[];
    const map = new Map<string, { active: number; dismissed: number }>();
    for (const r of rows) {
      let entry = map.get(r.workspace_id);
      if (!entry) {
        entry = { active: 0, dismissed: 0 };
        map.set(r.workspace_id, entry);
      }
      entry[r.status] = r.n;
    }
    return map;
  }
}
