import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expandHome } from './config.ts';

export interface MappedWindow {
  id: string;
  title: string;
  usedPercent: number | null;
  resetsAt: string | null;
  durationSeconds: number | null;
  ring: 'session' | 'weekly' | 'special' | null;
}

export interface MappedMeter {
  id: string;
  label: string;
  windows: MappedWindow[];
}

export interface MappedProviderRun {
  state: 'ready' | 'failed' | 'executableNotFound' | 'fileError';
  error: string | null;
  plan: string | null;
  limitsApply: boolean;
  executable: string | null;
  meters: MappedMeter[];
  notes: string[];
  lastSuccessAt: string | null;
}

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** RFC 6901 JSON Pointer lookup. */
export function jsonPointer(doc: unknown, pointer: string): unknown {
  if (pointer === '' || pointer === '/') return pointer === '' ? doc : isObj(doc) ? doc[''] : undefined;
  if (!pointer.startsWith('/')) return undefined;
  let cur: unknown = doc;
  const parts = pointer.slice(1).split('/').map((p) => p.replace(/~1/g, '/').replace(/~0/g, '~'));
  for (const part of parts) {
    if (Array.isArray(cur)) {
      if (!/^(0|[1-9]\d*)$/.test(part)) return undefined;
      cur = cur[Number(part)];
    } else if (isObj(cur)) {
      if (!Object.hasOwn(cur, part)) return undefined;
      cur = cur[part];
    } else {
      return undefined;
    }
  }
  return cur;
}

function matchPredicate(doc: unknown, pred: unknown): boolean {
  if (!isObj(pred) || typeof pred.path !== 'string') return false;
  const val = jsonPointer(doc, pred.path);
  if ('equals' in pred) return val === pred.equals;
  if ('notEquals' in pred) return val !== pred.notEquals;
  if ('exists' in pred) return pred.exists ? val !== undefined && val !== null : val === undefined || val === null;
  return false;
}

function matchesAll(doc: unknown, preds: unknown): boolean {
  if (!Array.isArray(preds)) return true;
  return preds.every((p) => matchPredicate(doc, p));
}

function extractErrorText(val: unknown): string | null {
  if (val === undefined || val === null) return null;
  if (typeof val === 'string') return val;
  if (isObj(val)) {
    if (val.code === -32601) return 'Update the CLI to see usage';
    if (val.code === 'notSignedIn') return 'Not signed in';
    if (typeof val.message === 'string') return val.message;
  }
  return JSON.stringify(val);
}

const KEY_LABELS: Record<string, string> = {
  plan: 'Plan limits',
  session: 'Session',
  weekly: 'Weekly',
  monthly: 'Monthly',
};

function parseDurationSeconds(spec: unknown, val: unknown): number | null {
  if (!isObj(spec)) return null;
  if (typeof spec.seconds === 'number') return spec.seconds;
  if (typeof spec.path !== 'string') return null;
  const raw = jsonPointer(val, spec.path);
  if (spec.as === 'seconds' && typeof raw === 'number') return raw;
  if (spec.as === 'minutes' && typeof raw === 'number') return raw * 60;
  if (spec.as === 'windowName' && typeof raw === 'string') {
    const k = raw.toLowerCase();
    if (k === 'five_hour' || k === '5h') return 5 * 3600;
    if (k === 'daily' || k === 'day') return 86400;
    if (k === 'weekly' || k === 'week' || k === '7d' || k === 'usage_period_type_weekly') return 7 * 86400;
    if (k === 'monthly' || k === 'month' || k === 'usage_period_type_monthly') return 30 * 86400;
  }
  return null;
}

function durationDefaultTitle(sec: number | null, fallback: string): string {
  if (sec === null) return fallback;
  if (sec === 18000) return 'Session';
  if (sec === 604800) return 'Weekly';
  if (sec === 2592000) return 'Monthly';
  if (sec < 3600) return `${Math.round(sec / 60)}m`;
  if (sec < 86400) return `${Math.round(sec / 3600)}h`;
  return `${Math.round(sec / 86400)}d`;
}

function parseUsedPercent(spec: unknown, val: unknown): number | null {
  if (!isObj(spec) || typeof spec.path !== 'string') return null;
  const raw = jsonPointer(val, spec.path);
  const num = typeof raw === 'number' ? raw : typeof raw === 'string' && raw.trim() !== '' ? Number(raw) : NaN;
  if (!Number.isFinite(num)) return null;
  switch (spec.as) {
    case 'percent':
      return num;
    case 'fraction':
      return num * 100;
    case 'remainingFraction':
      return (1 - num) * 100;
    case 'remainingPercent':
      return 100 - num;
    default:
      return null;
  }
}

