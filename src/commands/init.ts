import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { authFile } from '../auth/secrets.ts';
import {
  cloneStandbyThresholds,
  ConfigError,
  configFilePath,
  DEFAULT_RESUME_BELOW_PERCENT,
  DEFAULT_STANDBY_THRESHOLDS,
  ensureConfigDir,
  inspectMaestriDir,
  loadConfig,
  loadRawConfig,
  resolveMaestriDir,
  saveRawConfig,
} from '../config.ts';
import { discoverMaestriWorkspaces } from '../discovery.ts';
import {
  installService,
  isServiceInstalled,
  isServicePlatformSupported,
  restartService,
  ServiceError,
  startService,
  type ServiceCommandRunner,
  type ServiceOpts,
} from '../service.ts';
import { loadWorkspaces, saveWorkspaces, type Workspace } from '../workspaces.ts';
import { ArgError, parseArgs } from './args.ts';
import { authSetup, type AuthSetupOpts } from './auth-setup.ts';
import { askCheckbox, askConfirm, askKeepOrRedo, askLine, askSelect, defaultPromptIO, type PromptIO } from './prompt.ts';

export interface InitCliOpts {
  configDir: string;
  env?: NodeJS.ProcessEnv;
  homeDir?: string;
  io?: PromptIO;
  stdout?: NodeJS.WritableStream;
  authSetupFn?: (opts: AuthSetupOpts) => Promise<void>;
  servicePlatform?: NodeJS.Platform;
  serviceRunner?: ServiceCommandRunner;
  serviceCliPath?: string;
}

const PACKAGE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const BUNDLED_SKILL_PATH = path.join(PACKAGE_ROOT, 'skills', 'maestria-maestro', 'SKILL.md');

export type SkillTarget = 'claude' | 'antigravity' | 'both' | 'none';
export type SkillAgent = 'claude' | 'antigravity';

export function resolveAntigravitySkillDir(homeDir: string): string {
  const configSkills = path.join(homeDir, '.gemini', 'config', 'skills');
  if (fs.existsSync(configSkills)) return path.join(configSkills, 'maestria');
  return path.join(homeDir, '.gemini', 'antigravity', 'skills', 'maestria');
}

export function claudeSkillFilePath(homeDir: string = os.homedir()): string {
  return path.join(homeDir, '.claude', 'skills', 'maestria', 'SKILL.md');
}

export function antigravitySkillFilePath(homeDir: string = os.homedir()): string {
  return path.join(resolveAntigravitySkillDir(homeDir), 'SKILL.md');
}

export function parseSkillSelection(raw: string, allowShort = false): SkillTarget | null {
  const trimmed = raw.trim().toLowerCase();
  if (!trimmed) return null;

  const tokens = trimmed.split(',').map((t) => t.trim());
  if (tokens.some((t) => !t)) return null;

  let claude = false;
  let antigravity = false;
  let none = false;

  for (const token of tokens) {
    if (token === 'claude' || (allowShort && token === 'c')) claude = true;
    else if (token === 'antigravity' || (allowShort && token === 'a')) antigravity = true;
    else if (token === 'both' || token === 'all' || (allowShort && token === 'b')) {
      claude = true;
      antigravity = true;
    } else if (token === 'none' || (allowShort && token === 'n')) {
      none = true;
    } else {
      return null;
    }
  }

  if (none) {
    return claude || antigravity ? null : 'none';
  }
  return skillTargetFromCheckboxes([
    ...(claude ? (['claude'] as const) : []),
    ...(antigravity ? (['antigravity'] as const) : []),
  ]);
}

export function skillTargetFromCheckboxes(selected: readonly SkillAgent[]): SkillTarget {
  const hasClaude = selected.includes('claude');
  const hasAg = selected.includes('antigravity');
  if (hasClaude && hasAg) return 'both';
  if (hasClaude) return 'claude';
  if (hasAg) return 'antigravity';
  return 'none';
}

export function formatSkillTarget(target: SkillTarget): string {
  if (target === 'both') return 'claude, antigravity';
  return target;
}

