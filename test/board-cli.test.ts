import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import Database from 'better-sqlite3';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { ArgError } from '../src/commands/args.ts';
import { runNoticeCommand, runStatusCommand, runTaskCommand, runWorkspaceCommand } from '../src/commands/board.ts';
import { migrate, openDb, openMemoryDb } from '../src/db.ts';
import { NoticeStore } from '../src/notices.ts';
import { ConflictError, TaskStore, ValidationError } from '../src/tasks.ts';

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

describe('TaskStore and NoticeStore', () => {
  it('supports approve, ack, input, and choice task lifecycles and moves responses to history', () => {
    const db = openMemoryDb();
    const audit = new Audit(db);
    const c = clock();
    const tasks = new TaskStore(db, audit, c.now);

    const tApprove = tasks.add({ workspaceId: 'app', type: 'approve', text: 'Deploy v2?' });
    const tAck = tasks.add({ workspaceId: 'app', type: 'ack', text: 'Read QA report' });
    const tInput = tasks.add({ workspaceId: 'app', type: 'input', text: 'Which region?' });
    const tChoice = tasks.add({ workspaceId: 'app', type: 'choice', text: 'Pick target', options: ['Staging', 'Prod'] });

    expect(tasks.list('app', { status: 'open' })).toHaveLength(4);

    c.advance(1000);
    const rApprove = tasks.respond('app', tApprove.id, { decision: 'approved', response: 'Ship it!' }, { via: 'web', ip: '127.0.0.1' });
    expect(rApprove).toMatchObject({ status: 'responded', decision: 'approved', response: 'Ship it!' });

    c.advance(1000);
    const rAck = tasks.respond('app', tAck.id, {}, { via: 'web' });
    expect(rAck).toMatchObject({ status: 'responded', decision: 'ack', response: null });

    c.advance(1000);
    const rInput = tasks.respond('app', tInput.id, { response: 'us-east-1' }, { via: 'web' });
    expect(rInput).toMatchObject({ status: 'responded', decision: null, response: 'us-east-1' });

    c.advance(1000);
    const rChoice = tasks.respond('app', tChoice.id, { decision: 'Staging' }, { via: 'web' });
    expect(rChoice).toMatchObject({ status: 'responded', decision: 'Staging', response: null });

    // Once responded, tasks leave the open board and appear in history & answers.
    expect(tasks.list('app', { status: 'open' })).toHaveLength(0);
    expect(tasks.list('app', { status: 'history' })).toHaveLength(4);
    expect(tasks.answers('app').map((t) => t.id)).toEqual([tChoice.id, tInput.id, tAck.id, tApprove.id]);

    // Double-responding is rejected with ConflictError.
    expect(() => tasks.respond('app', tAck.id, { decision: 'ack' })).toThrow(ConflictError);

    const events = audit.recent().map((e) => e.event);
    expect(events).toContain('task_add');
    expect(events).toContain('task_respond');
  });

  it('archives dismissed open tasks without deleting them', () => {
    const db = openMemoryDb();
    const audit = new Audit(db);
    const tasks = new TaskStore(db, audit);
    const t = tasks.add({ workspaceId: 'app', type: 'ack', text: 'Dismiss me' });

    const dismissed = tasks.dismiss('app', t.id, { via: 'web' });
    expect(dismissed?.status).toBe('dismissed');
    expect(tasks.list('app', { status: 'open' })).toHaveLength(0);
    expect(tasks.list('app', { status: 'history' })).toHaveLength(1);
    expect(tasks.answers('app')).toHaveLength(0);
    expect(tasks.answers('app', { includeDismissed: true })).toHaveLength(1);
    expect(audit.recent().map((e) => e.event)).toContain('task_dismiss');
  });

  it('enforces validation rules on task types, options, responses, and notices', () => {
    const db = openMemoryDb();
    const tasks = new TaskStore(db);
    const notices = new NoticeStore(db);

    expect(() => tasks.add({ workspaceId: 'app', type: 'ack', text: '   ' })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', type: 'ack', text: 'x'.repeat(2001) })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', type: 'choice', text: 'Pick', options: ['Only one'] })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', type: 'choice', text: 'Pick', options: ['A', 'B', 'C', 'D', 'E'] })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', type: 'choice', text: 'Pick', options: ['A', 'A'] })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', type: 'ack', text: 'Ack', options: ['A', 'B'] })).toThrow(ValidationError);

    const tApprove = tasks.add({ workspaceId: 'app', type: 'approve', text: 'Ok?' });
    expect(() => tasks.respond('app', tApprove.id, { decision: 'maybe' })).toThrow(ValidationError);
    expect(() => tasks.respond('app', tApprove.id, { decision: 'approved', response: 'a'.repeat(8001) })).toThrow(ValidationError);

    const tInput = tasks.add({ workspaceId: 'app', type: 'input', text: 'Say something' });
    expect(() => tasks.respond('app', tInput.id, { response: '   ' })).toThrow(ValidationError);

    const tChoice = tasks.add({ workspaceId: 'app', type: 'choice', text: 'Pick', options: ['A', 'B'] });
    expect(() => tasks.respond('app', tChoice.id, { decision: 'C' })).toThrow(ValidationError);

    expect(() => notices.add({ workspaceId: 'app', body: '' })).toThrow(ValidationError);
    expect(() => notices.add({ workspaceId: 'app', body: 'n'.repeat(4001) })).toThrow(ValidationError);
  });

  it('migrates v2 tasks rows cleanly to v3 typed tasks', () => {
    const db = new Database(':memory:');
    db.exec(`
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
      INSERT INTO tasks VALUES ('t1', 'app', 'question', 'Port?', 'done', '8080', 'Maestro', 100, 200, 200, NULL);
      INSERT INTO tasks VALUES ('t2', 'app', 'task', 'Check', 'open', NULL, 'Maestro', 100, 100, NULL, NULL);
    `);
    db.pragma('user_version = 2');
    migrate(db);

    const store = new TaskStore(db);
    expect(store.get('app', 't1')).toMatchObject({ type: 'input', status: 'responded', response: '8080', respondedAt: 200 });
    expect(store.get('app', 't2')).toMatchObject({ type: 'ack', status: 'open' });
  });
});