function parseResetsAt(spec: unknown, val: unknown): string | null {
  if (!isObj(spec) || typeof spec.path !== 'string') return null;
  const raw = jsonPointer(val, spec.path);
  if (raw === undefined || raw === null) return null;
  if (spec.as === 'iso8601' && typeof raw === 'string') {
    const ms = Date.parse(raw);
    return Number.isFinite(ms) ? new Date(ms).toISOString() : null;
  }
  const n = typeof raw === 'number' ? raw : typeof raw === 'string' && /^\d+$/.test(raw) ? Number(raw) : NaN;
  if (!Number.isFinite(n)) return null;
  if (spec.as === 'epochSeconds') return new Date(n * 1000).toISOString();
  if (spec.as === 'epochMillis') return new Date(n).toISOString();
  return null;
}

function resolveId(spec: unknown, val: unknown, entryKey?: string): string | null {
  if (typeof spec === 'string') return spec;
  if (!isObj(spec)) return null;
  if (spec.entryKey === true && entryKey !== undefined) return entryKey;
  if (typeof spec.path === 'string') {
    const v = jsonPointer(val, spec.path);
    if (typeof v === 'string' && v.trim()) return v.trim();
    if (typeof v === 'number' && Number.isInteger(v)) return String(v);
  }
  return null;
}

function resolveLabel(spec: unknown, val: unknown, entryKey?: string): string | null {
  if (!isObj(spec)) return null;
  if (typeof spec.text === 'string') return spec.text;
  if (typeof spec.key === 'string') return KEY_LABELS[spec.key] ?? spec.key;
  if (spec.entryKey === true && entryKey !== undefined) return entryKey;
  if (typeof spec.path === 'string') {
    const v = jsonPointer(val, spec.path);
    if (typeof v === 'string' && v.trim()) return v.trim();
  }
  if (spec.fallback) return resolveLabel(spec.fallback, val, entryKey);
  return null;
}

function expandScope(spec: Obj, current: unknown): Array<{ value: unknown; entryKey?: string }> {
  if (typeof spec.select === 'string') {
    const v = jsonPointer(current, spec.select);
    return v === undefined || v === null ? [] : [{ value: v }];
  }
  if (typeof spec.each === 'string') {
    const arr = jsonPointer(current, spec.each);
    return Array.isArray(arr) ? arr.filter((v) => v !== null && v !== undefined).map((v) => ({ value: v })) : [];
  }
  if (typeof spec.eachEntry === 'string') {
    const obj = jsonPointer(current, spec.eachEntry);
    if (!isObj(obj)) return [];
    return Object.entries(obj)
      .filter(([, v]) => v !== null && v !== undefined)
      .map(([entryKey, value]) => ({ value, entryKey }));
  }
  return current === undefined || current === null ? [] : [{ value: current }];
}

function resolvePlan(spec: unknown, doc: unknown): string | null {
  if (!isObj(spec) || typeof spec.path !== 'string') return null;
  const raw = jsonPointer(doc, spec.path);
  if (typeof raw !== 'string' || !raw.trim()) return null;
  if (isObj(spec.names) && typeof spec.names[raw] === 'string') return spec.names[raw] as string;
  return raw;
}