export function installMaestroSkill(
  target: SkillTarget,
  homeDir: string = os.homedir(),
  skillSource: string = BUNDLED_SKILL_PATH,
): string[] {
  if (target === 'none') return [];
  const content = fs.readFileSync(skillSource, 'utf8');
  const installed: string[] = [];

  const installTo = (dir: string) => {
    fs.mkdirSync(dir, { recursive: true });
    const dest = path.join(dir, 'SKILL.md');
    fs.writeFileSync(dest, content, 'utf8');
    installed.push(dest);
  };

  if (target === 'claude' || target === 'both') {
    installTo(path.dirname(claudeSkillFilePath(homeDir)));
  }
  if (target === 'antigravity' || target === 'both') {
    installTo(resolveAntigravitySkillDir(homeDir));
  }
  return installed;
}

function writeUsageStatus(dirStatus: ReturnType<typeof inspectMaestriDir>, out: NodeJS.WritableStream): void {
  if (!dirStatus.hasDir) {
    out.write(
      `Warning: Maestri directory not found at "${dirStatus.dir}". Set "maestriDir" in config.json or MAESTRI_HOME if Maestri lives elsewhere.\n`,
    );
  } else if (!dirStatus.hasProvidersDir) {
    out.write(
      `Warning: "${dirStatus.providersDir}" not found (Agent Usage may be off in Maestri Settings -> Agents -> Usage; disabling it removes this folder).\n`,
    );
  } else if (!dirStatus.hasStatusFile) {
    out.write(
      `Note: "${dirStatus.providersDir}" found, waiting for Maestri to write .status.json while usage rings are active.\n`,
    );
  } else {
    out.write(`Verified Maestri usage providers at ${dirStatus.providersDir}\n`);
  }
}

async function promptMaestriDir(
  envDefault: string,
  env: NodeJS.ProcessEnv,
  homeDir: string,
  io: PromptIO,
): Promise<{ dir: string; custom: boolean }> {
  const mode = await askSelect<'default' | 'custom'>(
    'Maestri data directory:',
    [
      { name: `Use default (${envDefault})`, value: 'default' },
      { name: 'Enter a custom path', value: 'custom' },
    ],
    { defaultValue: 'default', io },
  );
  if (mode === 'default') {
    return { dir: envDefault, custom: false };
  }
  const entered = await askLine('Custom Maestri data directory path:', io, {
    validate: (raw) => {
      const trimmed = raw.trim();
      if (!trimmed) return 'Please enter a path (e.g. ~/.maestri or /path/to/.maestri).';
      try {
        resolveMaestriDir(trimmed, env, homeDir);
        return true;
      } catch (err) {
        return err instanceof ConfigError ? err.message : String(err);
      }
    },
  });
  const resolved = resolveMaestriDir(entered.trim(), env, homeDir);
  return { dir: resolved, custom: resolved !== envDefault };
}

async function resolveConfigStep(opts: {
  configDir: string;
  cliMaestriDir?: string;
  nonInteractive: boolean;
  env: NodeJS.ProcessEnv;
  homeDir: string;
  io: PromptIO;
}): Promise<string> {
  const { configDir, cliMaestriDir, nonInteractive, env, homeDir, io } = opts;
  const out = io.output;
  const hasConfigFile = fs.existsSync(configFilePath(configDir));
  const existingRaw = loadRawConfig(configDir);
  const configuredMaestriDir = typeof existingRaw.maestriDir === 'string' ? existingRaw.maestriDir : undefined;
  const envDefaultDir = resolveMaestriDir(undefined, env, homeDir);
  let maestriDir = resolveMaestriDir(cliMaestriDir ?? configuredMaestriDir, env, homeDir);

  if (cliMaestriDir) {
    const nextConfig: Record<string, unknown> = { ...existingRaw, maestriDir };
    saveRawConfig(configDir, nextConfig, env);
    writeUsageStatus(inspectMaestriDir(maestriDir), out);
    return maestriDir;
  }

  if (hasConfigFile) {
    const label = configuredMaestriDir ?? `${envDefaultDir} (default)`;
    out.write(`Existing configuration found at ${configFilePath(configDir)} (maestriDir: ${label}).\n`);
    if (nonInteractive) {
      writeUsageStatus(inspectMaestriDir(maestriDir), out);
      return maestriDir;
    }
    const action = await askKeepOrRedo('Configuration', io, {
      keep: `Keep existing config (${label})`,
      redo: 'Change Maestri data directory',
    });
    if (action === 'keep') {
      writeUsageStatus(inspectMaestriDir(maestriDir), out);
      return maestriDir;
    }
    const picked = await promptMaestriDir(envDefaultDir, env, homeDir, io);
    maestriDir = picked.dir;
    const nextConfig: Record<string, unknown> = { ...existingRaw };
    if (picked.custom) nextConfig.maestriDir = maestriDir;
    else delete nextConfig.maestriDir;
    saveRawConfig(configDir, nextConfig, env);
    writeUsageStatus(inspectMaestriDir(maestriDir), out);
    return maestriDir;
  }

  let custom = false;
  if (!nonInteractive) {
    const picked = await promptMaestriDir(envDefaultDir, env, homeDir, io);
    maestriDir = picked.dir;
    custom = picked.custom;
  }
  const nextConfig: Record<string, unknown> = { ...existingRaw };
  if (custom) nextConfig.maestriDir = maestriDir;
  saveRawConfig(configDir, nextConfig, env);
  writeUsageStatus(inspectMaestriDir(maestriDir), out);
  return maestriDir;
}

