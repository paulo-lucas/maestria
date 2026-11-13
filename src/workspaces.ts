import fs from 'node:fs';
import path from 'node:path';
import { ensureConfigDir } from './config.ts';

export interface Workspace {
  id: string;
  name: string;
  path: string;
  roadmapPath: string | null;
  maestroAgent: string;
}

export class WorkspaceError extends Error {}

const ID_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const KNOWN_KEYS = new Set(['id', 'name', 'path', 'roadmapPath', 'maestroAgent']);

export function slugifyWorkspaceId(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64);
}

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function nonEmptyString(v: unknown, field: string, max: number): string {
  if (typeof v !== 'string') throw new WorkspaceError(`workspaces: "${field}" must be a string`);
  const trimmed = v.trim();
  if (!trimmed || trimmed.length > max) {
    throw new WorkspaceError(`workspaces: "${field}" must be 1..${max} characters`);
  }
  return trimmed;
}

export function parseWorkspaces(raw: unknown): Workspace[] {
  if (!Array.isArray(raw)) throw new WorkspaceError('workspaces: top level must be an array');
  if (raw.length > 100) throw new WorkspaceError('workspaces: at most 100 workspaces are supported');

  const seenIds = new Set<string>();
  const seenPaths = new Set<string>();
  const out: Workspace[] = [];

  for (let i = 0; i < raw.length; i++) {
    const item = raw[i];
    if (!isObj(item)) throw new WorkspaceError(`workspaces[${i}]: entry must be an object`);
    for (const k of Object.keys(item)) {
      if (!KNOWN_KEYS.has(k)) throw new WorkspaceError(`workspaces[${i}]: unknown key "${k}"`);
    }

    const name = nonEmptyString(item.name, `workspaces[${i}].name`, 80);
    const rawPath = nonEmptyString(item.path, `workspaces[${i}].path`, 500);
    if (!path.isAbsolute(rawPath)) {
      throw new WorkspaceError(`workspaces[${i}].path: must be an absolute path`);
    }
    const resolvedPath = path.resolve(rawPath);

    let id: string;
    if (item.id !== undefined) {
      id = nonEmptyString(item.id, `workspaces[${i}].id`, 64);
    } else {
      id = slugifyWorkspaceId(name);
      if (!id) throw new WorkspaceError(`workspaces[${i}]: cannot derive an id from "${name}"; set "id" explicitly`);
    }
    if (!ID_RE.test(id)) {
      throw new WorkspaceError(`workspaces[${i}].id: must match ${ID_RE.source}`);
    }
    if (seenIds.has(id)) throw new WorkspaceError(`workspaces: duplicate id "${id}"`);
    if (seenPaths.has(resolvedPath)) throw new WorkspaceError(`workspaces: duplicate path "${resolvedPath}"`);
    seenIds.add(id);
    seenPaths.add(resolvedPath);

    let roadmapPath: string | null = null;
    if (item.roadmapPath !== undefined && item.roadmapPath !== null) {
      roadmapPath = nonEmptyString(item.roadmapPath, `workspaces[${i}].roadmapPath`, 500);
    }

    const maestroAgent =
      item.maestroAgent !== undefined && item.maestroAgent !== null
        ? nonEmptyString(item.maestroAgent, `workspaces[${i}].maestroAgent`, 64)
        : 'Maestro';

    out.push({ id, name, path: resolvedPath, roadmapPath, maestroAgent });
  }
  return out;
}

export function workspacesFile(configDir: string): string {
  return path.join(configDir, 'workspaces.json');
}

export function loadWorkspaces(configDir: string): Workspace[] {
  const file = workspacesFile(configDir);
  let raw: unknown;
  try {
    raw = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw new WorkspaceError(`workspaces: cannot read ${file}: ${(err as Error).message}`);
  }
  return parseWorkspaces(raw);
}

export function saveWorkspaces(configDir: string, workspaces: readonly Workspace[]): void {
  const validated = parseWorkspaces(workspaces);
  ensureConfigDir(configDir);
  const file = workspacesFile(configDir);
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(validated, null, 2) + '\n', { mode: 0o600 });
  fs.chmodSync(tmp, 0o600);
  fs.renameSync(tmp, file);
}

function safeRealpath(p: string): string {
  try {
    return fs.realpathSync(p);
  } catch {
    return path.resolve(p);
  }
}

function isInsideDir(target: string, dir: string): boolean {
  return target === dir || target.startsWith(dir.endsWith(path.sep) ? dir : dir + path.sep);
}

export function resolveWorkspace(
  workspaces: readonly Workspace[],
  opts: { workspace?: string; cwd?: string } = {},
): Workspace {
  if (opts.workspace !== undefined) {
    const q = opts.workspace.trim();
    const byId = workspaces.find((w) => w.id === q);
    if (byId) return byId;
    const lower = q.toLowerCase();
    const byName = workspaces.find((w) => w.id.toLowerCase() === lower || w.name.toLowerCase() === lower);
    if (byName) return byName;
    throw new WorkspaceError(`Unknown workspace "${q}".`);
  }

  const rawCwd = path.resolve(opts.cwd ?? process.cwd());
  const realCwd = safeRealpath(rawCwd);

  let best: { ws: Workspace; len: number } | null = null;
  for (const ws of workspaces) {
    const candidates = [ws.path, safeRealpath(ws.path)];
    for (const dir of candidates) {
      if (isInsideDir(realCwd, dir) || isInsideDir(rawCwd, dir)) {
        if (!best || dir.length > best.len) {
          best = { ws, len: dir.length };
        }
      }
    }
  }
  if (!best) {
    throw new WorkspaceError(`Current directory "${rawCwd}" is not inside any configured workspace. Pass --workspace <id> or add it to workspaces.json.`);
  }
  return best.ws;
}

export interface WorkspaceProvider {
  list(): Workspace[];
  get(id: string): Workspace | null;
}

/** Reads workspaces.json on demand, caching by mtime so edits take effect without restarting the server. */
export class WorkspaceRegistry implements WorkspaceProvider {
  private cachedMtimeMs = -1;
  private cached: Workspace[] = [];

  constructor(private configDir: string) {}

  list(): Workspace[] {
    const file = workspacesFile(this.configDir);
    let st: fs.Stats;
    try {
      st = fs.statSync(file);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
        this.cachedMtimeMs = -1;
        this.cached = [];
        return [];
      }
      throw err;
    }
    if (st.mtimeMs !== this.cachedMtimeMs) {
      this.cached = loadWorkspaces(this.configDir);
      this.cachedMtimeMs = st.mtimeMs;
    }
    return this.cached;
  }

  get(id: string): Workspace | null {
    return this.list().find((w) => w.id === id) ?? null;
  }
}

export class StaticWorkspaceProvider implements WorkspaceProvider {
  constructor(private workspaces: readonly Workspace[]) {}

  list(): Workspace[] {
    return [...this.workspaces];
  }

  get(id: string): Workspace | null {
    return this.workspaces.find((w) => w.id === id) ?? null;
  }
}
