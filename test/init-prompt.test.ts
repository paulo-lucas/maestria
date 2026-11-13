import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { hashPassword, verifyPassword } from '../src/auth/password.ts';
import { loadSecrets, saveSecrets } from '../src/auth/secrets.ts';
import { totp } from '../src/auth/totp.ts';
import { parseSkillSelection, runInitCommand, skillTargetFromCheckboxes } from '../src/commands/init.ts';
import { askCheckbox, askConfirm, askHidden, askKeepOrRedo, askLine, drainBufferedInput, type PromptInput, type PromptIO } from '../src/commands/prompt.ts';
import { loadConfig, loadRawConfig } from '../src/config.ts';

function stripAnsi(s: string): string {
  return s.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, '');
}

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
    return stripAnsi(this.text);
  }

  async waitFor(needle: string): Promise<string> {
    const deadline = Date.now() + 5000;
    while (true) {
      const slice = stripAnsi(this.text.slice(this.cursor));
      const idx = slice.indexOf(needle);
      if (idx >= 0) {
        // Advance raw cursor safely past what we matched in stripped view by snapping to text.length
        this.cursor = this.text.length;
        return slice;
      }
      if (Date.now() > deadline) {
        throw new Error(`Timed out waiting for prompt text ${JSON.stringify(needle)}. Output tail:\n${slice}`);
      }
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, 20);
        this.waiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
  }

  sendLine(line: string): void {
    this.input.write(`${line}\r`);
  }

  async pressEnter(): Promise<void> {
    this.input.write('\r');
    await new Promise((r) => setTimeout(r, 10));
  }

  async pressDown(): Promise<void> {
    this.input.write('\x1b[B');
    await new Promise((r) => setTimeout(r, 10));
  }

  async pressSpace(): Promise<void> {
    this.input.write(' ');
    await new Promise((r) => setTimeout(r, 10));
  }

  async typeCharByChar(text: string): Promise<void> {
    for (const ch of text) {
      this.input.write(ch);
      await new Promise((r) => setTimeout(r, 3));
    }
    this.input.write('\r');
  }
}

