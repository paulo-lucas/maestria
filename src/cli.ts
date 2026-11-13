#!/usr/bin/env node
import { loadConfig, defaultConfigDir, ConfigError } from './config.ts';
import { SecretsError, loadSecrets } from './auth/secrets.ts';
import { authSetup } from './commands/auth-setup.ts';
import { ArgError } from './commands/args.ts';
import {
  runAnswersCommand,
  runInboxCommand,
  runPostCommand,
  runStatusCommand,
  runWorkspaceCommand,
} from './commands/inbox.ts';
import { openDb } from './db.ts';
import { Audit } from './audit.ts';
import { SessionStore } from './auth/sessions.ts';
import { ConflictError, ValidationError } from './inbox.ts';
import { WorkspaceError } from './workspaces.ts';
import { buildServer } from './server.ts';

const HELP = `maestria: control panel and protocol for Maestri workspaces

Usage:
  maestria serve                    Start the panel (127.0.0.1 only)
  maestria auth setup [--force]     Set the password and enrol an authenticator app (TOTP)
  maestria auth logout-all          Invalidate every panel session

  maestria workspace list|add|remove ...
  maestria post --kind notice|approve|input|choice [--option <val> ...] [--workspace|-w <ws>] [--by <agent>] <markdown>
  maestria inbox [--workspace|-w <ws>] [--status open|responded|dismissed|history|all] [--kind ...] [--json]
  maestria answers [--workspace|-w <ws>] [--all] [--json]
  maestria status [--workspace|-w <ws>] [--json]

Config directory: $MAESTRIA_CONFIG_DIR or ~/.config/maestria
`;

async function serve(): Promise<void> {
  const configDir = defaultConfigDir();
  const config = loadConfig(configDir);
  const secrets = loadSecrets(configDir);
  if (!secrets) {
    throw new ConfigError('No credentials configured. Run "maestria auth setup" first.');
  }
  const db = openDb(configDir);
  const { app } = await buildServer({ config, db, secrets, logger: true, enableUsagePoll: true });
  // Loopback only, on purpose: the internet reaches the panel through the tunnel.
  await app.listen({ host: config.host, port: config.port });

  const stop = async () => {
    await app.close();
    db.close();
    process.exit(0);
  };
  process.on('SIGINT', stop);
  process.on('SIGTERM', stop);
}

function logoutAll(): void {
  const configDir = defaultConfigDir();
  const config = loadConfig(configDir);
  const db = openDb(configDir);
  const sessions = new SessionStore(db, { idleMs: config.session.idleMinutes * 60_000, absoluteMs: config.session.absoluteHours * 3_600_000 });
  const n = sessions.destroyAll();
  new Audit(db).log('logout_all', { detail: { sessions: n, via: 'cli' } });
  db.close();
  console.log(`Invalidated ${n} session${n === 1 ? '' : 's'}.`);
}

async function main(argv: string[]): Promise<void> {
  const [cmd, sub, ...rest] = argv;
  const configDir = defaultConfigDir();
  if (cmd === 'serve') return serve();
  if (cmd === 'auth' && sub === 'setup') return authSetup({ configDir, force: rest.includes('--force') });
  if (cmd === 'auth' && sub === 'logout-all') return logoutAll();
  if (cmd === 'workspace') return runWorkspaceCommand(argv.slice(1), { configDir });
  if (cmd === 'post') return runPostCommand(argv.slice(1), { configDir });
  if (cmd === 'inbox') return runInboxCommand(argv.slice(1), { configDir });
  if (cmd === 'answers') return runAnswersCommand(argv.slice(1), { configDir });
  if (cmd === 'status') return runStatusCommand(argv.slice(1), { configDir });
  if (!cmd || cmd === 'help' || cmd === '--help' || cmd === '-h') {
    process.stdout.write(HELP);
    return;
  }
  process.stderr.write(`Unknown command: ${argv.join(' ')}\n\n${HELP}`);
  process.exitCode = 2;
}

main(process.argv.slice(2)).catch((err: unknown) => {
  if (
    err instanceof ConfigError ||
    err instanceof SecretsError ||
    err instanceof WorkspaceError ||
    err instanceof ValidationError ||
    err instanceof ConflictError ||
    err instanceof ArgError
  ) {
    process.stderr.write(`${err.message}\n`);
    process.exitCode = err instanceof ArgError ? 2 : 1;
    return;
  }
  process.stderr.write(`${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
  process.exitCode = 1;
});
