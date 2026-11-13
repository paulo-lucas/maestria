import QRCode from 'qrcode';
import { loadSecrets, saveSecrets } from '../auth/secrets.ts';
import { hashPassword } from '../auth/password.ts';
import { generateSecret, otpauthUri, verifyTotp } from '../auth/totp.ts';
import { openDb } from '../db.ts';
import { Audit } from '../audit.ts';
import { SessionStore } from '../auth/sessions.ts';
import { askHidden, askLine, defaultPromptIO, type PromptIO } from './prompt.ts';

const MIN_PASSWORD_LENGTH = 12;
const MAX_PASSWORD_LENGTH = 256;

export interface AuthSetupOpts {
  configDir: string;
  force: boolean;
  io?: PromptIO;
  now?: () => number;
  onSecret?: (secret: string) => void;
}

export function validateNewPassword(value: string): string | true {
  if (value.length < MIN_PASSWORD_LENGTH) {
    return `Password must be at least ${MIN_PASSWORD_LENGTH} characters (entered ${value.length}).`;
  }
  if (value.length > MAX_PASSWORD_LENGTH) {
    return `Password must be at most ${MAX_PASSWORD_LENGTH} characters.`;
  }
  return true;
}

export async function authSetup(opts: AuthSetupOpts): Promise<void> {
  const io = opts.io ?? defaultPromptIO();
  const out = io.output;
  const now = opts.now ?? Date.now;

  const existing = loadSecrets(opts.configDir);
  if (existing && !opts.force) {
    throw new Error('Credentials already exist. Re-run with --force to replace them (this signs out every session).');
  }

  let password = '';
  while (true) {
    password = await askHidden(`New password (at least ${MIN_PASSWORD_LENGTH} characters): `, io);
    const pwCheck = validateNewPassword(password);
    if (pwCheck !== true) {
      out.write(`${pwCheck}\n`);
      continue;
    }
    const again = await askHidden('Repeat password: ', io);
    if (again !== password) {
      out.write('The passwords do not match. Please try again.\n');
      continue;
    }
    break;
  }

  const secret = generateSecret();
  opts.onSecret?.(secret);
  const uri = otpauthUri(secret, 'panel');
  out.write('\nScan this QR code with your authenticator app:\n\n');
  out.write(`${await QRCode.toString(uri, { type: 'terminal', small: true })}\n`);
  out.write(`Or enter the key manually: ${secret}\n\n`);

  while (true) {
    const code = (await askLine('Enter the 6-digit code shown by the app to confirm: ', io)).trim();
    if (verifyTotp(secret, code, now()) !== null) break;
    out.write('That code is not valid — check the 6-digit code in your app and try again.\n');
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
