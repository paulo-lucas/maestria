import fs from 'node:fs';
import path from 'node:path';
import type { Workspace } from './workspaces.ts';

export interface RoadmapDoneItem {
  id: string;
  title: string;
  result: string;
}

export interface RoadmapPipelineItem {
  id: string;
  title: string;
  size: string;
  dependsOn: string;
  notes: string;
  milestone: string;
  review: boolean;
}

export interface RoadmapTable {
  milestone: string | null;
  headers: string[];
  rows: string[][];
}

export interface RoadmapListItem {
  text: string;
  checked: boolean | null;
}

export interface RoadmapSection {
  heading: string;
  tables: RoadmapTable[];
  listItems: RoadmapListItem[];
}

export interface ParsedRoadmap {
  title: string | null;
  updatedNote: string | null;
  done: RoadmapDoneItem[];
  pipeline: RoadmapPipelineItem[];
  currentSprint: RoadmapTable[];
  sections: RoadmapSection[];
}

export interface WorkspaceRoadmapResult {
  status: 'ok' | 'unconfigured' | 'missing' | 'error';
  path: string | null;
  error?: string;
  mtimeMs?: number;
  roadmap?: ParsedRoadmap;
}

const MAX_ROADMAP_BYTES = 512 * 1024;

function stripBoldAndMarkers(s: string): { clean: string; review: boolean } {
  const review = /\(\*\*R\*\*\)/.test(s);
  const clean = s
    .replace(/\(\*\*R\*\*\)/g, '')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/\(\s*\)/g, '')
    .trim();
  return { clean, review };
}

function splitTableRow(line: string): string[] {
  const trimmed = line.trim();
  const inner = trimmed.replace(/^\|/, '').replace(/\|$/, '');
  return inner.split('|').map((c) => c.trim());
}

function isSeparatorRow(cells: string[]): boolean {
  return cells.length > 0 && cells.every((c) => /^:?-{2,}:?$/.test(c));
}

