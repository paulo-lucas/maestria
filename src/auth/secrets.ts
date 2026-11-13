import fs from 'node:fs';
import path from 'node:path';
import { ensureConfigDir } from '../config.ts';

export interface AuthSecrets {
  passwordHash: string;
  totpSecret: string;
  createdAt: string;
}

export function authFile(configDir: string): string {
  return path.join(configDir, 'auth.json');
}

export class SecretsError extends Error {}

/** Write auth.json atomically with 0600 permissions. */
export function saveSecrets(configDir: string, secrets: AuthSecrets): void {
  ensureConfigDir(configDir);
  const file = authFile(configDir);
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(secrets, null, 2) + '\n', { mode: 0o600 });
  fs.chmodSync(tmp, 0o600);
  fs.renameSync(tmp, file);
}

/** Read auth.json. Refuses files readable by group/others and malformed contents. Returns null if absent. */
export function loadSecrets(configDir: string): AuthSecrets | null {
  const file = authFile(configDir);
  let st: fs.Stats;
  try {
    st = fs.statSync(file);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw err;
  }
  if (st.mode & 0o077) {
    throw new SecretsError(`${file} has permissions ${(st.mode & 0o777).toString(8)}; it must be 600 (run: chmod 600 ${file})`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    throw new SecretsError(`${file} is not valid JSON`);
  }
  const p = parsed as Partial<AuthSecrets> | null;
  if (!p || typeof p.passwordHash !== 'string' || typeof p.totpSecret !== 'string' || typeof p.createdAt !== 'string') {
    throw new SecretsError(`${file} is malformed; run "maestria auth setup --force"`);
  }
  return { passwordHash: p.passwordHash, totpSecret: p.totpSecret, createdAt: p.createdAt };
}
