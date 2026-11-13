import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { ensurePrivateDir, writePrivateFileAtomic } from './permissions.ts';

export interface Config {
  /** Directory holding config.json, auth.json and the SQLite database. */
  configDir: string;
  /** Resolved Maestri data dir (`config.maestriDir` > `MAESTRI_HOME` > `~/.maestri`). */
  maestriDir: string;
  /** The panel only ever listens on loopback. Not configurable on purpose. */
  host: '127.0.0.1';
  port: number;
  /** Set the Secure flag on cookies (and use the __Host- prefix). Disable only for plain-http local dev. */
  cookieSecure: boolean;
  /** Extra Host header values accepted besides localhost / 127.0.0.1 (e.g. the public tunnel hostname). */
  allowedHosts: string[];
  /** Trust the CF-Connecting-IP header for the client address (only safe behind Cloudflare). */
  trustCfConnectingIp: boolean;
  session: {
    idleMinutes: number;
    absoluteHours: number;
  };
  login: {
    maxFailuresPerIp: number;
    maxFailuresGlobal: number;
    windowSeconds: number;
    lockoutSeconds: number;
  };
  cloudflareAccess: { teamDomain: string; audience: string } | null;
  usage: {
    /** Consider a provider reading stale if lastSuccessAt is older than this many minutes. */
    staleMinutes: number;
    /** Run `<id>.json` provider fallback locally when `.status.json` is stale. */
    fallbackEnabled: boolean;
    /** Server-side background poll interval for the standby guard (seconds). */
    pollSeconds: number;
  };
  standby: {
    /** Per-provider thresholds: `{ [providerId]: { [windowIdOrRingOrStar]: percent (1..100) } }`. */
    thresholds: Record<string, Record<string, number>>;
    /** Optional global hysteresis threshold to clear standby once all triggering windows drop below it. */
    resumeBelowPercent: number | null;
    /** Best-effort local `maestri notify` alert in addition to event stream entries. */
    notifyViaCli: boolean;
  };
  tunnel: {
    /** Argv (no shell) run by the kill switch, e.g. ["pkill", "-x", "cloudflared"]. */
    stopCommand: string[];
    /** Argv (no shell) run locally by `maestria tunnel on`. */
    startCommand: string[];
    /** Optional argv (no shell) used by status checks (exit 0 = running). */
    statusCommand: string[];
  };
}

function pathLibFor(platform: NodeJS.Platform): typeof path.posix | typeof path.win32 {
  return platform === 'win32' ? path.win32 : path.posix;
}

export function defaultConfigDir(
  env: NodeJS.ProcessEnv = process.env,
  platform: NodeJS.Platform = process.platform,
  homeDir: string = os.homedir(),
): string {
  const explicit = env.MAESTRIA_CONFIG_DIR?.trim();
  if (explicit) return explicit;
  const p = pathLibFor(platform);
  if (platform === 'win32') {
    const appData = env.APPDATA?.trim() || p.join(homeDir, 'AppData', 'Roaming');
    return p.join(appData, 'maestria');
  }
  return p.join(homeDir, '.config', 'maestria');
}

export function expandHome(
  rawPath: string,
  homeDir: string = os.homedir(),
  platform: NodeJS.Platform = process.platform,
): string {
  if (rawPath === '~') return homeDir;
  if (rawPath.startsWith('~/') || rawPath.startsWith('~\\')) {
    return pathLibFor(platform).join(homeDir, rawPath.slice(2));
  }
  return rawPath;
}

/**
 * Resolve Maestri's data directory cleanly:
 *   1. `maestriDir` from `config.json` (if provided)
 *   2. `MAESTRI_HOME` environment variable (if non-empty)
 *   3. Default location: `~/.maestri` (or `%APPDATA%\Maestri` on Windows if `~/.maestri` is absent and it has `usage/providers`)
 */
