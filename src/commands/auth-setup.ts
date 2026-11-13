import readline from 'node:readline';
import QRCode from 'qrcode';
import { loadSecrets, saveSecrets } from '../auth/secrets.ts';
import { hashPassword } from '../auth/password.ts';
import { generateSecret, otpauthUri, verifyTotp } from '../auth/totp.ts';
import { openDb } from '../db.ts';
import { Audit } from '../audit.ts';
import { SessionStore } from '../auth/sessions.ts';

const MIN_PASSWORD_LENGTH = 12;

/** Read a line from the terminal without echoing it. */
export function promptHidden(question: string): Promise<string> {
  return new Promise((resolve, reject) => {
    if (!process.stdin.isTTY) {
      reject(new Error('A terminal is required to enter the password.'));
      return;
    }
    process.stdout.write(question);
    const rl = readline.createInterface({ input: process.stdin, terminal: true });
    // Mute echo: readline writes through its output stream, which we do not give it; raw typing is hidden.
    (rl as unknown as { _writeToOutput: (s: string) => void })._writeToOutput = () => {};
    rl.question('', (answer) => {
      rl.close();
      process.stdout.write('\n');
      resolve(answer);
    });
  });
}

function prompt(question: string): Promise<string> {
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((resolve) =>
    rl.question(question, (a) => {
      rl.close();
      resolve(a);
    }),
  );
}

export async function authSetup(opts: { configDir: string; force: boolean }): Promise<void> {
  const existing = loadSecrets(opts.configDir);
  if (existing && !opts.force) {
    throw new Error('Credentials already exist. Re-run with --force to replace them (this signs out every session).');
  }

  const password = await promptHidden(`New password (min ${MIN_PASSWORD_LENGTH} characters): `);
  if (password.length < MIN_PASSWORD_LENGTH || password.length > 256) {
    throw new Error(`The password must be between ${MIN_PASSWORD_LENGTH} and 256 characters.`);
  }
  const again = await promptHidden('Repeat password: ');
  if (again !== password) throw new Error('The passwords do not match.');

  const secret = generateSecret();
  const uri = otpauthUri(secret, 'panel');
  console.log('\nScan this QR code with your authenticator app:\n');
  console.log(await QRCode.toString(uri, { type: 'terminal', small: true }));
  console.log(`Or enter the key manually: ${secret}\n`);

  const code = (await prompt('Enter the 6-digit code shown by the app to confirm: ')).trim();
  if (verifyTotp(secret, code, Date.now()) === null) {
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

  console.log('\nDone. Credentials saved with 0600 permissions. Start the panel with: maestria serve');
}
