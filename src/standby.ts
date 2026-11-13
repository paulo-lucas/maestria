import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import type { Audit } from './audit.ts';
import type { Config } from './config.ts';
import type { Db } from './db.ts';
import type { EventStore } from './events.ts';
import type { UsageProviderEntry, UsageSnapshot } from './usage.ts';
import type { MappedWindow } from './usage-runner.ts';
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

export interface WorkspaceStandbyState {
  workspaceId: string;
  workspaceName: string;
  maestroProvider: string;
  active: boolean;
  since: number | null;
  updatedAt: number | null;
  triggers: StandbyTrigger[];
}

export interface StandbyState {
  active: boolean;
  since: number | null;
  updatedAt: number | null;
  triggers: StandbyTrigger[];
  workspaces: WorkspaceStandbyState[];
  thresholds: Record<string, Record<string, number>>;
  resumeBelowPercent: number | null;
}

export type StandbyNotifier = (kind: 'standby' | 'resume', state: StandbyState, workspaces: readonly Workspace[]) => Promise<void> | void;

const KV_KEY = 'standby_state';

const PROVIDER_ALIASES: Record<string, readonly string[]> = {
  claude: ['claude', 'claude-code'],
  'claude-code': ['claude-code', 'claude'],
};

interface StoredWorkspaceStandby {
  workspaceName?: string;
  maestroProvider: string;
  active: boolean;
  since: number | null;
  updatedAt: number | null;
  triggers: StandbyTrigger[];
  clearedResets?: Record<string, string>;
}

interface StoredStandby {
  workspaces: Record<string, StoredWorkspaceStandby>;
}

export function formatStandbyTrigger(t: StandbyTrigger): string {
  const pct = `${Math.round(t.usedPercent)}% >= ${t.thresholdPercent}%`;
  const reset = t.resetsAt ? `, resets ${t.resetsAt}` : '';
  return `${t.providerName} / ${t.windowTitle}: ${pct}${reset}`;
}

function triggerWindowKey(providerId: string, windowId: string): string {
  return `${providerId}:${windowId}`;
}

function inferRing(win: Pick<MappedWindow, 'id' | 'ring' | 'durationSeconds'>): 'session' | 'weekly' | 'special' | null {
  if (win.ring === 'session' || win.ring === 'weekly' || win.ring === 'special') {
    return win.ring;
  }
  if (win.durationSeconds !== null) {
    return win.durationSeconds <= 86_400 ? 'session' : 'weekly';
  }
  const idLower = win.id.toLowerCase();
  if (idLower === 'five_hour' || idLower === '5h' || idLower === 'session') return 'session';
  if (idLower === 'seven_day' || idLower === '7d' || idLower === 'weekly') return 'weekly';
  return null;
}

function findMatchingProvider(providers: readonly UsageProviderEntry[], targetProvider: string): UsageProviderEntry | null {
  const target = targetProvider.toLowerCase();
  const exact = providers.find((p) => p.id.toLowerCase() === target);
  if (exact) return exact;
  const aliases = PROVIDER_ALIASES[target] ?? [];
  for (const alias of aliases) {
    const m = providers.find((p) => p.id.toLowerCase() === alias);
    if (m) return m;
  }
  return null;
}

function findThresholdRules(
  thresholds: Record<string, Record<string, number>>,
  candidates: readonly string[],
): Record<string, number> | null {
  const byLower = new Map<string, Record<string, number>>();
  for (const [k, v] of Object.entries(thresholds)) {
    byLower.set(k.toLowerCase(), v);
  }
  for (const raw of candidates) {
    const key = raw.toLowerCase();
    const direct = byLower.get(key);
    if (direct) return direct;
    for (const alias of PROVIDER_ALIASES[key] ?? []) {
      const aliased = byLower.get(alias);
      if (aliased) return aliased;
    }
  }
  return thresholds['*'] ?? null;
}

function deduplicateTriggers(workspaces: readonly WorkspaceStandbyState[]): StandbyTrigger[] {
  const out: StandbyTrigger[] = [];
  const seen = new Set<string>();
  for (const ws of workspaces) {
    if (!ws.active) continue;
    for (const t of ws.triggers) {
      const k = triggerWindowKey(t.providerId, t.windowId);
      if (seen.has(k)) continue;
      seen.add(k);
      out.push(t);
    }
  }
  return out;
}

export type CliCommandRunner = (file: string, args: readonly string[]) => Promise<void> | void;