export function mapProviderOutput(mapSpec: Obj, rawDoc: unknown, notes: string[] = []): {
  plan: string | null;
  limitsApply: boolean;
  error: string | null;
  meters: MappedMeter[];
  notes: string[];
} {
  const root = typeof mapSpec.root === 'string' ? jsonPointer(rawDoc, mapSpec.root) : rawDoc;
  if (root === undefined || root === null) {
    return { plan: null, limitsApply: true, error: 'Missing root in provider output', meters: [], notes };
  }

  let limitsApply = true;
  if (isObj(mapSpec.available) && typeof mapSpec.available.path === 'string') {
    const av = jsonPointer(root, mapSpec.available.path);
    if (av === false) limitsApply = false;
  }

  let error: string | null = null;
  if (isObj(mapSpec.access) && typeof mapSpec.access.path === 'string') {
    if (jsonPointer(root, mapSpec.access.path) === false) error = 'Usage limit reached';
  }

  const plan = resolvePlan(mapSpec.plan, root);
  const meterSpecs = Array.isArray(mapSpec.meters) ? mapSpec.meters : [];
  const meters: MappedMeter[] = [];
  const seenMeterIds = new Set<string>();

  for (const rawMeter of meterSpecs) {
    if (!isObj(rawMeter)) continue;
    for (const { value: mVal, entryKey: mKey } of expandScope(rawMeter, root)) {
      if (rawMeter.where && !matchesAll(mVal, rawMeter.where)) continue;
      const mId = resolveId(rawMeter.id, mVal, mKey);
      const mLabel = resolveLabel(rawMeter.label, mVal, mKey) ?? mId;
      if (!mId || !mLabel || seenMeterIds.has(mId)) continue;

      const windows: MappedWindow[] = [];
      const seenWinIds = new Set<string>();
      const winSpecs = Array.isArray(rawMeter.windows) ? rawMeter.windows : [];
      for (const rawWin of winSpecs) {
        if (!isObj(rawWin)) continue;
        for (const { value: wVal, entryKey: wKey } of expandScope(rawWin, mVal)) {
          if (rawWin.where && !matchesAll(wVal, rawWin.where)) continue;
          const wId = resolveId(rawWin.id, wVal, wKey);
          if (!wId || seenWinIds.has(wId)) continue;
          const durationSeconds = parseDurationSeconds(rawWin.duration, wVal);
          const usedPercent = parseUsedPercent(rawWin.used, wVal);
          const resetsAt = parseResetsAt(rawWin.resetsAt, wVal);
          if (usedPercent === null && resetsAt === null) {
            notes.push(`Skipped window "${wId}": no usedPercent or resetsAt`);
            continue;
          }
          const title = resolveLabel(rawWin.label, wVal, wKey) ?? durationDefaultTitle(durationSeconds, mLabel);
          seenWinIds.add(wId);
          windows.push({ id: wId, title, usedPercent, resetsAt, durationSeconds, ring: null });
        }
      }
      if (windows.length > 0) {
        seenMeterIds.add(mId);
        meters.push({ id: mId, label: mLabel, windows });
      }
    }
  }

  // Assign rings per AGENTS.md spec: 1st meter session/weekly, 2nd meter longest window = special.
  if (meters[0]) {
    const sess = meters[0].windows.find((w) => w.durationSeconds !== null && w.durationSeconds <= 86400);
    if (sess) sess.ring = 'session';
    const week = meters[0].windows.find((w) => w.durationSeconds !== null && w.durationSeconds > 86400);
    if (week) week.ring = 'weekly';
  }
  if (meters[1] && meters[1].windows.length > 0) {
    let best = meters[1].windows[0]!;
    for (const w of meters[1].windows) {
      if ((w.durationSeconds ?? 0) > (best.durationSeconds ?? 0)) best = w;
    }
    best.ring = 'special';
  }

  return { plan, limitsApply, error, meters, notes };
}

function buildMinimalEnv(source: Obj, cacheDir: string | null): NodeJS.ProcessEnv {
  const home = os.homedir();
  const extraPath = [path.join(home, '.local', 'bin'), '/opt/homebrew/bin', '/usr/local/bin'];
  const basePath = process.env.PATH ? `${extraPath.join(':')}:${process.env.PATH}` : `${extraPath.join(':')}:/usr/bin:/bin`;
  const env: NodeJS.ProcessEnv = {
    PATH: basePath,
    HOME: home,
    TERM: 'dumb',
    NO_COLOR: '1',
    LANG: process.env.LANG || 'en_US.UTF-8',
  };
  for (const key of ['USER', 'LOGNAME', 'LC_ALL', 'TMPDIR', 'SHELL', 'SSH_AUTH_SOCK']) {
    if (process.env[key]) env[key] = process.env[key];
  }
  if (isObj(source.env)) {
    for (const [k, v] of Object.entries(source.env)) {
      if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(k) && typeof v === 'string') {
        env[k] = v.startsWith('~/') ? expandHome(v, home) : v;
      }
    }
  }
  if (typeof source.cacheVariable === 'string' && cacheDir && !(source.cacheVariable in env)) {
    env[source.cacheVariable] = cacheDir;
  }
  return env;
}

function resolveExecutable(execSpec: string): string {
  return execSpec.startsWith('~/') ? expandHome(execSpec) : execSpec;
}