export function resolveMaestriDir(
  configured?: string,
  env: NodeJS.ProcessEnv = process.env,
  homeDir: string = os.homedir(),
  platform: NodeJS.Platform = process.platform,
): string {
  const p = pathLibFor(platform);
  const defaultHomeDot = p.join(homeDir, '.maestri');
  let pick = configured?.trim() || env.MAESTRI_HOME?.trim() || '';
  if (!pick) {
    if (platform === 'win32' && !isDir(defaultHomeDot)) {
      const appData = env.APPDATA?.trim() || p.join(homeDir, 'AppData', 'Roaming');
      const winCandidate = p.join(appData, 'Maestri');
      if (isDir(p.join(winCandidate, 'usage', 'providers')) || isDir(winCandidate)) {
        pick = winCandidate;
      }
    }
    if (!pick) pick = defaultHomeDot;
  }
  const expanded = expandHome(pick, homeDir, platform);
  if (!p.isAbsolute(expanded)) {
    throw new ConfigError(`config: "maestriDir" must resolve to an absolute path (got "${pick}")`);
  }
  return p.resolve(expanded);
}

export interface MaestriDirStatus {
  dir: string;
  providersDir: string;
  statusFile: string;
  hasDir: boolean;
  hasProvidersDir: boolean;
  hasStatusFile: boolean;
  valid: boolean;
}

export function inspectMaestriDir(maestriDir: string): MaestriDirStatus {
  const providersDir = path.join(maestriDir, 'usage', 'providers');
  const statusFile = path.join(providersDir, '.status.json');
  const hasDir = isDir(maestriDir);
  const hasProvidersDir = hasDir && isDir(providersDir);
  const hasStatusFile = hasProvidersDir && isFile(statusFile);
  return {
    dir: maestriDir,
    providersDir,
    statusFile,
    hasDir,
    hasProvidersDir,
    hasStatusFile,
    valid: hasProvidersDir,
  };
}

function isDir(p: string): boolean {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
}

function isFile(p: string): boolean {
  try {
    return fs.statSync(p).isFile();
  } catch {
    return false;
  }
}

export const DEFAULT_STANDBY_THRESHOLDS: Record<string, Record<string, number>> = {
  claude: { session: 95, weekly: 90 },
};

export const DEFAULT_RESUME_BELOW_PERCENT = 80;

export function cloneStandbyThresholds(
  thresholds: Record<string, Record<string, number>>,
): Record<string, Record<string, number>> {
  const out: Record<string, Record<string, number>> = {};
  for (const [k, v] of Object.entries(thresholds)) {
    out[k] = { ...v };
  }
  return out;
}

export function defaultConfig(
  configDir: string,
  env: NodeJS.ProcessEnv = process.env,
  homeDir: string = os.homedir(),
): Config {
  return {
    configDir,
    maestriDir: resolveMaestriDir(undefined, env, homeDir),
    host: '127.0.0.1',
    port: 4777,
    cookieSecure: true,
    allowedHosts: [],
    trustCfConnectingIp: false,
    session: { idleMinutes: 60, absoluteHours: 12 },
    login: { maxFailuresPerIp: 5, maxFailuresGlobal: 20, windowSeconds: 900, lockoutSeconds: 900 },
    cloudflareAccess: null,
    usage: { staleMinutes: 15, fallbackEnabled: true, pollSeconds: 30 },
    standby: {
      thresholds: cloneStandbyThresholds(DEFAULT_STANDBY_THRESHOLDS),
      resumeBelowPercent: DEFAULT_RESUME_BELOW_PERCENT,
      notifyViaCli: false,
    },
    tunnel: { stopCommand: [], startCommand: [], statusCommand: [] },
  };
}

export class ConfigError extends Error {}

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function int(v: unknown, name: string, min: number, max: number): number {
  if (typeof v !== 'number' || !Number.isInteger(v) || v < min || v > max) {
    throw new ConfigError(`config: "${name}" must be an integer between ${min} and ${max}`);
  }
  return v;
}

function bool(v: unknown, name: string): boolean {
  if (typeof v !== 'boolean') throw new ConfigError(`config: "${name}" must be a boolean`);
  return v;
}

function cmdArgv(v: unknown, name: string): string[] {
  if (!Array.isArray(v) || v.length > 16) {
    throw new ConfigError(`config: "${name}" must be an array of at most 16 command arguments`);
  }
  return v.map((entry, idx) => {
    if (typeof entry !== 'string' || !entry.length || entry.length > 512) {
      throw new ConfigError(`config: "${name}[${idx}]" must be a non-empty string`);
    }
    return entry;
  });
}

const HOST_RE = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:\d{1,5})?$/i;
const ID_KEY_RE = /^[a-z0-9*][a-z0-9._:-]{0,63}$/i;

