import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import readline from 'node:readline/promises';
import { fileURLToPath } from 'node:url';
import { authFile } from '../auth/secrets.ts';
import {
  ensureConfigDir,
  inspectMaestriDir,
  loadRawConfig,
  resolveMaestriDir,
  saveRawConfig,
} from '../config.ts';
import { discoverMaestriWorkspaces } from '../discovery.ts';
import { loadWorkspaces, saveWorkspaces, type Workspace } from '../workspaces.ts';
import { ArgError, parseArgs } from './args.ts';
import { authSetup } from './auth-setup.ts';

export interface InitCliOpts {
  configDir: string;
  env?: NodeJS.ProcessEnv;
  homeDir?: string;
  stdout?: NodeJS.WritableStream;
  prompt?: (question: string) => Promise<string>;
}

const PACKAGE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const BUNDLED_SKILL_PATH = path.join(PACKAGE_ROOT, 'skills', 'maestria-maestro', 'SKILL.md');

export type SkillTarget = 'claude' | 'antigravity' | 'both' | 'none';

export function resolveAntigravitySkillDir(homeDir: string): string {
  const configSkills = path.join(homeDir, '.gemini', 'config', 'skills');
  if (fs.existsSync(configSkills)) return path.join(configSkills, 'maestria');
  return path.join(homeDir, '.gemini', 'antigravity', 'skills', 'maestria');
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
    installTo(path.join(homeDir, '.claude', 'skills', 'maestria'));
  }
  if (target === 'antigravity' || target === 'both') {
    installTo(resolveAntigravitySkillDir(homeDir));
  }
  return installed;
}

