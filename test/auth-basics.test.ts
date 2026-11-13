import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { hashPassword, verifyPassword } from '../src/auth/password.ts';
import { loadSecrets, saveSecrets, SecretsError, authFile } from '../src/auth/secrets.ts';
import { ConfigError, defaultConfig, parseConfig } from '../src/config.ts';
import { checkPrivateFilePermissions } from '../src/permissions.ts';

describe('password', () => {
  it('hashes with argon2id and verifies', () => {
    const h = hashPassword('correct horse battery staple');
    expect(h).toMatch(/^\$argon2id\$v=19\$m=65536,t=3,p=4\$/);
    expect(verifyPassword('correct horse battery staple', h)).toBe(true);
    expect(verifyPassword('wrong', h)).toBe(false);
  });

  it('salts every hash', () => {
    expect(hashPassword('same')).not.toBe(hashPassword('same'));
  });

  it('rejects malformed or abusive hashes', () => {
    expect(verifyPassword('x', 'plaintext')).toBe(false);
    expect(verifyPassword('x', '$argon2id$v=19$m=99999999,t=3,p=4$AAAA$AAAA')).toBe(false);
  });
});

describe('secrets file', () => {
  const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-'));
  const secrets = { passwordHash: 'h', totpSecret: 'S', createdAt: '2026-01-01T00:00:00Z' };

  it('is written with 0600 inside a 0700 directory on POSIX and loads safely across platforms', () => {
    const dir = path.join(tmp(), 'cfg');
    saveSecrets(dir, secrets);
    if (process.platform !== 'win32') {
      expect(fs.statSync(authFile(dir)).mode & 0o777).toBe(0o600);
      expect(fs.statSync(dir).mode & 0o777).toBe(0o700);
    }
    expect(loadSecrets(dir)).toEqual(secrets);
  });

  it('returns null when absent, enforces 0600 on POSIX, and skips POSIX mode checks on Windows', () => {
    const dir = tmp();
    expect(loadSecrets(dir)).toBeNull();
    saveSecrets(dir, secrets);
    if (process.platform !== 'win32') {
      fs.chmodSync(authFile(dir), 0o644);
      expect(() => loadSecrets(dir)).toThrow(SecretsError);
    }
    expect(() =>
      checkPrivateFilePermissions('C:\\Users\\alice\\AppData\\Roaming\\maestria\\auth.json', 0o100666, (m) => new SecretsError(m), 'win32'),
    ).not.toThrow();
    expect(() =>
      checkPrivateFilePermissions('/home/alice/.config/maestria/auth.json', 0o100644, (m) => new SecretsError(m), 'linux'),
    ).toThrow(SecretsError);
  });
});

describe('config', () => {
  it('has safe defaults', () => {
    const c = defaultConfig('/x');
    expect(c.host).toBe('127.0.0.1');
    expect(c.port).toBe(4777);
    expect(c.cookieSecure).toBe(true);
  });

  it('does not allow changing the listen address', () => {
    expect(() => parseConfig({ host: '0.0.0.0' }, '/x')).toThrow(ConfigError);
  });

  it('validates fields', () => {
    expect(() => parseConfig({ port: 70000 }, '/x')).toThrow(ConfigError);
    expect(() => parseConfig({ session: { idleMinutes: 0 } }, '/x')).toThrow(ConfigError);
    expect(() => parseConfig({ cloudflareAccess: { teamDomain: 'https://x', audience: 'zz' } }, '/x')).toThrow(ConfigError);
    const ok = parseConfig({ port: 5000, allowedHosts: ['Panel.Example.com'], cloudflareAccess: { teamDomain: 'team.cloudflareaccess.com', audience: 'a'.repeat(64) } }, '/x');
    expect(ok.port).toBe(5000);
    expect(ok.allowedHosts).toEqual(['panel.example.com']);
    expect(ok.cloudflareAccess?.teamDomain).toBe('team.cloudflareaccess.com');
  });
});
