import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { inspectMaestriDir, type Config } from './config.ts';
import { executeProviderFile, type MappedMeter, type MappedProviderRun } from './usage-runner.ts';

export type UsageSetupStatus =
  | 'ok'
  | 'missing_maestri_dir'
  | 'missing_providers_dir'
  | 'missing_status_file'
  | 'invalid_status';

export interface UsageProviderEntry {
  id: string;
  name: string;
  enabled: boolean;
  state: string;
  error: string | null;
  plan: string | null;
  executable: string | null;
  file: string | null;
  limitsApply: boolean;
  lastSuccessAt: string | null;
  stale: boolean;
  source: 'maestri' | 'fallback';
  notes: string[];
  meters: MappedMeter[];
}

export interface UsageSnapshot {
  status: UsageSetupStatus;
  setupError: string | null;
  maestriDir: string;
  updatedAt: string | null;
  staleMinutes: number;
  providers: UsageProviderEntry[];
}

export type FallbackRunner = (providerFile: string, cacheRoot: string, now: () => number) => Promise<MappedProviderRun>;

interface FallbackCacheEntry {
  fetchedAtMs: number;
  result: MappedProviderRun;
}

const MAX_STATUS_BYTES = 1024 * 1024;
const NO_FALLBACK_STATES = new Set(['off', 'executableNotFound', 'fileError', 'needsNewerMaestri']);

function asStringOrNull(v: unknown): string | null {
  return typeof v === 'string' && v.trim() ? v : null;
}

