import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { readdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const docsRoot = path.resolve(fileURLToPath(new URL('..', import.meta.url)));
const skipDirs = new Set(['.next', 'content', 'node_modules', 'out', 'public']);

async function* mdxFiles(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    if (skipDirs.has(entry.name) || entry.name.startsWith('.')) continue;
    const entryPath = path.join(dir, entry.name);
    if (entry.isDirectory()) yield* mdxFiles(entryPath);
    else if (entry.name.endsWith('.mdx')) yield entryPath;
  }
}

// The docs CSP allows same-origin images only, so YouTubeVideo cannot load
// thumbnails from YouTube and each video needs a copy under images/youtube.
test('every YouTubeVideo has a local thumbnail', async () => {
  const missing = [];
  for await (const file of mdxFiles(docsRoot)) {
    const source = await readFile(file, 'utf8');
    for (const [, id] of source.matchAll(/<YouTubeVideo\s[^>]*\bid="([^"]+)"/g)) {
      if (!existsSync(path.join(docsRoot, 'images/youtube', `${id}.webp`))) {
        missing.push(`${path.relative(docsRoot, file)}: ${id}`);
      }
    }
  }
  assert.deepEqual(missing, []);
});
