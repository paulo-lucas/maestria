import path from 'node:path';
import { Audit } from '../audit.ts';
import { openDb } from '../db.ts';
import { INBOX_KINDS, InboxStore, type InboxKind, type InboxStatus } from '../inbox.ts';
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
      writeOut(ctx, `${ws.id}\t${ws.name}\t${ws.color}\t${ws.path}\t${ws.maestroAgent}${ws.roadmapPath ? `\t${ws.roadmapPath}` : ''}`);
    }
    return;
  }

  if (sub === 'add') {
    const { flags, positional } = parseArgs(rest, {
      id: { type: 'string' },
      roadmap: { type: 'string' },
      maestro: { type: 'string' },
      color: { type: 'string' },
      json: { type: 'boolean' },
    });
    if (positional.length !== 2) {
      throw new ArgError('Usage: maestria workspace add <name> <path> [--id <id>] [--roadmap <path>] [--maestro <agent>] [--color <#rrggbb>]');
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
        ...(typeof flags.color === 'string' ? { color: flags.color } : {}),
      },
    ]);
    saveWorkspaces(ctx.configDir, next);
    const added = next[next.length - 1]!;
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspace: added }, null, 2));
    } else {
      writeOut(ctx, `Added workspace ${added.id} (${added.name}, ${added.color}) -> ${added.path}`);
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

export function runPostCommand(argv: readonly string[], ctx: CommandContext): void {
  const { flags, positional } = parseArgs(argv, {
    kind: { type: 'string', short: 'k' },
    option: { type: 'string', short: 'o', multiple: true },
    workspace: { type: 'string', short: 'w' },
    by: { type: 'string' },
    json: { type: 'boolean' },
  });
  const body = positional.join(' ').trim();
  if (!body) {
    throw new ArgError('Usage: maestria post [--kind notice|approve|input|choice] [--option <val> ...] [--workspace|-w <ws>] [--by <agent>] <markdown>');
  }
  const rawKind = typeof flags.kind === 'string' ? flags.kind : 'notice';
  if (!INBOX_KINDS.includes(rawKind as InboxKind)) {
    throw new ArgError(`Invalid --kind "${rawKind}". Expected one of: ${INBOX_KINDS.join(', ')}`);
  }
  const options = Array.isArray(flags.option) ? flags.option : undefined;
  const workspaces = loadWorkspaces(ctx.configDir);
  const ws = resolveWorkspace(workspaces, {
    workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
    cwd: ctx.cwd,
  });

  const db = openDb(ctx.configDir);
  try {
    const item = new InboxStore(db, new Audit(db)).post(
      {
        workspaceId: ws.id,
        kind: rawKind as InboxKind,
        body,
        options,
        createdBy: typeof flags.by === 'string' ? flags.by : ws.maestroAgent,
      },
      { via: 'cli' },
    );
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ item }, null, 2));
    } else {
      const optSuffix = item.options.length ? `\toptions=[${item.options.join(', ')}]` : '';
      writeOut(ctx, `${item.id}\t${item.kind}\t${item.body}${optSuffix}`);
    }
  } finally {
    db.close();
  }
}

export function runInboxCommand(argv: readonly string[], ctx: CommandContext): void {
  const { flags, positional } = parseArgs(argv, {
    workspace: { type: 'string', short: 'w' },
    status: { type: 'string', short: 's' },
    kind: { type: 'string', short: 'k' },
    json: { type: 'boolean' },
  });
  if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);
  const status = typeof flags.status === 'string' ? flags.status : 'open';
  if (!['open', 'responded', 'dismissed', 'history', 'all'].includes(status)) {
    throw new ArgError('Invalid --status. Expected open, responded, dismissed, history, or all.');
  }
  let kind: InboxKind | undefined;
  if (typeof flags.kind === 'string') {
    if (!INBOX_KINDS.includes(flags.kind as InboxKind)) {
      throw new ArgError(`Invalid --kind "${flags.kind}". Expected one of: ${INBOX_KINDS.join(', ')}`);
    }
    kind = flags.kind as InboxKind;
  }

  const workspaces = loadWorkspaces(ctx.configDir);
  const ws = resolveWorkspace(workspaces, {
    workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
    cwd: ctx.cwd,
  });

  const db = openDb(ctx.configDir);
  try {
    const items = new InboxStore(db).list({
      workspaceId: ws.id,
      status: status as InboxStatus | 'history' | 'all',
      kind,
    });
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspace: ws.id, items }, null, 2));
      return;
    }
    if (items.length === 0) {
      writeOut(ctx, `No ${status === 'all' ? '' : status + ' '}inbox items in ${ws.id}.`);
      return;
    }
    for (const item of items) {
      const decisionPart = item.decision ? `\tdecision=${item.decision}` : '';
      const responsePart = item.response ? `\tresponse=${JSON.stringify(item.response)}` : '';
      writeOut(ctx, `${item.id}\t[${item.status}]\t(${item.kind})\t${item.body}${decisionPart}${responsePart}`);
    }
  } finally {
    db.close();
  }
}

export function runAnswersCommand(argv: readonly string[], ctx: CommandContext): void {
  const { flags, positional } = parseArgs(argv, {
    workspace: { type: 'string', short: 'w' },
    all: { type: 'boolean' },
    json: { type: 'boolean' },
  });
  if (positional.length > 0) throw new ArgError(`Unexpected argument: ${positional[0]}`);

  const workspaces = loadWorkspaces(ctx.configDir);
  const ws = resolveWorkspace(workspaces, {
    workspace: typeof flags.workspace === 'string' ? flags.workspace : undefined,
    cwd: ctx.cwd,
  });

  const db = openDb(ctx.configDir);
  try {
    const answers = new InboxStore(db).answers(ws.id, { includeDismissed: Boolean(flags.all) });
    if (flags.json) {
      writeOut(ctx, JSON.stringify({ workspace: ws.id, answers }, null, 2));
      return;
    }
    if (answers.length === 0) {
      writeOut(ctx, `No responses in ${ws.id}.`);
      return;
    }
    for (const item of answers) {
      const ts = fmtIso(item.resolvedAt);
      const summary =
        item.status === 'dismissed'
          ? item.decision === 'ack'
            ? 'dismissed (got it)'
            : 'dismissed'
          : [item.decision, item.response].filter(Boolean).join(': ') || '(responded)';
      writeOut(ctx, `${item.id}\t${ts}\t(${item.kind})\t${item.body}\n  -> ${summary}`);
    }
  } finally {
    db.close();
  }
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
    const counts = new InboxStore(db).countsByWorkspace();

    const formatWs = (ws: Workspace) => {
      const c = counts.get(ws.id) ?? { needsYou: 0, updates: 0, history: 0 };
      return {
        id: ws.id,
        name: ws.name,
        path: ws.path,
        color: ws.color,
        maestroAgent: ws.maestroAgent,
        needsYou: c.needsYou,
        updates: c.updates,
        history: c.history,
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
          `${summary.name} (${summary.id}) · Maestro: ${summary.maestroAgent} · needs you: ${summary.needsYou}, updates: ${summary.updates}, history: ${summary.history}`,
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
      writeOut(ctx, `${s.id}\t${s.name}\tneeds_you:${s.needsYou}\tupdates:${s.updates}\thistory:${s.history}`);
    }
  } finally {
    db.close();
  }
}
