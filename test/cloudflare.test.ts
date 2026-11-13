import crypto from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { AccessVerifier, type Jwk } from '../src/auth/cloudflare.ts';

const TEAM = 'team.cloudflareaccess.com';
const AUD = 'a'.repeat(64);
const NOW = 1_700_000_000_000;

function makeKey(kid: string) {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 });
  return { kid, privateKey, jwk: { ...publicKey.export({ format: 'jwk' }), kid } as Jwk };
}

function sign(key: ReturnType<typeof makeKey>, claims: Record<string, unknown>, alg = 'RS256') {
  const enc = (o: unknown) => Buffer.from(JSON.stringify(o)).toString('base64url');
  const input = `${enc({ alg, kid: key.kid })}.${enc(claims)}`;
  const sig = crypto.sign('RSA-SHA256', Buffer.from(input), key.privateKey).toString('base64url');
  return `${input}.${sig}`;
}

const good = (over: Record<string, unknown> = {}) => ({
  iss: `https://${TEAM}`,
  aud: [AUD],
  exp: Math.floor(NOW / 1000) + 600,
  ...over,
});

describe('Cloudflare Access JWT', () => {
  const key = makeKey('k1');
  const other = makeKey('k1'); // same kid, different key: signature must fail
  const verifier = () => new AccessVerifier({ teamDomain: TEAM, audience: AUD }, async () => [key.jwk], () => NOW);

  it('accepts a valid token', async () => {
    expect(await verifier().verify(sign(key, good()))).toBe(true);
  });

  it('rejects missing, malformed, or wrongly signed tokens', async () => {
    const v = verifier();
    expect(await v.verify(undefined)).toBe(false);
    expect(await v.verify('a.b')).toBe(false);
    expect(await v.verify(sign(other, good()))).toBe(false);
    expect(await v.verify(sign(makeKey('unknown'), good()))).toBe(false);
  });

  it('rejects wrong audience, issuer, expired, and non-RS256 tokens', async () => {
    const v = verifier();
    expect(await v.verify(sign(key, good({ aud: ['b'.repeat(64)] })))).toBe(false);
    expect(await v.verify(sign(key, good({ iss: 'https://evil.example.com' })))).toBe(false);
    expect(await v.verify(sign(key, good({ exp: Math.floor(NOW / 1000) - 1 })))).toBe(false);
    expect(await v.verify(sign(key, good(), 'none'))).toBe(false);
  });

  it('fails closed when the key set cannot be fetched', async () => {
    const v = new AccessVerifier({ teamDomain: TEAM, audience: AUD }, async () => {
      throw new Error('network');
    }, () => NOW);
    expect(await v.verify(sign(key, good()))).toBe(false);
  });
});
