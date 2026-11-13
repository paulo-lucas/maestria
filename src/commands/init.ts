import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { authFile } from '../auth/secrets.ts';
import {
  ConfigError,
  configFilePath,
  ensureConfigDir,
  inspectMaestriDir,
  loadRawConfig,
  resolveMaestriDir,
  saveRawConfig,
} from '../config.ts';
import { discoverMaestriWorkspaces } from '../discovery.ts';
import { loadWorkspaces, saveWorkspaces, type Workspace } from '../workspaces.ts';
import { ArgError, parseArgs } from './args.ts';
import { authSetup, type AuthSetupOpts } from './auth-setup.ts';
import { askKeepOrRedo, askValidated, askYesNo, defaultPromptIO, type PromptIO } from './prompt.ts';

export interface InitCliOpts {
  configDir: string;
  env?: NodeJS.ProcessEnv;
  homeDir?: string;
  io?: PromptIO;
  stdout?: NodeJS.WritableStream;
  authSetupFn?: (opts: AuthSetupOpts) => Promise<void>;
}

const PACKAGE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const BUNDLED_SKILL_PATH = path.join(PACKAGE_ROOT, 'skills', 'maestria-maestro', 'SKILL.md');

export type SkillTarget = 'claude' | 'antigravity' | 'both' | 'none';

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
  if (claude && antigravity) return 'both';
  if (claude) return 'claude';
  if (antigravity) return 'antigravity';
  return null;
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
  return askValidated<{ dir: string; custom: boolean }>(
    `Maestri data directory (enter a path, or "default" for ${envDefault}): `,
    (raw) => {
      const trimmed = raw.trim();
      if (!trimmed) {
        return { ok: false, error: `Please enter a path or "default" (default: ${envDefault}).` };
      }
      if (trimmed.toLowerCase() === 'default') {
        return { ok: true, value: { dir: envDefault, custom: false } };
      }
      try {
        const resolved = resolveMaestriDir(trimmed, env, homeDir);
        return { ok: true, value: { dir: resolved, custom: resolved !== envDefault } };
      } catch (err) {
        const msg = err instanceof ConfigError ? err.message : String(err);
        return { ok: false, error: msg };
      }
    },
    io,
  );
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
    const action = await askKeepOrRedo('Keep existing config or redo? [keep/redo, default: keep]: ', io);
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
      : await askYesNo('Add detected workspaces to workspaces.json? [y/n, default: y]: ', {
          defaultLabel: 'y',
          io,
        });
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
    const action = await askKeepOrRedo('Keep existing workspaces.json or redo from discovery? [keep/redo, default: keep]: ', io);
    if (action === 'redo') {
      saveWorkspaces(configDir, discovered);
      out.write(`Replaced workspaces.json with ${discovered.length} discovered workspace(s).\n`);
    }
  }
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
    const action = await askKeepOrRedo('Keep existing credentials or redo password & TOTP? [keep/redo, default: keep]: ', io);
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

function findInstalledSkills(homeDir: string): { label: string; path: string }[] {
  const found: { label: string; path: string }[] = [];
  const claude = claudeSkillFilePath(homeDir);
  if (fs.existsSync(claude)) found.push({ label: 'claude', path: claude });
  const ag = antigravitySkillFilePath(homeDir);
  if (fs.existsSync(ag)) found.push({ label: 'antigravity', path: ag });
  return found;
}

async function promptSkillChoice(io: PromptIO): Promise<SkillTarget> {
  while (true) {
    const choice = await askValidated<SkillTarget>(
      'Install Maestro protocol skill for [claude,antigravity | claude | antigravity | none] (default: claude,antigravity): ',
      (raw) => {
        const parsed = parseSkillSelection(raw, true);
        if (!parsed) {
          return {
            ok: false,
            error:
              'Invalid or empty choice. Enter "claude,antigravity" (default), "claude", "antigravity", or "none".',
          };
        }
        return { ok: true, value: parsed };
      },
      io,
    );

    const confirmPrompt =
      choice === 'none'
        ? 'Confirm: skip installing Maestro protocol skill? [y/n, default: y]: '
        : `Confirm: install Maestro protocol skill for ${formatSkillTarget(choice)}? [y/n, default: y]: `;
    const confirmed = await askYesNo(confirmPrompt, { defaultLabel: 'y', io });
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
    const action = await askKeepOrRedo('Keep existing Maestro skill(s) or redo? [keep/redo, default: keep]: ', io);
    if (action === 'keep') return;
  } else if (nonInteractive) {
    out.write('Skipping skill installation in non-interactive mode (pass --skill claude,antigravity to install).\n');
    return;
  }

  const choice = await promptSkillChoice(io);
  const installed = installMaestroSkill(choice, homeDir);
  if (installed.length === 0) {
    out.write('Skipped Maestro protocol skill installation.\n');
  } else {
    for (const dest of installed) out.write(`Installed Maestro skill: ${dest}\n`);
  }
}

export async function runInitCommand(args: string[], opts: InitCliOpts): Promise<void> {
  const { flags, positional } = parseArgs(args, {
    'maestri-dir': { type: 'string' },
    skill: { type: 'string' },
    'install-skill': { type: 'string' },
    'skip-auth': { type: 'boolean' },
    yes: { type: 'boolean', short: 'y' },
    'non-interactive': { type: 'boolean' },
  });
  if (positional.length > 0) {
    throw new ArgError(
      'Usage: maestria init [--maestri-dir <path>] [--skill claude,antigravity|none] [--skip-auth] [--yes]',
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

  out.write('\nSetup complete. Next steps:\n');
  out.write('  1. Start the panel locally: maestria serve\n');
  out.write('  2. Optionally expose securely via Cloudflare Tunnel + Access (see README.md).\n');
}
