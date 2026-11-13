import fs from 'node:fs';
import path from 'node:path';
import Database from 'better-sqlite3';
import { ensureConfigDir } from './config.ts';

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
  try {
    fs.chmodSync(file, 0o600);
  } catch {
    /* best effort */
  }
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