async function runCommandSource(source: Obj, executable: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): Promise<unknown> {
  const timeoutMs = (typeof source.timeoutSeconds === 'number' ? source.timeoutSeconds : 30) * 1000;
  const maxBytes = typeof source.maxOutputBytes === 'number' ? source.maxOutputBytes : 1048576;

  return new Promise((resolve, reject) => {
    const child = spawn(executable, args, { cwd, env, stdio: ['ignore', 'pipe', 'ignore'] });
    const chunks: Buffer[] = [];
    let total = 0;
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        child.kill('SIGKILL');
        reject(new Error('Provider command timed out'));
      }
    }, timeoutMs);

    child.on('error', (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(err);
    });

    child.stdout.on('data', (buf: Buffer) => {
      total += buf.length;
      if (total > maxBytes) {
        if (!settled) {
          settled = true;
          clearTimeout(timer);
          child.kill('SIGKILL');
          reject(new Error('Provider output exceeded limit'));
        }
        return;
      }
      chunks.push(buf);
    });

    child.on('close', (code) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (code !== 0) {
        reject(new Error(`Provider exited with status ${code ?? 'signal'}`));
        return;
      }
      let doc: unknown;
      try {
        doc = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      } catch {
        reject(new Error('Provider output was not valid JSON'));
        return;
      }
      if (isObj(source.expect)) {
        const exp = source.expect;
        if (typeof exp.error === 'string') {
          const msg = extractErrorText(jsonPointer(doc, exp.error));
          if (msg) {
            reject(new Error(msg));
            return;
          }
        }
        if (exp.match && !matchesAll(doc, exp.match)) {
          reject(new Error('Provider output did not match expect predicate'));
          return;
        }
        if (typeof exp.require === 'string') {
          const req = jsonPointer(doc, exp.require);
          if (req === undefined || req === null) {
            reject(new Error(`Provider output missing required ${exp.require}`));
            return;
          }
        }
      }
      resolve(doc);
    });
  });
}

async function runStdioSource(source: Obj, executable: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): Promise<unknown> {
  const timeoutMs = (typeof source.timeoutSeconds === 'number' ? source.timeoutSeconds : 15) * 1000;
  const framing = source.framing === 'content-length' ? 'content-length' : 'lines';
  const maxTotal = typeof source.maxTotalBytes === 'number' ? source.maxTotalBytes : 4194304;
  const steps = Array.isArray(source.steps) ? source.steps : [];
  const outputKey = typeof source.output === 'string' ? source.output : '';

  return new Promise((resolve, reject) => {
    const child = spawn(executable, args, { cwd, env, stdio: ['pipe', 'pipe', 'ignore'] });
    const captures = new Map<string, unknown>();
    let stepIdx = 0;
    let buf = Buffer.alloc(0);
    let totalRead = 0;
    let settled = false;

    const fail = (msg: string) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        child.kill('SIGKILL');
      } catch {
        /* ignore */
      }
      reject(new Error(msg));
    };

    const finish = () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        child.stdin.end();
      } catch {
        /* ignore */
      }
      if (!captures.has(outputKey)) {
        reject(new Error(`Missing captured output "${outputKey}"`));
        return;
      }
      resolve(captures.get(outputKey));
    };

    const timer = setTimeout(() => fail('Provider stdio timed out'), timeoutMs);

    const advanceWrites = () => {
      while (stepIdx < steps.length) {
        const s = steps[stepIdx];
        if (!isObj(s) || !('write' in s)) break;
        const payload = JSON.stringify(s.write);
        if (framing === 'content-length') {
          const bytes = Buffer.from(payload, 'utf8');
          child.stdin.write(`Content-Length: ${bytes.length}\r\n\r\n`);
          child.stdin.write(bytes);
        } else {
          child.stdin.write(payload + '\n');
        }
        stepIdx += 1;
      }
      if (stepIdx >= steps.length) finish();
    };

    const handleMsg = (msg: unknown) => {
      if (settled || stepIdx >= steps.length) return;
      const cur = steps[stepIdx];
      if (!isObj(cur) || !isObj(cur.await)) return;
      const aw = cur.await;
      if (aw.match && !matchesAll(msg, aw.match)) return;
      if (typeof aw.error === 'string') {
        const errText = extractErrorText(jsonPointer(msg, aw.error));
        if (errText) {
          fail(errText);
          return;
        }
      }
      if (typeof aw.require === 'string') {
        const req = jsonPointer(msg, aw.require);
        if (req === undefined || req === null) {
          fail(`Response missing ${aw.require}`);
          return;
        }
      }
      if (typeof aw.capture === 'string') captures.set(aw.capture, msg);
      stepIdx += 1;
      advanceWrites();
    };

    child.on('error', (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(err);
    });

    child.stdout.on('data', (chunk: Buffer) => {
      if (settled) return;
      totalRead += chunk.length;
      if (totalRead > maxTotal) {
        fail('Provider stdio exceeded maxTotalBytes');
        return;
      }
      buf = Buffer.concat([buf, chunk]);
      if (framing === 'lines') {
        let nl = buf.indexOf(0x0a);
        while (nl !== -1 && !settled) {
          const line = buf.subarray(0, nl).toString('utf8').trim();
          buf = buf.subarray(nl + 1);
          if (line) {
            try {
              handleMsg(JSON.parse(line));
            } catch {
              /* non-JSON lines are skipped in lines mode */
            }
          }
          nl = buf.indexOf(0x0a);
        }
      } else {
        while (!settled) {
          const sep = buf.indexOf('\r\n\r\n');
          if (sep === -1) break;
          const headers = buf.subarray(0, sep).toString('ascii');
          const m = /content-length:\s*(\d+)/i.exec(headers);
          if (!m) {
            fail('Malformed Content-Length framing');
            return;
          }
          const len = Number(m[1]);
          if (buf.length < sep + 4 + len) break;
          const body = buf.subarray(sep + 4, sep + 4 + len).toString('utf8');
          buf = buf.subarray(sep + 4 + len);
          try {
            handleMsg(JSON.parse(body));
          } catch {
            fail('Invalid JSON body in framed stdio message');
            return;
          }
        }
      }
    });

    child.on('close', (code) => {
      if (!settled) fail(`Provider closed prematurely (code ${code ?? 'signal'})`);
    });

    advanceWrites();
  });
}

