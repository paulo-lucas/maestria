import { execFile } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defaultConfigDir, loadConfig } from './config.ts';

export class ServiceError extends Error {}

export const LAUNCHD_LABEL = 'dev.maestria.panel';
export const SYSTEMD_UNIT = 'maestria.service';
export const WINDOWS_TASK_NAME = 'MaestriaPanel';

export interface ServiceCmdResult {
  ok: boolean;
  code: number | null;
  stdout: string;
  stderr: string;
}

export type ServiceCommandRunner = (argv: readonly string[]) => Promise<ServiceCmdResult>;

export function defaultServiceRunner(argv: readonly string[]): Promise<ServiceCmdResult> {
  const [cmd, ...args] = argv;
  if (!cmd) return Promise.resolve({ ok: true, code: 0, stdout: '', stderr: '' });
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout: 15_000, maxBuffer: 128 * 1024, windowsHide: true }, (err, stdout, stderr) => {
      if (!err) {
        resolve({
          ok: true,
          code: 0,
          stdout: String(stdout ?? '').trim(),
          stderr: String(stderr ?? '').trim(),
        });
        return;
      }
      const code =
        typeof (err as NodeJS.ErrnoException).code === 'number'
          ? Number((err as NodeJS.ErrnoException).code)
          : null;
      resolve({
        ok: false,
        code,
        stdout: String(stdout ?? '').trim(),
        stderr: String(stderr || err.message).trim().slice(0, 500),
      });
    });
  });
}