describe('CLI board commands', () => {
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

  it('infers workspace from cwd and supports typed tasks, choices, answers, notices, and status', () => {
    const { configDir, wsDir, capture, takeOut } = makeEnv();

    runWorkspaceCommand(['add', 'My Project', wsDir, '--maestro', 'Bulbasaur'], { configDir, out: capture });
    expect(takeOut()).toContain('Added workspace my-project');

    const subCwd = path.join(wsDir, 'sub');
    runTaskCommand(['add', '--type', 'approve', '--json', 'Deploy to prod?'], { configDir, cwd: subCwd, out: capture });
    const approveTask = JSON.parse(takeOut()) as { task: { id: string; type: string; createdBy: string } };
    expect(approveTask.task.type).toBe('approve');
    expect(approveTask.task.createdBy).toBe('Bulbasaur');

    runTaskCommand(
      ['add', '--type', 'choice', '--option', 'Fast', '--option', 'Safe', '--json', 'Mode?'],
      { configDir, cwd: subCwd, out: capture },
    );
    const choiceTask = JSON.parse(takeOut()) as { task: { id: string; type: string; options: string[] } };
    expect(choiceTask.task.type).toBe('choice');
    expect(choiceTask.task.options).toEqual(['Fast', 'Safe']);

    runTaskCommand(['list', '--json'], { configDir, cwd: subCwd, out: capture });
    expect((JSON.parse(takeOut()) as { tasks: unknown[] }).tasks).toHaveLength(2);

    const db = openDb(configDir);
    try {
      new TaskStore(db).respond('my-project', approveTask.task.id, { decision: 'approved', response: 'Go ahead' });
    } finally {
      db.close();
    }

    runTaskCommand(['answers', '--json'], { configDir, cwd: subCwd, out: capture });
    const answers = JSON.parse(takeOut()) as { answers: { id: string; decision: string; response: string }[] };
    expect(answers.answers).toHaveLength(1);
    expect(answers.answers[0]).toMatchObject({ id: approveTask.task.id, decision: 'approved', response: 'Go ahead' });

    runNoticeCommand(['add', '**Build green** on `dev`.'], { configDir, cwd: subCwd, out: capture });
    expect(takeOut()).toContain('**Build green**');

    runStatusCommand(['--json'], { configDir, cwd: subCwd, out: capture });
    const st = JSON.parse(takeOut()) as { id: string; openTasks: number; respondedTasks: number; activeNotices: number };
    expect(st).toMatchObject({ id: 'my-project', openTasks: 1, respondedTasks: 1, activeNotices: 1 });
  });

  it('rejects unknown flags and invalid task types with ArgError', () => {
    const { configDir, capture } = makeEnv();
    expect(() => runTaskCommand(['add', '--bogus', 'hi'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runTaskCommand(['add', '--type', 'checkbox', 'hi'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runTaskCommand(['add'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runNoticeCommand(['list', '--status', 'bogus'], { configDir, out: capture })).toThrow(ArgError);
  });
});
