import type { Audit } from '../audit.ts';
import type { Db } from '../db.ts';
import type { LoginLimiter } from './ratelimit.ts';
import type { Session, SessionStore } from './sessions.ts';
import type { AuthSecrets } from './secrets.ts';
import { DUMMY_HASH, verifyPassword } from './password.ts';
import { verifyTotp } from './totp.ts';

export type LoginResult =
  | { ok: true; token: string; session: Session }
  | { ok: false; reason: 'locked'; retryAfterSeconds: number }
  | { ok: false; reason: 'invalid' };

const LAST_STEP_KEY = 'totp_last_step';

export class AuthService {
  constructor(
    private deps: {
      db: Db;
      secrets: AuthSecrets;
      sessions: SessionStore;
      limiter: LoginLimiter;
      audit: Audit;
      now: () => number;
    },
  ) {}

  private lastStep(): number {
    const row = this.deps.db.prepare('SELECT value FROM kv WHERE key = ?').get(LAST_STEP_KEY) as { value: string } | undefined;
    return row ? Number(row.value) : -1;
  }

  private setLastStep(step: number): void {
    this.deps.db.prepare('INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value').run(LAST_STEP_KEY, String(step));
  }

  /**
   * Password + TOTP in one step. Both factors are always evaluated and the failure message never says
   * which one was wrong. A TOTP code can be used once (replay protection).
   */
  login(input: { ip: string; userAgent: string; password: string; code: string }): LoginResult {
    const { limiter, audit, secrets, sessions, now } = this.deps;
    const { ip } = input;

    const decision = limiter.check(ip);
    if (!decision.allowed) {
      audit.logThrottled(`blocked:${decision.scope}:${ip}`, 'login_blocked', { ip, detail: { scope: decision.scope } });
      return { ok: false, reason: 'locked', retryAfterSeconds: decision.retryAfterSeconds };
    }

    const pwOk = verifyPassword(input.password, secrets.passwordHash || DUMMY_HASH);
    const step = verifyTotp(secrets.totpSecret, input.code, now());
    const fresh = step !== null && step > this.lastStep();

    if (!pwOk || !fresh) {
      const locked = limiter.fail(ip);
      audit.log('login_fail', { ip });
      if (locked) audit.log('login_lockout', { ip });
      return { ok: false, reason: 'invalid' };
    }

    this.setLastStep(step);
    limiter.success(ip);
    const { token, session } = sessions.create(ip, input.userAgent);
    audit.log('login_ok', { ip });
    return { ok: true, token, session };
  }
}