export function resolvePackageRoot(): string {
  let dir = path.dirname(fileURLToPath(import.meta.url));
  for (let i = 0; i < 4; i++) {
    if (fs.existsSync(path.join(dir, 'package.json'))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
}

export function resolveCliEntrypoint(): string {
  return path.join(resolvePackageRoot(), 'dist', 'cli.js');
}

export function launchdPlistPath(homeDir: string = os.homedir()): string {
  return path.join(homeDir, 'Library', 'LaunchAgents', `${LAUNCHD_LABEL}.plist`);
}

export function launchdLogDir(homeDir: string = os.homedir()): string {
  return path.join(homeDir, 'Library', 'Logs', 'maestria');
}

export function systemdUnitPath(homeDir: string = os.homedir(), env: NodeJS.ProcessEnv = process.env): string {
  const xdg = env.XDG_CONFIG_HOME?.trim();
  const base = xdg ? path.resolve(xdg) : path.join(homeDir, '.config');
  return path.join(base, 'systemd', 'user', SYSTEMD_UNIT);
}

export function windowsServiceScriptPath(configDir: string): string {
  return path.join(configDir, 'maestria-service.cmd');
}

export function windowsLogDir(configDir: string): string {
  return path.join(configDir, 'logs');
}

function escapeXml(raw: string): string {
  return raw
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;');
}

function buildServiceEnv(opts: {
  nodePath: string;
  configDir: string;
  platform: SupportedPlatform;
  env?: NodeJS.ProcessEnv;
}): Record<string, string> {
  const src = opts.env ?? process.env;
  const p = opts.platform === 'win32' ? path.win32 : path.posix;
  const delim = opts.platform === 'win32' ? ';' : ':';
  const nodeDir = p.dirname(opts.nodePath);
  const rawPath = src.PATH || src.Path || '';
  const pathParts = [
    nodeDir,
    ...(rawPath ? rawPath.split(delim) : []),
    ...(opts.platform === 'win32' ? [] : ['/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin']),
  ].filter(Boolean);
  const uniquePath = Array.from(new Set(pathParts)).join(delim);

  const out: Record<string, string> = {
    PATH: uniquePath,
    MAESTRIA_CONFIG_DIR: opts.configDir,
  };
  if (src.MAESTRI_HOME?.trim()) {
    out.MAESTRI_HOME = src.MAESTRI_HOME.trim();
  }
  return out;
}

export function renderLaunchdPlist(opts: {
  nodePath: string;
  cliPath: string;
  configDir: string;
  homeDir?: string;
  env?: NodeJS.ProcessEnv;
}): string {
  const homeDir = opts.homeDir ?? os.homedir();
  const logDir = launchdLogDir(homeDir);
  const envVars = buildServiceEnv({ nodePath: opts.nodePath, configDir: opts.configDir, platform: 'darwin', env: opts.env });
  const envEntries = Object.entries(envVars)
    .map(([k, v]) => `    <key>${escapeXml(k)}</key>\n    <string>${escapeXml(v)}</string>`)
    .join('\n');

  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    '<dict>',
    '  <key>Label</key>',
    `  <string>${LAUNCHD_LABEL}</string>`,
    '  <key>ProgramArguments</key>',
    '  <array>',
    `    <string>${escapeXml(opts.nodePath)}</string>`,
    `    <string>${escapeXml(opts.cliPath)}</string>`,
    '    <string>serve</string>',
    '  </array>',
    '  <key>EnvironmentVariables</key>',
    '  <dict>',
    envEntries,
    '  </dict>',
    '  <key>RunAtLoad</key>',
    '  <true/>',
    '  <key>KeepAlive</key>',
    '  <true/>',
    '  <key>StandardOutPath</key>',
    `  <string>${escapeXml(path.join(logDir, 'panel.out.log'))}</string>`,
    '  <key>StandardErrorPath</key>',
    `  <string>${escapeXml(path.join(logDir, 'panel.err.log'))}</string>`,
    '</dict>',
    '</plist>',
    '',
  ].join('\n');
}

function escapeSystemdArg(raw: string): string {
  return `"${raw.replaceAll('\\', '\\\\').replaceAll('"', '\\"').replaceAll('%', '%%')}"`;
}

export function renderSystemdUnit(opts: {
  nodePath: string;
  cliPath: string;
  configDir: string;
  env?: NodeJS.ProcessEnv;
}): string {
  const envVars = buildServiceEnv({ nodePath: opts.nodePath, configDir: opts.configDir, platform: 'linux', env: opts.env });
  const envLines = Object.entries(envVars).map(([k, v]) => `Environment=${escapeSystemdArg(`${k}=${v}`)}`);

  return [
    '[Unit]',
    'Description=Maestria control panel',
    'After=network-online.target',
    '',
    '[Service]',
    'Type=simple',
    `ExecStart=${escapeSystemdArg(opts.nodePath)} ${escapeSystemdArg(opts.cliPath)} serve`,
    ...envLines,
    'Restart=always',
    'RestartSec=3',
    '',
    '[Install]',
    'WantedBy=default.target',
    '',
  ].join('\n');
}

function escapeCmdValue(raw: string): string {
  return raw.replaceAll('%', '%%').replaceAll('"', '');
}

export function renderWindowsServiceScript(opts: {
  nodePath: string;
  cliPath: string;
  configDir: string;
  logDir?: string;
  env?: NodeJS.ProcessEnv;
}): string {
  const logDir = opts.logDir ?? windowsLogDir(opts.configDir);
  const envVars = buildServiceEnv({ nodePath: opts.nodePath, configDir: opts.configDir, platform: 'win32', env: opts.env });
  const envLines = Object.entries(envVars).map(([k, v]) => `set "${k}=${escapeCmdValue(v)}"`);
  const logFile = path.win32.join(logDir, 'panel.log');

  return [
    '@echo off',
    'setlocal',
    `set "MAESTRIA_LOG_DIR=${escapeCmdValue(logDir)}"`,
    'if not exist "%MAESTRIA_LOG_DIR%" mkdir "%MAESTRIA_LOG_DIR%"',
    ...envLines,
    `"${escapeCmdValue(opts.nodePath)}" "${escapeCmdValue(opts.cliPath)}" serve >> "${escapeCmdValue(logFile)}" 2>&1`,
    '',
  ].join('\r\n');
}

export interface ServerReachability {
  reachable: boolean;
  url: string;
  statusCode?: number;
}

export type HttpGetFn = (
  options: { host: string; port: number; path: string; timeout: number },
  cb: (res: { statusCode?: number; resume: () => void }) => void,
) => {
  once: (event: string, listener: () => void) => unknown;
  destroy: () => unknown;
};

export function checkServerReachable(
  host: string,
  port: number,
  timeoutMs = 600,
  httpGet: HttpGetFn = http.get as unknown as HttpGetFn,
): Promise<ServerReachability> {
  const probeHost = host === '0.0.0.0' || host === '::' ? '127.0.0.1' : host;
  const url = `http://${probeHost}:${port}`;
  return new Promise((resolve) => {
    const req = httpGet({ host: probeHost, port, path: '/login', timeout: timeoutMs }, (res) => {
      res.resume();
      resolve({ reachable: true, url, statusCode: res.statusCode });
    });
    req.once('timeout', () => {
      req.destroy();
      resolve({ reachable: false, url });
    });
    req.once('error', () => {
      resolve({ reachable: false, url });
    });
  });
}

export type SupportedPlatform = 'darwin' | 'linux' | 'win32';

export function isServicePlatformSupported(platform: NodeJS.Platform = process.platform): platform is SupportedPlatform {
  return platform === 'darwin' || platform === 'linux' || platform === 'win32';
}

function assertSupportedPlatform(platform: NodeJS.Platform): SupportedPlatform {
  if (platform === 'darwin' || platform === 'linux' || platform === 'win32') return platform;
  throw new ServiceError(
    `Background service management is supported on macOS (launchd), Linux (systemd --user), and Windows (Task Scheduler), not "${platform}". Run "maestria serve" directly.`,
  );
}

export interface ServiceOpts {
  configDir?: string;
  homeDir?: string;
  env?: NodeJS.ProcessEnv;
  platform?: NodeJS.Platform;
  nodePath?: string;
  cliPath?: string;
  uid?: number;
  runner?: ServiceCommandRunner;
  reachabilityCheck?: (host: string, port: number) => Promise<ServerReachability>;
}

function resolveServiceContext(opts: ServiceOpts) {
  const platform = assertSupportedPlatform(opts.platform ?? process.platform);
  const env = opts.env ?? process.env;
  const homeDir = opts.homeDir ?? os.homedir();
  const configDir = opts.configDir ?? defaultConfigDir(env, platform, homeDir);
  const nodePath = opts.nodePath ?? process.execPath;
  const cliPath = opts.cliPath ?? resolveCliEntrypoint();
  const uid = opts.uid ?? process.getuid?.() ?? 501;
  const runner = opts.runner ?? defaultServiceRunner;
  return { platform, env, homeDir, configDir, nodePath, cliPath, uid, runner };
}

export function serviceUnitFile(opts: ServiceOpts = {}): string {
  const platform = assertSupportedPlatform(opts.platform ?? process.platform);
  const env = opts.env ?? process.env;
  const homeDir = opts.homeDir ?? os.homedir();
  if (platform === 'darwin') return launchdPlistPath(homeDir);
  if (platform === 'linux') return systemdUnitPath(homeDir, env);
  const configDir = opts.configDir ?? defaultConfigDir(env, platform, homeDir);
  return windowsServiceScriptPath(configDir);
}

export function isServiceInstalled(opts: ServiceOpts = {}): boolean {
  const platform = opts.platform ?? process.platform;
  if (!isServicePlatformSupported(platform)) return false;
  return fs.existsSync(serviceUnitFile(opts));
}

export interface ServiceStatusReport {
  platform: SupportedPlatform;
  manager: 'launchd' | 'systemd-user' | 'task-scheduler';
  unitPath: string;
  logDir: string | null;
  installed: boolean;
  loaded: boolean;
  running: boolean;
  server: ServerReachability;
}

export async function installService(opts: ServiceOpts = {}): Promise<{ unitPath: string; logDir: string | null }> {
  const ctx = resolveServiceContext(opts);
  if (!fs.existsSync(ctx.cliPath)) {
    throw new ServiceError(
      `Compiled CLI entrypoint not found at ${ctx.cliPath}. Run "pnpm build" first (or reinstall the maestria package).`,
    );
  }

  if (ctx.platform === 'darwin') {
    const plist = launchdPlistPath(ctx.homeDir);
    const logDir = launchdLogDir(ctx.homeDir);
    fs.mkdirSync(path.dirname(plist), { recursive: true });
    fs.mkdirSync(logDir, { recursive: true });
    fs.writeFileSync(
      plist,
      renderLaunchdPlist({
        nodePath: ctx.nodePath,
        cliPath: ctx.cliPath,
        configDir: ctx.configDir,
        homeDir: ctx.homeDir,
        env: ctx.env,
      }),
      { mode: 0o644 },
    );
    return { unitPath: plist, logDir };
  }

  if (ctx.platform === 'win32') {
    const script = windowsServiceScriptPath(ctx.configDir);
    const logDir = windowsLogDir(ctx.configDir);
    fs.mkdirSync(path.dirname(script), { recursive: true });
    fs.mkdirSync(logDir, { recursive: true });
    fs.writeFileSync(
      script,
      renderWindowsServiceScript({
        nodePath: ctx.nodePath,
        cliPath: ctx.cliPath,
        configDir: ctx.configDir,
        logDir,
        env: ctx.env,
      }),
      'utf8',
    );
    const create = await ctx.runner([
      'schtasks',
      '/Create',
      '/TN',
      WINDOWS_TASK_NAME,
      '/TR',
      `"${script}"`,
      '/SC',
      'ONLOGON',
      '/RL',
      'LIMITED',
      '/F',
    ]);
    if (!create.ok) {
      throw new ServiceError(`schtasks /Create failed: ${create.stderr || 'unknown error'}`);
    }
    return { unitPath: script, logDir };
  }

  const unit = systemdUnitPath(ctx.homeDir, ctx.env);
  fs.mkdirSync(path.dirname(unit), { recursive: true });
  fs.writeFileSync(
    unit,
    renderSystemdUnit({
      nodePath: ctx.nodePath,
      cliPath: ctx.cliPath,
      configDir: ctx.configDir,
      env: ctx.env,
    }),
    { mode: 0o644 },
  );
  const reload = await ctx.runner(['systemctl', '--user', 'daemon-reload']);
  if (!reload.ok) {
    throw new ServiceError(`systemctl --user daemon-reload failed: ${reload.stderr || 'unknown error'}`);
  }
  const enable = await ctx.runner(['systemctl', '--user', 'enable', SYSTEMD_UNIT]);
  if (!enable.ok) {
    throw new ServiceError(`systemctl --user enable ${SYSTEMD_UNIT} failed: ${enable.stderr || 'unknown error'}`);
  }
  return { unitPath: unit, logDir: null };
}

export async function startService(opts: ServiceOpts = {}): Promise<void> {
  const ctx = resolveServiceContext(opts);
  const unit = serviceUnitFile(ctx);
  if (!fs.existsSync(unit)) {
    throw new ServiceError('Service is not installed. Run "maestria service install" first.');
  }

  if (ctx.platform === 'darwin') {
    const domain = `gui/${ctx.uid}`;
    const target = `${domain}/${LAUNCHD_LABEL}`;
    const probe = await ctx.runner(['launchctl', 'print', target]);
    if (probe.ok) {
      const kick = await ctx.runner(['launchctl', 'kickstart', '-k', target]);
      if (!kick.ok) throw new ServiceError(`launchctl kickstart failed: ${kick.stderr || 'unknown error'}`);
      return;
    }
    const boot = await ctx.runner(['launchctl', 'bootstrap', domain, unit]);
    if (!boot.ok) {
      throw new ServiceError(`launchctl bootstrap failed: ${boot.stderr || 'unknown error'}`);
    }
    return;
  }

  if (ctx.platform === 'win32') {
    const run = await ctx.runner(['schtasks', '/Run', '/TN', WINDOWS_TASK_NAME]);
    if (!run.ok) throw new ServiceError(`schtasks /Run failed: ${run.stderr || 'unknown error'}`);
    return;
  }

  const res = await ctx.runner(['systemctl', '--user', 'start', SYSTEMD_UNIT]);
  if (!res.ok) throw new ServiceError(`systemctl --user start failed: ${res.stderr || 'unknown error'}`);
}

export async function stopService(opts: ServiceOpts = {}): Promise<{ stopped: boolean }> {
  const ctx = resolveServiceContext(opts);

  if (ctx.platform === 'darwin') {
    const target = `gui/${ctx.uid}/${LAUNCHD_LABEL}`;
    const probe = await ctx.runner(['launchctl', 'print', target]);
    if (!probe.ok) return { stopped: false };
    const bootout = await ctx.runner(['launchctl', 'bootout', target]);
    if (!bootout.ok) {
      // bootout can return non-zero if it unloaded between probe and bootout; verify state.
      const recheck = await ctx.runner(['launchctl', 'print', target]);
      if (recheck.ok) {
        throw new ServiceError(`launchctl bootout failed: ${bootout.stderr || 'unknown error'}`);
      }
    }
    return { stopped: true };
  }

  if (ctx.platform === 'win32') {
    const probe = await ctx.runner(['schtasks', '/Query', '/TN', WINDOWS_TASK_NAME, '/FO', 'LIST']);
    if (!probe.ok || !/Status:\s*(?:Running|Queued)/i.test(probe.stdout)) {
      return { stopped: false };
    }
    const end = await ctx.runner(['schtasks', '/End', '/TN', WINDOWS_TASK_NAME]);
    if (!end.ok) throw new ServiceError(`schtasks /End failed: ${end.stderr || 'unknown error'}`);
    return { stopped: true };
  }

  const unit = systemdUnitPath(ctx.homeDir, ctx.env);
  if (!fs.existsSync(unit)) return { stopped: false };
  const active = await ctx.runner(['systemctl', '--user', 'is-active', SYSTEMD_UNIT]);
  if (!active.ok) return { stopped: false };
  const res = await ctx.runner(['systemctl', '--user', 'stop', SYSTEMD_UNIT]);
  if (!res.ok) throw new ServiceError(`systemctl --user stop failed: ${res.stderr || 'unknown error'}`);
  return { stopped: true };
}

export async function restartService(opts: ServiceOpts = {}): Promise<void> {
  const ctx = resolveServiceContext(opts);
  const unit = serviceUnitFile(ctx);
  if (!fs.existsSync(unit)) {
    throw new ServiceError('Service is not installed. Run "maestria service install" first.');
  }

  if (ctx.platform === 'darwin') {
    const domain = `gui/${ctx.uid}`;
    const target = `${domain}/${LAUNCHD_LABEL}`;
    await ctx.runner(['launchctl', 'bootout', target]);
    const boot = await ctx.runner(['launchctl', 'bootstrap', domain, unit]);
    if (!boot.ok) {
      throw new ServiceError(`launchctl bootstrap failed: ${boot.stderr || 'unknown error'}`);
    }
    return;
  }

  if (ctx.platform === 'win32') {
    await ctx.runner(['schtasks', '/End', '/TN', WINDOWS_TASK_NAME]);
    const run = await ctx.runner(['schtasks', '/Run', '/TN', WINDOWS_TASK_NAME]);
    if (!run.ok) throw new ServiceError(`schtasks /Run failed: ${run.stderr || 'unknown error'}`);
    return;
  }

  const res = await ctx.runner(['systemctl', '--user', 'restart', SYSTEMD_UNIT]);
  if (!res.ok) throw new ServiceError(`systemctl --user restart failed: ${res.stderr || 'unknown error'}`);
}

export async function uninstallService(opts: ServiceOpts = {}): Promise<{ removed: boolean; unitPath: string }> {
  const ctx = resolveServiceContext(opts);
  const unit = serviceUnitFile(ctx);
  const existed = fs.existsSync(unit);

  if (ctx.platform === 'darwin') {
    const target = `gui/${ctx.uid}/${LAUNCHD_LABEL}`;
    await ctx.runner(['launchctl', 'bootout', target]);
    fs.rmSync(unit, { force: true });
    return { removed: existed, unitPath: unit };
  }

  if (ctx.platform === 'win32') {
    await ctx.runner(['schtasks', '/End', '/TN', WINDOWS_TASK_NAME]);
    const probe = await ctx.runner(['schtasks', '/Query', '/TN', WINDOWS_TASK_NAME, '/FO', 'LIST']);
    if (probe.ok) {
      const del = await ctx.runner(['schtasks', '/Delete', '/TN', WINDOWS_TASK_NAME, '/F']);
      if (!del.ok) throw new ServiceError(`schtasks /Delete failed: ${del.stderr || 'unknown error'}`);
    }
    fs.rmSync(unit, { force: true });
    return { removed: existed || probe.ok, unitPath: unit };
  }

  await ctx.runner(['systemctl', '--user', 'disable', '--now', SYSTEMD_UNIT]);
  fs.rmSync(unit, { force: true });
  await ctx.runner(['systemctl', '--user', 'daemon-reload']);
  return { removed: existed, unitPath: unit };
}

export async function getServiceStatus(opts: ServiceOpts = {}): Promise<ServiceStatusReport> {
  const ctx = resolveServiceContext(opts);
  const unit = serviceUnitFile(ctx);
  const installed = fs.existsSync(unit);
  const config = loadConfig(ctx.configDir, ctx.env);
  const check = opts.reachabilityCheck ?? checkServerReachable;
  const server = await check(config.host, config.port);

  if (ctx.platform === 'darwin') {
    const target = `gui/${ctx.uid}/${LAUNCHD_LABEL}`;
    const probe = await ctx.runner(['launchctl', 'print', target]);
    const loaded = probe.ok;
    const running = loaded && (/state\s*=\s*running/i.test(probe.stdout) || /pid\s*=\s*\d+/i.test(probe.stdout) || server.reachable);
    return {
      platform: 'darwin',
      manager: 'launchd',
      unitPath: unit,
      logDir: launchdLogDir(ctx.homeDir),
      installed,
      loaded,
      running,
      server,
    };
  }

  if (ctx.platform === 'win32') {
    const probe = await ctx.runner(['schtasks', '/Query', '/TN', WINDOWS_TASK_NAME, '/FO', 'LIST']);
    const loaded = probe.ok;
    const running = (loaded && /Status:\s*Running/i.test(probe.stdout)) || server.reachable;
    return {
      platform: 'win32',
      manager: 'task-scheduler',
      unitPath: unit,
      logDir: windowsLogDir(ctx.configDir),
      installed,
      loaded,
      running,
      server,
    };
  }

  const active = await ctx.runner(['systemctl', '--user', 'is-active', SYSTEMD_UNIT]);
  const running = active.ok && active.stdout.trim() === 'active';
  return {
    platform: 'linux',
    manager: 'systemd-user',
    unitPath: unit,
    logDir: null,
    installed,
    loaded: installed,
    running: running || server.reachable,
    server,
  };
}