describe('interactive @inquirer/prompts wizard, flags, and fake TTY flows', () => {
  it('parses skill flags and checkbox target combinations strictly', () => {
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

    expect(skillTargetFromCheckboxes([])).toBe('none');
    expect(skillTargetFromCheckboxes(['claude'])).toBe('claude');
    expect(skillTargetFromCheckboxes(['antigravity'])).toBe('antigravity');
    expect(skillTargetFromCheckboxes(['claude', 'antigravity'])).toBe('both');
  });

  it('supports non-interactive --skill claude,antigravity --yes and --skill none --yes', async () => {
    const tmp1 = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-init-flags-'));
    const env1 = setupSampleEnv(tmp1);
    await runInitCommand(['--skill', 'claude,antigravity', '--yes', '--skip-auth'], {
      configDir: env1.configDir,
      homeDir: env1.homeDir,
      stdout: new PassThrough(),
    });
    expect(fs.existsSync(path.join(env1.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(env1.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(true);

    const tmp2 = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-init-none-'));
    const env2 = setupSampleEnv(tmp2);
    await runInitCommand(['--skill', 'none', '--yes', '--skip-auth'], {
      configDir: env2.configDir,
      homeDir: env2.homeDir,
      stdout: new PassThrough(),
    });
    expect(fs.existsSync(path.join(env2.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
    expect(fs.existsSync(path.join(env2.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
  });

  it('drains buffered stdin before showing interactive prompts and handles single-echo input + masked password', async () => {
    const tty = new FakeTtyHarness();
    tty.input.write('\nstale buffered junk\n');
    expect(drainBufferedInput(tty.input)).toBeGreaterThan(0);

    tty.input.write('pre-buffered\n');
    const keepPromise = askKeepOrRedo('Configuration', tty.io);
    await tty.waitFor('Configuration');
    // Default ('keep') is preselected: pressing Enter picks 'keep'
    await tty.pressEnter();
    expect(await keepPromise).toBe('keep');

    const pwPromise = askHidden('New password:', tty.io);
    await tty.waitFor('New password:');
    await tty.typeCharByChar('super-secret-password');
    expect(await pwPromise).toBe('super-secret-password');
    expect(tty.allOutput).not.toContain('super-secret');

    const codePromise = askLine('Enter code:', tty.io);
    await tty.waitFor('Enter code:');
    await tty.typeCharByChar('482913');
    expect(await codePromise).toBe('482913');
    const out = tty.allOutput;
    expect(out).toContain('482913');
    expect(out).not.toContain('448822991133');

    const cbPromise = askCheckbox<'a' | 'b'>(
      'Pick targets:',
      [
        { name: 'Alpha', value: 'a', checked: true },
        { name: 'Beta', value: 'b', checked: true },
      ],
      tty.io,
    );
    await tty.waitFor('Pick targets:');
    await tty.pressDown();
    await tty.pressSpace(); // untoggle Beta
    await tty.pressEnter();
    expect(await cbPromise).toEqual(['a']);

    const confPromise = askConfirm('Continue?', { defaultValue: false, io: tty.io });
    await tty.waitFor('Continue?');
    tty.sendLine('y');
    expect(await confPromise).toBe(true);
  });

  it('runs fresh TTY wizard (arrow select, y/N confirm, auth, checkbox skill toggle) and idempotent re-run', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-tty-'));
    const env = setupSampleEnv(tmp);

    // --- Pass 1: fresh interactive setup over fake TTY ---
    const tty1 = new FakeTtyHarness();
    tty1.input.write('\n\n'); // buffered Enter must be drained

    const run1 = runInitCommand([], {
      configDir: env.configDir,
      homeDir: env.homeDir,
      io: tty1.io,
    });

    // Maestri data directory select menu (Use default preselected) -> Enter
    await tty1.waitFor('Maestri data directory:');
    await tty1.pressEnter();

    // Workspace confirm (Y/n) -> Enter accepts yes
    await tty1.waitFor('Add detected workspaces to workspaces.json?');
    await tty1.pressEnter();

    // Password + Repeat + TOTP
    await tty1.waitFor('New password (at least 12 characters):');
    await tty1.typeCharByChar('maestria-test-pass-123');
    await tty1.waitFor('Repeat password:');
    await tty1.typeCharByChar('maestria-test-pass-123');

    await tty1.waitFor('Enter the 6-digit code shown by the app to confirm:');
    const secret1 = /Or enter the key manually: ([A-Z2-7]{16,})/.exec(tty1.allOutput)![1]!;
    const code1 = totp(secret1, Date.now());
    await tty1.typeCharByChar(code1);
    const codeEcho1 = await tty1.waitFor('Done. Credentials saved');
    expect(codeEcho1).toContain(code1);
    expect(codeEcho1).not.toContain(
      code1
        .split('')
        .map((c) => c + c)
        .join(''),
    );

    // Inject stray line before checkbox; must be drained so both default-checked options aren't auto-accepted
    tty1.input.write('\nbogus-prebuffered-input\n');
    await tty1.waitFor('Install Maestro protocol skill for:');
    // Uncheck Antigravity (down + space), confirm only Claude Code
    await tty1.pressDown();
    await tty1.pressSpace();
    await tty1.pressEnter();

    // Decline confirm first ('n'), verify checkbox re-appears, then accept on second pass
    await tty1.waitFor('Install Maestro protocol skill for claude?');
    tty1.sendLine('n');
    await tty1.waitFor('Install Maestro protocol skill for:');
    await tty1.pressDown();
    await tty1.pressSpace();
    await tty1.pressEnter();
    await tty1.waitFor('Install Maestro protocol skill for claude?');
    tty1.sendLine('y');

    await run1;

    expect(loadRawConfig(env.configDir).maestriDir).toBeUndefined();
    expect(loadConfig(env.configDir, { MAESTRI_HOME: env.maestriDir }).maestriDir).toBe(env.maestriDir);
    expect(loadSecrets(env.configDir)?.totpSecret).toBe(secret1);
    expect(fs.existsSync(path.join(env.homeDir, '.claude', 'skills', 'maestria', 'SKILL.md'))).toBe(true);
    expect(fs.existsSync(path.join(env.homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md'))).toBe(false);
    expect(tty1.allOutput).not.toContain('maestria-test-pass-123');

    // --- Pass 2: idempotent re-run via arrow keys (keep config/workspaces/skill; down-arrow -> redo auth) ---
    const tty2 = new FakeTtyHarness();
    const run2 = runInitCommand([], {
      configDir: env.configDir,
      homeDir: env.homeDir,
      io: tty2.io,
    });

    await tty2.waitFor('Configuration');
    await tty2.pressEnter(); // Keep preselected

    await tty2.waitFor('Workspaces');
    await tty2.pressEnter(); // Keep preselected

    await tty2.waitFor('Credentials');
    await tty2.pressDown(); // Move to Reset password & TOTP
    await tty2.pressEnter();

    await tty2.waitFor('New password (at least 12 characters):');
    await tty2.typeCharByChar('maestria-new-pass-456');
    await tty2.waitFor('Repeat password:');
    await tty2.typeCharByChar('maestria-new-pass-456');

    await tty2.waitFor('Enter the 6-digit code shown by the app to confirm:');
    const secret2 = /Or enter the key manually: ([A-Z2-7]{16,})/.exec(tty2.allOutput)![1]!;
    expect(secret2).not.toBe(secret1);
    const code2 = totp(secret2, Date.now());
    await tty2.typeCharByChar(code2);
    const codeEcho2 = await tty2.waitFor('Done. Credentials saved');
    expect(codeEcho2).toContain(code2);
    expect(codeEcho2).not.toContain(
      code2
        .split('')
        .map((c) => c + c)
        .join(''),
    );

    await tty2.waitFor('Maestro protocol skill');
    await tty2.pressEnter(); // Keep preselected

    await run2;

    expect(loadSecrets(env.configDir)?.totpSecret).toBe(secret2);
    expect(tty2.allOutput).not.toContain('maestria-new-pass-456');
  });

  it('handles existing config + existing auth (no workspaces) choosing redo via arrow menu -> password without hang', async () => {
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

    await tty.waitFor('Configuration');
    await tty.pressEnter(); // Keep existing config

    await tty.waitFor('Credentials');
    await tty.pressDown(); // Reset password & TOTP
    await tty.pressEnter();

    await tty.waitFor('New password (at least 12 characters):');
    await tty.typeCharByChar('updated-pass-98765');
    await tty.waitFor('Repeat password:');
    await tty.typeCharByChar('updated-pass-98765');

    await tty.waitFor('Enter the 6-digit code shown by the app to confirm:');
    const secret = /Or enter the key manually: ([A-Z2-7]{16,})/.exec(tty.allOutput)![1]!;
    await tty.typeCharByChar(totp(secret, Date.now()));

    // Uncheck both skills via space toggles -> None -> confirm skip
    await tty.waitFor('Install Maestro protocol skill for:');
    await tty.pressSpace(); // untoggle Claude Code
    await tty.pressDown();
    await tty.pressSpace(); // untoggle Antigravity
    await tty.pressEnter();

    await tty.waitFor('Skip installing Maestro protocol skill?');
    tty.sendLine('y');

    await run;
    expect(loadSecrets(configDir)?.totpSecret).toBe(secret);
    expect(tty.allOutput).not.toContain('updated-pass-98765');
  });

  it('validates short password, confirmation mismatch, and invalid TOTP inline in init without throwing', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-pw-reask-'));
    const env = setupSampleEnv(tmp);
    const tty = new FakeTtyHarness();

    const run = runInitCommand([], {
      configDir: env.configDir,
      homeDir: env.homeDir,
      io: tty.io,
    });

    await tty.waitFor('Maestri data directory:');
    await tty.pressEnter();
    await tty.waitFor('Add detected workspaces to workspaces.json?');
    await tty.pressEnter();

    await tty.waitFor('New password (at least 12 characters):');
    // Too short (< 12 chars): must report rule and re-ask cleanly without throwing
    await tty.typeCharByChar('short');
    await tty.waitFor('Password must be at least 12 characters (entered 5).');

    await tty.waitFor('New password (at least 12 characters):');
    await tty.typeCharByChar('shorter-valid-123');

    await tty.waitFor('Repeat password:');
    // Mismatched repeat password: must report mismatch and re-ask cleanly
    await tty.typeCharByChar('wrong-match');
    await tty.waitFor('The passwords do not match. Please try again.');

    await tty.waitFor('New password (at least 12 characters):');
    await tty.typeCharByChar('shorter-valid-123');
    await tty.waitFor('Repeat password:');
    await tty.typeCharByChar('shorter-valid-123');

    await tty.waitFor('Enter the 6-digit code shown by the app to confirm:');
    const secret = /Or enter the key manually: ([A-Z2-7]{16,})/.exec(tty.allOutput)![1]!;
    const validCode = totp(secret, Date.now());
    const bogusCode = validCode === '000000' ? '111111' : '000000';
    await tty.typeCharByChar(bogusCode);
    await tty.waitFor('That code is not valid');
    await tty.waitFor('Enter the 6-digit code shown by the app to confirm:');
    await tty.typeCharByChar(validCode);

    await tty.waitFor('Install Maestro protocol skill for:');
    await tty.pressSpace();
    await tty.pressDown();
    await tty.pressSpace();
    await tty.pressEnter();
    await tty.waitFor('Skip installing Maestro protocol skill?');
    tty.sendLine('y');

    await run;

    const saved = loadSecrets(env.configDir);
    expect(saved?.totpSecret).toBe(secret);
    expect(verifyPassword('shorter-valid-123', saved!.passwordHash)).toBe(true);
    expect(tty.allOutput).not.toContain('shorter-valid-123');
    expect(tty.allOutput).not.toContain('wrong-match');
  });

  it('prints a friendly error message without stack traces on unexpected CLI errors', () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-cli-err-'));
    const cliPath = path.resolve(import.meta.dirname, '..', 'src', 'cli.ts');
    const res = spawnSync(process.execPath, ['--experimental-transform-types', cliPath, 'auth', 'setup'], {
      env: { ...process.env, MAESTRIA_CONFIG_DIR: tmp },
      input: '',
      encoding: 'utf8',
    });
    expect(res.status).toBe(1);
    expect(res.stderr).toContain('Error: A terminal is required to enter the password.');
    expect(res.stderr).not.toContain('at authSetup');
    expect(res.stderr).not.toContain('.ts:');
  });
});
