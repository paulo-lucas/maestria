import path from 'node:path';
import { Audit } from '../audit.ts';
import { openDb } from '../db.ts';
import { NoticeStore, type NoticeStatus } from '../notices.ts';
import { TaskStore, type TaskStatus } from '../tasks.ts';
import {
  loadWorkspaces,
  parseWorkspaces,
  resolveWorkspace,
  saveWorkspaces,
  slugifyWorkspaceId,
  WorkspaceError,
  type Workspace,
} from '../workspaces.ts';
import { ArgError, parseArgs } from './args.ts';

export interface CommandContext {
  configDir: string;
  cwd?: string;
  out?: (text: string) => void;
}

function writeOut(ctx: CommandContext, line: string): void {
  if (ctx.out) ctx.out(line + '\n');
  else process.stdout.write(line + '\n');
}

function fmtIso(ms: number | null): string {
  return ms === null ? '' : new Date(ms).toISOString();
}

export function runWorkspaceCommand(argv: readonly string[], ctx: CommandContext): void {
  const [sub, ...rest] = argv;
  if (!sub || sub === 'list') {
    const { flags, positional } = parseArgs(rest, { json: { type: 'boolean' } });
    if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);
    const list = loadWorkspaces(ctx.configDir);
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspaces: list }, null, 2));
      return;
    }
    if (list.length === 0) {
      writeOut(ctx, 'No workspaces configured.');
      return;
    }
    for (const ws of list) {
      writeOut(ctx, `${ws.id}\t${ws.name}\t${ws.path}\t${ws.maestroAgent}${ws.roadmapPath ? `\t${ws.roadmapPath}` : ''}`);
    }
    return;
  }

  if (sub === 'add') {
    const { flags, positional } = parseArgs(rest, {
      id: { type: 'string' },
      roadmap: { type: 'string' },
      maestro: { type: 'string' },
      json: { type: 'boolean' },
    });
    if (positional.length < 2 || positional.length > 2) {
      throw new ArgError('Usage: maestria workspace add <name> <path> [--id <id>] [--roadmap <path>] [--maestro <agent>]');
    }
    const [name, rawPath] = positional as [string, string];
    const absPath = path.resolve(ctx.cwd ?? process.cwd(), rawPath);
    const id = typeof flags.id === 'string' ? flags.id : slugifyWorkspaceId(name);
    const existing = loadWorkspaces(ctx.configDir);
    const next: Workspace[] = parseWorkspaces([
      ...existing,
      {
        id,
        name,
        path: absPath,
        roadmapPath: typeof flags.roadmap === 'string' ? flags.roadmap : null,
        maestroAgent: typeof flags.maestro === 'string' ? flags.maestro : 'Maestro',
      },
    ]);
    saveWorkspaces(ctx.configDir, next);
    const added = next[next.length - 1]!;
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspace: added }, null, 2));
    } else {
      writeOut(ctx, `Added workspace ${added.id} (${added.name}) -> ${added.path}`);
    }
    return;
  }

  if (sub === 'remove') {
    const { positional } = parseArgs(rest, {});
    if (positional.length !== 1) throw new ArgError('Usage: maestria workspace remove <id-or-name>');
    const existing = loadWorkspaces(ctx.configDir);
    const target = resolveWorkspace(existing, { workspace: positional[0]! });
    const next = existing.filter((w) => w.id !== target.id);
    saveWorkspaces(ctx.configDir, next);
    writeOut(ctx, `Removed workspace ${target.id} (${target.name})`);
    return;
  }

  throw new ArgError(`Unknown workspace subcommand: ${sub}`);
}

export function runTaskCommand(argv: readonly string[], ctx: CommandContext): void {
  const [sub, ...rest] = argv;
  if (!sub) throw new ArgError('Usage: maestria task <add|list|answers> ...');

  const workspaces = loadWorkspaces(ctx.configDir);

  if (sub === 'add') {
    const { flags, positional } = parseArgs(rest, {
      question: { type: 'boolean', short: 'q' },
      workspace: { type: 'string', short: 'w' },
      by: { type: 'string' },
      json: { type: 'boolean' },
    });
    const text = positional.join(' ').trim();
    if (!text) throw new ArgError('Usage: maestria task add [--question|-q] [--workspace|-w <ws>] [--by <agent>] <text>');
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const store = new TaskStore(db, new Audit(db));
      const task = store.add(
        {
          workspaceId: ws.id,
          kind: flags.question ? 'question' : 'task',
          text,
          createdBy: typeof flags.by === 'string' ? flags.by : ws.maestroAgent,
        },
        { via: 'cli' },
      );
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ task }, null, 2));
      } else {
        writeOut(ctx, `${task.id}\t${task.kind}\t${task.text}`);
      }
    } finally {
      db.close();
    }
    return;
  }

  if (sub === 'list') {
    const { flags, positional } = parseArgs(rest, {
      workspace: { type: 'string', short: 'w' },
      status: { type: 'string', short: 's' },
      json: { type: 'boolean' },
    });
    if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);
    const status = typeof flags.status === 'string' ? flags.status : 'open';
    if (!['open', 'done', 'dismissed', 'all'].includes(status)) {
      throw new ArgError('Invalid --status. Expected open, done, dismissed, or all.');
    }
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const tasks = new TaskStore(db).list(ws.id, { status: status as TaskStatus | 'all' });
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ workspace: ws.id, tasks }, null, 2));
        return;
      }
      if (tasks.length === 0) {
        writeOut(ctx, `No ${status === 'all' ? '' : status + ' '}tasks in ${ws.id}.`);
        return;
      }
      for (const t of tasks) {
        const answerPart = t.answer ? `\tanswer=${JSON.stringify(t.answer)}` : '';
        writeOut(ctx, `${t.id}\t[${t.status}]\t(${t.kind})\t${t.text}${answerPart}`);
      }
    } finally {
      db.close();
    }
    return;
  }

  if (sub === 'answers') {
    const { flags, positional } = parseArgs(rest, {
      workspace: { type: 'string', short: 'w' },
      all: { type: 'boolean' },
      json: { type: 'boolean' },
    });
    if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const tasks = new TaskStore(db).answers(ws.id, { includeDismissed: Boolean(flags.all) });
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ workspace: ws.id, answers: tasks }, null, 2));
        return;
      }
      if (tasks.length === 0) {
        writeOut(ctx, `No answered or completed tasks in ${ws.id}.`);
        return;
      }
      for (const t of tasks) {
        const ts = fmtIso(t.doneAt ?? t.dismissedAt);
        const ans = t.answer !== null && t.answer !== '' ? t.answer : '(marked done)';
        writeOut(ctx, `${t.id}\t${ts}\tQ: ${t.text}\n  A: ${ans}`);
      }
    } finally {
      db.close();
    }
    return;
  }

  throw new ArgError(`Unknown task subcommand: ${sub}`);
}

