import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { frontmatterValue, publishMarkdown } from './publish-markdown.mjs';

async function withContent(files, run) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'wendy-docs-'));
  try {
    const content = path.join(root, 'content');
    const publicRoot = path.join(root, 'public');
    await mkdir(publicRoot);
    for (const [file, raw] of Object.entries(files)) {
      await mkdir(path.dirname(path.join(content, file)), { recursive: true });
      await writeFile(path.join(content, file), raw);
    }
    await run(content, publicRoot);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

test('publishes homepage and nested pages with version-aware canonical URLs', async () => {
  const home = '---\ntitle: Quickstart\n---\n\nRun an app.\n';
  await withContent({
    'index.mdx': home,
    'reference/cli/run/index.mdx': '---\ntitle: "wendy run"\n---\n\n## Flags\n',
  }, async (content, publicRoot) => {
    await publishMarkdown(content, publicRoot, '/release-test');
    assert.equal(await readFile(path.join(publicRoot, 'markdown/index.md'), 'utf8'), home);
    assert.equal(await readFile(path.join(publicRoot, 'index.md'), 'utf8'), home);
    assert.match(await readFile(path.join(publicRoot, 'markdown/reference/cli/run.md'), 'utf8'), /## Flags/);
    assert.match(await readFile(path.join(publicRoot, 'reference/cli/run.md'), 'utf8'), /## Flags/);
    const index = await readFile(path.join(publicRoot, 'llms.txt'), 'utf8');
    assert.match(index, /\[Quickstart\]\(https:\/\/docs.wendy.dev\/release-test\/\)/);
    assert.match(index, /\[wendy run\]\(https:\/\/docs.wendy.dev\/release-test\/reference\/cli\/run.md\)/);
    const full = await readFile(path.join(publicRoot, 'llms-full.txt'), 'utf8');
    assert.match(full, /Source: https:\/\/docs.wendy.dev\/release-test\/reference\/cli\/run\/\n/);
    assert.match(full, /## Flags/);
  });
});

test('follows the llms.txt layout: summary first, descriptions, skippable pages under Optional', async () => {
  await withContent({
    'index.mdx': '---\ntitle: Quickstart\ndescription: Deploy your first app.\n---\n\nBody.\n',
    'installation/linux.mdx': '---\ntitle: Linux computer\ndescription: "Install the \\"wendy\\" CLI."\n---\n\nBody.\n',
    'installation/ubuntu.mdx': '---\ntitle: Ubuntu\ndescription: Redirects to the Linux installation guide\n---\n',
    'reference/cli/run/index.mdx': '---\ntitle: "wendy run"\ndescription: "Build and run an app."\n---\n\nBody.\n',
    'advanced/wendy-lite/index.md': '---\ntitle: "Wendy Lite"\ndescription: "Wendy for ESP32."\n---\n\nBody.\n',
    'advanced/development/testing.md': '---\ntitle: "Testing"\ndescription: "Run the agent tests."\n---\n\nBody.\n',
    'advanced/development/local-mesh-radio.md': '---\ntitle: "Wendy Net over Bluetooth"\n---\n\nBody.\n',
    'advanced/RELEASES.md': '---\ntitle: "Release Process"\n---\n\nBody.\n',
  }, async (content, publicRoot) => {
    const listed = await publishMarkdown(content, publicRoot, '/latest');
    const index = await readFile(path.join(publicRoot, 'llms.txt'), 'utf8');
    assert.match(index, /^# Wendy documentation\n\n> Wendy is /);
    const [, docs, detailed, optional] = index.split(/\n## (?:Docs|Detailed reference|Optional)\n/);
    assert.match(docs, /^- \[Quickstart\]\(https:\/\/docs.wendy.dev\/latest\/index.md\): Deploy your first app.$/m);
    assert.match(docs, /^- \[Linux computer\]\(https:\/\/docs.wendy.dev\/latest\/installation\/linux.md\): Install the "wendy" CLI.$/m);
    assert.doesNotMatch(docs, /reference\/cli\/run|advanced/);
    assert.match(detailed, /^- \[Wendy Lite\]\(https:\/\/docs.wendy.dev\/latest\/advanced\/wendy-lite.md\): Wendy for ESP32.$/m);
    assert.match(detailed, /^- \[Wendy Net over Bluetooth\]\(https:\/\/docs.wendy.dev\/latest\/advanced\/development\/local-mesh-radio.md\)$/m);
    assert.doesNotMatch(detailed, /testing|RELEASES/);
    assert.match(optional, /\[wendy run\]\(https:\/\/docs.wendy.dev\/latest\/reference\/cli\/run.md\): Build and run an app./);
    assert.match(optional, /\[Testing\]\(https:\/\/docs.wendy.dev\/latest\/advanced\/development\/testing.md\)/);
    assert.match(optional, /\[Release Process\]\(https:\/\/docs.wendy.dev\/latest\/advanced\/RELEASES.md\)/);
    // A page without a body, such as a legacy redirect stub, is published but not listed.
    assert.doesNotMatch(index, /Ubuntu/);
    assert.ok(!listed.some((p) => p.slug === 'installation/ubuntu'));
    assert.equal(listed.length, 7);
    assert.match(await readFile(path.join(publicRoot, 'installation/ubuntu.md'), 'utf8'), /Redirects/);
  });
});

test('reads plain and JSON-quoted frontmatter values only from the frontmatter', () => {
  const raw = '---\ntitle: "A \\"quoted\\" title"\ndescription: Plain text: with a colon\n---\n\n```yaml\nsummary: not frontmatter\n```\n';
  assert.equal(frontmatterValue(raw, 'title'), 'A "quoted" title');
  assert.equal(frontmatterValue(raw, 'description'), 'Plain text: with a colon');
  assert.equal(frontmatterValue(raw, 'summary'), '');
  assert.equal(frontmatterValue('# No frontmatter\n\ntitle: body text\n', 'title'), '');
  assert.equal(frontmatterValue('---\ndescription:\ntitle: Next line\n---\n', 'description'), '');
});
