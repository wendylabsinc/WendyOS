import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { publishMarkdown } from './publish-markdown.mjs';

test('publishes homepage and nested pages with version-aware canonical URLs', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'wendy-docs-'));
  try {
    const content = path.join(root, 'content');
    const publicRoot = path.join(root, 'public');
    await mkdir(path.join(content, 'reference/cli/run'), { recursive: true });
    await mkdir(publicRoot);
    const home = '---\ntitle: Quickstart\n---\n\nRun an app.\n';
    await writeFile(path.join(content, 'index.mdx'), home);
    await writeFile(path.join(content, 'reference/cli/run/index.mdx'), '---\ntitle: "wendy run"\n---\n\n## Flags\n');
    await publishMarkdown(content, publicRoot, '/release-test');
    assert.equal(await readFile(path.join(publicRoot, 'markdown/index.md'), 'utf8'), home);
    const index = await readFile(path.join(publicRoot, 'llms.txt'), 'utf8');
    assert.match(index, /https:\/\/docs.wendy.dev\/release-test\/reference\/cli\/run\//);
    assert.match(index, /https:\/\/docs.wendy.dev\/release-test\/markdown\/reference\/cli\/run.md/);
    assert.match(await readFile(path.join(publicRoot, 'llms-full.txt'), 'utf8'), /## Flags/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
