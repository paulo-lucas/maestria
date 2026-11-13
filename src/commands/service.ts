import { loadSecrets } from '../auth/secrets.ts';
import {
  getServiceStatus,
  installService,
  restartService,
  startService,
  stopService,
  uninstallService,
  type ServiceOpts,
} from '../service.ts';
import { ArgError, parseArgs } from './args.ts';

export interface ServiceCliOpts extends ServiceOpts {
  configDir: string;
  stdout?: NodeJS.WritableStream;
}

const VALID_SUBCOMMANDS = new Set(['install', 'uninstall', 'start', 'stop', 'restart', 'status']);

export async function runServiceCommand(args: string[], opts: ServiceCliOpts): Promise<void> {
  const [sub, ...rest] = args;
  const { flags, positional } = parseArgs(rest, { json: { type: 'boolean' } });
  if (!sub || !VALID_SUBCOMMANDS.has(sub) || positional.length > 0) {
    throw new ArgError('Usage: maestria service install|uninstall|start|stop|restart|status [--json]');
  }

  const out = opts.stdout ?? process.stdout;
  const json = flags.json === true;

  if (sub === 'install') {
    const res = await installService(opts);
    if (json) {
      out.write(JSON.stringify({ ok: true, action: 'install', ...res }, null, 2) + '\n');
      return;
    }
    out.write(`Installed Maestria service: ${res.unitPath}\n`);
    if (res.logDir) out.write(`Logs: ${res.logDir}\n`);
    if (!loadSecrets(opts.configDir)) {
      out.write('Note: credentials are not configured yet. Run "maestria auth setup" before starting.\n');
    }
    out.write('Start now with: maestria service start\n');
    return;
  }

  if (sub === 'uninstall') {
    const res = await uninstallService(opts);
    if (json) {
      out.write(JSON.stringify({ ok: true, action: 'uninstall', ...res }, null, 2) + '\n');
      return;
    }
    out.write(res.removed ? `Removed service: ${res.unitPath}\n` : `Service was not installed (${res.unitPath}).\n`);
    return;
  }

  if (sub === 'start') {
    await startService(opts);
    if (json) {
      out.write(JSON.stringify({ ok: true, action: 'start' }, null, 2) + '\n');
      return;
    }
    out.write('Started Maestria service.\n');
    return;
  }

  if (sub === 'stop') {
    const res = await stopService(opts);
    if (json) {
      out.write(JSON.stringify({ ok: true, action: 'stop', ...res }, null, 2) + '\n');
      return;
    }
    out.write(res.stopped ? 'Stopped Maestria service.\n' : 'Maestria service is not running.\n');
    return;
  }

  if (sub === 'restart') {
    await restartService(opts);
    if (json) {
      out.write(JSON.stringify({ ok: true, action: 'restart' }, null, 2) + '\n');
      return;
    }
    out.write('Restarted Maestria service.\n');
    return;
  }

  const report = await getServiceStatus(opts);
  if (json) {
    out.write(JSON.stringify(report, null, 2) + '\n');
    return;
  }
  out.write(`Service (${report.manager}): ${report.installed ? `installed (${report.unitPath})` : 'not installed'}\n`);
  out.write(`State: ${report.running ? 'running' : report.loaded ? 'loaded (not running)' : 'stopped'}\n`);
  out.write(`Panel server: ${report.server.reachable ? `reachable (${report.server.url})` : `unreachable (${report.server.url})`}\n`);
  if (report.logDir) out.write(`Logs: ${report.logDir}\n`);
}