export function runNoticeCommand(argv: readonly string[], ctx: CommandContext): void {
  const [sub, ...rest] = argv;
  if (!sub) throw new ArgError('Usage: maestria notice <add|list> ...');

  const workspaces = loadWorkspaces(ctx.configDir);

  if (sub === 'add') {
    const { flags, positional } = parseArgs(rest, {
      workspace: { type: 'string', short: 'w' },
      by: { type: 'string' },
      json: { type: 'boolean' },
    });
    const body = positional.join(' ').trim();
    if (!body) throw new ArgError('Usage: maestria notice add [--workspace|-w <ws>] [--by <agent>] <text>');
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const notice = new NoticeStore(db, new Audit(db)).add(
        {
          workspaceId: ws.id,
          body,
          createdBy: typeof flags.by === 'string' ? flags.by : ws.maestroAgent,
        },
        { via: 'cli' },
      );
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ notice }, null, 2));
      } else {
        writeOut(ctx, `${notice.id}\t${notice.body}`);
      }
    } finally {
      db.close();
    }
    return;
  }

  if (sub === 'list') {
    const { flags, positional } = parseArgs(rest, {
      workspace: { type: 'string', short: 'w' },
      status: { type: 'string', short: 's' },
      json: { type: 'boolean' },
    });
    if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);
    const status = typeof flags.status === 'string' ? flags.status : 'active';
    if (!['active', 'dismissed', 'all'].includes(status)) {
      throw new ArgError('Invalid --status. Expected active, dismissed, or all.');
    }
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const notices = new NoticeStore(db).list(ws.id, { status: status as NoticeStatus | 'all' });
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ workspace: ws.id, notices }, null, 2));
        return;
      }
      if (notices.length === 0) {
        writeOut(ctx, `No ${status === 'all' ? '' : status + ' '}notices in ${ws.id}.`);
        return;
      }
      for (const n of notices) {
        writeOut(ctx, `${n.id}\t[${n.status}]\t${n.body}`);
      }
    } finally {
      db.close();
    }
    return;
  }

  throw new ArgError(`Unknown notice subcommand: ${sub}`);
}

export function runStatusCommand(argv: readonly string[], ctx: CommandContext): void {
  const { flags, positional } = parseArgs(argv, {
    workspace: { type: 'string', short: 'w' },
    json: { type: 'boolean' },
  });
  if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);

  const workspaces = loadWorkspaces(ctx.configDir);
  const db = openDb(ctx.configDir);
  try {
    const taskCounts = new TaskStore(db).countsByWorkspace();
    const noticeCounts = new NoticeStore(db).countsByWorkspace();

    const formatWs = (ws: Workspace) => {
      const tc = taskCounts.get(ws.id) ?? { open: 0, done: 0, dismissed: 0 };
      const nc = noticeCounts.get(ws.id) ?? { active: 0, dismissed: 0 };
      return {
        id: ws.id,
        name: ws.name,
        path: ws.path,
        maestroAgent: ws.maestroAgent,
        openTasks: tc.open,
        doneTasks: tc.done,
        dismissedTasks: tc.dismissed,
        activeNotices: nc.active,
      };
    };

    let selected: Workspace | null = null;
    if (typeof flags.workspace === 'string') {
      selected = resolveWorkspace(workspaces, { workspace: flags.workspace });
    } else {
      try {
        selected = resolveWorkspace(workspaces, { cwd: ctx.cwd });
      } catch (err) {
        if (!(err instanceof WorkspaceError)) throw err;
      }
    }

    if (selected) {
      const summary = formatWs(selected);
      if (flags.json) {
        writeOut(ctx, JSON.stringify(summary, null, 2));
      } else {
        writeOut(
          ctx,
          `${summary.name} (${summary.id}) · Maestro: ${summary.maestroAgent} · open tasks: ${summary.openTasks}, done: ${summary.doneTasks}, notices: ${summary.activeNotices}`,
        );
      }
      return;
    }

    const list = workspaces.map(formatWs);
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspaces: list }, null, 2));
      return;
    }
    if (list.length === 0) {
      writeOut(ctx, 'No workspaces configured.');
      return;
    }
    for (const s of list) {
      writeOut(ctx, `${s.id}\t${s.name}\topen:${s.openTasks}\tdone:${s.doneTasks}\tnotices:${s.activeNotices}`);
    }
  } finally {
    db.close();
  }
}
