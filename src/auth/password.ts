import crypto from 'node:crypto';

// argon2id via node:crypto (no native dependency). Parameters follow the OWASP minimum
// recommendation (m=64 MiB, t=3, p=4) and are stored in the PHC string so they can be raised later.
const PARAMS = { memory: 65536, passes: 3, parallelism: 4, tagLength: 32 };
const VERSION = 19;

function b64(buf: Buffer): string {
  return buf.toString('base64').replace(/=+$/, '');
}

export function hashPassword(password: string): string {
  const nonce = crypto.randomBytes(16);
  const tag = crypto.argon2Sync('argon2id', { message: password, nonce, ...PARAMS });
  return `$argon2id$v=${VERSION}$m=${PARAMS.memory},t=${PARAMS.passes},p=${PARAMS.parallelism}$${b64(nonce)}$${b64(tag)}`;
}

interface Parsed {
  memory: number;
  passes: number;
  parallelism: number;
  nonce: Buffer;
  tag: Buffer;
}

function parse(hash: string): Parsed | null {
  const m = /^\$argon2id\$v=19\$m=(\d+),t=(\d+),p=(\d+)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$/.exec(hash);
  if (!m) return null;
  const memory = Number(m[1]);
  const passes = Number(m[2]);
  const parallelism = Number(m[3]);
  // Refuse absurd parameters from a tampered file (memory DoS).
  if (memory < 8 || memory > 1024 * 1024 || passes < 1 || passes > 20 || parallelism < 1 || parallelism > 16) return null;
  return { memory, passes, parallelism, nonce: Buffer.from(m[4]!, 'base64'), tag: Buffer.from(m[5]!, 'base64') };
}

export function verifyPassword(password: string, hash: string): boolean {
  const p = parse(hash);
  if (!p) return false;
  const actual = crypto.argon2Sync('argon2id', {
    message: password,
    nonce: p.nonce,
    memory: p.memory,
    passes: p.passes,
    parallelism: p.parallelism,
    tagLength: p.tag.length,
  });
  return actual.length === p.tag.length && crypto.timingSafeEqual(actual, p.tag);
}

/** A well-formed hash of a random password, used to keep login timing flat when no real hash applies. */
export const DUMMY_HASH = hashPassword(crypto.randomBytes(16).toString('hex'));
