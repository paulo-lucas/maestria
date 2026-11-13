import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseRoadmapMarkdown, readWorkspaceRoadmap } from '../src/roadmap.ts';
import type { Workspace } from '../src/workspaces.ts';

describe('roadmap parser and reader', () => {
  it('parses Done, Pipeline milestones with review flags, Current Sprint, and lists', () => {
    const md = `# Roadmap

_Last updated: 2026-11-12 (sprint 2 done)._

## Current Sprint

| # | Item | Owner |
| --- | --- | --- |
| 3 | Auth hardening | Agent A |

## Done

| # | Sprint | Result |
| --- | --- | --- |
| 1 | Skeleton | Setup complete |
| 2 | Core UI | Views & tests |

## Pipeline

### Milestone 1: MVP

| # | Sprint | Size | Depends on | Notes |
| --- | --- | --- | --- | --- |
| 4 | **Payments** (**R**) | M | 3 | Needs security review |

### Milestone 2: Polish

| # | Sprint | Size | Depends on | Notes |
| --- | --- | --- | --- | --- |

## Notes

- [x] Verify CI
- [ ] Draft release notes
`;
    const parsed = parseRoadmapMarkdown(md);
    expect(parsed.title).toBe('Roadmap');
    expect(parsed.updatedNote).toBe('Last updated: 2026-11-12 (sprint 2 done).');
    expect(parsed.currentSprint).toHaveLength(1);
    expect(parsed.currentSprint[0]?.rows).toEqual([['3', 'Auth hardening', 'Agent A']]);
    expect(parsed.done).toEqual([
      { id: '1', title: 'Skeleton', result: 'Setup complete' },
      { id: '2', title: 'Core UI', result: 'Views & tests' },
    ]);
    expect(parsed.pipeline).toEqual([
      {
        id: '4',
        title: 'Payments',
        size: 'M',
        dependsOn: '3',
        notes: 'Needs security review',
        milestone: 'Milestone 1: MVP',
        review: true,
      },
    ]);
    const notesSec = parsed.sections.find((s) => s.heading === 'Notes');
    expect(notesSec?.listItems).toEqual([
      { text: 'Verify CI', checked: true },
      { text: 'Draft release notes', checked: false },
    ]);
  });

  it('handles unconfigured, missing, and valid relative roadmap paths', () => {
    const wsDir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-rm-'));
    fs.mkdirSync(path.join(wsDir, 'docs'));
    fs.writeFileSync(path.join(wsDir, 'docs', 'roadmap.md'), '## Done\n| # | Sprint | Result |\n| --- | --- | --- |\n| 1 | Init | Ok |\n');

    const baseWs: Workspace = {
      id: 'app',
      name: 'App',
      path: wsDir,
      roadmapPath: null,
      maestroAgent: 'Maestro',
      maestroProvider: 'claude',
      color: '#5b4bd6',
    };

    expect(readWorkspaceRoadmap(baseWs).status).toBe('unconfigured');
    expect(readWorkspaceRoadmap({ ...baseWs, roadmapPath: 'docs/missing.md' }).status).toBe('missing');

    const ok = readWorkspaceRoadmap({ ...baseWs, roadmapPath: 'docs/roadmap.md' });
    expect(ok.status).toBe('ok');
    expect(ok.roadmap?.done).toHaveLength(1);
    expect(ok.roadmap?.done[0]?.title).toBe('Init');
  });
});
