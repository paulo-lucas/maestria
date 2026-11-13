import { Audit } from '../audit.ts';
import { ChatStore } from '../chat.ts';
import { openDb } from '../db.ts';
import { EventStore, type WorkspaceEvent } from '../events.ts';
import { loadWorkspaces, resolveWorkspace } from '../workspaces.ts';
import { ArgError, parseArgs } from './args.ts';

export interface ChatCliOpts {
  configDir: string;
  cwd?: string;
  stdout?: NodeJS.WritableStream;
  pollIntervalMs?: number;
}

function strFlag(flags: Record<string, string | boolean | string[]>, name: string): string | undefined {
  const v = flags[name];
  return typeof v === 'string' ? v : undefined;
}

export function runSayCommand(args: string[], opts: ChatCliOpts): void {
  const { flags, positional } = parseArgs(args, {
    workspace: { type: 'string', short: 'w' },
    by: { type: 'string' },
    json: { type: 'boolean' },
  });
  const text = positional.join(' ').trim();
  if (!text) {
    throw new ArgError('Usage: maestria say [--workspace|-w <ws>] [--by <agent>] [--json] <text>');
  }
  const workspaces = loadWorkspaces(opts.configDir);
  const ws = resolveWorkspace(workspaces, {
    workspace: strFlag(flags, 'workspace'),
    cwd: opts.cwd,
  });
  const db = openDb(opts.configDir);
  try {
    const audit = new Audit(db);
    const chat = new ChatStore(db, audit);
    const msg = chat.send(
      ws.id,
      { sender: 'maestro', author: strFlag(flags, 'by') ?? ws.maestroAgent, body: text },
      { via: 'cli' },
    );
    const out = opts.stdout ?? process.stdout;
    if (flags.json === true) {
      out.write(JSON.stringify(msg, null, 2) + '\n');
    } else {
      out.write(`[${ws.name}] ${msg.author}: sent (${msg.id})\n`);
    }
  } finally {
    db.close();
  }
}

export function formatEventSummary(ev: WorkspaceEvent): string {
  const ts = new Date(ev.createdAt).toISOString();
  switch (ev.type) {
    case 'chat':
      return `[${ts}] chat from ${String(ev.payload.author ?? 'Human')}: ${String(ev.payload.body ?? '')}`;
    case 'inbox_response': {
      const dec = ev.payload.decision ? ` decision=${String(ev.payload.decision)}` : '';
      const resp = ev.payload.response ? ` response=${JSON.stringify(ev.payload.response)}` : '';
      return `[${ts}] inbox_response (${String(ev.payload.kind ?? '')} ${String(ev.payload.itemId ?? '')}):${dec}${resp}`;
    }
    case 'inbox_dismiss':
      return `[${ts}] inbox_dismiss (${String(ev.payload.kind ?? '')} ${String(ev.payload.itemId ?? '')})`;
    case 'standby': {
      const rawTriggers = Array.isArray(ev.payload.triggers) ? ev.payload.triggers : [];
      const triggerText = rawTriggers
        .filter((t): t is Record<string, unknown> => typeof t === 'object' && t !== null)
        .map((t) => {
          const prov = String(t.providerName ?? t.providerId ?? 'provider');
          const win = String(t.windowTitle ?? t.windowId ?? 'window');
          const used = typeof t.usedPercent === 'number' ? `${Math.round(t.usedPercent)}%` : '?%';
          const limit = typeof t.thresholdPercent === 'number' ? `${t.thresholdPercent}%` : '?%';
          return `${prov} / ${win}: ${used} >= ${limit}`;
        })
        .join('; ');
      const msg = String(ev.payload.message ?? 'Threshold reached');
      return triggerText ? `[${ts}] STANDBY (${triggerText}): ${msg}` : `[${ts}] STANDBY: ${msg}`;
    }
    case 'resume':
      return `[${ts}] RESUME: ${String(ev.payload.message ?? 'Usage below threshold')}`;
  }
}

