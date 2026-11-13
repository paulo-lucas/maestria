import { describe, expect, it } from 'vitest';
import { base32Decode, base32Encode, generateSecret, hotp, otpauthUri, totp, verifyTotp } from '../src/auth/totp.ts';

// RFC 6238 appendix B, SHA-1 secret "12345678901234567890", 8 digits.
const RFC_SECRET = Buffer.from('12345678901234567890');
const RFC_B32 = base32Encode(RFC_SECRET);

describe('totp', () => {
  it('matches the RFC 6238 SHA-1 test vectors', () => {
    const vectors: [number, string][] = [
      [59, '94287082'],
      [1111111109, '07081804'],
      [1111111111, '14050471'],
      [1234567890, '89005924'],
      [2000000000, '69279037'],
    ];
    for (const [t, expected] of vectors) {
      expect(hotp(RFC_SECRET, Math.floor(t / 30), 8)).toBe(expected);
    }
  });

  it('round-trips base32', () => {
    const s = generateSecret();
    expect(base32Encode(base32Decode(s))).toBe(s);
    expect(base32Decode('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ').toString()).toBe('12345678901234567890');
  });

  it('accepts the current code and ±1 step, rejects others', () => {
    const now = 1_700_000_000_000;
    const code = totp(RFC_B32, now);
    expect(verifyTotp(RFC_B32, code, now)).toBe(Math.floor(now / 30000));
    expect(verifyTotp(RFC_B32, code, now + 30_000)).not.toBeNull();
    expect(verifyTotp(RFC_B32, code, now - 30_000)).not.toBeNull();
    expect(verifyTotp(RFC_B32, code, now + 90_000)).toBeNull();
  });

  it('rejects malformed codes', () => {
    const now = 1_700_000_000_000;
    for (const bad of ['', '12345', '1234567', 'abcdef', '12 456', '１２３４５６']) {
      expect(verifyTotp(RFC_B32, bad, now)).toBeNull();
    }
  });

  it('builds an otpauth URI', () => {
    const uri = otpauthUri('ABC', 'panel');
    expect(uri).toBe('otpauth://totp/Maestria:panel?secret=ABC&issuer=Maestria&algorithm=SHA1&digits=6&period=30');
  });
});
