import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { hashPassword } from '../src/auth/password.ts';
import { loadSecrets, saveSecrets } from '../src/auth/secrets.ts';
import { totp } from '../src/auth/totp.ts';
import { parseSkillSelection, runInitCommand } from '../src/commands/init.ts';
import {
  askHidden,
  askKeepOrRedo,
  askLine,
  drainBufferedInput,
  isPromptInFlight,
  type PromptInput,
  type PromptIO,
  type WatchdogEvent,
} from '../src/commands/prompt.ts';
import { loadConfig, loadRawConfig } from '../src/config.ts';

function setupSampleEnv(baseDir: string): { homeDir: string; configDir: string; maestriDir: string; projDir: string } {
  const homeDir = path.join(baseDir, 'home');
  const configDir = path.join(baseDir, 'cfg');
  const maestriDir = path.join(homeDir, '.maestri');
  const projDir = path.join(baseDir, 'sample-project');

  fs.mkdirSync(path.join(homeDir, '.gemini', 'config', 'skills'), { recursive: true });
  fs.mkdirSync(projDir, { recursive: true });
  fs.writeFileSync(path.join(projDir, 'ROADMAP.md'), '# Roadmap\n');
  fs.mkdirSync(path.join(maestriDir, 'usage', 'providers'), { recursive: true });
  fs.writeFileSync(path.join(maestriDir, 'usage', 'providers', '.status.json'), '{"providers":[]}');
  fs.mkdirSync(path.join(maestriDir, 'workspaces', 'UUID-1'), { recursive: true });
  fs.writeFileSync(
    path.join(maestriDir, 'workspaces', 'UUID-1', 'workspace.json'),
    JSON.stringify({ payload: { name: 'Sample Project', workingDirectory: projDir } }),
  );
  return { homeDir, configDir, maestriDir, projDir };
}

class FakeTtyHarness {
  readonly input: PassThrough & PromptInput;
  readonly output: PassThrough;
  readonly io: PromptIO;
  readonly rawModeHistory: boolean[] = [];
  private text = '';
  private cursor = 0;
  private waiters: Array<() => void> = [];

  constructor() {
    const input = new PassThrough() as PassThrough & PromptInput;
    input.isTTY = true;
    input.setRawMode = (mode: boolean) => {
      this.rawModeHistory.push(mode);
      return input;
    };
    const output = new PassThrough();
    output.on('data', (chunk) => {
      this.text += chunk.toString('utf8');
      const notify = this.waiters.splice(0, this.waiters.length);
      for (const fn of notify) fn();
    });
    this.input = input;
    this.output = output;
    this.io = { input, output };
  }

  get allOutput(): string {
    return this.text;
  }

  async waitFor(needle: string): Promise<string> {
    const deadline = Date.now() + 4000;
    while (true) {
      const slice = this.text.slice(this.cursor);
      const idx = slice.indexOf(needle);
      if (idx >= 0) {
        this.cursor += idx + needle.length;
        return slice.slice(0, idx + needle.length);
      }
      if (Date.now() > deadline) {
        throw new Error(`Timed out waiting for prompt text ${JSON.stringify(needle)}. Output tail:\n${slice}`);
      }
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, 25);
        this.waiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
  }

  /** Capture output emitted after the current cursor up to `endNeedle`. */
  sliceSinceCursor(): string {
    return this.text.slice(this.cursor);
  }

  sendLine(line: string): void {
    this.input.write(`${line}\r`);
  }

  async typeCharByChar(text: string): Promise<void> {
    for (const ch of text) {
      this.input.write(ch);
      await new Promise((r) => setTimeout(r, 2));
    }
    this.input.write('\r');
  }
}

