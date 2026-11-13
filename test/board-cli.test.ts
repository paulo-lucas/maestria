import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { Audit } from '../src/audit.ts';
import { ArgError } from '../src/commands/args.ts';
import { runNoticeCommand, runStatusCommand, runTaskCommand, runWorkspaceCommand } from '../src/commands/board.ts';
import { openDb, openMemoryDb } from '../src/db.ts';
import { NoticeStore } from '../src/notices.ts';
import { TaskStore, ValidationError } from '../src/tasks.ts';

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

describe('TaskStore and NoticeStore', () => {
  it('manages task lifecycle: open -> done (with answer) -> open (preserves answer) -> dismissed (archived)', () => {
    const db = openMemoryDb();
    const audit = new Audit(db);
    const c = clock();
    const tasks = new TaskStore(db, audit, c.now);

    const q = tasks.add({ workspaceId: 'app', kind: 'question', text: 'Which port?' });
    expect(q.status).toBe('open');
    expect(q.kind).toBe('question');

    c.advance(1000);
    const answered = tasks.updateState('app', q.id, { status: 'done', answer: 'Use 8080' }, { via: 'web', ip: '127.0.0.1' });
    expect(answered?.status).toBe('done');
    expect(answered?.answer).toBe('Use 8080');
    expect(answered?.doneAt).toBe(c.now());

    // Reopening preserves the existing answer for editing.
    const reopened = tasks.updateState('app', q.id, { status: 'open' }, { via: 'web' });
    expect(reopened?.status).toBe('open');
    expect(reopened?.answer).toBe('Use 8080');
    expect(reopened?.doneAt).toBeNull();

    // Dismissing archives rather than deleting.
    const dismissed = tasks.updateState('app', q.id, { status: 'dismissed' }, { via: 'web' });
    expect(dismissed?.status).toBe('dismissed');
    expect(tasks.list('app', { status: 'board' })).toHaveLength(0);
    expect(tasks.list('app', { status: 'dismissed' })).toHaveLength(1);
    expect(tasks.answers('app', { includeDismissed: true })).toHaveLength(1);

    const events = audit.recent().map((e) => e.event);
    expect(events).toContain('task_add');
    expect(events).toContain('task_update');
    expect(events).toContain('task_dismiss');
  });

  it('enforces validation limits on tasks and notices', () => {
    const db = openMemoryDb();
    const tasks = new TaskStore(db);
    const notices = new NoticeStore(db);

    expect(() => tasks.add({ workspaceId: 'app', text: '   ' })).toThrow(ValidationError);
    expect(() => tasks.add({ workspaceId: 'app', text: 'x'.repeat(2001) })).toThrow(ValidationError);
    const t = tasks.add({ workspaceId: 'app', text: 'ok' });
    expect(() => tasks.updateState('app', t.id, { status: 'done', answer: 'a'.repeat(8001) })).toThrow(ValidationError);

    expect(() => notices.add({ workspaceId: 'app', body: '' })).toThrow(ValidationError);
    expect(() => notices.add({ workspaceId: 'app', body: 'n'.repeat(4001) })).toThrow(ValidationError);
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

  it('infers workspace from cwd and supports workspace, task, notice, and status commands', () => {
    const { configDir, wsDir, capture, takeOut } = makeEnv();

    runWorkspaceCommand(['add', 'My Project', wsDir, '--maestro', 'Bulbasaur'], { configDir, out: capture });
    expect(takeOut()).toContain('Added workspace my-project');

    runWorkspaceCommand(['list', '--json'], { configDir, out: capture });
    const wsList = JSON.parse(takeOut()) as { workspaces: { id: string; maestroAgent: string }[] };
    expect(wsList.workspaces[0]).toMatchObject({ id: 'my-project', maestroAgent: 'Bulbasaur' });

    // Run inside a subdirectory of the workspace without passing --workspace
    const subCwd = path.join(wsDir, 'sub');
    runTaskCommand(['add', '--question', '--json', 'Deploy to prod?'], { configDir, cwd: subCwd, out: capture });
    const addedTask = JSON.parse(takeOut()) as { task: { id: string; kind: string; createdBy: string } };
    expect(addedTask.task.kind).toBe('question');
    expect(addedTask.task.createdBy).toBe('Bulbasaur');

    runTaskCommand(['add', 'Review PR #42'], { configDir, cwd: subCwd, out: capture });
    expect(takeOut()).toContain('Review PR #42');

    runTaskCommand(['list', '--json'], { configDir, cwd: subCwd, out: capture });
    const openList = JSON.parse(takeOut()) as { tasks: { id: string }[] };
    expect(openList.tasks).toHaveLength(2);

    // Answer the question in the real configDir db and verify `task answers`
    const db = openDb(configDir);
    try {
      new TaskStore(db).updateState('my-project', addedTask.task.id, { status: 'done', answer: 'Yes, tonight' });
    } finally {
      db.close();
    }
    runTaskCommand(['answers', '--json'], { configDir, cwd: subCwd, out: capture });
    const answers = JSON.parse(takeOut()) as { answers: { id: string; answer: string }[] };
    expect(answers.answers).toHaveLength(1);
    expect(answers.answers[0]).toMatchObject({ id: addedTask.task.id, answer: 'Yes, tonight' });

    runNoticeCommand(['add', '**Build green** on `dev`.'], { configDir, cwd: subCwd, out: capture });
    expect(takeOut()).toContain('**Build green**');

    runNoticeCommand(['list', '--json'], { configDir, cwd: subCwd, out: capture });
    const noticeList = JSON.parse(takeOut()) as { notices: { bodyHtml: string }[] };
    expect(noticeList.notices).toHaveLength(1);
    expect(noticeList.notices[0]!.bodyHtml).toContain('<strong>Build green</strong>');

    runStatusCommand(['--json'], { configDir, cwd: subCwd, out: capture });
    const st = JSON.parse(takeOut()) as { id: string; openTasks: number; doneTasks: number; activeNotices: number };
    expect(st).toMatchObject({ id: 'my-project', openTasks: 1, doneTasks: 1, activeNotices: 1 });

    // Outside any workspace, status prints overview
    runStatusCommand(['--json'], { configDir, cwd: os.tmpdir(), out: capture });
    const overview = JSON.parse(takeOut()) as { workspaces: unknown[] };
    expect(overview.workspaces).toHaveLength(1);
  });

  it('rejects unknown flags and missing arguments with ArgError', () => {
    const { configDir, capture } = makeEnv();
    expect(() => runTaskCommand(['add', '--bogus', 'hi'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runTaskCommand(['add'], { configDir, out: capture })).toThrow(ArgError);
    expect(() => runNoticeCommand(['list', '--status', 'bogus'], { configDir, out: capture })).toThrow(ArgError);
  });
});