async function resolveWorkspacesStep(opts: {
  configDir: string;
  maestriDir: string;
  nonInteractive: boolean;
  homeDir: string;
  io: PromptIO;
}): Promise<void> {
  const { configDir, maestriDir, nonInteractive, homeDir, io } = opts;
  const out = io.output;
  const discovered = discoverMaestriWorkspaces(maestriDir, homeDir);
  const existingWs = loadWorkspaces(configDir);
  const existingPaths = new Set(existingWs.map((w) => path.resolve(w.path)));
  const newOnes = discovered.filter((d) => !existingPaths.has(path.resolve(d.path)));

  if (newOnes.length > 0) {
    out.write(`Detected ${newOnes.length} new Maestri workspace(s):\n`);
    for (const d of newOnes) {
      out.write(`  - ${d.name} (${d.path})${d.roadmapPath ? ` [roadmap: ${d.roadmapPath}]` : ''}\n`);
    }
    const importOk = nonInteractive
      ? true
      : await askConfirm('Add detected workspaces to workspaces.json?', { defaultValue: true, io });
    if (importOk) {
      const merged: Partial<Workspace>[] = [...existingWs, ...newOnes];
      saveWorkspaces(configDir, merged);
      out.write(`Saved ${merged.length} workspace(s) to workspaces.json.\n`);
    } else {
      out.write('Skipped adding newly detected workspaces.\n');
    }
    return;
  }

  if (existingWs.length === 0) {
    out.write('No Maestri workspaces found automatically. You can add them later with "maestria workspace add".\n');
    return;
  }

  out.write(`Existing workspaces.json has ${existingWs.length} configured workspace(s).\n`);
  if (!nonInteractive && discovered.length > 0) {
    const action = await askKeepOrRedo('Workspaces', io, {
      keep: `Keep existing workspaces (${existingWs.length})`,
      redo: `Replace from discovery (${discovered.length})`,
    });
    if (action === 'redo') {
      saveWorkspaces(configDir, discovered);
      out.write(`Replaced workspaces.json with ${discovered.length} discovered workspace(s).\n`);
    }
  }
}

function formatStandbySummary(standby: { thresholds: Record<string, Record<string, number>>; resumeBelowPercent: number | null }): string {
  const entries = Object.entries(standby.thresholds).filter(([, wins]) => Object.keys(wins).length > 0);
  if (entries.length === 0) return 'disabled';
  const parts = entries.map(([prov, wins]) => {
    const winText = Object.entries(wins)
      .map(([w, pct]) => `${w} ${pct}%`)
      .join(', ');
    return `${prov}: ${winText}`;
  });
  const resumeText =
    standby.resumeBelowPercent !== null
      ? `resume on reset or below ${standby.resumeBelowPercent}%`
      : 'resume on reset or below threshold';
  return `${parts.join(' | ')}; ${resumeText}`;
}

function validatePercentOrNone(raw: string, min: number, max: number): true | string {
  const t = raw.trim().toLowerCase();
  if (t === 'none' || t === 'off' || t === '') return true;
  if (!/^\d+$/.test(t)) return `Enter a whole number between ${min} and ${max}, or "none".`;
  const n = Number(t);
  if (!Number.isInteger(n) || n < min || n > max) {
    return `Value must be between ${min} and ${max} (or "none").`;
  }
  return true;
}

function parsePercentOrNone(raw: string): number | null {
  const t = raw.trim().toLowerCase();
  if (t === 'none' || t === 'off' || t === '') return null;
  return Number(t);
}

