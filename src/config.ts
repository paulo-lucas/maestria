import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export interface Config {
  /** Directory holding config.json, auth.json and the SQLite database. */
  configDir: string;
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
}

export function defaultConfigDir(env: NodeJS.ProcessEnv = process.env): string {
  return env.MAESTRIA_CONFIG_DIR || path.join(os.homedir(), '.config', 'maestria');
}

export function defaultConfig(configDir: string): Config {
  return {
    configDir,
    host: '127.0.0.1',
    port: 4777,
    cookieSecure: true,
    allowedHosts: [],
    trustCfConnectingIp: false,
    session: { idleMinutes: 60, absoluteHours: 12 },
    login: { maxFailuresPerIp: 5, maxFailuresGlobal: 20, windowSeconds: 900, lockoutSeconds: 900 },
    cloudflareAccess: null,
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

const HOST_RE = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:\d{1,5})?$/i;

/** Merge a parsed config.json over the defaults, validating every field. Unknown keys are rejected. */
export function parseConfig(raw: unknown, configDir: string): Config {
  const cfg = defaultConfig(configDir);
  if (raw === undefined) return cfg;
  if (!isObj(raw)) throw new ConfigError('config: top level must be an object');
  const known = new Set(['port', 'cookieSecure', 'allowedHosts', 'trustCfConnectingIp', 'session', 'login', 'cloudflareAccess']);
  for (const k of Object.keys(raw)) {
    if (!known.has(k)) throw new ConfigError(`config: unknown key "${k}" (note: the listen address is fixed to 127.0.0.1)`);
  }
  if (raw.port !== undefined) cfg.port = int(raw.port, 'port', 1, 65535);
  if (raw.cookieSecure !== undefined) cfg.cookieSecure = bool(raw.cookieSecure, 'cookieSecure');
  if (raw.trustCfConnectingIp !== undefined) cfg.trustCfConnectingIp = bool(raw.trustCfConnectingIp, 'trustCfConnectingIp');
  if (raw.allowedHosts !== undefined) {
    if (!Array.isArray(raw.allowedHosts) || raw.allowedHosts.length > 20) {
      throw new ConfigError('config: "allowedHosts" must be an array of at most 20 host names');
    }
    cfg.allowedHosts = raw.allowedHosts.map((h) => {
      if (typeof h !== 'string' || !HOST_RE.test(h)) throw new ConfigError(`config: invalid allowedHosts entry`);
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
  return cfg;
}

export function ensureConfigDir(configDir: string): void {
  fs.mkdirSync(configDir, { recursive: true, mode: 0o700 });
  fs.chmodSync(configDir, 0o700);
}

export function loadConfig(configDir: string = defaultConfigDir()): Config {
  const file = path.join(configDir, 'config.json');
  let raw: unknown;
  try {
    raw = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return defaultConfig(configDir);
    throw new ConfigError(`config: cannot read ${file}: ${(err as Error).message}`);
  }
  return parseConfig(raw, configDir);
}
