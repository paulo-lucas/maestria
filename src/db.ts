import path from 'node:path';
import Database from 'better-sqlite3';
import { ensureConfigDir } from './config.ts';
import { restrictPrivateFileBestEffort } from './permissions.ts';

export type Db = Database.Database;

/** Ordered, append-only migrations. `PRAGMA user_version` tracks how many have been applied. */
const MIGRATIONS: string[] = [
  `
  CREATE TABLE sessions (
    id TEXT PRIMARY KEY,            -- sha256 of the cookie token (the token itself is never stored)
    csrf_token TEXT NOT NULL,
    created_at INTEGER NOT NULL,    -- epoch ms
    last_seen_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,    -- absolute expiry, epoch ms
    ip TEXT NOT NULL,
    user_agent TEXT NOT NULL
  );
  CREATE TABLE kv (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
  );
  CREATE TABLE audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,            -- epoch ms
    event TEXT NOT NULL,
    ip TEXT,
    detail TEXT
  );
  CREATE INDEX audit_ts ON audit (ts);
  `,
  `
  CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('task', 'question')),
    text TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done', 'dismissed')),
    answer TEXT,
    created_by TEXT NOT NULL DEFAULT 'Maestro',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    done_at INTEGER,
    dismissed_at INTEGER
  );
  CREATE INDEX tasks_ws_status ON tasks (workspace_id, status, created_at);

  CREATE TABLE notices (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    body TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'dismissed')),
    created_by TEXT NOT NULL DEFAULT 'Maestro',
    created_at INTEGER NOT NULL,
    dismissed_at INTEGER
  );
  CREATE INDEX notices_ws_status ON notices (workspace_id, status, created_at);
  `,
  `
  CREATE TABLE tasks_v2 (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('approve', 'ack', 'input', 'choice')),
    text TEXT NOT NULL,
    options TEXT,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'responded', 'dismissed')),
    decision TEXT,
    response TEXT,
    created_by TEXT NOT NULL DEFAULT 'Maestro',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    responded_at INTEGER,
    dismissed_at INTEGER
  );
  INSERT INTO tasks_v2 (id, workspace_id, type, text, options, status, decision, response, created_by, created_at, updated_at, responded_at, dismissed_at)
  SELECT
    id,
    workspace_id,
    CASE WHEN kind = 'question' THEN 'input' ELSE 'ack' END,
    text,
    NULL,
    CASE WHEN status = 'done' THEN 'responded' ELSE status END,
    CASE WHEN status = 'done' AND kind = 'task' THEN 'ack' ELSE NULL END,
    answer,
    created_by,
    created_at,
    updated_at,
    done_at,
    dismissed_at
  FROM tasks;
  DROP TABLE tasks;
  ALTER TABLE tasks_v2 RENAME TO tasks;
  CREATE INDEX tasks_ws_status ON tasks (workspace_id, status, created_at);
  `,
  `
  CREATE TABLE inbox (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('notice', 'approve', 'input', 'choice')),
    body TEXT NOT NULL,
    options TEXT,
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'responded', 'dismissed')),
    decision TEXT,
    response TEXT,
    created_by TEXT NOT NULL DEFAULT 'Maestro',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    resolved_at INTEGER
  );
  INSERT INTO inbox (id, workspace_id, kind, body, options, status, decision, response, created_by, created_at, updated_at, resolved_at)
  SELECT
    id,
    workspace_id,
    CASE WHEN type = 'ack' THEN 'notice' ELSE type END,
    text,
    options,
    CASE WHEN type = 'ack' AND status = 'responded' THEN 'dismissed' ELSE status END,
    decision,
    response,
    created_by,
    created_at,
    updated_at,
    COALESCE(responded_at, dismissed_at)
  FROM tasks;
  INSERT OR IGNORE INTO inbox (id, workspace_id, kind, body, options, status, decision, response, created_by, created_at, updated_at, resolved_at)
  SELECT
    id,
    workspace_id,
    'notice',
    body,
    NULL,
    CASE WHEN status = 'active' THEN 'open' ELSE 'dismissed' END,
    CASE WHEN status = 'dismissed' THEN 'ack' ELSE NULL END,
    NULL,
    created_by,
    created_at,
    COALESCE(dismissed_at, created_at),
    dismissed_at
  FROM notices;
  DROP TABLE tasks;
  DROP TABLE notices;
  CREATE INDEX inbox_ws_status ON inbox (workspace_id, status, created_at);
  CREATE INDEX inbox_status_created ON inbox (status, created_at);
  `,
  `
  CREATE TABLE chat_messages (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    sender TEXT NOT NULL CHECK (sender IN ('human', 'maestro')),
    author TEXT NOT NULL,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL
  );
  CREATE INDEX chat_ws_created ON chat_messages (workspace_id, created_at, id);

  CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id TEXT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('chat', 'inbox_response', 'inbox_dismiss', 'standby', 'resume')),
    payload TEXT NOT NULL,
    created_at INTEGER NOT NULL
  );
  CREATE INDEX events_ws_id ON events (workspace_id, id);

  CREATE TABLE wait_cursors (
    workspace_id TEXT NOT NULL,
    consumer TEXT NOT NULL,
    last_event_id INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, consumer)
  );
  `,
];

export function migrate(db: Db): void {
  const current = db.pragma('user_version', { simple: true }) as number;
  for (let v = current; v < MIGRATIONS.length; v++) {
    db.transaction(() => {
      db.exec(MIGRATIONS[v]!);
      db.pragma(`user_version = ${v + 1}`);
    })();
  }
}

export function dbPath(configDir: string): string {
  return path.join(configDir, 'maestria.db');
}

export function openDb(configDir: string): Db {
  ensureConfigDir(configDir);
  const file = dbPath(configDir);
  const db = new Database(file);
  restrictPrivateFileBestEffort(file);
  db.pragma('journal_mode = WAL');
  db.pragma('busy_timeout = 5000');
  db.pragma('foreign_keys = ON');
  migrate(db);
  return db;
}

/** In-memory database for tests. */
export function openMemoryDb(): Db {
  const db = new Database(':memory:');
  db.pragma('foreign_keys = ON');
  migrate(db);
  return db;
}