async function resolveStandbyStep(opts: {
  configDir: string;
  flagStandby?: string;
  nonInteractive: boolean;
  env: NodeJS.ProcessEnv;
  io: PromptIO;
}): Promise<void> {
  const { configDir, flagStandby, nonInteractive, env, io } = opts;
  const out = io.output;
  const existingRaw = loadRawConfig(configDir);
  const existingStandby =
    typeof existingRaw.standby === 'object' && existingRaw.standby !== null && !Array.isArray(existingRaw.standby)
      ? (existingRaw.standby as Record<string, unknown>)
      : null;

  const saveStandbyConfig = (
    thresholds: Record<string, Record<string, number>>,
    resumeBelowPercent: number | null,
  ) => {
    const nextRaw: Record<string, unknown> = {
      ...existingRaw,
      standby: {
        ...(existingStandby ?? {}),
        thresholds,
        resumeBelowPercent,
      },
    };
    saveRawConfig(configDir, nextRaw, env);
  };

  if (typeof flagStandby === 'string') {
    const mode = flagStandby.trim().toLowerCase();
    if (mode === 'default') {
      const thresholds = cloneStandbyThresholds(DEFAULT_STANDBY_THRESHOLDS);
      saveStandbyConfig(thresholds, DEFAULT_RESUME_BELOW_PERCENT);
      out.write(`Standby thresholds set to defaults (${formatStandbySummary({ thresholds, resumeBelowPercent: DEFAULT_RESUME_BELOW_PERCENT })}).\n`);
      return;
    }
    if (mode === 'none' || mode === 'disable' || mode === 'off') {
      saveStandbyConfig({}, null);
      out.write('Usage standby disabled (--standby none).\n');
      return;
    }
    throw new ArgError('--standby must be "default" or "none"');
  }

  if (existingStandby && existingStandby.thresholds !== undefined) {
    const current = loadConfig(configDir, env).standby;
    const summary = formatStandbySummary(current);
    out.write(`Existing standby thresholds (${summary}).\n`);
    if (nonInteractive) return;
    const action = await askKeepOrRedo('Standby thresholds', io, {
      keep: `Keep existing standby (${summary})`,
      redo: 'Change standby thresholds',
    });
    if (action === 'keep') return;
  } else if (nonInteractive) {
    const thresholds = cloneStandbyThresholds(DEFAULT_STANDBY_THRESHOLDS);
    saveStandbyConfig(thresholds, DEFAULT_RESUME_BELOW_PERCENT);
    out.write(`Configured default standby thresholds (${formatStandbySummary({ thresholds, resumeBelowPercent: DEFAULT_RESUME_BELOW_PERCENT })}).\n`);
    return;
  }

  const mode = await askSelect<'default' | 'custom' | 'disable'>(
    'Usage standby thresholds (auto-pause agents near quota limits):',
    [
      {
        name: 'Use defaults (claude: session 95%, weekly 90%; resume on reset or below 80%)',
        value: 'default',
      },
      { name: 'Customize provider/window thresholds', value: 'custom' },
      { name: 'Disable automatic standby', value: 'disable' },
    ],
    { defaultValue: 'default', io },
  );

  if (mode === 'default') {
    const thresholds = cloneStandbyThresholds(DEFAULT_STANDBY_THRESHOLDS);
    saveStandbyConfig(thresholds, DEFAULT_RESUME_BELOW_PERCENT);
    out.write(`Standby thresholds confirmed (${formatStandbySummary({ thresholds, resumeBelowPercent: DEFAULT_RESUME_BELOW_PERCENT })}).\n`);
    return;
  }

  if (mode === 'disable') {
    saveStandbyConfig({}, null);
    out.write('Usage standby disabled.\n');
    return;
  }

  const provider = (
    await askLine('Provider ID to guard (e.g. claude, antigravity, codex):', io, {
      defaultValue: 'claude',
      validate: (raw) =>
        /^[a-z0-9*][a-z0-9._:-]{0,63}$/i.test(raw.trim()) ? true : 'Enter a valid provider ID (letters, digits, ., _, :, -).',
    })
  )
    .trim()
    .toLowerCase();

  const sessionRaw = await askLine(`Session window limit % for ${provider} (1-100, or "none"):`, io, {
    defaultValue: '95',
    validate: (raw) => validatePercentOrNone(raw, 1, 100),
  });
  const weeklyRaw = await askLine(`Weekly window limit % for ${provider} (1-100, or "none"):`, io, {
    defaultValue: '90',
    validate: (raw) => validatePercentOrNone(raw, 1, 100),
  });
  const resumeRaw = await askLine('Resume below % before reset (0-99, or "none" for reset-only):', io, {
    defaultValue: '80',
    validate: (raw) => validatePercentOrNone(raw, 0, 99),
  });

  const providerRules: Record<string, number> = {};
  const sessionPct = parsePercentOrNone(sessionRaw);
  const weeklyPct = parsePercentOrNone(weeklyRaw);
  if (sessionPct !== null) providerRules.session = sessionPct;
  if (weeklyPct !== null) providerRules.weekly = weeklyPct;

  const baseThresholds = existingStandby ? loadConfig(configDir, env).standby.thresholds : {};
  const nextThresholds = cloneStandbyThresholds(baseThresholds);
  if (Object.keys(providerRules).length > 0) {
    nextThresholds[provider] = providerRules;
  } else {
    delete nextThresholds[provider];
  }
  const resumeBelowPercent = parsePercentOrNone(resumeRaw);

  saveStandbyConfig(nextThresholds, resumeBelowPercent);
  out.write(`Standby thresholds saved (${formatStandbySummary({ thresholds: nextThresholds, resumeBelowPercent })}).\n`);
}

