import type { Db } from './db.ts';

export type EventType = 'chat' | 'inbox_response' | 'inbox_dismiss' | 'standby' | 'resume';

export interface WorkspaceEvent {
  id: number;
  workspaceId: string;
  type: EventType;
  payload: Record<string, unknown>;
  createdAt: number;
}

interface RawEventRow {
  id: number;
  workspace_id: string;
  type: EventType;
  payload: string;
  created_at: number;
}

function toEvent(row: RawEventRow): WorkspaceEvent {
  let payload: Record<string, unknown> = {};
  try {
    const parsed = JSON.parse(row.payload);
    if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) {
      payload = parsed as Record<string, unknown>;
    }
  } catch {
    /* ignore corrupt payload */
  }
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    type: row.type,
    payload,
    createdAt: row.created_at,
  };
}

export class EventStore {
  constructor(
    private db: Db,
    private now: () => number = Date.now,
  ) {}

  append(workspaceId: string, type: EventType, payload: Record<string, unknown>): WorkspaceEvent {
    const createdAt = this.now();
    const json = JSON.stringify(payload);
    const info = this.db
      .prepare('INSERT INTO events (workspace_id, type, payload, created_at) VALUES (?, ?, ?, ?)')
      .run(workspaceId, type, json, createdAt);
    return {
      id: Number(info.lastInsertRowid),
      workspaceId,
      type,
      payload,
      createdAt,
    };
  }

  since(workspaceId: string, afterId: number, limit = 50): WorkspaceEvent[] {
    const rows = this.db
      .prepare('SELECT id, workspace_id, type, payload, created_at FROM events WHERE workspace_id = ? AND id > ? ORDER BY id ASC LIMIT ?')
      .all(workspaceId, afterId, limit) as RawEventRow[];
    return rows.map(toEvent);
  }

  getCursor(workspaceId: string, consumer = 'default'): number | null {
    const row = this.db
      .prepare('SELECT last_event_id FROM wait_cursors WHERE workspace_id = ? AND consumer = ?')
      .get(workspaceId, consumer) as { last_event_id: number } | undefined;
    return row ? row.last_event_id : null;
  }

  setCursor(workspaceId: string, lastEventId: number, consumer = 'default'): void {
    this.db
      .prepare(
        `INSERT INTO wait_cursors (workspace_id, consumer, last_event_id, updated_at)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(workspace_id, consumer) DO UPDATE SET last_event_id = excluded.last_event_id, updated_at = excluded.updated_at`,
      )
      .run(workspaceId, consumer, lastEventId, this.now());
  }

  latestId(workspaceId: string): number {
    const row = this.db
      .prepare('SELECT COALESCE(MAX(id), 0) AS max_id FROM events WHERE workspace_id = ?')
      .get(workspaceId) as { max_id: number };
    return row.max_id;
  }
}
