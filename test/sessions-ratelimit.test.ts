import { describe, expect, it } from 'vitest';
import { openMemoryDb } from '../src/db.ts';
import { SessionStore } from '../src/auth/sessions.ts';
import { LoginLimiter } from '../src/auth/ratelimit.ts';

function clock(start = 1_700_000_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

describe('sessions', () => {
  const opts = { idleMs: 10 * 60_000, absoluteMs: 60 * 60_000 };

  it('stores only a hash of the token', () => {
    const db = openMemoryDb();
    const s = new SessionStore(db, opts);
    const { token } = s.create('1.2.3.4', 'ua');
    const rows = db.prepare('SELECT id FROM sessions').all() as { id: string }[];
    expect(rows).toHaveLength(1);
    expect(rows[0]!.id).not.toBe(token);
    expect(s.validate(token)).not.toBeNull();
    expect(s.validate('nope')).toBeNull();
    expect(s.validate(undefined)).toBeNull();
  });

  it('expires after the idle timeout, but activity extends it', () => {
    const c = clock();
    const s = new SessionStore(openMemoryDb(), opts, c.now);
    const { token } = s.create('ip', 'ua');
    c.advance(9 * 60_000);
    expect(s.validate(token)).not.toBeNull();
    c.advance(9 * 60_000);
    expect(s.validate(token)).not.toBeNull();
    c.advance(10 * 60_000);
    expect(s.validate(token)).toBeNull();
    expect(s.count()).toBe(0);
  });

  it('expires at the absolute limit even when active', () => {
    const c = clock();
    const s = new SessionStore(openMemoryDb(), opts, c.now);
    const { token } = s.create('ip', 'ua');
    for (let i = 0; i < 5; i++) {
      c.advance(9 * 60_000);
      expect(s.validate(token)).not.toBeNull();
    }
    c.advance(9 * 60_000); // 54 min
    expect(s.validate(token)).not.toBeNull();
    c.advance(9 * 60_000); // 63 min
    expect(s.validate(token)).toBeNull();
  });

  it('destroys one or all sessions', () => {
    const s = new SessionStore(openMemoryDb(), opts);
    const a = s.create('ip', 'ua');
    const b = s.create('ip', 'ua');
    s.destroy(a.token);
    expect(s.validate(a.token)).toBeNull();
    expect(s.validate(b.token)).not.toBeNull();
    s.create('ip', 'ua');
    expect(s.destroyAll()).toBe(2);
    expect(s.validate(b.token)).toBeNull();
  });
});

describe('login limiter', () => {
  const opts = { maxFailuresPerIp: 3, maxFailuresGlobal: 5, windowMs: 60_000, lockoutMs: 120_000 };

  it('locks an IP after repeated failures and unlocks after the lockout', () => {
    const c = clock();
    const l = new LoginLimiter(opts, c.now);
    expect(l.fail('a')).toBe(false);
    expect(l.fail('a')).toBe(false);
    expect(l.check('a').allowed).toBe(true);
    expect(l.fail('a')).toBe(true);
    const d = l.check('a');
    expect(d.allowed).toBe(false);
    expect(d.retryAfterSeconds).toBe(120);
    expect(l.check('b').allowed).toBe(true);
    c.advance(121_000);
    expect(l.check('a').allowed).toBe(true);
  });

  it('forgets failures outside the window', () => {
    const c = clock();
    const l = new LoginLimiter(opts, c.now);
    l.fail('a');
    l.fail('a');
    c.advance(61_000);
    expect(l.fail('a')).toBe(false);
    expect(l.check('a').allowed).toBe(true);
  });

  it('applies a global lockout against distributed guessing', () => {
    const c = clock();
    const l = new LoginLimiter(opts, c.now);
    for (const ip of ['a', 'b', 'c', 'd', 'e']) l.fail(ip);
    expect(l.check('z')).toMatchObject({ allowed: false, scope: 'global' });
    c.advance(121_000);
    expect(l.check('z').allowed).toBe(true);
  });

  it('clears an IP on success', () => {
    const l = new LoginLimiter(opts);
    l.fail('a');
    l.fail('a');
    l.success('a');
    expect(l.fail('a')).toBe(false);
  });
});