async function resolveAuthStep(opts: {
  configDir: string;
  skipAuth: boolean;
  nonInteractive: boolean;
  io: PromptIO;
  authSetupFn: (opts: AuthSetupOpts) => Promise<void>;
}): Promise<void> {
  const { configDir, skipAuth, nonInteractive, io, authSetupFn } = opts;
  const out = io.output;
  if (skipAuth) {
    out.write('Skipping auth setup (--skip-auth).\n');
    return;
  }

  const hasAuth = fs.existsSync(authFile(configDir));
  if (hasAuth) {
    out.write(`Existing credentials found (${authFile(configDir)}).\n`);
    if (nonInteractive) {
      out.write('Keeping existing credentials.\n');
      return;
    }
    const action = await askKeepOrRedo('Credentials', io, {
      keep: 'Keep existing credentials',
      redo: 'Reset password & TOTP',
    });
    if (action === 'keep') return;
    await authSetupFn({ configDir, force: true, io });
    return;
  }

  if (nonInteractive) {
    out.write('No credentials configured yet; run "maestria auth setup" interactively to set password & TOTP.\n');
    return;
  }

  await authSetupFn({ configDir, force: false, io });
}

function findInstalledSkills(homeDir: string): { label: SkillAgent; path: string }[] {
  const found: { label: SkillAgent; path: string }[] = [];
  const claude = claudeSkillFilePath(homeDir);
  if (fs.existsSync(claude)) found.push({ label: 'claude', path: claude });
  const ag = antigravitySkillFilePath(homeDir);
  if (fs.existsSync(ag)) found.push({ label: 'antigravity', path: ag });
  return found;
}

async function promptSkillChoice(
  homeDir: string,
  existingSkills: readonly { label: SkillAgent; path: string }[],
  io: PromptIO,
): Promise<SkillTarget> {
  const hasClaude = existingSkills.some((s) => s.label === 'claude');
  const hasAg = existingSkills.some((s) => s.label === 'antigravity');
  const defaultAll = existingSkills.length === 0;

  while (true) {
    const selected = await askCheckbox<SkillAgent>(
      'Install Maestro protocol skill for:',
      [
        {
          name: `Claude Code (${claudeSkillFilePath(homeDir)})`,
          value: 'claude',
          checked: hasClaude || defaultAll,
        },
        {
          name: `Antigravity (${antigravitySkillFilePath(homeDir)})`,
          value: 'antigravity',
          checked: hasAg || defaultAll,
        },
      ],
      io,
    );

    const choice = skillTargetFromCheckboxes(selected);
    const confirmMsg =
      choice === 'none'
        ? 'Skip installing Maestro protocol skill?'
        : `Install Maestro protocol skill for ${formatSkillTarget(choice)}?`;
    const confirmed = await askConfirm(confirmMsg, { defaultValue: true, io });
    if (confirmed) return choice;
  }
}

