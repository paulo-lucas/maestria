import QRCode from 'qrcode';
import { loadSecrets, saveSecrets } from '../auth/secrets.ts';
import { hashPassword } from '../auth/password.ts';
import { generateSecret, otpauthUri, verifyTotp } from '../auth/totp.ts';
import { openDb } from '../db.ts';
import { Audit } from '../audit.ts';
import { SessionStore } from '../auth/sessions.ts';
import { askHidden, askLine, defaultPromptIO, type PromptIO } from './prompt.ts';

const MIN_PASSWORD_LENGTH = 12;

export interface AuthSetupOpts {
  configDir: string;
  force: boolean;
  io?: PromptIO;
  now?: () => number;
  onSecret?: (secret: string) => void;
}

export async function authSetup(opts: AuthSetupOpts): Promise<void> {
  const io = opts.io ?? defaultPromptIO();
  const out = io.output;
  const now = opts.now ?? Date.now;

  const existing = loadSecrets(opts.configDir);
  if (existing && !opts.force) {
    throw new Error('Credentials already exist. Re-run with --force to replace them (this signs out every session).');
  }

  const password = await askHidden(`New password (min ${MIN_PASSWORD_LENGTH} characters): `, io);
  if (password.length < MIN_PASSWORD_LENGTH || password.length > 256) {
    throw new Error(`The password must be between ${MIN_PASSWORD_LENGTH} and 256 characters.`);
  }
  const again = await askHidden('Repeat password: ', io);
  if (again !== password) throw new Error('The passwords do not match.');

  const secret = generateSecret();
  opts.onSecret?.(secret);
  const uri = otpauthUri(secret, 'panel');
  out.write('\nScan this QR code with your authenticator app:\n\n');
  out.write(`${await QRCode.toString(uri, { type: 'terminal', small: true })}\n`);
  out.write(`Or enter the key manually: ${secret}\n\n`);

  const code = (await askLine('Enter the 6-digit code shown by the app to confirm: ', io)).trim();
  if (verifyTotp(secret, code, now()) === null) {
    throw new Error('That code is not valid. Nothing was saved; run the command again.');
  }

  saveSecrets(opts.configDir, { passwordHash: hashPassword(password), totpSecret: secret, createdAt: new Date().toISOString() });

  const db = openDb(opts.configDir);
  const audit = new Audit(db);
  const sessions = new SessionStore(db, { idleMs: 0, absoluteMs: 0 });
  const dropped = sessions.destroyAll();
  // A fresh TOTP secret restarts replay protection.
  db.prepare("DELETE FROM kv WHERE key = 'totp_last_step'").run();
  audit.log('auth_setup', { detail: { replaced: existing !== null, sessionsDropped: dropped, via: 'cli' } });
  db.close();

  out.write('\nDone. Credentials saved with 0600 permissions. Start the panel with: maestria serve\n');
}
