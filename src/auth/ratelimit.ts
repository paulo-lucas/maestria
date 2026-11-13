export interface LimiterOptions {
  maxFailuresPerIp: number;
  maxFailuresGlobal: number;
  windowMs: number;
  lockoutMs: number;
}

export interface LimitDecision {
  allowed: boolean;
  retryAfterSeconds: number;
  scope?: 'ip' | 'global';
}

interface Bucket {
  failures: number[]; // timestamps of recent failures
  lockedUntil: number;
}

const MAX_TRACKED_IPS = 10_000;

/**
 * Failed-login limiter. A bucket per client IP plus one global bucket (which protects against
 * distributed guessing). Exceeding `max` failures within the window locks the bucket for `lockoutMs`;
 * while locked, even correct credentials are refused. A successful login clears only the IP's bucket.
 */
export class LoginLimiter {
  private ips = new Map<string, Bucket>();
  private global: Bucket = { failures: [], lockedUntil: 0 };

  constructor(
    private opts: LimiterOptions,
    private now: () => number = Date.now,
  ) {}

  private prune(b: Bucket, t: number): void {
    b.failures = b.failures.filter((f) => t - f < this.opts.windowMs);
  }

  check(ip: string): LimitDecision {
    const t = this.now();
    if (this.global.lockedUntil > t) {
      return { allowed: false, retryAfterSeconds: Math.ceil((this.global.lockedUntil - t) / 1000), scope: 'global' };
    }
    const b = this.ips.get(ip);
    if (b && b.lockedUntil > t) {
      return { allowed: false, retryAfterSeconds: Math.ceil((b.lockedUntil - t) / 1000), scope: 'ip' };
    }
    return { allowed: true, retryAfterSeconds: 0 };
  }

  /** Record a failed attempt. Returns true if this failure triggered a lockout. */
  fail(ip: string): boolean {
    const t = this.now();
    let b = this.ips.get(ip);
    if (!b) {
      if (this.ips.size >= MAX_TRACKED_IPS) this.evict(t);
      b = { failures: [], lockedUntil: 0 };
      this.ips.set(ip, b);
    }
    let locked = false;
    this.prune(b, t);
    b.failures.push(t);
    if (b.failures.length >= this.opts.maxFailuresPerIp) {
      b.lockedUntil = t + this.opts.lockoutMs;
      b.failures = [];
      locked = true;
    }
    this.prune(this.global, t);
    this.global.failures.push(t);
    if (this.global.failures.length >= this.opts.maxFailuresGlobal) {
      this.global.lockedUntil = t + this.opts.lockoutMs;
      this.global.failures = [];
      locked = true;
    }
    return locked;
  }

  success(ip: string): void {
    this.ips.delete(ip);
  }

  private evict(t: number): void {
    for (const [ip, b] of this.ips) {
      this.prune(b, t);
      if (b.failures.length === 0 && b.lockedUntil <= t) this.ips.delete(ip);
    }
    if (this.ips.size >= MAX_TRACKED_IPS) this.ips.clear();
  }
}
