import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { expandHome } from './config.ts';
import { slugifyWorkspaceId } from './workspaces.ts';

export interface DiscoveredWorkspace {
  id: string;
  name: string;
  path: string;
  roadmapPath: string | null;
  maestroAgent: string;
  color?: string;
}

const ROADMAP_CANDIDATES = ['docs/roadmap.md', 'docs/ROADMAP.md', 'ROADMAP.md', 'roadmap.md'];
const HEX_RE = /^#[0-9a-f]{6}$/i;

function detectRoadmap(wsPath: string): string | null {
  for (const rel of ROADMAP_CANDIDATES) {
    try {
      const full = path.join(wsPath, rel);
      if (fs.statSync(full).isFile()) return rel;
    } catch {
      /* ignore */
    }
  }
  return null;
}

function tryReadJson(file: string): unknown {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    return null;
  }
}

/**
 * Discover Maestri workspaces from `<maestriDir>` (strictly read-only).
 * Reads `<maestriDir>/workspaces/<uuid>/workspace.json` (and `<maestriDir>/manifest.json` index if present).
 */
export function discoverMaestriWorkspaces(maestriDir: string, homeDir: string = os.homedir()): DiscoveredWorkspace[] {
  const workspacesRoot = path.join(maestriDir, 'workspaces');
  const candidateFiles = new Set<string>();

  const manifest = tryReadJson(path.join(maestriDir, 'manifest.json'));
  if (typeof manifest === 'object' && manifest !== null && Array.isArray((manifest as Record<string, unknown>).workspaces)) {
    for (const entry of (manifest as { workspaces: unknown[] }).workspaces) {
      if (typeof entry !== 'object' || entry === null) continue;
      const e = entry as Record<string, unknown>;
      if (typeof e.path === 'string') {
        const p = path.isAbsolute(e.path) ? e.path : path.join(maestriDir, e.path);
        candidateFiles.add(p);
      } else if (typeof e.id === 'string') {
        candidateFiles.add(path.join(workspacesRoot, e.id, 'workspace.json'));
      }
    }
  }

  try {
    for (const sub of fs.readdirSync(workspacesRoot, { withFileTypes: true })) {
      if (sub.isDirectory()) {
        candidateFiles.add(path.join(workspacesRoot, sub.name, 'workspace.json'));
      }
    }
  } catch {
    /* missing workspaces directory */
  }

  const found: DiscoveredWorkspace[] = [];
  const seenPaths = new Set<string>();
  const seenIds = new Set<string>();

  for (const wf of candidateFiles) {
    const raw = tryReadJson(wf);
    if (typeof raw !== 'object' || raw === null) continue;
    const payload =
      typeof (raw as Record<string, unknown>).payload === 'object' && (raw as Record<string, unknown>).payload !== null
        ? ((raw as Record<string, unknown>).payload as Record<string, unknown>)
        : (raw as Record<string, unknown>);

    const name = typeof payload.name === 'string' ? payload.name.trim() : '';
    const rawDir = typeof payload.workingDirectory === 'string' ? payload.workingDirectory.trim() : '';
    if (!name || !rawDir) continue;

    const absDir = path.resolve(expandHome(rawDir, homeDir));
    if (!path.isAbsolute(absDir) || seenPaths.has(absDir)) continue;
    seenPaths.add(absDir);

    let id = slugifyWorkspaceId(name) || `ws-${found.length + 1}`;
    let suffix = 2;
    while (seenIds.has(id)) {
      id = `${slugifyWorkspaceId(name).slice(0, 56)}-${suffix++}`;
    }
    seenIds.add(id);

    const rawColor = typeof payload.color === 'string' ? payload.color.trim() : '';
    const color = HEX_RE.test(rawColor) ? rawColor.toLowerCase() : undefined;

    found.push({
      id,
      name,
      path: absDir,
      roadmapPath: detectRoadmap(absDir),
      maestroAgent: 'Maestro',
      ...(color ? { color } : {}),
    });
  }

  return found;
}
