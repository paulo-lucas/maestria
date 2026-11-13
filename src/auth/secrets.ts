import fs from 'node:fs';
import path from 'node:path';
import { ensureConfigDir } from '../config.ts';
import { checkPrivateFilePermissions, writePrivateFileAtomic } from '../permissions.ts';

export interface AuthSecrets {
  passwordHash: string;
  totpSecret: string;
  createdAt: string;
}

export function authFile(configDir: string): string {
  return path.join(configDir, 'auth.json');
}

export class SecretsError extends Error {}

/** Write auth.json atomically with 0600 permissions on POSIX. */
export function saveSecrets(configDir: string, secrets: AuthSecrets, platform: NodeJS.Platform = process.platform): void {
  ensureConfigDir(configDir, platform);
  writePrivateFileAtomic(authFile(configDir), JSON.stringify(secrets, null, 2) + '\n', platform);
}

/** Read auth.json. Refuses loose permissions on POSIX and malformed contents. Returns null if absent. */
export function loadSecrets(configDir: string, platform: NodeJS.Platform = process.platform): AuthSecrets | null {
  const file = authFile(configDir);
  let st: fs.Stats;
  try {
    st = fs.statSync(file);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw err;
  }
  checkPrivateFilePermissions(file, st.mode, (msg) => new SecretsError(msg), platform);
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
