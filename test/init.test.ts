import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { PassThrough } from 'node:stream';
import { describe, expect, it } from 'vitest';
import { installMaestroSkill, runInitCommand } from '../src/commands/init.ts';
import { loadConfig } from '../src/config.ts';
import { discoverMaestriWorkspaces } from '../src/discovery.ts';
import { loadWorkspaces } from '../src/workspaces.ts';

function capture() {
  const s = new PassThrough();
  let text = '';
  s.on('data', (c) => (text += c.toString('utf8')));
  return { stream: s, read: () => text };
}

describe('maestria init, workspace discovery, and skill installer', () => {
  it('discovers Maestri workspaces and detects roadmap files read-only', () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-disc-'));
    const maestriDir = path.join(tmp, 'maestri');
    const ws1 = path.join(tmp, 'proj-alpha');
    const ws2 = path.join(tmp, 'proj-beta');
    fs.mkdirSync(path.join(ws1, 'docs'), { recursive: true });
    fs.writeFileSync(path.join(ws1, 'docs', 'roadmap.md'), '# Alpha\n');
    fs.mkdirSync(ws2, { recursive: true });

    fs.mkdirSync(path.join(maestriDir, 'workspaces', 'W1'), { recursive: true });
    fs.mkdirSync(path.join(maestriDir, 'usage', 'providers'), { recursive: true });
    fs.writeFileSync(
      path.join(maestriDir, 'manifest.json'),
      JSON.stringify({ workspaces: [{ id: 'W1', name: 'Alpha', path: 'workspaces/W1/workspace.json' }] }),
    );
    fs.writeFileSync(
      path.join(maestriDir, 'workspaces', 'W1', 'workspace.json'),
      JSON.stringify({
        schemaVersion: 2,
        payload: { name: 'Alpha', workingDirectory: ws1, color: '#2563EB' },
      }),
    );
    fs.mkdirSync(path.join(maestriDir, 'workspaces', 'W2'), { recursive: true });
    fs.writeFileSync(
      path.join(maestriDir, 'workspaces', 'W2', 'workspace.json'),
      JSON.stringify({
        schemaVersion: 2,
        payload: { name: 'Beta', workingDirectory: ws2 },
      }),
    );

    const found = discoverMaestriWorkspaces(maestriDir, tmp);
    expect(found).toHaveLength(2);
    const alpha = found.find((w) => w.name === 'Alpha');
    const beta = found.find((w) => w.name === 'Beta');
    expect(alpha).toMatchObject({ id: 'alpha', path: ws1, roadmapPath: 'docs/roadmap.md', color: '#2563eb' });
    expect(beta).toMatchObject({ id: 'beta', path: ws2, roadmapPath: null });
  });

  it('runs maestria init end-to-end with workspace discovery and skill installation', async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-init-'));
    const homeDir = path.join(tmp, 'home');
    const configDir = path.join(tmp, 'cfg');
    const maestriDir = path.join(tmp, 'custom-maestri');
    const projDir = path.join(tmp, 'sample-project');

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

    const out = capture();
    await runInitCommand(
      ['--yes', '--maestri-dir', maestriDir, '--skip-auth', '--install-skill', 'both'],
      { configDir, homeDir, stdout: out.stream },
    );

    const output = out.read();
    expect(output).toContain('Verified Maestri usage providers');
    expect(output).toContain('Detected 1 new Maestri workspace');

    const cfg = loadConfig(configDir);
    expect(cfg.maestriDir).toBe(maestriDir);
    expect(cfg.standby.thresholds).toEqual({ claude: { session: 95, weekly: 90 } });
    expect(cfg.standby.resumeBelowPercent).toBe(80);
    const savedWs = loadWorkspaces(configDir);
    expect(savedWs).toHaveLength(1);
    expect(savedWs[0]).toMatchObject({ id: 'sample-project', name: 'Sample Project', path: projDir, roadmapPath: 'ROADMAP.md', maestroProvider: 'claude' });

    const claudeSkill = path.join(homeDir, '.claude', 'skills', 'maestria', 'SKILL.md');
    const agSkill = path.join(homeDir, '.gemini', 'config', 'skills', 'maestria', 'SKILL.md');
    expect(fs.existsSync(claudeSkill)).toBe(true);
    expect(fs.existsSync(agSkill)).toBe(true);
    const skillText = fs.readFileSync(claudeSkill, 'utf8');
    expect(skillText).toContain('maestria wait');
    expect(skillText).toContain('## Standby');
    expect(skillText).toContain("maestri ask <agent> --raw '\\e'");

    // Re-running init when no .gemini/config/skills exists defaults to ~/.gemini/antigravity/skills
    const freshHome = path.join(tmp, 'home2');
    const installed = installMaestroSkill('antigravity', freshHome);
    expect(installed[0]).toBe(path.join(freshHome, '.gemini', 'antigravity', 'skills', 'maestria', 'SKILL.md'));
    expect(fs.existsSync(installed[0]!)).toBe(true);
  });

  it('contains no owner-specific or machine-specific references in shipped source, skills, or docs', () => {
    const root = path.resolve(import.meta.dirname, '..');
    const checkDirs = ['src', 'public', 'skills', 'docs'];
    const forbidden = [/chapter-dashboard/i, /\/Users\/plucas/i, /Armarouge|Whimsicott|Klefki/];
    const visit = (dir: string) => {
      for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
          visit(full);
        } else {
          const text = fs.readFileSync(full, 'utf8');
          for (const pat of forbidden) {
            expect(text, `Unexpected match ${pat} in ${full}`).not.toMatch(pat);
          }
        }
      }
    };
    for (const d of checkDirs) visit(path.join(root, d));
    const readme = fs.readFileSync(path.join(root, 'README.md'), 'utf8');
    for (const pat of forbidden) expect(readme).not.toMatch(pat);
  });
});
