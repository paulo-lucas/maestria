import { execFile } from 'node:child_process';
import type { Audit } from './audit.ts';
import type { SessionStore } from './auth/sessions.ts';
import type { Config } from './config.ts';
import type { Db } from './db.ts';

export interface CommandResult {
  ok: boolean;
  code: number | null;
  stderr: string;
}

export type CommandRunner = (argv: readonly string[]) => Promise<CommandResult>;

export function defaultCommandRunner(argv: readonly string[]): Promise<CommandResult> {
  const [cmd, ...args] = argv;
  if (!cmd) return Promise.resolve({ ok: true, code: 0, stderr: '' });
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout: 15_000, maxBuffer: 64 * 1024, windowsHide: true }, (err, _stdout, stderr) => {
      if (!err) {
        resolve({ ok: true, code: 0, stderr: String(stderr ?? '').trim() });
        return;
      }
      const code = typeof (err as NodeJS.ErrnoException).code === 'number' ? Number((err as NodeJS.ErrnoException).code) : null;
      resolve({ ok: false, code, stderr: String(stderr || err.message).trim().slice(0, 400) });
    });
  });
}

const KEY_KILLED = 'tunnel_killed';
const KEY_KILLED_AT = 'tunnel_killed_at';
const KEY_ENABLED_AT = 'tunnel_enabled_at';

export interface TunnelStatus {
  killed: boolean;
  killedAt: number | null;
  enabledAt: number | null;
  stopConfigured: boolean;
  startConfigured: boolean;
  statusConfigured: boolean;
  running: boolean | null;
}

export class TunnelController {
  constructor(
    private db: Db,
    private audit: Audit,
    private sessions: SessionStore,
    private config: Pick<Config, 'tunnel'>,
    private now: () => number = Date.now,
    private runner: CommandRunner = defaultCommandRunner,
  ) {}

  private getKv(key: string): string | null {
    const row = this.db.prepare('SELECT value FROM kv WHERE key = ?').get(key) as { value: string } | undefined;
    return row ? row.value : null;
  }

  private setKv(key: string, value: string): void {
    this.db
      .prepare('INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value')
      .run(key, value);
  }

  isKilled(): boolean {
    return this.getKv(KEY_KILLED) === '1';
  }

  async status(): Promise<TunnelStatus> {
    const killed = this.isKilled();
    const ka = this.getKv(KEY_KILLED_AT);
    const ea = this.getKv(KEY_ENABLED_AT);
    const statusCmd = this.config.tunnel.statusCommand;
    let running: boolean | null = null;
    if (statusCmd.length > 0) {
      if (killed) {
        running = false;
      } else {
        const res = await this.runner(statusCmd);
        running = res.ok;
      }
    }
    return {
      killed,
      killedAt: ka ? Number(ka) : null,
      enabledAt: ea ? Number(ea) : null,
      stopConfigured: this.config.tunnel.stopCommand.length > 0,
      startConfigured: this.config.tunnel.startCommand.length > 0,
      statusConfigured: statusCmd.length > 0,
      running,
    };
  }

  async killSwitch(actor: { via: 'web' | 'cli'; ip?: string | null }): Promise<{
    sessionsInvalidated: number;
    stopResult: CommandResult | null;
  }> {
    this.setKv(KEY_KILLED, '1');
    this.setKv(KEY_KILLED_AT, String(this.now()));
    const sessionsInvalidated = this.sessions.destroyAll();

    let stopResult: CommandResult | null = null;
    if (this.config.tunnel.stopCommand.length > 0) {
      stopResult = await this.runner(this.config.tunnel.stopCommand);
    }

    this.audit.log('kill_switch', {
      ip: actor.ip ?? null,
      detail: {
        via: actor.via,
        sessionsInvalidated,
        stopExecuted: stopResult !== null,
        stopOk: stopResult?.ok ?? null,
        stopCode: stopResult?.code ?? null,
      },
    });

    return { sessionsInvalidated, stopResult };
  }

  async enableLocal(): Promise<{
    wasKilled: boolean;
    startResult: CommandResult | null;
  }> {
    const wasKilled = this.isKilled();
    this.setKv(KEY_KILLED, '0');
    this.setKv(KEY_ENABLED_AT, String(this.now()));

    let startResult: CommandResult | null = null;
    if (this.config.tunnel.startCommand.length > 0) {
      startResult = await this.runner(this.config.tunnel.startCommand);
    }

    this.audit.log('tunnel_on', {
      detail: {
        wasKilled,
        startExecuted: startResult !== null,
        startOk: startResult?.ok ?? null,
        startCode: startResult?.code ?? null,
      },
    });

    return { wasKilled, startResult };
  }
}