export function parseRoadmapMarkdown(markdown: string): ParsedRoadmap {
  const lines = markdown.split(/\r?\n/);
  let title: string | null = null;
  let updatedNote: string | null = null;

  const sections: RoadmapSection[] = [];
  let currentSection: RoadmapSection | null = null;
  let currentMilestone: string | null = null;

  let activeTableHeaders: string[] | null = null;
  let activeTableSeenSep = false;
  let activeTableRows: string[][] = [];

  const flushTable = () => {
    if (activeTableHeaders && activeTableSeenSep && currentSection) {
      currentSection.tables.push({
        milestone: currentMilestone,
        headers: activeTableHeaders,
        rows: activeTableRows,
      });
    }
    activeTableHeaders = null;
    activeTableSeenSep = false;
    activeTableRows = [];
  };

  const ensureSection = (heading: string): RoadmapSection => {
    let sec = sections.find((s) => s.heading === heading);
    if (!sec) {
      sec = { heading, tables: [], listItems: [] };
      sections.push(sec);
    }
    return sec;
  };

  for (const rawLine of lines) {
    const line = rawLine.trimEnd();

    if (!title && line.startsWith('# ')) {
      title = line.slice(2).trim();
      continue;
    }

    if (!updatedNote) {
      const m = /^_?(Last updated:\s*.+?)_?$/i.exec(line.trim());
      if (m) {
        updatedNote = m[1]!.replace(/_$/, '').trim();
        continue;
      }
    }

    if (line.startsWith('## ')) {
      flushTable();
      currentMilestone = null;
      currentSection = ensureSection(line.slice(3).trim());
      continue;
    }

    if (line.startsWith('### ')) {
      flushTable();
      currentMilestone = line.slice(4).trim();
      if (!currentSection) currentSection = ensureSection('Roadmap');
      continue;
    }

    if (line.trimStart().startsWith('|')) {
      if (!currentSection) currentSection = ensureSection('Overview');
      const cells = splitTableRow(line);
      if (!activeTableHeaders) {
        activeTableHeaders = cells;
        activeTableSeenSep = false;
        activeTableRows = [];
      } else if (!activeTableSeenSep) {
        if (isSeparatorRow(cells)) {
          activeTableSeenSep = true;
        } else {
          // Not a valid markdown table header; reset.
          activeTableHeaders = cells;
          activeTableRows = [];
        }
      } else if (!isSeparatorRow(cells)) {
        activeTableRows.push(cells);
      }
      continue;
    }

    if (activeTableHeaders) {
      flushTable();
    }

    const listMatch = /^-\s+(?:\[(x|X| )\]\s+)?(.+)$/.exec(line.trim());
    if (listMatch && currentSection) {
      const checked = listMatch[1] === undefined ? null : listMatch[1].toLowerCase() === 'x';
      currentSection.listItems.push({ text: listMatch[2]!.trim(), checked });
    }
  }

  flushTable();

  const done: RoadmapDoneItem[] = [];
  const pipeline: RoadmapPipelineItem[] = [];
  const currentSprint: RoadmapTable[] = [];

  for (const sec of sections) {
    const lower = sec.heading.toLowerCase();
    if (lower === 'done' || lower.startsWith('done ')) {
      for (const tbl of sec.tables) {
        for (const row of tbl.rows) {
          if (row.length < 2) continue;
          const id = row[0] ?? '';
          const { clean: name } = stripBoldAndMarkers(row[1] ?? '');
          const result = row.slice(2).join(' · ').trim();
          done.push({ id, title: name, result });
        }
      }
      for (const item of sec.listItems) {
        const { clean } = stripBoldAndMarkers(item.text);
        done.push({ id: '', title: clean, result: '' });
      }
    } else if (lower === 'pipeline' || lower.startsWith('pipeline ')) {
      for (const tbl of sec.tables) {
        for (const row of tbl.rows) {
          if (row.length < 2) continue;
          const id = row[0] ?? '';
          const { clean, review } = stripBoldAndMarkers(row[1] ?? '');
          pipeline.push({
            id,
            title: clean,
            size: row[2] ?? '',
            dependsOn: row[3] ?? '',
            notes: row[4] ?? '',
            milestone: tbl.milestone ?? '',
            review,
          });
        }
      }
    } else if (lower.includes('current sprint')) {
      currentSprint.push(...sec.tables);
    }
  }

  return { title, updatedNote, done, pipeline, currentSprint, sections };
}

export function resolveRoadmapFile(ws: Workspace): string | null {
  if (!ws.roadmapPath) return null;
  return path.isAbsolute(ws.roadmapPath) ? path.resolve(ws.roadmapPath) : path.resolve(ws.path, ws.roadmapPath);
}

export function readWorkspaceRoadmap(ws: Workspace): WorkspaceRoadmapResult {
  const file = resolveRoadmapFile(ws);
  if (!file) {
    return { status: 'unconfigured', path: null };
  }
  let st: fs.Stats;
  try {
    st = fs.lstatSync(file);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
      return { status: 'missing', path: file, error: `Roadmap file not found at ${file}` };
    }
    return { status: 'error', path: file, error: (err as Error).message };
  }
  if (!st.isFile()) {
    return { status: 'error', path: file, error: `Roadmap path must be a regular file` };
  }
  if (st.size > MAX_ROADMAP_BYTES) {
    return { status: 'error', path: file, error: `Roadmap file exceeds ${MAX_ROADMAP_BYTES} bytes` };
  }
  try {
    const text = fs.readFileSync(file, 'utf8');
    return {
      status: 'ok',
      path: file,
      mtimeMs: st.mtimeMs,
      roadmap: parseRoadmapMarkdown(text),
    };
  } catch (err) {
    return { status: 'error', path: file, error: (err as Error).message };
  }
}