async function resolveSkillStep(opts: {
  flagSkill?: string;
  nonInteractive: boolean;
  homeDir: string;
  io: PromptIO;
}): Promise<void> {
  const { flagSkill, nonInteractive, homeDir, io } = opts;
  const out = io.output;

  if (typeof flagSkill === 'string') {
    const target = parseSkillSelection(flagSkill, false);
    if (!target) {
      throw new ArgError(
        '--skill must be "none" or a comma-separated combination of: claude, antigravity (or "both")',
      );
    }
    const installed = installMaestroSkill(target, homeDir);
    if (installed.length === 0) {
      out.write('Skipped Maestro protocol skill installation (--skill none).\n');
    } else {
      for (const dest of installed) out.write(`Installed Maestro skill: ${dest}\n`);
    }
    return;
  }

  const existingSkills = findInstalledSkills(homeDir);
  if (existingSkills.length > 0) {
    const desc = existingSkills.map((s) => `${s.label}: ${s.path}`).join(', ');
    out.write(`Existing Maestro skill(s) found (${desc}).\n`);
    if (nonInteractive) {
      out.write('Keeping existing Maestro skill(s).\n');
      return;
    }
    const action = await askKeepOrRedo('Maestro protocol skill', io, {
      keep: `Keep existing skill(s) (${existingSkills.map((s) => s.label).join(', ')})`,
      redo: 'Re-select skills to install',
    });
    if (action === 'keep') return;
  } else if (nonInteractive) {
    out.write('Skipping skill installation in non-interactive mode (pass --skill claude,antigravity to install).\n');
    return;
  }

  const choice = await promptSkillChoice(homeDir, existingSkills, io);
  const installed = installMaestroSkill(choice, homeDir);
  if (installed.length === 0) {
    out.write('Skipped Maestro protocol skill installation.\n');
  } else {
    for (const dest of installed) out.write(`Installed Maestro skill: ${dest}\n`);
  }
}

async function resolveServiceStep(opts: {
  flagService?: string;
  configDir: string;
  env: NodeJS.ProcessEnv;
  homeDir: string;
  nonInteractive: boolean;
  io: PromptIO;
  platform: NodeJS.Platform;
  runner?: ServiceCommandRunner;
  cliPath?: string;
}): Promise<'started' | 'installed' | 'skipped'> {
  const { flagService, configDir, env, homeDir, nonInteractive, io, platform, runner, cliPath } = opts;
  const out = io.output;

  let action: 'start' | 'install' | 'skip';
  if (typeof flagService === 'string') {
    const v = flagService.trim().toLowerCase();
    if (v === 'start') action = 'start';
    else if (v === 'install') action = 'install';
    else if (v === 'none' || v === 'skip') action = 'skip';
    else throw new ArgError('--service must be one of: install, start, none');
  } else if (!isServicePlatformSupported(platform)) {
    return 'skipped';
  } else if (nonInteractive) {
    return 'skipped';
  } else {
    const serviceOpts: ServiceOpts = { configDir, homeDir, env, platform, runner, cliPath };
    const alreadyInstalled = isServiceInstalled(serviceOpts);
    const hasAuth = fs.existsSync(authFile(configDir));
    if (alreadyInstalled) {
      action = await askSelect<'start' | 'install' | 'skip'>(
        'Background service (already installed):',
        [
          { name: 'Keep existing service configuration', value: 'skip' },
          { name: 'Reinstall and restart background service', value: 'start' },
        ],
        { defaultValue: 'skip', io },
      );
    } else {
      action = await askSelect<'start' | 'install' | 'skip'>(
        'Keep Maestria running in the background?',
        [
          { name: 'Install and start background service now (recommended)', value: 'start' },
          { name: 'Install service now, start later', value: 'install' },
          { name: 'Skip for now (start manually with "maestria serve")', value: 'skip' },
        ],
        { defaultValue: hasAuth ? 'start' : 'skip', io },
      );
    }
  }

  if (action === 'skip') return 'skipped';

  const serviceOpts: ServiceOpts = { configDir, homeDir, env, platform, runner, cliPath };
  const wasInstalled = isServiceInstalled(serviceOpts);
  try {
    const res = await installService(serviceOpts);
    out.write(`Installed Maestria service: ${res.unitPath}\n`);
    if (res.logDir) out.write(`Service logs: ${res.logDir}\n`);
    if (action === 'start') {
      if (wasInstalled) await restartService(serviceOpts);
      else await startService(serviceOpts);
      out.write('Started Maestria background service.\n');
      return 'started';
    }
    return 'installed';
  } catch (err) {
    if (err instanceof ServiceError) {
      out.write(`Warning: could not set up background service (${err.message}).\n`);
      return 'skipped';
    }
    throw err;
  }
}