/**
 * Execute a Maestri `<id>.json` provider file read-only as a fallback when `.status.json` is stale.
 * Never writes or touches files inside `providersDir`.
 */
export async function executeProviderFile(
  providerFile: string,
  cacheRoot: string,
  now: () => number = Date.now,
): Promise<MappedProviderRun> {
  let st: fs.Stats;
  try {
    st = fs.lstatSync(providerFile);
  } catch (err) {
    return {
      state: 'fileError',
      error: (err as Error).message,
      plan: null,
      limitsApply: true,
      executable: null,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  }
  if (!st.isFile() || st.size > 256 * 1024) {
    return {
      state: 'fileError',
      error: 'Provider file must be a regular file <= 256 KB',
      plan: null,
      limitsApply: true,
      executable: null,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  }

  let spec: unknown;
  try {
    spec = JSON.parse(fs.readFileSync(providerFile, 'utf8'));
  } catch (err) {
    return {
      state: 'fileError',
      error: `Invalid provider JSON: ${(err as Error).message}`,
      plan: null,
      limitsApply: true,
      executable: null,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  }
  if (!isObj(spec) || spec.schemaVersion !== 1 || !isObj(spec.source) || !isObj(spec.map)) {
    return {
      state: 'fileError',
      error: 'Invalid provider schema',
      plan: null,
      limitsApply: true,
      executable: null,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  }
  if (typeof spec.source.executable !== 'string' || !spec.source.executable.trim()) {
    return {
      state: 'fileError',
      error: 'Missing source.executable',
      plan: null,
      limitsApply: true,
      executable: null,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  }

  const executable = resolveExecutable(spec.source.executable);
  const args = Array.isArray(spec.source.args) ? spec.source.args.filter((a): a is string => typeof a === 'string') : [];
  const tmpWorkDir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-prov-'));
  let cacheDir: string | null = null;
  if (typeof spec.source.cacheVariable === 'string' && typeof spec.id === 'string') {
    cacheDir = path.join(cacheRoot, spec.id.replace(/[^a-z0-9._-]/gi, '_'));
    fs.mkdirSync(cacheDir, { recursive: true, mode: 0o700 });
  }
  const env = buildMinimalEnv(spec.source, cacheDir);

  try {
    const doc =
      spec.source.type === 'stdio'
        ? await runStdioSource(spec.source, executable, args, tmpWorkDir, env)
        : await runCommandSource(spec.source, executable, args, tmpWorkDir, env);
    const mapped = mapProviderOutput(spec.map, doc);
    return {
      state: mapped.error ? 'failed' : 'ready',
      error: mapped.error,
      plan: mapped.plan,
      limitsApply: mapped.limitsApply,
      executable,
      meters: mapped.meters,
      notes: mapped.notes,
      lastSuccessAt: new Date(now()).toISOString(),
    };
  } catch (err) {
    const e = err as NodeJS.ErrnoException;
    const notFound = e.code === 'ENOENT';
    return {
      state: notFound ? 'executableNotFound' : 'failed',
      error: notFound ? `The executable "${spec.source.executable}" wasn't found on PATH or at its path` : e.message,
      plan: null,
      limitsApply: true,
      executable,
      meters: [],
      notes: [],
      lastSuccessAt: null,
    };
  } finally {
    fs.rmSync(tmpWorkDir, { recursive: true, force: true });
  }
}
