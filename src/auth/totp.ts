import crypto from 'node:crypto';

// RFC 6238 TOTP (HMAC-SHA1, 30 s step, 6 digits: what every authenticator app supports).

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

export function base32Encode(buf: Buffer): string {
  let bits = 0;
  let value = 0;
  let out = '';
  for (const byte of buf) {
    value = (value << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      out += ALPHABET[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += ALPHABET[(value << (5 - bits)) & 31];
  return out;
}

export function base32Decode(s: string): Buffer {
  const clean = s.replace(/=+$/, '').replace(/\s+/g, '').toUpperCase();
  let bits = 0;
  let value = 0;
  const out: number[] = [];
  for (const ch of clean) {
    const idx = ALPHABET.indexOf(ch);
    if (idx < 0) throw new Error('invalid base32');
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

export function generateSecret(): string {
  return base32Encode(crypto.randomBytes(20));
}

export function hotp(secret: Buffer, counter: number, digits = 6): string {
  const buf = Buffer.alloc(8);
  buf.writeBigUInt64BE(BigInt(counter));
  const mac = crypto.createHmac('sha1', secret).update(buf).digest();
  const offset = mac[mac.length - 1]! & 0x0f;
  const code = (mac.readUInt32BE(offset) & 0x7fffffff) % 10 ** digits;
  return code.toString().padStart(digits, '0');
}

export const STEP_SECONDS = 30;

export function stepAt(nowMs: number): number {
  return Math.floor(nowMs / 1000 / STEP_SECONDS);
}

export function totp(secretB32: string, nowMs: number, digits = 6): string {
  return hotp(base32Decode(secretB32), stepAt(nowMs), digits);
}

/**
 * Check a user-supplied code against the steps in [-window, +window] around now.
 * Returns the matching step (so the caller can reject replays) or null.
 * All candidate steps are always compared, in constant time.
 */
export function verifyTotp(secretB32: string, code: string, nowMs: number, window = 1): number | null {
  if (!/^\d{6}$/.test(code)) return null;
  const secret = base32Decode(secretB32);
  const current = stepAt(nowMs);
  const given = Buffer.from(code);
  let matched: number | null = null;
  for (let s = current - window; s <= current + window; s++) {
    const expected = Buffer.from(hotp(secret, s));
    if (crypto.timingSafeEqual(expected, given)) matched = s;
  }
  return matched;
}

export function otpauthUri(secretB32: string, account: string, issuer = 'Maestria'): string {
  const label = `${encodeURIComponent(issuer)}:${encodeURIComponent(account)}`;
  return `otpauth://totp/${label}?secret=${secretB32}&issuer=${encodeURIComponent(issuer)}&algorithm=SHA1&digits=6&period=${STEP_SECONDS}`;
}
