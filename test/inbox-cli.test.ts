import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import Database from 'better-sqlite3';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { ArgError } from '../src/commands/args.ts';
import {
  runAnswersCommand,
  runInboxCommand,
  runPostCommand,
  runStatusCommand,
  runWorkspaceCommand,
} from '../src/commands/inbox.ts';
import { migrate, openDb, openMemoryDb } from '../src/db.ts';
import { ConflictError, InboxStore, ValidationError } from '../src/inbox.ts';

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

describe('InboxStore', () => {
  it('supports notice, approve, input, and choice lifecycles and moves resolved items to history', () => {
    const db = openMemoryDb();
    const audit = new Audit(db);
    const c = clock();
    const inbox = new InboxStore(db, audit, c.now);

    const iNotice = inbox.post({ workspaceId: 'app', kind: 'notice', body: '**Build green**' });
    c.advance(10);
    const iApprove = inbox.post({ workspaceId: 'app', kind: 'approve', body: 'Deploy v2?' });
    c.advance(10);
    const iInput = inbox.post({ workspaceId: 'app', kind: 'input', body: 'Which region?' });
    c.advance(10);
    const iChoice = inbox.post({ workspaceId: 'app', kind: 'choice', body: 'Pick target', options: ['Staging', 'Prod'] });

    // Newest first ordering
    expect(inbox.list({ workspaceId: 'app', status: 'open' }).map((i) => i.id)).toEqual([
      iChoice.id,
      iInput.id,
      iApprove.id,
      iNotice.id,
    ]);
    expect(iNotice.bodyHtml).toContain('<strong>Build green</strong>');

    c.advance(1000);
    const rNotice = inbox.respond('app', iNotice.id, {}, { via: 'web' });
    expect(rNotice).toMatchObject({ status: 'dismissed', decision: 'ack', response: null });

    c.advance(1000);
    const rApprove = inbox.respond('app', iApprove.id, { decision: 'approved', response: 'Ship it!' }, { via: 'web', ip: '127.0.0.1' });
    expect(rApprove).toMatchObject({ status: 'responded', decision: 'approved', response: 'Ship it!' });

    c.advance(1000);
    const rInput = inbox.respond('app', iInput.id, { response: 'us-east-1' }, { via: 'web' });
    expect(rInput).toMatchObject({ status: 'responded', decision: null, response: 'us-east-1' });

    c.advance(1000);
    const rChoice = inbox.respond('app', iChoice.id, { decision: 'Staging' }, { via: 'web' });
    expect(rChoice).toMatchObject({ status: 'responded', decision: 'Staging', response: null });

    expect(inbox.list({ workspaceId: 'app', status: 'open' })).toHaveLength(0);
    expect(inbox.list({ workspaceId: 'app', status: 'history' })).toHaveLength(4);
    expect(inbox.answers('app').map((i) => i.id)).toEqual([iChoice.id, iInput.id, iApprove.id]);
    expect(inbox.answers('app', { includeDismissed: true })).toHaveLength(4);

    // Double-responding is rejected with ConflictError.
    expect(() => inbox.respond('app', iNotice.id, {})).toThrow(ConflictError);

    const events = audit.recent().map((e) => e.event);
    expect(events).toContain('inbox_post');
    expect(events).toContain('inbox_respond');
    expect(events).toContain('inbox_dismiss');
  });

  it('archives dismissed open prompt items without deleting them', () => {
    const db = openMemoryDb();
    const audit = new Audit(db);
    const inbox = new InboxStore(db, audit);
    const item = inbox.post({ workspaceId: 'app', kind: 'approve', body: 'Dismiss me' });

    const dismissed = inbox.dismiss('app', item.id, { via: 'web' });
    expect(dismissed?.status).toBe('dismissed');
    expect(dismissed?.decision).toBeNull();
    expect(inbox.list({ workspaceId: 'app', status: 'open' })).toHaveLength(0);
    expect(inbox.list({ workspaceId: 'app', status: 'history' })).toHaveLength(1);
    expect(inbox.answers('app')).toHaveLength(0);
  });

  it('enforces validation rules on inbox kinds, options, and responses', () => {
    const db = openMemoryDb();
    const inbox = new InboxStore(db);

    expect(() => inbox.post({ workspaceId: 'app', kind: 'notice', body: '   ' })).toThrow(ValidationError);
    expect(() => inbox.post({ workspaceId: 'app', kind: 'notice', body: 'x'.repeat(4001) })).toThrow(ValidationError);
    expect(() => inbox.post({ workspaceId: 'app', kind: 'choice', body: 'Pick', options: ['One'] })).toThrow(ValidationError);
    expect(() => inbox.post({ workspaceId: 'app', kind: 'choice', body: 'Pick', options: ['A', 'B', 'C', 'D', 'E'] })).toThrow(ValidationError);
    expect(() => inbox.post({ workspaceId: 'app', kind: 'choice', body: 'Pick', options: ['A', 'A'] })).toThrow(ValidationError);
    expect(() => inbox.post({ workspaceId: 'app', kind: 'notice', body: 'Notice', options: ['A', 'B'] })).toThrow(ValidationError);

    const pApprove = inbox.post({ workspaceId: 'app', kind: 'approve', body: 'Ok?' });
    expect(() => inbox.respond('app', pApprove.id, { decision: 'maybe' })).toThrow(ValidationError);
    expect(() => inbox.respond('app', pApprove.id, { decision: 'approved', response: 'a'.repeat(8001) })).toThrow(ValidationError);

    const pInput = inbox.post({ workspaceId: 'app', kind: 'input', body: 'Say something' });
    expect(() => inbox.respond('app', pInput.id, { response: '   ' })).toThrow(ValidationError);

    const pChoice = inbox.post({ workspaceId: 'app', kind: 'choice', body: 'Pick', options: ['A', 'B'] });
    expect(() => inbox.respond('app', pChoice.id, { decision: 'C' })).toThrow(ValidationError);
  });

  it('migrates v3 tasks and notices tables cleanly into unified inbox table', () => {
    const db = new Database(':memory:');
    db.exec(`
      CREATE TABLE tasks (
        id TEXT PRIMARY KEY,
        workspace_id TEXT NOT NULL,
        type TEXT NOT NULL,
        text TEXT NOT NULL,
        options TEXT,
        status TEXT NOT NULL,
        decision TEXT,
        response TEXT,
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        responded_at INTEGER,
        dismissed_at INTEGER
      );
      CREATE TABLE notices (
        id TEXT PRIMARY KEY,
        workspace_id TEXT NOT NULL,
        body TEXT NOT NULL,
        status TEXT NOT NULL,
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        dismissed_at INTEGER
      );
      INSERT INTO tasks VALUES ('t1', 'app', 'approve', 'Ship?', NULL, 'responded', 'approved', 'Yes', 'Maestro', 100, 200, 200, NULL);
      INSERT INTO tasks VALUES ('t2', 'app', 'ack', 'Read me', NULL, 'open', NULL, NULL, 'Maestro', 150, 150, NULL, NULL);
      INSERT INTO notices VALUES ('n1', 'app', 'Build green', 'active', 'Maestro', 300, NULL);
    `);
    db.pragma('user_version = 3');
    migrate(db);

    const store = new InboxStore(db);
    expect(store.get('app', 't1')).toMatchObject({ kind: 'approve', status: 'responded', decision: 'approved', response: 'Yes' });
    expect(store.get('app', 't2')).toMatchObject({ kind: 'notice', status: 'open' });
    expect(store.get('app', 'n1')).toMatchObject({ kind: 'notice', status: 'open', body: 'Build green' });
  });
});

