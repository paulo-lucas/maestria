import path from 'node:path';
import { Audit } from '../audit.ts';
import { openDb } from '../db.ts';
import { NoticeStore, type NoticeStatus } from '../notices.ts';
import { TASK_TYPES, TaskStore, type TaskStatus, type TaskType } from '../tasks.ts';
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
    if (positional.length !== 2) {
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
      type: { type: 'string', short: 't' },
      option: { type: 'string', short: 'o', multiple: true },
      workspace: { type: 'string', short: 'w' },
      by: { type: 'string' },
      json: { type: 'boolean' },
    });
    const text = positional.join(' ').trim();
    if (!text) {
      throw new ArgError('Usage: maestria task add [--type approve|ack|input|choice] [--option <val> ...] [--workspace|-w <ws>] [--by <agent>] <text>');
    }
    const rawType = typeof flags.type === 'string' ? flags.type : 'ack';
    if (!TASK_TYPES.includes(rawType as TaskType)) {
      throw new ArgError(`Invalid --type "${rawType}". Expected one of: ${TASK_TYPES.join(', ')}`);
    }
    const options = Array.isArray(flags.option) ? flags.option : undefined;
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
          type: rawType as TaskType,
          text,
          options,
          createdBy: typeof flags.by === 'string' ? flags.by : ws.maestroAgent,
        },
        { via: 'cli' },
      );
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ task }, null, 2));
      } else {
        const optSuffix = task.options.length ? `\toptions=[${task.options.join(', ')}]` : '';
        writeOut(ctx, `${task.id}\t${task.type}\t${task.text}${optSuffix}`);
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
    if (!['open', 'responded', 'dismissed', 'history', 'all'].includes(status)) {
      throw new ArgError('Invalid --status. Expected open, responded, dismissed, history, or all.');
    }
    const ws = resolveWorkspace(workspaces, {
      workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
      cwd: ctx.cwd,
    });
    const db = openDb(ctx.configDir);
    try {
      const tasks = new TaskStore(db).list(ws.id, { status: status as TaskStatus | 'history' | 'all' });
      if (flags.json) {
        writeOut(ctx, JSON.stringify({ workspace: ws.id, tasks }, null, 2));
        return;
      }
      if (tasks.length === 0) {
        writeOut(ctx, `No ${status === 'all' ? '' : status + ' '}tasks in ${ws.id}.`);
        return;
      }
      for (const t of tasks) {
        const decisionPart = t.decision ? `\tdecision=${t.decision}` : '';
        const responsePart = t.response ? `\tresponse=${JSON.stringify(t.response)}` : '';
        writeOut(ctx, `${t.id}\t[${t.status}]\t(${t.type})\t${t.text}${decisionPart}${responsePart}`);
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
        writeOut(ctx, `No task responses in ${ws.id}.`);
        return;
      }
      for (const t of tasks) {
        const ts = fmtIso(t.respondedAt ?? t.dismissedAt);
        const summary =
          t.status === 'dismissed'
            ? 'dismissed'
            : [t.decision, t.response].filter(Boolean).join(': ') || '(responded)';
        writeOut(ctx, `${t.id}\t${ts}\t(${t.type})\t${t.text}\n  -> ${summary}`);
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
      const tc = taskCounts.get(ws.id) ?? { open: 0, responded: 0, dismissed: 0 };
      const nc = noticeCounts.get(ws.id) ?? { active: 0, dismissed: 0 };
      return {
        id: ws.id,
        name: ws.name,
        path: ws.path,
        maestroAgent: ws.maestroAgent,
        openTasks: tc.open,
        respondedTasks: tc.responded,
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
          `${summary.name} (${summary.id}) · Maestro: ${summary.maestroAgent} · open tasks: ${summary.openTasks}, responded: ${summary.respondedTasks}, notices: ${summary.activeNotices}`,
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
      writeOut(ctx, `${s.id}\t${s.name}\topen:${s.openTasks}\tresponded:${s.respondedTasks}\tnotices:${s.activeNotices}`);
    }
  } finally {
    db.close();
  }
}