/** Merge a parsed config.json over the defaults, validating every field. Unknown keys are rejected. */
export function parseConfig(
  raw: unknown,
  configDir: string,
  env: NodeJS.ProcessEnv = process.env,
  homeDir: string = os.homedir(),
): Config {
  const cfg = defaultConfig(configDir, env, homeDir);
  if (raw === undefined) return cfg;
  if (!isObj(raw)) throw new ConfigError('config: top level must be an object');
  const known = new Set([
    'maestriDir',
    'port',
    'cookieSecure',
    'allowedHosts',
    'trustCfConnectingIp',
    'session',
    'login',
    'cloudflareAccess',
    'usage',
    'standby',
    'tunnel',
  ]);
  for (const k of Object.keys(raw)) {
    if (!known.has(k)) throw new ConfigError(`config: unknown key "${k}" (note: the listen address is fixed to 127.0.0.1)`);
  }
  if (raw.maestriDir !== undefined && raw.maestriDir !== null) {
    if (typeof raw.maestriDir !== 'string' || !raw.maestriDir.trim()) {
      throw new ConfigError('config: "maestriDir" must be a non-empty path string');
    }
    cfg.maestriDir = resolveMaestriDir(raw.maestriDir, env, homeDir);
  }
  if (raw.port !== undefined) cfg.port = int(raw.port, 'port', 1, 65535);
  if (raw.cookieSecure !== undefined) cfg.cookieSecure = bool(raw.cookieSecure, 'cookieSecure');
  if (raw.trustCfConnectingIp !== undefined) cfg.trustCfConnectingIp = bool(raw.trustCfConnectingIp, 'trustCfConnectingIp');
  if (raw.allowedHosts !== undefined) {
    if (!Array.isArray(raw.allowedHosts) || raw.allowedHosts.length > 20) {
      throw new ConfigError('config: "allowedHosts" must be an array of at most 20 host names');
    }
    cfg.allowedHosts = raw.allowedHosts.map((h) => {
      if (typeof h !== 'string' || !HOST_RE.test(h)) throw new ConfigError('config: invalid allowedHosts entry');
      return h.toLowerCase();
    });
  }
  if (raw.session !== undefined) {
    if (!isObj(raw.session)) throw new ConfigError('config: "session" must be an object');
    if (raw.session.idleMinutes !== undefined) cfg.session.idleMinutes = int(raw.session.idleMinutes, 'session.idleMinutes', 1, 24 * 60);
    if (raw.session.absoluteHours !== undefined) cfg.session.absoluteHours = int(raw.session.absoluteHours, 'session.absoluteHours', 1, 24 * 30);
  }
  if (raw.login !== undefined) {
    if (!isObj(raw.login)) throw new ConfigError('config: "login" must be an object');
    const l = raw.login;
    if (l.maxFailuresPerIp !== undefined) cfg.login.maxFailuresPerIp = int(l.maxFailuresPerIp, 'login.maxFailuresPerIp', 1, 100);
    if (l.maxFailuresGlobal !== undefined) cfg.login.maxFailuresGlobal = int(l.maxFailuresGlobal, 'login.maxFailuresGlobal', 1, 1000);
    if (l.windowSeconds !== undefined) cfg.login.windowSeconds = int(l.windowSeconds, 'login.windowSeconds', 10, 86400);
    if (l.lockoutSeconds !== undefined) cfg.login.lockoutSeconds = int(l.lockoutSeconds, 'login.lockoutSeconds', 10, 86400);
  }
  if (raw.cloudflareAccess !== undefined && raw.cloudflareAccess !== null) {
    const c = raw.cloudflareAccess;
    if (!isObj(c) || typeof c.teamDomain !== 'string' || typeof c.audience !== 'string') {
      throw new ConfigError('config: "cloudflareAccess" needs string "teamDomain" and "audience"');
    }
    if (!/^[a-z0-9-]+(\.[a-z0-9-]+)+$/i.test(c.teamDomain)) {
      throw new ConfigError('config: "cloudflareAccess.teamDomain" must be a bare host name, e.g. myteam.cloudflareaccess.com');
    }
    if (!/^[a-f0-9]{16,128}$/i.test(c.audience)) {
      throw new ConfigError('config: "cloudflareAccess.audience" must be the application AUD tag (hex)');
    }
    cfg.cloudflareAccess = { teamDomain: c.teamDomain.toLowerCase(), audience: c.audience };
  }
  if (raw.usage !== undefined) {
    if (!isObj(raw.usage)) throw new ConfigError('config: "usage" must be an object');
    const u = raw.usage;
    if (u.staleMinutes !== undefined) cfg.usage.staleMinutes = int(u.staleMinutes, 'usage.staleMinutes', 1, 1440);
    if (u.fallbackEnabled !== undefined) cfg.usage.fallbackEnabled = bool(u.fallbackEnabled, 'usage.fallbackEnabled');
    if (u.pollSeconds !== undefined) cfg.usage.pollSeconds = int(u.pollSeconds, 'usage.pollSeconds', 5, 3600);
  }
  if (raw.standby !== undefined) {
    if (!isObj(raw.standby)) throw new ConfigError('config: "standby" must be an object');
    const s = raw.standby;
    if (s.notifyViaCli !== undefined) cfg.standby.notifyViaCli = bool(s.notifyViaCli, 'standby.notifyViaCli');
    if (s.resumeBelowPercent !== undefined) {
      cfg.standby.resumeBelowPercent =
        s.resumeBelowPercent === null ? null : int(s.resumeBelowPercent, 'standby.resumeBelowPercent', 0, 99);
    }
    if (s.thresholds !== undefined) {
      if (!isObj(s.thresholds)) throw new ConfigError('config: "standby.thresholds" must be an object');
      const map: Record<string, Record<string, number>> = {};
      for (const [providerId, windows] of Object.entries(s.thresholds)) {
        if (!ID_KEY_RE.test(providerId) || !isObj(windows)) {
          throw new ConfigError(`config: invalid "standby.thresholds.${providerId}"`);
        }
        const winMap: Record<string, number> = {};
        for (const [winKey, pct] of Object.entries(windows)) {
          if (!ID_KEY_RE.test(winKey)) {
            throw new ConfigError(`config: invalid standby window key "${winKey}"`);
          }
          winMap[winKey] = int(pct, `standby.thresholds.${providerId}.${winKey}`, 1, 100);
        }
        map[providerId] = winMap;
      }
      cfg.standby.thresholds = map;
    }
  }
  if (raw.tunnel !== undefined) {
    if (!isObj(raw.tunnel)) throw new ConfigError('config: "tunnel" must be an object');
    const t = raw.tunnel;
    if (t.stopCommand !== undefined) cfg.tunnel.stopCommand = cmdArgv(t.stopCommand, 'tunnel.stopCommand');
    if (t.startCommand !== undefined) cfg.tunnel.startCommand = cmdArgv(t.startCommand, 'tunnel.startCommand');
    if (t.statusCommand !== undefined) cfg.tunnel.statusCommand = cmdArgv(t.statusCommand, 'tunnel.statusCommand');
  }
  return cfg;
}

