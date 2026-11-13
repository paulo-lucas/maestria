import { Audit } from '../audit.ts';
import { SessionStore } from '../auth/sessions.ts';
import { loadConfig } from '../config.ts';
import { openDb } from '../db.ts';
import { TunnelController, type CommandRunner } from '../tunnel.ts';
import { ArgError, parseArgs } from './args.ts';

export interface TunnelCliOpts {
  configDir: string;
  stdout?: NodeJS.WritableStream;
  runner?: CommandRunner;
}

export async function runTunnelCommand(args: string[], opts: TunnelCliOpts): Promise<void> {
  const [sub, ...rest] = args;
  const { flags, positional } = parseArgs(rest, { json: { type: 'boolean' } });
  if (!sub || !['on', 'off', 'status'].includes(sub) || positional.length > 0) {
    throw new ArgError('Usage: maestria tunnel on|off|status [--json]');
  }

  const config = loadConfig(opts.configDir);
  const db = openDb(opts.configDir);
  try {
    const audit = new Audit(db);
    const sessions = new SessionStore(db, {
      idleMs: config.session.idleMinutes * 60_000,
      absoluteMs: config.session.absoluteHours * 3_600_000,
    });
    const tunnel = new TunnelController(db, audit, sessions, config, Date.now, opts.runner);
    const out = opts.stdout ?? process.stdout;
    const json = flags.json === true;

    if (sub === 'status') {
      const st = await tunnel.status();
      if (json) {
        out.write(JSON.stringify(st, null, 2) + '\n');
      } else {
        out.write(`Kill switch: ${st.killed ? 'LOCKED (re-enable with "maestria tunnel on")' : 'inactive'}\n`);
        out.write(`Stop command: ${st.stopConfigured ? config.tunnel.stopCommand.join(' ') : 'not configured'}\n`);
        out.write(`Start command: ${st.startConfigured ? config.tunnel.startCommand.join(' ') : 'not configured'}\n`);
        if (st.running !== null) out.write(`Tunnel running: ${st.running ? 'yes' : 'no'}\n`);
      }
      return;
    }

    if (sub === 'off') {
      const res = await tunnel.killSwitch({ via: 'cli' });
      if (json) {
        out.write(JSON.stringify({ ok: true, ...res }, null, 2) + '\n');
      } else {
        out.write(`Kill switch activated. Invalidated ${res.sessionsInvalidated} session(s).`);
        if (res.stopResult) {
          out.write(` Stop command ${res.stopResult.ok ? 'succeeded' : `exited with error (${res.stopResult.stderr})`}.`);
        }
        out.write('\n');
      }
      return;
    }

    const res = await tunnel.enableLocal();
    if (json) {
      out.write(JSON.stringify({ ok: true, ...res }, null, 2) + '\n');
    } else {
      out.write(`Tunnel access re-enabled locally${res.wasKilled ? ' (kill switch cleared)' : ''}.`);
      if (res.startResult) {
        out.write(` Start command ${res.startResult.ok ? 'succeeded' : `failed: ${res.startResult.stderr}`}.`);
      }
      out.write('\n');
    }
    if (res.startResult && !res.startResult.ok) {
      process.exitCode = 1;
    }
  } finally {
    db.close();
  }
}