export async function waitForEvents(opts: {
  events: EventStore;
  workspaceId: string;
  cursor?: string;
  since?: number;
  fromNow?: boolean;
  timeoutSeconds?: number;
  pollIntervalMs?: number;
  signal?: AbortSignal;
}): Promise<{ events: WorkspaceEvent[]; timedOut: boolean }> {
  const cursorName = opts.cursor ?? 'default';
  let afterId: number;
  if (opts.since !== undefined) {
    afterId = opts.since;
  } else if (opts.fromNow) {
    afterId = opts.events.latestId(opts.workspaceId);
    opts.events.setCursor(opts.workspaceId, afterId, cursorName);
  } else {
    const saved = opts.events.getCursor(opts.workspaceId, cursorName);
    afterId = saved ?? 0;
  }

  const deadline = opts.timeoutSeconds && opts.timeoutSeconds > 0 ? Date.now() + opts.timeoutSeconds * 1000 : null;
  const pollMs = opts.pollIntervalMs ?? 500;

  while (!opts.signal?.aborted) {
    const batch = opts.events.since(opts.workspaceId, afterId, 50);
    if (batch.length > 0) {
      const lastId = batch[batch.length - 1]!.id;
      if (opts.since === undefined) {
        opts.events.setCursor(opts.workspaceId, lastId, cursorName);
      }
      return { events: batch, timedOut: false };
    }
    if (deadline !== null && Date.now() >= deadline) {
      return { events: [], timedOut: true };
    }
    await new Promise<void>((resolve) => {
      const t = setTimeout(resolve, pollMs);
      t.unref?.();
      if (opts.signal) {
        const onAbort = () => {
          clearTimeout(t);
          resolve();
        };
        opts.signal.addEventListener('abort', onAbort, { once: true });
      }
    });
  }
  return { events: [], timedOut: false };
}

export async function runWaitCommand(args: string[], opts: ChatCliOpts): Promise<void> {
  const { flags, positional } = parseArgs(args, {
    workspace: { type: 'string', short: 'w' },
    cursor: { type: 'string' },
    since: { type: 'string' },
    timeout: { type: 'string' },
    json: { type: 'boolean' },
    'from-now': { type: 'boolean' },
  });
  if (positional.length > 0) {
    throw new ArgError('Usage: maestria wait [--workspace|-w <ws>] [--cursor <name>] [--since <id>] [--from-now] [--timeout <seconds>] [--json]');
  }
  let since: number | undefined;
  const rawSince = strFlag(flags, 'since');
  if (rawSince !== undefined) {
    if (!/^\d+$/.test(rawSince)) throw new ArgError('--since must be a non-negative integer event id');
    since = Number(rawSince);
  }
  let timeoutSeconds: number | undefined;
  const rawTimeout = strFlag(flags, 'timeout');
  if (rawTimeout !== undefined) {
    const n = Number(rawTimeout);
    if (!Number.isFinite(n) || n < 0) throw new ArgError('--timeout must be a non-negative number of seconds');
    timeoutSeconds = n;
  }

  const workspaces = loadWorkspaces(opts.configDir);
  const ws = resolveWorkspace(workspaces, {
    workspace: strFlag(flags, 'workspace'),
    cwd: opts.cwd,
  });

  const controller = new AbortController();
  const abort = () => controller.abort();
  process.on('SIGINT', abort);
  process.on('SIGTERM', abort);

  const db = openDb(opts.configDir);
  try {
    const events = new EventStore(db);
    const result = await waitForEvents({
      events,
      workspaceId: ws.id,
      cursor: strFlag(flags, 'cursor') ?? 'default',
      since,
      fromNow: flags['from-now'] === true,
      timeoutSeconds,
      pollIntervalMs: opts.pollIntervalMs ?? 500,
      signal: controller.signal,
    });
    const out = opts.stdout ?? process.stdout;
    if (flags.json === true) {
      out.write(JSON.stringify({ workspaceId: ws.id, ...result }, null, 2) + '\n');
    } else {
      for (const ev of result.events) {
        out.write(formatEventSummary(ev) + '\n');
      }
    }
  } finally {
    process.off('SIGINT', abort);
    process.off('SIGTERM', abort);
    db.close();
  }
}