const defaultCliRunner: CliCommandRunner = async (file, args) => {
  await execFileAsync(file, [...args], { timeout: 10_000, windowsHide: true });
};

/**
 * Best-effort local notification via `maestri notify` only.
 * The panel never interrupts or controls agents directly — the workspace's Maestro owns that.
 */
export async function defaultCliNotifier(
  kind: 'standby' | 'resume',
  state: StandbyState,
  workspaces: readonly Workspace[],
  runner: CliCommandRunner = defaultCliRunner,
): Promise<void> {
  const wsScope = workspaces.length > 0 ? ` (${workspaces.map((w) => w.name).join(', ')})` : '';
  if (kind === 'standby') {
    const summary = state.triggers.length > 0 ? state.triggers.map(formatStandbyTrigger).join('; ') : 'Usage threshold reached';
    try {
      await runner('maestri', ['notify', `Maestria standby${wsScope}: ${summary}`]);
    } catch {
      /* maestri CLI optional */
    }
  } else {
    try {
      await runner('maestri', ['notify', `Maestria: usage standby cleared${wsScope}; resuming.`]);
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

  private loadStored(): StoredStandby {
    const row = this.db.prepare('SELECT value FROM kv WHERE key = ?').get(KV_KEY) as { value: string } | undefined;
    if (!row) return { workspaces: {} };
    try {
      const p = JSON.parse(row.value) as Partial<StoredStandby> & {
        active?: boolean;
        since?: number | null;
        updatedAt?: number | null;
        triggers?: StandbyTrigger[];
      };
      if (p.workspaces && typeof p.workspaces === 'object' && !Array.isArray(p.workspaces)) {
        const clean: Record<string, StoredWorkspaceStandby> = {};
        for (const [wsId, wsState] of Object.entries(p.workspaces)) {
          if (!wsState || typeof wsState !== 'object') continue;
          clean[wsId] = {
            workspaceName: typeof wsState.workspaceName === 'string' ? wsState.workspaceName : wsId,
            maestroProvider: typeof wsState.maestroProvider === 'string' ? wsState.maestroProvider : 'claude',
            active: Boolean(wsState.active),
            since: typeof wsState.since === 'number' ? wsState.since : null,
            updatedAt: typeof wsState.updatedAt === 'number' ? wsState.updatedAt : null,
            triggers: Array.isArray(wsState.triggers) ? wsState.triggers : [],
            clearedResets:
              wsState.clearedResets && typeof wsState.clearedResets === 'object' ? { ...wsState.clearedResets } : undefined,
          };
        }
        return { workspaces: clean };
      }
    } catch {
      /* reset malformed */
    }
    return { workspaces: {} };
  }

  private saveStored(next: StoredStandby): void {
    this.db
      .prepare('INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value')
      .run(KV_KEY, JSON.stringify(next));
  }

  private toPublicState(stored: StoredStandby): StandbyState {
    const workspaces: WorkspaceStandbyState[] = Object.entries(stored.workspaces).map(([workspaceId, s]) => ({
      workspaceId,
      workspaceName: s.workspaceName ?? workspaceId,
      maestroProvider: s.maestroProvider,
      active: s.active,
      since: s.since,
      updatedAt: s.updatedAt,
      triggers: s.triggers,
    }));
    const activeWorkspaces = workspaces.filter((w) => w.active);
    const active = activeWorkspaces.length > 0;
    const sinceValues = activeWorkspaces.map((w) => w.since).filter((v): v is number => v !== null);
    const updatedValues = workspaces.map((w) => w.updatedAt).filter((v): v is number => v !== null);
    return {
      active,
      since: sinceValues.length > 0 ? Math.min(...sinceValues) : null,
      updatedAt: updatedValues.length > 0 ? Math.max(...updatedValues) : null,
      triggers: deduplicateTriggers(activeWorkspaces),
      workspaces,
      thresholds: this.config.standby.thresholds,
      resumeBelowPercent: this.config.standby.resumeBelowPercent,
    };
  }

  current(): StandbyState {
    return this.toPublicState(this.loadStored());
  }

  async evaluate(usage: UsageSnapshot, workspaces: readonly Workspace[]): Promise<StandbyState> {
    const thresholds = this.config.standby.thresholds;
    const prevStored = this.loadStored();
    const nowMs = this.now();
    const nextStored: StoredStandby = { workspaces: {} };

    const enteredWorkspaces: Workspace[] = [];
    const resumedWorkspaces: Workspace[] = [];

    for (const ws of workspaces) {
      const maestroProvider = (ws.maestroProvider || 'claude').toLowerCase();
      const prevWs: StoredWorkspaceStandby = prevStored.workspaces[ws.id] ?? {
        workspaceName: ws.name,
        maestroProvider,
        active: false,
        since: null,
        updatedAt: null,
        triggers: [],
      };
      const nextCleared: Record<string, string> = { ...(prevWs.clearedResets ?? {}) };

      if (usage.status !== 'ok') {
        const allElapsed =
          prevWs.active &&
          prevWs.since !== null &&
          prevWs.triggers.length > 0 &&
          prevWs.triggers.every((t) => {
            if (!t.resetsAt) return false;
            const ms = Date.parse(t.resetsAt);
            return Number.isFinite(ms) && ms > prevWs.since! && nowMs >= ms;
          });
        if (allElapsed) {
          for (const t of prevWs.triggers) {
            if (t.resetsAt) nextCleared[triggerWindowKey(t.providerId, t.windowId)] = t.resetsAt;
          }
          nextStored.workspaces[ws.id] = {
            workspaceName: ws.name,
            maestroProvider,
            active: false,
            since: null,
            updatedAt: nowMs,
            triggers: [],
            ...(Object.keys(nextCleared).length > 0 ? { clearedResets: nextCleared } : {}),
          };
          this.recordResume(ws, prevWs.since, 'reset');
          resumedWorkspaces.push(ws);
        } else {
          nextStored.workspaces[ws.id] = { ...prevWs, workspaceName: ws.name, maestroProvider };
        }
        continue;
      }

      const provider = findMatchingProvider(usage.providers, maestroProvider);
      const rules = findThresholdRules(thresholds, provider ? [provider.id, maestroProvider] : [maestroProvider]);

      if (!rules || Object.keys(rules).length === 0) {
        if (prevWs.active) {
          nextStored.workspaces[ws.id] = {
            workspaceName: ws.name,
            maestroProvider,
            active: false,
            since: null,
            updatedAt: nowMs,
            triggers: [],
          };
          this.recordResume(ws, prevWs.since, 'thresholds_cleared');
          resumedWorkspaces.push(ws);
        } else {
          nextStored.workspaces[ws.id] = {
            workspaceName: ws.name,
            maestroProvider,
            active: false,
            since: null,
            updatedAt: prevWs.updatedAt,
            triggers: [],
          };
        }
        continue;
      }

      if (!provider || (provider.state !== 'ready' && provider.meters.length === 0)) {
        const allElapsed =
          prevWs.active &&
          prevWs.since !== null &&
          prevWs.triggers.length > 0 &&
          prevWs.triggers.every((t) => {
            if (!t.resetsAt) return false;
            const ms = Date.parse(t.resetsAt);
            return Number.isFinite(ms) && ms > prevWs.since! && nowMs >= ms;
          });
        if (allElapsed) {
          for (const t of prevWs.triggers) {
            if (t.resetsAt) nextCleared[triggerWindowKey(t.providerId, t.windowId)] = t.resetsAt;
          }
          nextStored.workspaces[ws.id] = {
            workspaceName: ws.name,
            maestroProvider,
            active: false,
            since: null,
            updatedAt: nowMs,
            triggers: [],
            ...(Object.keys(nextCleared).length > 0 ? { clearedResets: nextCleared } : {}),
          };
          this.recordResume(ws, prevWs.since, 'reset');
          resumedWorkspaces.push(ws);
        } else {
          nextStored.workspaces[ws.id] = { ...prevWs, workspaceName: ws.name, maestroProvider };
        }
        continue;
      }

      const triggers: StandbyTrigger[] = [];
      let evaluatedWindows = 0;
      let heldByPrevTrigger = false;
      let hasUnknownWindow = false;
      let sawReset = false;

      for (const meter of provider.meters) {
        for (const win of meter.windows) {
          const ring = inferRing(win);
          const threshold = rules[win.id] ?? (ring ? rules[ring] : undefined) ?? rules['*'];
          if (threshold === undefined) continue;

          const winKey = triggerWindowKey(provider.id, win.id);
          const prevTrig = prevWs.active
            ? prevWs.triggers.find((t) => t.providerId === provider.id && t.windowId === win.id)
            : undefined;
          const prevResetMs = prevTrig?.resetsAt ? Date.parse(prevTrig.resetsAt) : NaN;
          const curResetMs = win.resetsAt ? Date.parse(win.resetsAt) : NaN;

          const elapsedExpiredReset = Boolean(
            prevTrig &&
              prevWs.since !== null &&
              Number.isFinite(prevResetMs) &&
              prevResetMs > prevWs.since &&
              nowMs >= prevResetMs &&
              win.resetsAt === prevTrig.resetsAt,
          );

          if (elapsedExpiredReset) {
            evaluatedWindows += 1;
            sawReset = true;
            if (win.resetsAt) nextCleared[winKey] = win.resetsAt;
            continue;
          }

          if (win.usedPercent === null) {
            hasUnknownWindow = true;
            continue;
          }

          // Clear stale-reset suppression once the snapshot rotates to a new window or drops below threshold.
          if (nextCleared[winKey] && (win.resetsAt !== nextCleared[winKey] || win.usedPercent < threshold)) {
            delete nextCleared[winKey];
          }
          if (nextCleared[winKey] && win.resetsAt === nextCleared[winKey] && win.usedPercent >= threshold) {
            evaluatedWindows += 1;
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
            continue;
          }

          if (prevTrig) {
            const windowAdvanced =
              Number.isFinite(prevResetMs) && Number.isFinite(curResetMs) && curResetMs > prevResetMs;
            if (windowAdvanced) {
              sawReset = true;
            } else {
              const resumeLimit =
                this.config.standby.resumeBelowPercent !== null
                  ? Math.min(threshold, this.config.standby.resumeBelowPercent)
                  : threshold;
              if (win.usedPercent >= resumeLimit) {
                heldByPrevTrigger = true;
              }
            }
          }
        }
      }

      if (triggers.length > 0) {
        const nextWs: StoredWorkspaceStandby = {
          workspaceName: ws.name,
          maestroProvider,
          active: true,
          since: prevWs.active && prevWs.since !== null ? prevWs.since : nowMs,
          updatedAt: nowMs,
          triggers,
          ...(Object.keys(nextCleared).length > 0 ? { clearedResets: nextCleared } : {}),
        };
        nextStored.workspaces[ws.id] = nextWs;
        if (!prevWs.active) {
          this.audit.log('standby_enter', {
            detail: { workspaceId: ws.id, maestroProvider, triggers },
          });
          if (this.events) {
            const triggerList = triggers.map(formatStandbyTrigger).join('; ');
            this.events.append(ws.id, 'standby', {
              provider: maestroProvider,
              triggers,
              message: `Usage standby threshold reached (${triggerList}); pause agents until resumed.`,
            });
          }
          enteredWorkspaces.push(ws);
        }
      } else if (prevWs.active && !heldByPrevTrigger && !hasUnknownWindow && evaluatedWindows > 0) {
        const reason = sawReset ? 'reset' : 'below_threshold';
        nextStored.workspaces[ws.id] = {
          workspaceName: ws.name,
          maestroProvider,
          active: false,
          since: null,
          updatedAt: nowMs,
          triggers: [],
          ...(Object.keys(nextCleared).length > 0 ? { clearedResets: nextCleared } : {}),
        };
        this.recordResume(ws, prevWs.since, reason);
        resumedWorkspaces.push(ws);
      } else {
        nextStored.workspaces[ws.id] = {
          ...prevWs,
          workspaceName: ws.name,
          maestroProvider,
          ...(Object.keys(nextCleared).length > 0 ? { clearedResets: nextCleared } : { clearedResets: undefined }),
        };
      }
    }

    this.saveStored(nextStored);
    const state = this.toPublicState(nextStored);

    if (this.config.standby.notifyViaCli) {
      if (enteredWorkspaces.length > 0) {
        await this.notifier('standby', state, enteredWorkspaces);
      }
      if (resumedWorkspaces.length > 0) {
        await this.notifier('resume', state, resumedWorkspaces);
      }
    }

    return state;
  }

  private recordResume(ws: Workspace, prevSince: number | null, reason: 'reset' | 'below_threshold' | 'thresholds_cleared'): void {
    this.audit.log('standby_resume', {
      detail: { workspaceId: ws.id, maestroProvider: ws.maestroProvider, prevSince, reason },
    });
    if (this.events) {
      const msg =
        reason === 'reset'
          ? 'Usage window reset; resume work.'
          : reason === 'thresholds_cleared'
            ? 'Standby thresholds cleared; resume work.'
            : 'Usage dropped below standby threshold; resume work.';
      this.events.append(ws.id, 'resume', {
        provider: ws.maestroProvider,
        reason,
        message: msg,
      });
    }
  }
}
