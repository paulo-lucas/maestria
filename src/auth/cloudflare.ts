import crypto from 'node:crypto';

export interface AccessConfig {
  teamDomain: string;
  audience: string;
}

export interface Jwk {
  kid?: string;
  kty: string;
  n?: string;
  e?: string;
  [k: string]: unknown;
}

export type JwksFetcher = (teamDomain: string) => Promise<Jwk[]>;

export const fetchJwks: JwksFetcher = async (teamDomain) => {
  const res = await fetch(`https://${teamDomain}/cdn-cgi/access/certs`, { signal: AbortSignal.timeout(5000) });
  if (!res.ok) throw new Error(`JWKS fetch failed: ${res.status}`);
  const body = (await res.json()) as { keys?: Jwk[] };
  return Array.isArray(body.keys) ? body.keys : [];
};

function b64urlJson(s: string): Record<string, unknown> | null {
  try {
    const v: unknown = JSON.parse(Buffer.from(s, 'base64url').toString('utf8'));
    return typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/**
 * Verifies the Cloudflare Access JWT (`Cf-Access-Jwt-Assertion`): RS256 signature against the team's
 * published keys, issuer, audience and expiry. Defence in depth behind the panel's own login.
 */
export class AccessVerifier {
  private keys = new Map<string, crypto.KeyObject>();
  private lastFetch = 0;
  private static readonly REFRESH_MIN_MS = 60_000;

  constructor(
    private cfg: AccessConfig,
    private fetcher: JwksFetcher = fetchJwks,
    private now: () => number = Date.now,
  ) {}

  private async refresh(): Promise<void> {
    const t = this.now();
    if (t - this.lastFetch < AccessVerifier.REFRESH_MIN_MS) return;
    this.lastFetch = t;
    const jwks = await this.fetcher(this.cfg.teamDomain);
    const next = new Map<string, crypto.KeyObject>();
    for (const jwk of jwks) {
      if (jwk.kty !== 'RSA' || typeof jwk.kid !== 'string') continue;
      try {
        next.set(jwk.kid, crypto.createPublicKey({ key: jwk, format: 'jwk' } as crypto.JsonWebKeyInput));
      } catch {
        /* skip malformed key */
      }
    }
    this.keys = next;
  }

  async verify(token: string | undefined): Promise<boolean> {
    if (!token || token.length > 8192) return false;
    const parts = token.split('.');
    if (parts.length !== 3) return false;
    const [h, p, s] = parts as [string, string, string];
    const header = b64urlJson(h);
    const payload = b64urlJson(p);
    if (!header || !payload) return false;
    if (header.alg !== 'RS256' || typeof header.kid !== 'string') return false;

    let key = this.keys.get(header.kid);
    if (!key) {
      try {
        await this.refresh();
      } catch {
        return false;
      }
      key = this.keys.get(header.kid);
    }
    if (!key) return false;

    const ok = crypto.verify('RSA-SHA256', Buffer.from(`${h}.${p}`), key, Buffer.from(s, 'base64url'));
    if (!ok) return false;

    const nowSec = Math.floor(this.now() / 1000);
    if (typeof payload.exp !== 'number' || payload.exp <= nowSec) return false;
    if (typeof payload.nbf === 'number' && payload.nbf > nowSec + 30) return false;
    if (payload.iss !== `https://${this.cfg.teamDomain}`) return false;
    const aud = payload.aud;
    const audOk = Array.isArray(aud) ? aud.includes(this.cfg.audience) : aud === this.cfg.audience;
    return audOk;
  }
}