describe('CLI inbox commands', () => {
  function makeEnv() {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-cli-'));
    const configDir = path.join(root, 'config');
    const wsDir = path.join(root, 'my-project');
    fs.mkdirSync(path.join(wsDir, 'sub'), { recursive: true });
    let out = '';
    const capture = (s: string) => {
      out += s;
    };
    const takeOut = () => {
      const v = out;
      out = '';
      return v;
    };
    return { configDir, wsDir, capture, takeOut };
  }

  it('infers workspace from cwd and supports post, inbox, answers, and status', () => {
    const { configDir, wsDir, capture, takeOut } = makeEnv();

    runWorkspaceCommand(['add', 'My Project', wsDir, '--maestro', 'Bulbasaur', '--color', '#0e7490'], {
      configDir,
      out: capture,
    });
    expect(takeOut()).toContain('Added workspace my-project');

    const subCwd = path.join(wsDir, 'sub');
    runPostCommand(['--kind', 'approve', '--json', 'Deploy to prod?'], { configDir, cwd: subCwd, out: capture });
    const approveItem = JSON.parse(takeOut()) as { item: { id: string; kind: string; createdBy: string } };
    expect(approveItem.item.kind).toBe('approve');
    expect(approveItem.item.createdBy).toBe('Bulbasaur');

    runPostCommand(
      ['--kind', 'choice', '--option', 'Fast', '--option', 'Safe', '--json', 'Mode?'],
      { configDir, cwd: subCwd, out: capture },
    );
    const choiceItem = JSON.parse(takeOut()) as { item: { id: string; kind: string; options: string[] } };
    expect(choiceItem.item.options).toEqual(['Fast', 'Safe']);

    runPostCommand(['**Build green** on `dev`.'], { configDir, cwd: subCwd, out: capture });
    expect(takeOut()).toContain('notice\t**Build green**');

    runInboxCommand(['--json'], { configDir, cwd: subCwd, out: capture });
    expect((JSON.parse(takeOut()) as { items: unknown[] }).items).toHaveLength(3);

    const db = openDb(configDir);
    try {
      new InboxStore(db).respond('my-project', approveItem.item.id, { decision: 'approved', response: 'Go ahead' });
    } finally {
      db.close();
    }

    runAnswersCommand(['--json'], { configDir, cwd: subCwd, out: capture });
    const answers = JSON.parse(takeOut()) as { answers: { id: string; decision: string; response: string }[] };
    expect(answers.answers).toHaveLength(1);
    expect(answers.answers[0]).toMatchObject({ id: approveItem.item.id, decision: 'approved', response: 'Go ahead' });

    runStatusCommand(['--json'], { configDir, cwd: subCwd, out: capture });
    const st = JSON.parse(takeOut()) as { id: string; needsYou: number; updates: number; history: number };
    expect(st).toMatchObject({ id: 'my-project', needsYou: 1, updates: 1, history: 1 });
  });

  it('rejects unknown flags and invalid kinds with ArgError', () => {
    const { configDir, capture } = makeEnv();
    expect(() => runPostCommand(['--bogus', 'hi'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runPostCommand(['--kind', 'checkbox', 'hi'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runPostCommand([], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runInboxCommand(['--status', 'bogus'], { configDir, out: capture })).toThrow(ArgError);
  });
});