export async function runInitCommand(args: string[], opts: InitCliOpts): Promise<void> {
  const { flags, positional } = parseArgs(args, {
    'maestri-dir': { type: 'string' },
    'install-skill': { type: 'string' },
    'skip-auth': { type: 'boolean' },
    yes: { type: 'boolean', short: 'y' },
    'non-interactive': { type: 'boolean' },
  });
  if (positional.length > 0) {
    throw new ArgError(
      'Usage: maestria init [--maestri-dir <path>] [--install-skill claude|antigravity|both|none] [--skip-auth] [--yes]',
    );
  }

  const env = opts.env ?? process.env;
  const homeDir = opts.homeDir ?? os.homedir();
  const out = opts.stdout ?? process.stdout;
  const nonInteractive = flags.yes === true || flags['non-interactive'] === true || (!opts.prompt && !process.stdin.isTTY);

  const rl = !nonInteractive && !opts.prompt ? readline.createInterface({ input: process.stdin, output: out }) : null;
  const ask = async (q: string): Promise<string> => {
    if (opts.prompt) return opts.prompt(q);
    if (rl) return rl.question(q);
    return '';
  };

  try {
    ensureConfigDir(opts.configDir);
    out.write(`Using config directory: ${opts.configDir}\n`);

    const existingRaw = loadRawConfig(opts.configDir);
    const cliMaestriDir = typeof flags['maestri-dir'] === 'string' ? flags['maestri-dir'] : undefined;
    const configuredMaestriDir = typeof existingRaw.maestriDir === 'string' ? existingRaw.maestriDir : undefined;
    let maestriDir = resolveMaestriDir(cliMaestriDir ?? configuredMaestriDir, env, homeDir);

    if (!nonInteractive && !cliMaestriDir) {
      const answer = (await ask(`Maestri data directory [${maestriDir}]: `)).trim();
      if (answer) maestriDir = resolveMaestriDir(answer, env, homeDir);
    }

    const dirStatus = inspectMaestriDir(maestriDir);
    if (!dirStatus.hasDir) {
      out.write(`Warning: Maestri directory not found at "${maestriDir}". Set "maestriDir" in config.json or MAESTRI_HOME if Maestri lives elsewhere.\n`);
    } else if (!dirStatus.hasProvidersDir) {
      out.write(`Warning: "${dirStatus.providersDir}" not found (Agent Usage may be off in Maestri Settings -> Agents -> Usage; disabling it removes this folder).\n`);
    } else if (!dirStatus.hasStatusFile) {
      out.write(`Note: "${dirStatus.providersDir}" found, waiting for Maestri to write .status.json while usage rings are active.\n`);
    } else {
      out.write(`Verified Maestri usage providers at ${dirStatus.providersDir}\n`);
    }

    const nextConfig: Record<string, unknown> = { ...existingRaw };
    if (cliMaestriDir || configuredMaestriDir || maestriDir !== resolveMaestriDir(undefined, env, homeDir)) {
      nextConfig.maestriDir = maestriDir;
    } else if (!configuredMaestriDir && maestriDir === path.join(homeDir, '.maestri') && dirStatus.hasDir) {
      // Keep config minimal unless user supplied a custom path.
    }
    saveRawConfig(opts.configDir, nextConfig, env);

    // Workspace discovery from maestriDir
    const discovered = discoverMaestriWorkspaces(maestriDir, homeDir);
    const existingWs = loadWorkspaces(opts.configDir);
    const existingPaths = new Set(existingWs.map((w) => path.resolve(w.path)));
    const newOnes = discovered.filter((d) => !existingPaths.has(path.resolve(d.path)));

    if (newOnes.length > 0) {
      out.write(`Detected ${newOnes.length} new Maestri workspace(s):\n`);
      for (const d of newOnes) {
        out.write(`  - ${d.name} (${d.path})${d.roadmapPath ? ` [roadmap: ${d.roadmapPath}]` : ''}\n`);
      }
      let importOk = true;
      if (!nonInteractive) {
        const ans = (await ask('Add detected workspaces to workspaces.json? [Y/n]: ')).trim().toLowerCase();
        importOk = ans === '' || ans === 'y' || ans === 'yes';
      }
      if (importOk) {
        const merged: Partial<Workspace>[] = [...existingWs, ...newOnes];
        saveWorkspaces(opts.configDir, merged);
        out.write(`Saved ${merged.length} workspace(s) to workspaces.json.\n`);
      }
    } else if (existingWs.length === 0) {
      out.write('No Maestri workspaces found automatically. You can add them later with "maestria workspace add".\n');
    } else {
      out.write(`Keeping ${existingWs.length} existing configured workspace(s).\n`);
    }

    // Auth setup
    if (flags['skip-auth'] !== true) {
      if (fs.existsSync(authFile(opts.configDir))) {
        if (nonInteractive) {
          out.write('Credentials already configured; skipping auth setup.\n');
        } else {
          const ans = (await ask('Credentials already exist. Reconfigure password & TOTP? [y/N]: ')).trim().toLowerCase();
          if (ans === 'y' || ans === 'yes') {
            await authSetup({ configDir: opts.configDir, force: true });
          }
        }
      } else {
        await authSetup({ configDir: opts.configDir, force: false });
      }
    }

    // Skill installation
    let skillChoice: SkillTarget | undefined;
    const rawSkill = flags['install-skill'];
    if (typeof rawSkill === 'string') {
      if (!['claude', 'antigravity', 'both', 'none'].includes(rawSkill)) {
        throw new ArgError('--install-skill must be one of: claude, antigravity, both, none');
      }
      skillChoice = rawSkill as SkillTarget;
    } else if (!nonInteractive) {
      const ans = (await ask('Install Maestro protocol skill for [c]laude, [a]ntigravity, [b]oth, or [n]one? [b]: ')).trim().toLowerCase();
      if (ans === 'c' || ans === 'claude') skillChoice = 'claude';
      else if (ans === 'a' || ans === 'antigravity') skillChoice = 'antigravity';
      else if (ans === 'n' || ans === 'none') skillChoice = 'none';
      else skillChoice = 'both';
    } else {
      skillChoice = 'none';
    }

    const installed = installMaestroSkill(skillChoice, homeDir);
    if (installed.length > 0) {
      for (const dest of installed) {
        out.write(`Installed Maestro skill: ${dest}\n`);
      }
    }

    out.write('\nSetup complete. Next steps:\n');
    out.write('  1. Start the panel locally: maestria serve\n');
    out.write('  2. Optionally expose securely via Cloudflare Tunnel + Access (see README.md).\n');
  } finally {
    rl?.close();
  }
}