describe('prompt helpers and init wizard bugs', () => {
  it('parses skill selections strictly and rejects empty or conflicting choices', () => {
    expect(parseSkillSelection('claude')).toBe('claude');
    expect(parseSkillSelection('antigravity')).toBe('antigravity');
    expect(parseSkillSelection('claude,antigravity')).toBe('both');
    expect(parseSkillSelection('antigravity, claude')).toBe('both');
    expect(parseSkillSelection('both')).toBe('both');
    expect(parseSkillSelection('none')).toBe('none');
    expect(parseSkillSelection('')).toBeNull();
    expect(parseSkillSelection('   ')).toBeNull();
    expect(parseSkillSelection('unknown')).toBeNull();
    expect(parseSkillSelection('claude,none')).toBeNull();
    expect(parseSkillSelection('claude,')).toBeNull();
  });

  it('supports non-interactive --skill claude,antigravity --yes and --skill none --yes', async () => {
    const tmp1 = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-init-flags-'));
    const env1 = setupSampleEnv(tmp1);
    const out1 = new PassThrough();
    await runInitCommand(['--skill', 'claude,antigravity', '--yes', '--skip-auth'], {
      configDir: env1.configDir,
      homeDir: env1.homeDir,
      stdout: out1,
    });
    expect(fs.existsSync(path.join(env1.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(env1.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(true);

    const tmp2 = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-init-none-'));
    const env2 = setupSampleEnv(tmp2);
    const out2 = new PassThrough();
    await runInitCommand(['--skill', 'none', '--yes', '--skip-auth'], {
      configDir: env2.configDir,
      homeDir: env2.homeDir,
      stdout: out2,
    });
    expect(fs.existsSync(path.join(env2.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
    expect(fs.existsSync(path.join(env2.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
  });

  it('drains buffered stdin before asking so stray input never skips prompts', async () => {
    const tty = new FakeTtyHarness();
    // Simulate stray enter + text buffered in stdin before a question is shown.
    tty.input.write('\nstale buffered junk\n');
    expect(drainBufferedInput(tty.input)).toBeGreaterThan(0);

    // Also test askLine drains right before asking even without manual drain:
    tty.input.write('pre-buffered-line\n');
    const pending = askLine('Question? ', tty.io);
    await tty.waitFor('Question? ');
    tty.sendLine('real-answer');
    const answer = await pending;
    expect(answer).toBe('real-answer');
  });

  it('echoes TOTP digits once (no double readline echo) and keeps hidden password un-echoed', async () => {
    const tty = new FakeTtyHarness();
    const askPwd = askHidden('Password: ', tty.io);
    await tty.waitFor('Password: ');
    await tty.typeCharByChar('super-secret-password');
    const pwd = await askPwd;
    expect(pwd).toBe('super-secret-password');
    expect(tty.allOutput).not.toContain('super-secret');

    // Immediately follow with askLine (e.g. TOTP 6-digit confirmation)
    const askCode = askLine('Code: ', tty.io);
    await tty.waitFor('Code: ');
    await tty.typeCharByChar('482913');
    const code = await askCode;
    expect(code).toBe('482913');

    // Verify digits were echoed once cleanly, NOT doubled as 448822991133
    const codeRegion = tty.sliceSinceCursor();
    expect(codeRegion).toContain('482913');
    expect(codeRegion).not.toContain('448822991133');
    expect(tty.rawModeHistory.at(-1)).toBe(false);
  });

  it('runs interactive fake TTY init (drains stray input, validates/re-asks/confirms skill) and idempotent re-run without double echo', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-tty-'));
    const env = setupSampleEnv(tmp);

    // --- Pass 1: fresh interactive setup over fake TTY ---
    const tty1 = new FakeTtyHarness();
    // Inject stray buffered newline before starting wizard
    tty1.input.write('\n\n');

    const run1 = runInitCommand([], {
      configDir: env.configDir,
      homeDir: env.homeDir,
      io: tty1.io,
    });

    await tty1.waitFor('Maestri data directory');
    // Empty enter should NOT silently succeed — it must re-ask with an error
    tty1.sendLine('');
    await tty1.waitFor('Please enter a path or "default"');
    await tty1.waitFor('Maestri data directory');
    tty1.sendLine('default');

    await tty1.waitFor('Add detected workspaces to workspaces.json?');
    tty1.sendLine('y');

    await tty1.waitFor('New password (min 12 characters): ');
    await tty1.typeCharByChar('maestria-test-pass-123');
    await tty1.waitFor('Repeat password: ');
    await tty1.typeCharByChar('maestria-test-pass-123');

    await tty1.waitFor('Or enter the key manually: ');
    const afterManual = await tty1.waitFor('Enter the 6-digit code shown by the app to confirm: ');
    const secretMatch = /([A-Z2-7]{16,})/.exec(afterManual);
    expect(secretMatch).not.toBeNull();
    const secret1 = secretMatch![1]!;
    const code1 = totp(secret1, Date.now());
    await tty1.typeCharByChar(code1);
    const codeEcho1 = await tty1.waitFor('Done. Credentials saved');
    const doubledCode1 = code1
      .split('')
      .map((c) => c + c)
      .join('');
    expect(codeEcho1).toContain(code1);
    expect(codeEcho1).not.toContain(doubledCode1);

    // Inject stray line right before the skill prompt; verify it is drained and ignored!
    tty1.input.write('\nbogus-prebuffered-input\n');

    await tty1.waitFor('Install Maestro protocol skill for');
    // Empty input must re-ask
    tty1.sendLine('');
    await tty1.waitFor('Invalid or empty choice.');
    await tty1.waitFor('Install Maestro protocol skill for');
    // Invalid input must re-ask
    tty1.sendLine('bogus');
    await tty1.waitFor('Invalid or empty choice.');
    await tty1.waitFor('Install Maestro protocol skill for');
    // Pick claude,antigravity, then say 'n' to confirmation to verify re-prompt loop
    tty1.sendLine('claude,antigravity');
    await tty1.waitFor('Confirm: install Maestro protocol skill for claude, antigravity?');
    tty1.sendLine('n');
    await tty1.waitFor('Install Maestro protocol skill for');
    // Finally choose only 'claude' and confirm 'y'
    tty1.sendLine('claude');
    await tty1.waitFor('Confirm: install Maestro protocol skill for claude?');
    tty1.sendLine('y');

    await run1;

    expect(loadRawConfig(env.configDir).maestriDir).toBeUndefined();
    expect(loadConfig(env.configDir, { MAESTRI_HOME: env.maestriDir }).maestriDir).toBe(env.maestriDir);
    expect(loadSecrets(env.configDir)?.totpSecret).toBe(secret1);
    expect(fs.existsSync(path.join(env.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(env.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
    expect(tty1.allOutput).not.toContain('maestria-test-pass-123');

    // --- Pass 2: idempotent re-run (keep config, workspaces, skill; redo auth and verify NO double echo on TOTP) ---
    const tty2 = new FakeTtyHarness();
    const run2 = runInitCommand([], {
      configDir: env.configDir,
      homeDir: env.homeDir,
      io: tty2.io,
    });

    await tty2.waitFor('Keep existing config or redo?');
    tty2.sendLine('keep');

    await tty2.waitFor('Keep existing workspaces.json or redo from discovery?');
    tty2.sendLine('keep');

    await tty2.waitFor('Keep existing credentials or redo password & TOTP?');
    tty2.sendLine('redo');

    await tty2.waitFor('New password (min 12 characters): ');
    await tty2.typeCharByChar('maestria-new-pass-456');
    await tty2.waitFor('Repeat password: ');
    await tty2.typeCharByChar('maestria-new-pass-456');

    await tty2.waitFor('Or enter the key manually: ');
    const afterManual2 = await tty2.waitFor('Enter the 6-digit code shown by the app to confirm: ');
    const secretMatch2 = /([A-Z2-7]{16,})/.exec(afterManual2);
    expect(secretMatch2).not.toBeNull();
    const secret2 = secretMatch2![1]!;
    expect(secret2).not.toBe(secret1);
    const code2 = totp(secret2, Date.now());

    await tty2.typeCharByChar(code2);
    const codeEcho2 = await tty2.waitFor('Done. Credentials saved');
    const doubledCode2 = code2
      .split('')
      .map((c) => c + c)
      .join('');
    expect(codeEcho2).toContain(code2);
    expect(codeEcho2).not.toContain(doubledCode2);

    await tty2.waitFor('Keep existing Maestro skill(s) or redo?');
    tty2.sendLine('keep');

    await run2;

    expect(loadSecrets(env.configDir)?.totpSecret).toBe(secret2);
    expect(fs.existsSync(path.join(env.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(env.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
    expect(tty2.allOutput).not.toContain('maestria-new-pass-456');
  });

  it('reproduces existing config + existing auth (no workspaces) choosing redo -> askHidden without deadlock', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-auth-redo-'));
    const homeDir = path.join(tmp, 'home');
    const configDir = path.join(tmp, 'cfg');
    const maestriDir = path.join(homeDir, '.maestri');
    fs.mkdirSync(path.join(maestriDir, 'usage', 'providers'), { recursive: true });
    fs.writeFileSync(path.join(maestriDir, 'usage', 'providers', '.status.json'), '{"providers":[]}');
    fs.mkdirSync(configDir, { recursive: true });
    fs.writeFileSync(path.join(configDir, 'config.json'), '{}\n');
    saveSecrets(configDir, {
      passwordHash: hashPassword('old-password-12345'),
      totpSecret: 'JBSWY3DPEHPK3PXP',
      createdAt: '2026-01-01T00:00:00Z',
    });

    const tty = new FakeTtyHarness();
    const run = runInitCommand([], { configDir, homeDir, io: tty.io });

    await tty.waitFor('Keep existing config or redo? [keep/redo, default: keep]: ');
    tty.sendLine('keep');

    await tty.waitFor('Keep existing credentials or redo password & TOTP? [keep/redo, default: keep]: ');
    // Pre-pause check + redo -> immediately transitions to askHidden('New password...')
    tty.sendLine('redo');

    await tty.waitFor('New password (min 12 characters): ');
    expect(tty.input.isPaused()).toBe(false);
    expect(isPromptInFlight()).toBe(true);
    await tty.typeCharByChar('updated-pass-98765');

    await tty.waitFor('Repeat password: ');
    await tty.typeCharByChar('updated-pass-98765');

    await tty.waitFor('Or enter the key manually: ');
    const afterManual = await tty.waitFor('Enter the 6-digit code shown by the app to confirm: ');
    const secret = /([A-Z2-7]{16,})/.exec(afterManual)![1]!;
    await tty.typeCharByChar(totp(secret, Date.now()));

    await tty.waitFor('Install Maestro protocol skill for');
    tty.sendLine('none');
    await tty.waitFor('Confirm: skip installing Maestro protocol skill?');
    tty.sendLine('y');

    await run;
    expect(isPromptInFlight()).toBe(false);
    expect(loadSecrets(configDir)?.totpSecret).toBe(secret);
    expect(tty.allOutput).not.toContain('updated-pass-98765');
  });

  it('watchdog resumes paused TTY, ensures question is printed, and releases lock on stream close/error', async () => {
    const tty = new FakeTtyHarness();
    const events: WatchdogEvent[] = [];
    tty.io.watchdogMs = 25;
    tty.io.onWatchdog = (ev) => events.push(ev);

    // Step 1: askKeepOrRedo -> 'redo', followed by askHidden where stdin gets paused before watchdog fires
    const p1 = askKeepOrRedo('Keep existing credentials or redo? [keep/redo]: ', tty.io);
    await tty.waitFor('Keep existing credentials or redo? [keep/redo]: ');
    tty.sendLine('redo');
    expect(await p1).toBe('redo');

    const p2 = askHidden('New password (min 12 characters): ', tty.io);
    await tty.waitFor('New password (min 12 characters): ');
    // Simulate stdin stuck paused before user types
    tty.input.pause();
    expect(tty.input.isPaused()).toBe(true);

    // Wait past watchdogMs -> watchdog must fire, resume stdin, and re-print question
    await tty.waitFor('\nNew password (min 12 characters): ');
    expect(events.length).toBe(1);
    expect(events[0]).toMatchObject({ question: 'New password (min 12 characters): ', wasPaused: true });
    expect(tty.input.isPaused()).toBe(false);

    await tty.typeCharByChar('watchdog-pass-1234');
    expect(await p2).toBe('watchdog-pass-1234');
    expect(isPromptInFlight()).toBe(false);

    // Step 2: if stream closes mid-prompt, promptInFlight lock must still reset so next prompt works
    const ttyErr = new FakeTtyHarness();
    const aborted = askLine('Will EOF: ', ttyErr.io);
    await ttyErr.waitFor('Will EOF: ');
    ttyErr.input.destroy();
    await expect(aborted).rejects.toThrow(/Input stream closed/);
    expect(isPromptInFlight()).toBe(false);
  });
});
