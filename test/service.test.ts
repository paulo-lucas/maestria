import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { runStatusCommand } from '../src/commands/inbox.ts';
import { runServiceCommand } from '../src/commands/service.ts';
import { saveRawConfig } from '../src/config.ts';
import {
  checkServerReachable,
  type HttpGetFn,
  LAUNCHD_LABEL,
  launchdLogDir,
  launchdPlistPath,
  renderLaunchdPlist,
  renderSystemdUnit,
  renderWindowsServiceScript,
  ServiceError,
  systemdUnitPath,
  WINDOWS_TASK_NAME,
  windowsServiceScriptPath,
} from '../src/service.ts';

function makeCaptureStream() {
  const s = new PassThrough();
  let text = '';
  s.on('data', (c) => (text += c.toString('utf8')));
  return { stream: s, read: () => text };
}

describe('maestria service unit generation, lifecycle, and server reachability', () => {
  it('renders macOS launchd plist with node, dist/cli.js serve, RunAtLoad, KeepAlive, logs in ~/Library/Logs/maestria, and XML escaping', () => {
    const plist = renderLaunchdPlist({
      nodePath: '/opt/homebrew/bin/node',
      cliPath: '/path/to/maestria & panel/dist/cli.js',
      configDir: '/tmp/cfg<1>',
      homeDir: '/Users/alice',
      env: { PATH: '/custom/bin:/usr/bin', MAESTRI_HOME: '/Users/alice/.maestri' },
    });

    expect(plist).toContain(`<string>${LAUNCHD_LABEL}</string>`);
    expect(plist).toContain('<string>/opt/homebrew/bin/node</string>');
    expect(plist).toContain('<string>/path/to/maestria &amp; panel/dist/cli.js</string>');
    expect(plist).toContain('<string>serve</string>');
    expect(plist).toContain('<key>RunAtLoad</key>\n  <true/>');
    expect(plist).toContain('<key>KeepAlive</key>\n  <true/>');
    expect(plist).toContain('<string>/Users/alice/Library/Logs/maestria/panel.out.log</string>');
    expect(plist).toContain('<string>/Users/alice/Library/Logs/maestria/panel.err.log</string>');
    expect(plist).toContain('<key>MAESTRIA_CONFIG_DIR</key>\n    <string>/tmp/cfg&lt;1&gt;</string>');
    expect(plist).toContain('<key>MAESTRI_HOME</key>\n    <string>/Users/alice/.maestri</string>');
  });

  it('renders Linux systemd --user unit cleanly', () => {
    const unit = renderSystemdUnit({
      nodePath: '/usr/bin/node',
      cliPath: '/opt/maestria/dist/cli.js',
      configDir: '/home/alice/.config/maestria',
      env: { PATH: '/usr/local/bin:/usr/bin' },
    });

    expect(unit).toContain('[Unit]');
    expect(unit).toContain('[Service]');
    expect(unit).toContain('ExecStart="/usr/bin/node" "/opt/maestria/dist/cli.js" serve');
    expect(unit).toContain('Environment="MAESTRIA_CONFIG_DIR=/home/alice/.config/maestria"');
    expect(unit).toContain('Restart=always');
    expect(unit).toContain('[Install]\nWantedBy=default.target');
  });

  it('manages launchd service install/start/restart/stop/status/uninstall on macOS via mocked launchctl', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-svc-mac-'));
    const homeDir = path.join(tmp, 'home');
    const configDir = path.join(tmp, 'cfg');
    const cliPath = path.join(tmp, 'dist', 'cli.js');
    fs.mkdirSync(path.dirname(cliPath), { recursive: true });
    fs.mkdirSync(configDir, { recursive: true });
    fs.writeFileSync(cliPath, '// cli\n');

    let loaded = false;
    const calls: string[][] = [];
    const runner = async (argv: readonly string[]) => {
      calls.push([...argv]);
      const sub = argv[1];
      if (sub === 'print') {
        return loaded
          ? { ok: true, code: 0, stdout: 'state = running\npid = 4242', stderr: '' }
          : { ok: false, code: 113, stdout: '', stderr: 'Could not find service' };
      }
      if (sub === 'bootstrap') {
        loaded = true;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (sub === 'bootout') {
        loaded = false;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (sub === 'kickstart') {
        loaded = true;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      return { ok: true, code: 0, stdout: '', stderr: '' };
    };

    const baseOpts = {
      configDir,
      homeDir,
      platform: 'darwin' as const,
      nodePath: '/usr/local/bin/node',
      cliPath,
      uid: 501,
      runner,
      reachabilityCheck: async (h: string, p: number) => ({ reachable: loaded, url: `http://${h}:${p}` }),
    };

    const out1 = makeCaptureStream();
    await runServiceCommand(['install'], { ...baseOpts, stdout: out1.stream });
    const expectedPlist = launchdPlistPath(homeDir);
    expect(fs.existsSync(expectedPlist)).toBe(true);
    expect(fs.existsSync(launchdLogDir(homeDir))).toBe(true);
    expect(fs.readFileSync(expectedPlist, 'utf8')).toContain(cliPath);
    expect(out1.read()).toContain(expectedPlist);

    const outStart = makeCaptureStream();
    await runServiceCommand(['start'], { ...baseOpts, stdout: outStart.stream });
    expect(calls.some((c) => c[1] === 'bootstrap' && c[2] === 'gui/501' && c[3] === expectedPlist)).toBe(true);

    const outStatus = makeCaptureStream();
    await runServiceCommand(['status', '--json'], { ...baseOpts, stdout: outStatus.stream });
    const statusJson = JSON.parse(outStatus.read()) as { installed: boolean; running: boolean; server: { reachable: boolean } };
    expect(statusJson).toMatchObject({ installed: true, running: true, server: { reachable: true } });

    await runServiceCommand(['restart'], { ...baseOpts, stdout: makeCaptureStream().stream });
    expect(calls.slice(-2).map((c) => c[1])).toEqual(['bootout', 'bootstrap']);

    await runServiceCommand(['stop'], { ...baseOpts, stdout: makeCaptureStream().stream });
    expect(loaded).toBe(false);

    await runServiceCommand(['uninstall'], { ...baseOpts, stdout: makeCaptureStream().stream });
    expect(fs.existsSync(expectedPlist)).toBe(false);
  });

  it('manages systemd --user lifecycle on Linux and rejects unsupported platforms cleanly', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-svc-linux-'));
    const homeDir = path.join(tmp, 'home');
    const configDir = path.join(tmp, 'cfg');
    const cliPath = path.join(tmp, 'dist', 'cli.js');
    const xdg = path.join(tmp, 'xdg');
    fs.mkdirSync(path.dirname(cliPath), { recursive: true });
    fs.mkdirSync(configDir, { recursive: true });
    fs.writeFileSync(cliPath, '// cli\n');

    let active = false;
    const calls: string[][] = [];
    const runner = async (argv: readonly string[]) => {
      calls.push([...argv]);
      if (argv.includes('is-active')) {
        return active
          ? { ok: true, code: 0, stdout: 'active', stderr: '' }
          : { ok: false, code: 3, stdout: 'inactive', stderr: '' };
      }
      if (argv.includes('start') || argv.includes('restart')) active = true;
      if (argv.includes('stop') || argv.includes('--now')) active = false;
      return { ok: true, code: 0, stdout: '', stderr: '' };
    };

    const opts = {
      configDir,
      homeDir,
      env: { XDG_CONFIG_HOME: xdg },
      platform: 'linux' as const,
      nodePath: '/usr/bin/node',
      cliPath,
      runner,
      reachabilityCheck: async (h: string, p: number) => ({ reachable: active, url: `http://${h}:${p}` }),
      stdout: makeCaptureStream().stream,
    };

    await runServiceCommand(['install'], opts);
    const unitFile = systemdUnitPath(homeDir, { XDG_CONFIG_HOME: xdg });
    expect(fs.existsSync(unitFile)).toBe(true);
    expect(calls).toContainEqual(['systemctl', '--user', 'daemon-reload']);
    expect(calls).toContainEqual(['systemctl', '--user', 'enable', 'maestria.service']);

    await runServiceCommand(['start'], opts);
    await runServiceCommand(['restart'], opts);
    const statusOut = makeCaptureStream();
    await runServiceCommand(['status'], { ...opts, stdout: statusOut.stream });
    expect(statusOut.read()).toContain('State: running');

    await runServiceCommand(['uninstall'], opts);
    expect(fs.existsSync(unitFile)).toBe(false);

    // Unsupported platform throws crisp ServiceError
    await expect(
      runServiceCommand(['install'], {
        configDir,
        homeDir,
        platform: 'freebsd',
        cliPath,
        stdout: makeCaptureStream().stream,
      }),
    ).rejects.toThrow(ServiceError);
  });

  it('renders and manages Windows Task Scheduler script and lifecycle via mocked schtasks', async () => {
    const scriptBody = renderWindowsServiceScript({
      nodePath: 'C:\\Program Files\\nodejs\\node.exe',
      cliPath: 'C:\\tools\\maestria\\dist\\cli.js',
      configDir: 'C:\\Users\\alice\\AppData\\Roaming\\maestria',
      env: { PATH: 'C:\\Windows\\System32', MAESTRI_HOME: 'C:\\Users\\alice\\.maestri' },
    });
    expect(scriptBody).toContain('set "MAESTRIA_CONFIG_DIR=C:\\Users\\alice\\AppData\\Roaming\\maestria"');
    expect(scriptBody).toContain('set "MAESTRI_HOME=C:\\Users\\alice\\.maestri"');
    expect(scriptBody).toContain('"C:\\Program Files\\nodejs\\node.exe" "C:\\tools\\maestria\\dist\\cli.js" serve');
    expect(scriptBody).toContain('C:\\Users\\alice\\AppData\\Roaming\\maestria\\logs\\panel.log');

    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-svc-win-'));
    const configDir = path.join(tmp, 'cfg');
    const cliPath = path.join(tmp, 'cli.js');
    fs.writeFileSync(cliPath, '// cli\n');

    const calls: string[][] = [];
    let taskExists = false;
    let taskRunning = false;
    const runner = async (argv: readonly string[]) => {
      calls.push([...argv]);
      const action = argv[1];
      if (action === '/Create') {
        taskExists = true;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (action === '/Run') {
        taskRunning = true;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (action === '/End') {
        taskRunning = false;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (action === '/Delete') {
        taskExists = false;
        taskRunning = false;
        return { ok: true, code: 0, stdout: '', stderr: '' };
      }
      if (action === '/Query') {
        if (!taskExists) return { ok: false, code: 1, stdout: '', stderr: 'ERROR: cannot find the file' };
        return {
          ok: true,
          code: 0,
          stdout: `TaskName: \\${WINDOWS_TASK_NAME}\nStatus: ${taskRunning ? 'Running' : 'Ready'}\n`,
          stderr: '',
        };
      }
      return { ok: false, code: 1, stdout: '', stderr: 'unexpected' };
    };

    const opts = {
      configDir,
      platform: 'win32' as const,
      nodePath: 'C:\\Program Files\\nodejs\\node.exe',
      cliPath,
      runner,
      reachabilityCheck: async (h: string, p: number) => ({ reachable: taskRunning, url: `http://${h}:${p}` }),
      stdout: makeCaptureStream().stream,
    };

    await runServiceCommand(['install'], opts);
    const scriptFile = windowsServiceScriptPath(configDir);
    expect(fs.existsSync(scriptFile)).toBe(true);
    expect(calls[0]).toContain('/Create');
    expect(calls[0]).toContain(WINDOWS_TASK_NAME);

    await runServiceCommand(['start'], opts);
    const statusOut = makeCaptureStream();
    await runServiceCommand(['status'], { ...opts, stdout: statusOut.stream });
    expect(statusOut.read()).toContain('Service (task-scheduler): installed');
    expect(statusOut.read()).toContain('State: running');

    await runServiceCommand(['stop'], opts);
    expect(taskRunning).toBe(false);

    await runServiceCommand(['restart'], opts);
    expect(taskRunning).toBe(true);

    await runServiceCommand(['uninstall'], opts);
    expect(fs.existsSync(scriptFile)).toBe(false);
    expect(taskExists).toBe(false);
  });

  it('checkServerReachable and maestria status report live vs down panel server accurately', async () => {
    let seenHost = '';
    let seenPath = '';
    let resumed = false;
    const okGet: HttpGetFn = (opts, cb) => {
      seenHost = opts.host;
      seenPath = opts.path;
      const req = new EventEmitter() as EventEmitter & { destroy: () => void };
      req.destroy = () => {};
      queueMicrotask(() => cb({ statusCode: 200, resume: () => { resumed = true; } }));
      return req;
    };

    const up = await checkServerReachable('0.0.0.0', 7788, 500, okGet);
    expect(seenHost).toBe('127.0.0.1');
    expect(seenPath).toBe('/login');
    expect(resumed).toBe(true);
    expect(up).toEqual({ reachable: true, url: 'http://127.0.0.1:7788', statusCode: 200 });

    const errGet: HttpGetFn = () => {
      const req = new EventEmitter() as EventEmitter & { destroy: () => void };
      req.destroy = () => {};
      queueMicrotask(() => req.emit('error', new Error('ECONNREFUSED')));
      return req;
    };
    const down = await checkServerReachable('127.0.0.1', 7788, 300, errGet);
    expect(down).toEqual({ reachable: false, url: 'http://127.0.0.1:7788' });

    let destroyed = false;
    const timeoutGet: HttpGetFn = () => {
      const req = new EventEmitter() as EventEmitter & { destroy: () => void };
      req.destroy = () => { destroyed = true; };
      queueMicrotask(() => req.emit('timeout'));
      return req;
    };
    const timedOut = await checkServerReachable('127.0.0.1', 7788, 100, timeoutGet);
    expect(destroyed).toBe(true);
    expect(timedOut).toEqual({ reachable: false, url: 'http://127.0.0.1:7788' });

    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-status-live-'));
    saveRawConfig(tmp, { port: 7788 });
    const upLines: string[] = [];
    await runStatusCommand([], {
      configDir: tmp,
      out: (line) => upLines.push(line),
      reachabilityCheck: async (host, port) => ({ reachable: true, url: `http://${host}:${port}`, statusCode: 200 }),
    });
    expect(upLines.join('\n')).toContain('Panel server: running (http://127.0.0.1:7788)');

    const downLines: string[] = [];
    await runStatusCommand([], {
      configDir: tmp,
      out: (line) => downLines.push(line),
      reachabilityCheck: async (host, port) => ({ reachable: false, url: `http://${host}:${port}` }),
    });
    expect(downLines.join('\n')).toContain(
      'Panel server: down (http://127.0.0.1:7788 not reachable — start with: maestria service start)',
    );
  });
});