function asNumberOrNull(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function normalizeMeters(raw: unknown): MappedMeter[] {
  if (!Array.isArray(raw)) return [];
  const meters: MappedMeter[] = [];
  for (const m of raw) {
    if (typeof m !== 'object' || m === null) continue;
    const mo = m as Record<string, unknown>;
    const id = asStringOrNull(mo.id) ?? '';
    const label = asStringOrNull(mo.label) ?? id;
    if (!id && !label) continue;
    const rawWins = Array.isArray(mo.windows) ? mo.windows : [];
    const windows = rawWins
      .filter((w): w is Record<string, unknown> => typeof w === 'object' && w !== null)
      .map((w) => ({
        id: asStringOrNull(w.id) ?? '',
        title: asStringOrNull(w.title) ?? asStringOrNull(w.id) ?? 'Window',
        usedPercent: asNumberOrNull(w.usedPercent),
        resetsAt: asStringOrNull(w.resetsAt),
        durationSeconds: asNumberOrNull(w.durationSeconds),
        ring: (w.ring === 'session' || w.ring === 'weekly' || w.ring === 'special' ? w.ring : null) as
          | 'session'
          | 'weekly'
          | 'special'
          | null,
      }));
    meters.push({ id: id || label, label, windows });
  }
  return meters;
}

function isTimestampStale(iso: string | null, nowMs: number, cutoffMs: number): boolean {
  if (!iso) return false;
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return false;
  return nowMs - ms > cutoffMs;
}

export class UsageService {
  private cache = new Map<string, FallbackCacheEntry>();
  private inFlight = new Map<string, Promise<MappedProviderRun>>();

  constructor(
    private config: Pick<Config, 'maestriDir' | 'configDir' | 'usage'>,
    private now: () => number = Date.now,
    private runner: FallbackRunner = executeProviderFile,
  ) {}

  async read(opts: { forceFallback?: boolean } = {}): Promise<UsageSnapshot> {
    const dirInfo = inspectMaestriDir(this.config.maestriDir);
    const base = {
      maestriDir: dirInfo.dir,
      updatedAt: null as string | null,
      staleMinutes: this.config.usage.staleMinutes,
    };

    if (!dirInfo.hasDir) {
      return {
        ...base,
        status: 'missing_maestri_dir',
        setupError: `Maestri data directory not found at "${dirInfo.dir}". Set "maestriDir" in config.json or MAESTRI_HOME, or run "maestria init".`,
        providers: [],
      };
    }
    if (!dirInfo.hasProvidersDir) {
      return {
        ...base,
        status: 'missing_providers_dir',
        setupError: `Usage providers folder missing at "${dirInfo.providersDir}". Enable Agent Usage in Maestri (Settings -> Agents -> Usage).`,
        providers: [],
      };
    }
    if (!dirInfo.hasStatusFile) {
      return {
        ...base,
        status: 'missing_status_file',
        setupError: `Usage status file (.status.json) not found in "${dirInfo.providersDir}". Turn on Agent Usage in Maestri so Maestri writes normalized readings.`,
        providers: [],
      };
    }

    let rawJson: unknown;
    try {
      const st = fs.lstatSync(dirInfo.statusFile);
      if (!st.isFile() || st.size > MAX_STATUS_BYTES) {
        return {
          ...base,
          status: 'invalid_status',
          setupError: `Invalid .status.json file in "${dirInfo.providersDir}".`,
          providers: [],
        };
      }
      rawJson = JSON.parse(fs.readFileSync(dirInfo.statusFile, 'utf8'));
    } catch (err) {
      return {
        ...base,
        status: 'invalid_status',
        setupError: `Could not read .status.json: ${(err as Error).message}`,
        providers: [],
      };
    }

    if (typeof rawJson !== 'object' || rawJson === null || !Array.isArray((rawJson as Record<string, unknown>).providers)) {
      return {
        ...base,
        status: 'invalid_status',
        setupError: '.status.json is missing a "providers" array.',
        providers: [],
      };
    }

    const doc = rawJson as Record<string, unknown>;
    const updatedAt = asStringOrNull(doc.updatedAt);
    const nowMs = this.now();
    const staleMs = this.config.usage.staleMinutes * 60_000;
    const providers: UsageProviderEntry[] = [];

    for (const rawProv of doc.providers as unknown[]) {
      if (typeof rawProv !== 'object' || rawProv === null) continue;
      const p = rawProv as Record<string, unknown>;
      const id = asStringOrNull(p.id);
      if (!id) continue;

      const enabled = typeof p.enabled === 'boolean' ? p.enabled : true;
      const state = asStringOrNull(p.state) ?? 'waiting';
      const lastSuccessAt = asStringOrNull(p.lastSuccessAt);
      const stale =
        enabled &&
        !NO_FALLBACK_STATES.has(state) &&
        (isTimestampStale(lastSuccessAt, nowMs, staleMs) || (!lastSuccessAt && state === 'waiting'));

      let entry: UsageProviderEntry = {
        id,
        name: asStringOrNull(p.name) ?? id,
        enabled,
        state,
        error: asStringOrNull(p.error),
        plan: asStringOrNull(p.plan),
        executable: asStringOrNull(p.executable),
        file: asStringOrNull(p.file),
        limitsApply: typeof p.limitsApply === 'boolean' ? p.limitsApply : true,
        lastSuccessAt,
        stale,
        source: 'maestri',
        notes: Array.isArray(p.notes) ? p.notes.filter((n): n is string => typeof n === 'string') : [],
        meters: normalizeMeters(p.meters),
      };

      if (this.config.usage.fallbackEnabled && stale && /^[a-z0-9][a-z0-9-]{0,39}$/.test(id)) {
        const providerFile = path.join(dirInfo.providersDir, `${id}.json`);
        if (fs.existsSync(providerFile)) {
          const fb = await this.runWithCache(id, providerFile, staleMs, Boolean(opts.forceFallback));
          if (fb.state === 'ready' || entry.meters.length === 0) {
            entry = {
              ...entry,
              state: fb.state,
              error: fb.error,
              plan: fb.plan ?? entry.plan,
              executable: fb.executable ?? entry.executable,
              limitsApply: fb.limitsApply,
              lastSuccessAt: fb.lastSuccessAt ?? entry.lastSuccessAt,
              stale: fb.state !== 'ready',
              source: 'fallback',
              notes: fb.notes,
              meters: fb.meters.length > 0 ? fb.meters : entry.meters,
            };
          }
        }
      }

      providers.push(entry);
    }

    return {
      ...base,
      status: 'ok',
      setupError: null,
      updatedAt,
      providers,
    };
  }

  private async runWithCache(id: string, providerFile: string, staleMs: number, force: boolean): Promise<MappedProviderRun> {
    const nowMs = this.now();
    const cached = this.cache.get(id);
    const minTtl = cached ? (cached.result.state === 'ready' ? Math.max(60_000, staleMs / 2) : 120_000) : 0;
    if (!force && cached && nowMs - cached.fetchedAtMs < minTtl) {
      return cached.result;
    }

    let pending = this.inFlight.get(id);
    if (!pending) {
      const cacheRoot = path.join(os.tmpdir(), 'maestria-provider-cache');
      pending = this.runner(providerFile, cacheRoot, this.now)
        .then((res) => {
          this.cache.set(id, { fetchedAtMs: this.now(), result: res });
          return res;
        })
        .finally(() => {
          this.inFlight.delete(id);
        });
      this.inFlight.set(id, pending);
    }
    return pending;
  }
}
