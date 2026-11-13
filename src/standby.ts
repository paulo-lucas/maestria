import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import type { Audit } from './audit.ts';
import type { Config } from './config.ts';
import type { Db } from './db.ts';
import type { EventStore } from './events.ts';
import type { UsageSnapshot } from './usage.ts';
import type { Workspace } from './workspaces.ts';

const execFileAsync = promisify(execFile);

export interface StandbyTrigger {
  providerId: string;
  providerName: string;
  windowId: string;
  windowTitle: string;
  usedPercent: number;
  thresholdPercent: number;
  resetsAt: string | null;
}

export interface StandbyState {
  active: boolean;
  since: number | null;
  updatedAt: number | null;
  triggers: StandbyTrigger[];
  thresholds: Record<string, Record<string, number>>;
  resumeBelowPercent: number | null;
}

export type StandbyNotifier = (kind: 'standby' | 'resume', state: StandbyState, workspaces: readonly Workspace[]) => Promise<void> | void;

const KV_KEY = 'standby_state';

interface StoredStandby {
  active: boolean;
  since: number | null;
  updatedAt: number | null;
  triggers: StandbyTrigger[];
}

export async function defaultCliNotifier(
  kind: 'standby' | 'resume',
  state: StandbyState,
  workspaces: readonly Workspace[],
): Promise<void> {
  const agents = new Set(workspaces.map((w) => w.maestroAgent).filter(Boolean));
  if (kind === 'standby') {
    const top = state.triggers[0];
    const summary = top
      ? `${top.providerName} ${top.windowTitle} reached ${Math.round(top.usedPercent)}% (threshold ${top.thresholdPercent}%)`
      : 'Usage threshold reached';
    try {
      await execFileAsync('maestri', ['notify', `Maestria standby: ${summary}`], { timeout: 10_000 });
    } catch {
      /* maestri CLI optional */
    }
    for (const agent of agents) {
      try {
        await execFileAsync('maestri', ['ask', agent, '--raw', '\\e'], { timeout: 10_000 });
      } catch {
        /* best effort */
      }
    }
  } else {
    try {
      await execFileAsync('maestri', ['notify', 'Maestria: usage dropped below standby threshold; resuming.'], { timeout: 10_000 });
    } catch {
      /* maestri CLI optional */
    }
  }
}

export class StandbyGuard {
  constructor(
    private db: Db,
    private audit: Audit,
    private config: Pick<Config, 'standby'>,
    private events: EventStore | null = null,
    private now: () => number = Date.now,
    private notifier: StandbyNotifier = defaultCliNotifier,
  ) {}

  current(): StandbyState {
    const row = this.db.prepare('SELECT value FROM kv WHERE key = ?').get(KV_KEY) as { value: string } | undefined;
    let stored: StoredStandby = { active: false, since: null, updatedAt: null, triggers: [] };
    if (row) {
      try {
        const p = JSON.parse(row.value) as Partial<StoredStandby>;
        stored = {
          active: Boolean(p.active),
          since: typeof p.since === 'number' ? p.since : null,
          updatedAt: typeof p.updatedAt === 'number' ? p.updatedAt : null,
          triggers: Array.isArray(p.triggers) ? p.triggers : [],
        };
      } catch {
        /* reset malformed */
      }
    }
    return {
      ...stored,
      thresholds: this.config.standby.thresholds,
      resumeBelowPercent: this.config.standby.resumeBelowPercent,
    };
  }

  private save(next: StoredStandby): void {
    this.db
      .prepare('INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value')
      .run(KV_KEY, JSON.stringify(next));
  }

  async evaluate(usage: UsageSnapshot, workspaces: readonly Workspace[]): Promise<StandbyState> {
    const thresholds = this.config.standby.thresholds;
    const prev = this.current();
    const hasConfiguredRules = Object.values(thresholds).some((m) => Object.keys(m).length > 0);
    if (!hasConfiguredRules) {
      if (prev.active) {
        this.save({ active: false, since: null, updatedAt: this.now(), triggers: [] });
      }
      return this.current();
    }

    if (usage.status !== 'ok') {
      return prev;
    }

    const triggers: StandbyTrigger[] = [];
    let evaluatedWindows = 0;
    let allBelowResume = true;

    for (const provider of usage.providers) {
      const rules = thresholds[provider.id] ?? thresholds['*'];
      if (!rules || Object.keys(rules).length === 0) continue;
      if (provider.state !== 'ready' && provider.meters.length === 0) {
        allBelowResume = false;
        continue;
      }

      for (const meter of provider.meters) {
        for (const win of meter.windows) {
          const threshold =
            rules[win.id] ?? (win.ring ? rules[win.ring] : undefined) ?? rules['*'];
          if (threshold === undefined) continue;
          if (win.usedPercent === null) {
            allBelowResume = false;
            continue;
          }
          evaluatedWindows += 1;
          if (win.usedPercent >= threshold) {
            triggers.push({
              providerId: provider.id,
              providerName: provider.name,
              windowId: win.id,
              windowTitle: `${meter.label} · ${win.title}`,
              usedPercent: win.usedPercent,
              thresholdPercent: threshold,
              resetsAt: win.resetsAt,
            });
          }
          const resumeLimit =
            this.config.standby.resumeBelowPercent !== null
              ? Math.min(threshold, this.config.standby.resumeBelowPercent)
              : threshold;
          if (win.usedPercent >= resumeLimit) {
            allBelowResume = false;
          }
        }
      }
    }

    const nowMs = this.now();

    if (triggers.length > 0) {
      const next: StoredStandby = {
        active: true,
        since: prev.active && prev.since !== null ? prev.since : nowMs,
        updatedAt: nowMs,
        triggers,
      };
      this.save(next);
      if (!prev.active) {
        this.audit.log('standby_enter', { detail: { triggers } });
        if (this.events) {
          for (const ws of workspaces) {
            this.events.append(ws.id, 'standby', {
              message: 'Usage standby threshold reached; pause work until resumed.',
              triggers,
            });
          }
        }
        if (this.config.standby.notifyViaCli) {
          await this.notifier('standby', { ...next, thresholds, resumeBelowPercent: this.config.standby.resumeBelowPercent }, workspaces);
        }
      }
      return this.current();
    }

    if (prev.active && evaluatedWindows > 0 && allBelowResume) {
      const next: StoredStandby = {
        active: false,
        since: null,
        updatedAt: nowMs,
        triggers: [],
      };
      this.save(next);
      this.audit.log('standby_resume', { detail: { prevSince: prev.since } });
      if (this.events) {
        for (const ws of workspaces) {
          this.events.append(ws.id, 'resume', {
            message: 'Usage dropped below standby threshold; resume work.',
          });
        }
      }
      if (this.config.standby.notifyViaCli) {
        await this.notifier('resume', { ...next, thresholds, resumeBelowPercent: this.config.standby.resumeBelowPercent }, workspaces);
      }
      return this.current();
    }

    return prev;
  }
}
