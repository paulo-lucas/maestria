import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  loadWorkspaces,
  parseWorkspaces,
  resolveWorkspace,
  saveWorkspaces,
  WorkspaceError,
  WorkspaceRegistry,
  workspacesFile,
} from '../src/workspaces.ts';

describe('workspaces config', () => {
  it('parses valid entries and applies defaults', () => {
    const list = parseWorkspaces([
      { name: 'My App', path: '/tmp/my-app' },
      { id: 'docs', name: 'Docs Site', path: '/tmp/docs', roadmapPath: 'ROADMAP.md', maestroAgent: 'Bulbasaur' },
    ]);
    expect(list).toEqual([
      { id: 'my-app', name: 'My App', path: '/tmp/my-app', roadmapPath: null, maestroAgent: 'Maestro' },
      { id: 'docs', name: 'Docs Site', path: '/tmp/docs', roadmapPath: 'ROADMAP.md', maestroAgent: 'Bulbasaur' },
    ]);
  });

  it('rejects malformed entries, unknown keys, relative paths, and duplicates', () => {
    expect(() => parseWorkspaces({})).toThrow(WorkspaceError);
    expect(() => parseWorkspaces([{ name: 'A', path: '/tmp/a', extra: 1 }])).toThrow(WorkspaceError);
    expect(() => parseWorkspaces([{ name: 'A', path: 'relative/path' }])).toThrow(WorkspaceError);
    expect(() => parseWorkspaces([{ id: 'BAD ID', name: 'A', path: '/tmp/a' }])).toThrow(WorkspaceError);
    expect(() =>
      parseWorkspaces([
        { id: 'dup', name: 'One', path: '/tmp/one' },
        { id: 'dup', name: 'Two', path: '/tmp/two' },
      ]),
    ).toThrow(WorkspaceError);
    expect(() =>
      parseWorkspaces([
        { id: 'one', name: 'One', path: '/tmp/same' },
        { id: 'two', name: 'Two', path: '/tmp/same' },
      ]),
    ).toThrow(WorkspaceError);
  });

  it('saves with 0600 permissions, loads, and reloads on mtime change', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'maestria-ws-'));
    expect(loadWorkspaces(dir)).toEqual([]);

    const reg = new WorkspaceRegistry(dir);
    expect(reg.list()).toEqual([]);

    saveWorkspaces(dir, [{ id: 'app', name: 'App', path: '/tmp/app', roadmapPath: null, maestroAgent: 'Maestro' }]);
    expect(fs.statSync(workspacesFile(dir)).mode & 0o777).toBe(0o600);
    expect(reg.list()).toHaveLength(1);
    expect(reg.get('app')?.name).toBe('App');
  });
});

describe('workspace resolution', () => {
  const list = parseWorkspaces([
    { id: 'root', name: 'Root', path: '/Users/dev/work' },
    { id: 'nested', name: 'Nested App', path: '/Users/dev/work/sub/app' },
  ]);

  it('resolves by explicit id or name', () => {
    expect(resolveWorkspace(list, { workspace: 'root' }).id).toBe('root');
    expect(resolveWorkspace(list, { workspace: 'nested app' }).id).toBe('nested');
    expect(() => resolveWorkspace(list, { workspace: 'missing' })).toThrow(WorkspaceError);
  });

  it('infers the most specific workspace from cwd', () => {
    expect(resolveWorkspace(list, { cwd: '/Users/dev/work' }).id).toBe('root');
    expect(resolveWorkspace(list, { cwd: '/Users/dev/work/other/dir' }).id).toBe('root');
    expect(resolveWorkspace(list, { cwd: '/Users/dev/work/sub/app' }).id).toBe('nested');
    expect(resolveWorkspace(list, { cwd: '/Users/dev/work/sub/app/src' }).id).toBe('nested');
    // Prefix trap: /Users/dev/workspace must not match /Users/dev/work
    expect(() => resolveWorkspace(list, { cwd: '/Users/dev/workspace' })).toThrow(WorkspaceError);
  });
});