export function ensureConfigDir(configDir: string, platform: NodeJS.Platform = process.platform): void {
  ensurePrivateDir(configDir, platform);
}

export function configFilePath(configDir: string): string {
  return path.join(configDir, 'config.json');
}

export function loadRawConfig(configDir: string = defaultConfigDir()): Record<string, unknown> {
  const file = configFilePath(configDir);
  try {
    const parsed = JSON.parse(fs.readFileSync(file, 'utf8'));
    if (!isObj(parsed)) throw new ConfigError(`config: ${file} must contain a JSON object`);
    return parsed;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return {};
    if (err instanceof ConfigError) throw err;
    throw new ConfigError(`config: cannot read ${file}: ${(err as Error).message}`);
  }
}

export function saveRawConfig(
  configDir: string,
  raw: Record<string, unknown>,
  env: NodeJS.ProcessEnv = process.env,
  platform: NodeJS.Platform = process.platform,
): void {
  parseConfig(raw, configDir, env);
  ensureConfigDir(configDir, platform);
  writePrivateFileAtomic(configFilePath(configDir), JSON.stringify(raw, null, 2) + '\n', platform);
}

export function loadConfig(configDir: string = defaultConfigDir(), env: NodeJS.ProcessEnv = process.env): Config {
  const file = configFilePath(configDir);
  let raw: unknown;
  try {
    raw = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return defaultConfig(configDir, env);
    throw new ConfigError(`config: cannot read ${file}: ${(err as Error).message}`);
  }
  return parseConfig(raw, configDir, env);
}
