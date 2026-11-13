import fs from 'node:fs';

/**
 * Ensure a private directory is accessible only to the current user on POSIX.
 * On Windows, NTFS inherits the user's profile ACL and POSIX mode bits are not meaningful.
 */
export function ensurePrivateDir(dirPath: string, platform: NodeJS.Platform = process.platform): void {
  fs.mkdirSync(dirPath, { recursive: true, mode: 0o700 });
  if (platform !== 'win32') {
    fs.chmodSync(dirPath, 0o700);
  }
}

/**
 * Restrict an existing file to owner-only read/write (0600) on POSIX; no-op on Windows.
 */
export function restrictPrivateFile(filePath: string, platform: NodeJS.Platform = process.platform): void {
  if (platform !== 'win32') {
    fs.chmodSync(filePath, 0o600);
  }
}

/**
 * Best-effort 0600 restriction on POSIX (e.g. for newly created SQLite database files); no-op on Windows.
 */
export function restrictPrivateFileBestEffort(filePath: string, platform: NodeJS.Platform = process.platform): void {
  if (platform === 'win32') return;
  try {
    fs.chmodSync(filePath, 0o600);
  } catch {
    /* best effort */
  }
}

/**
 * Atomically write a private file with 0600 permissions on POSIX.
 */
export function writePrivateFileAtomic(
  filePath: string,
  content: string,
  platform: NodeJS.Platform = process.platform,
): void {
  const tmp = `${filePath}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, content, { mode: 0o600 });
  restrictPrivateFile(tmp, platform);
  fs.renameSync(tmp, filePath);
}

/**
 * Verify a sensitive file has private permissions:
 *  - POSIX: rejects if group/other bits are set (`mode & 0o077`).
 *  - Windows: skips POSIX mode checks because libuv synthesizes 0666/0444 bits and
 *    user isolation is governed by NTFS profile ACLs (`%APPDATA%\maestria`).
 */
export function checkPrivateFilePermissions(
  filePath: string,
  mode: number,
  createError: (message: string) => Error,
  platform: NodeJS.Platform = process.platform,
): void {
  if (platform === 'win32') return;
  if (mode & 0o077) {
    throw createError(`${filePath} has permissions ${(mode & 0o777).toString(8)}; it must be 600 (run: chmod 600 ${filePath})`);
  }
}
