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

test('follows the llms.txt layout: summary first, generated reference under Optional', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'wendy-docs-'));
  try {
    const content = path.join(root, 'content');
    const publicRoot = path.join(root, 'public');
    await mkdir(path.join(content, 'installation'), { recursive: true });
    await mkdir(path.join(content, 'reference/cli/run'), { recursive: true });
    await mkdir(path.join(content, 'advanced/apps'), { recursive: true });
    await mkdir(publicRoot);
    await writeFile(path.join(content, 'index.mdx'), '---\ntitle: Quickstart\n---\n');
    await writeFile(path.join(content, 'installation/linux.mdx'), '---\ntitle: Linux computer\n---\n');
    await writeFile(path.join(content, 'reference/cli/run/index.mdx'), '---\ntitle: "wendy run"\n---\n');
    await writeFile(path.join(content, 'advanced/apps/compose.md'), '---\ntitle: Compose\n---\n');
    await publishMarkdown(content, publicRoot, '/latest');
    const index = await readFile(path.join(publicRoot, 'llms.txt'), 'utf8');
    assert.match(index, /^# Wendy documentation\n\n> Wendy is /);
    const [docs, optional] = index.split('\n## Optional\n');
    const pages = docs.split('\n## Docs\n')[1];
    assert.match(pages, /\[Linux computer\]\(https:\/\/docs.wendy.dev\/latest\/installation\/linux\/\)/);
    assert.match(pages, /\[Quickstart\]\(https:\/\/docs.wendy.dev\/latest\/\)/);
    assert.doesNotMatch(pages, /reference\/cli\/run|advanced\/apps/);
    assert.match(optional, /\[wendy run\]\(https:\/\/docs.wendy.dev\/latest\/reference\/cli\/run\/\)/);
    assert.match(optional, /\[Compose\]\(https:\/\/docs.wendy.dev\/latest\/advanced\/apps\/compose\/\)/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
