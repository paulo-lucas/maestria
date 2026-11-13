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

  it('parses real-world GFM fixture (intro prose, tables, lists, sections, sanitized inline/block HTML)', () => {
    const fixturePath = path.resolve(import.meta.dirname, 'fixtures', 'chapter-roadmap.md');
    const md = fs.readFileSync(fixturePath, 'utf8');
    const parsed = parseRoadmapMarkdown(md);

    expect(parsed.title).toBe('Roadmap');
    expect(parsed.updatedNote).toContain('milestone 2 done: sprints 7, 8, 9');
    expect(parsed.done).toHaveLength(19);
    expect(parsed.pipeline).toHaveLength(7);

    // Intro prose before ## Done is preserved and formatted cleanly
    expect(parsed.introHtml).toContain('<p>');
    expect(parsed.introHtml).toContain('<strong>one at a time</strong>');
    expect(parsed.introHtml).toContain('<code>main</code>');

    // All sections present including prose-only sections like "Known gaps carried along"
    const headings = parsed.sections.map((s) => s.heading);
    expect(headings).toEqual([
      'Done',
      'Pipeline',
      'Postponed by the owner (2026-11-11)',
      'Legal track (starts early, finishes before release)',
      'Backlog (deferred, not scheduled)',
      'Things only you can provide (and when)',
      'Known gaps carried along',
    ]);

    const pipeline = parsed.sections.find((s) => s.heading === 'Pipeline')!;
    expect(pipeline.bodyHtml).toContain('<h4>Milestone 4: delight</h4>');
    expect(pipeline.bodyHtml).toContain('<strong>Profile customization</strong>');
    expect(pipeline.bodyHtml).toContain('<code>profiles.theme</code>');
    expect(pipeline.bodyHtml).toContain('<div class="md-table-wrap"><table>');

    const postponed = parsed.sections.find((s) => s.heading.startsWith('Postponed'))!;
    expect(postponed.bodyHtml).toContain('<code>parked/privacy</code>');
    expect(postponed.bodyHtml).toContain('<p>Original rows:</p>');
    expect(postponed.bodyHtml).toContain('<strong>Legal and privacy (LGPD)</strong> (<strong>R</strong>)');

    const legal = parsed.sections.find((s) => s.heading.startsWith('Legal track'))!;
    expect(legal.bodyHtml).toContain('This is a plan, not legal advice');
    expect(legal.bodyHtml).toContain('<strong>Start alongside sprint 6</strong>');

    const knownGaps = parsed.sections.find((s) => s.heading === 'Known gaps carried along')!;
    expect(knownGaps.bodyHtml).toContain('<code>/u/[username]</code>');
    expect(knownGaps.bodyHtml).toContain('CI has never run on GitHub');
  });

  it('escapes HTML injections inside roadmap titles, paragraphs, lists, and tables', () => {
    const md = [
      '# Road<script>alert(1)</script>',
      '',
      'Intro with [bad](javascript:alert(2)) and <img src=x onerror=alert(3)>.',
      '',
      '## <svg onload=alert(4)>Sec',
      '',
      '| Col | Link |',
      '| --- | --- |',
      '| <b>x</b> | [ok](https://example.com) |',
    ].join('\n');
    const parsed = parseRoadmapMarkdown(md);
    expect(parsed.introHtml).not.toContain('<img');
    expect(parsed.introHtml).not.toContain('<a ');
    expect(parsed.introHtml).not.toContain('href=');
    const sec = parsed.sections[0]!;
    expect(sec.headingHtml).not.toContain('<svg');
    expect(sec.bodyHtml).not.toContain('<b>');
    expect(sec.bodyHtml).toContain('&lt;b&gt;x&lt;/b&gt;');
    expect(sec.bodyHtml).toContain('href="https://example.com/"');
  });
});