export async function runInitCommand(args: string[], opts: InitCliOpts): Promise<void> {
  const { flags, positional } = parseArgs(args, {
    'maestri-dir': { type: 'string' },
    standby: { type: 'string' },
    skill: { type: 'string' },
    'install-skill': { type: 'string' },
    service: { type: 'string' },
    'skip-auth': { type: 'boolean' },
    yes: { type: 'boolean', short: 'y' },
    'non-interactive': { type: 'boolean' },
  });
  if (positional.length > 0) {
    throw new ArgError(
      'Usage: maestria init [--maestri-dir <path>] [--standby default|none] [--skill claude,antigravity|none] [--service install|start|none] [--skip-auth] [--yes]',
    );
  }
  if (typeof flags.skill === 'string' && typeof flags['install-skill'] === 'string' && flags.skill !== flags['install-skill']) {
    throw new ArgError('Pass either --skill or --install-skill, not both with different values.');
  }

  const env = opts.env ?? process.env;
  const homeDir = opts.homeDir ?? os.homedir();
  const baseIo = opts.io ?? defaultPromptIO();
  const io: PromptIO = opts.stdout ? { input: baseIo.input, output: opts.stdout } : baseIo;
  const out = io.output;
  const authSetupFn = opts.authSetupFn ?? authSetup;
  const nonInteractive = flags.yes === true || flags['non-interactive'] === true || !io.input.isTTY;

  ensureConfigDir(opts.configDir);
  out.write(`Using config directory: ${opts.configDir}\n`);

  const maestriDir = await resolveConfigStep({
    configDir: opts.configDir,
    cliMaestriDir: typeof flags['maestri-dir'] === 'string' ? flags['maestri-dir'] : undefined,
    nonInteractive,
    env,
    homeDir,
    io,
  });

  await resolveWorkspacesStep({
    configDir: opts.configDir,
    maestriDir,
    nonInteractive,
    homeDir,
    io,
  });

  await resolveStandbyStep({
    configDir: opts.configDir,
    flagStandby: typeof flags.standby === 'string' ? flags.standby : undefined,
    nonInteractive,
    env,
    io,
  });

  await resolveAuthStep({
    configDir: opts.configDir,
    skipAuth: flags['skip-auth'] === true,
    nonInteractive,
    io,
    authSetupFn,
  });

  const rawSkillFlag =
    typeof flags.skill === 'string'
      ? flags.skill
      : typeof flags['install-skill'] === 'string'
        ? flags['install-skill']
        : undefined;

  await resolveSkillStep({
    flagSkill: rawSkillFlag,
    nonInteractive,
    homeDir,
    io,
  });

  const platform = opts.servicePlatform ?? process.platform;
  const serviceResult = await resolveServiceStep({
    flagService: typeof flags.service === 'string' ? flags.service : undefined,
    configDir: opts.configDir,
    env,
    homeDir,
    nonInteractive,
    io,
    platform,
    runner: opts.serviceRunner,
    cliPath: opts.serviceCliPath,
  });

  out.write('\nSetup complete. Next steps:\n');
  if (serviceResult === 'started') {
    out.write('  Panel service is running.\n');
    out.write('  Stop:    maestria service stop\n');
    out.write('  Restart: maestria service restart\n');
    out.write('  Status:  maestria status\n');
  } else if (serviceResult === 'installed') {
    out.write('  Start:   maestria service start\n');
    out.write('  Stop:    maestria service stop\n');
    out.write('  Restart: maestria service restart\n');
    out.write('  Status:  maestria status\n');
  } else if (isServicePlatformSupported(platform)) {
    out.write('  1. Start now in foreground: maestria serve\n');
    out.write('     Or install background service: maestria service install && maestria service start\n');
    out.write('     Manage anytime with: maestria service start | stop | restart | status\n');
  } else {
    out.write('  1. Start the panel locally: maestria serve\n');
  }
  out.write('  2. Optionally expose securely via Cloudflare Tunnel + Access (see README.md).\n');
}
