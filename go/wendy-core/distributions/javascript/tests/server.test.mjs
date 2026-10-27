import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, realpath, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { resolveServedFile } from './server-files.mjs';

test('static server confines files and symlinks to the selected package', async () => {
  const temporary = await mkdtemp(join(tmpdir(), 'core-server-'));
  try {
    const root = join(temporary, 'package');
    await mkdir(root);
    await writeFile(join(root, 'browser.mjs'), 'export {}');
    await writeFile(join(temporary, 'secret.mjs'), 'outside');
    assert.equal(await resolveServedFile(root, '/browser.mjs'), await realpath(join(root, 'browser.mjs')));
    await assert.rejects(resolveServedFile(root, '/../secret.mjs'), /Outside package/);
    // A directory junction works on Windows without elevated symlink privileges.
    await symlink(temporary, join(root, 'escape'), 'junction');
    await assert.rejects(resolveServedFile(root, '/escape/secret.mjs'), /Outside package/);
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
});
