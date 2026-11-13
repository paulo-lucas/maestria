import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  ConfigError,
  defaultConfig,
  inspectMaestriDir,
  loadConfig,
  parseConfig,
  resolveMaestriDir,
  saveRawConfig,
} from '../src/config.ts';

describe('maestriDir resolution and config validation', () => {
  it('resolves maestriDir in priority order: config > MAESTRI_HOME > ~/.maestri', () => {
    const home = '/Users/example';
    expect(resolveMaestriDir('/custom/maestri', { MAESTRI_HOME: '/env/maestri' }, home)).toBe('/custom/maestri');
    expect(resolveMaestriDir('~/custom-maestri', { MAESTRI_HOME: '/env/maestri' }, home)).toBe('/Users/example/custom-maestri');
    expect(resolveMaestriDir(undefined, { MAESTRI_HOME: '/env/maestri' }, home)).toBe('/env/maestri');
    expect(resolveMaestriDir(undefined, {}, home)).toBe('/Users/example/.maestri');
    expect(() => resolveMaestriDir('relative/path', {}, home)).toThrow(ConfigError);
  });

  it('inspects missing maestriDir, missing usage/providers, and present .status.json', () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-mdir-'));
    const missing = path.join(root, 'absent');
    expect(inspectMaestriDir(missing)).toMatchObject({ hasDir: false, hasProvidersDir: false, hasStatusFile: false, valid: false });

    const partial = path.join(root, 'partial');
    fs.mkdirSync(partial);
    expect(inspectMaestriDir(partial)).toMatchObject({ hasDir: true, hasProvidersDir: false, hasStatusFile: false, valid: false });

    const providersDir = path.join(partial, 'usage', 'providers');
    fs.mkdirSync(providersDir, { recursive: true });
    expect(inspectMaestriDir(partial)).toMatchObject({ hasDir: true, hasProvidersDir: true, hasStatusFile: false, valid: true });

    fs.writeFileSync(path.join(providersDir, '.status.json'), '{"providers":[]}');
    expect(inspectMaestriDir(partial)).toMatchObject({ hasDir: true, hasProvidersDir: true, hasStatusFile: true, valid: true });
  });

  it('validates usage, standby thresholds, and tunnel commands', () => {
    const cfg = parseConfig(
      {
        maestriDir: '/tmp/my-maestri',
        usage: { staleMinutes: 10, fallbackEnabled: false, pollSeconds: 45 },
        standby: {
          resumeBelowPercent: 70,
          notifyViaCli: true,
          thresholds: {
            claude: { five_hour: 95, seven_day: 90 },
            antigravity: { weekly: 85 },
          },
        },
        tunnel: {
          stopCommand: ['pkill', '-x', 'cloudflared'],
          startCommand: ['launchctl', 'kickstart', 'gui/501/com.cloudflare.tunnel'],
        },
      },
      '/tmp/cfg',
    );
    expect(cfg.maestriDir).toBe('/tmp/my-maestri');
    expect(cfg.usage).toEqual({ staleMinutes: 10, fallbackEnabled: false, pollSeconds: 45 });
    expect(cfg.standby.thresholds.claude?.five_hour).toBe(95);
    expect(cfg.standby.resumeBelowPercent).toBe(70);
    expect(cfg.tunnel.stopCommand).toEqual(['pkill', '-x', 'cloudflared']);

    expect(() => parseConfig({ standby: { thresholds: { claude: { five_hour: 101 } } } }, '/tmp/cfg')).toThrow(ConfigError);
    expect(() => parseConfig({ tunnel: { stopCommand: [''] } }, '/tmp/cfg')).toThrow(ConfigError);
  });

  it('persists config.json with 0600 permissions and loads defaults cleanly', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-cfg-'));
    expect(loadConfig(dir, { MAESTRI_HOME: '/tmp/env-maestri' }).maestriDir).toBe('/tmp/env-maestri');
    saveRawConfig(dir, { maestriDir: '/tmp/saved-maestri', port: 4888 });
    expect(fs.statSync(path.join(dir, 'config.json')).mode & 0o777).toBe(0o600);
    expect(loadConfig(dir).maestriDir).toBe('/tmp/saved-maestri');
    expect(defaultConfig(dir).port).toBe(4777);
  });
});
