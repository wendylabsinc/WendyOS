import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { gitLastModified, renderSitemap, writeSitemap } from './sitemap.mjs';

test('lists canonical page URLs with lastmod only when a date is known', () => {
  const sitemap = renderSitemap([
    { slug: '', lastmod: '2026-10-01T12:00:00+00:00' },
    { slug: 'guides/stagefile', lastmod: '2026-09-06T05:01:05-06:00' },
    { slug: 'reference/cli/run' },
  ], '/latest');
  assert.match(sitemap, /^<\?xml version="1.0" encoding="UTF-8"\?>\n<urlset xmlns="http:\/\/www.sitemaps.org\/schemas\/sitemap\/0.9">\n/);
  assert.match(sitemap, /<loc>https:\/\/docs.wendy.dev\/latest\/<\/loc>\n {4}<lastmod>2026-10-01T12:00:00\+00:00<\/lastmod>/);
  assert.match(sitemap, /<loc>https:\/\/docs.wendy.dev\/latest\/guides\/stagefile\/<\/loc>\n {4}<lastmod>2026-09-06T05:01:05-06:00<\/lastmod>/);
  assert.match(sitemap, /<loc>https:\/\/docs.wendy.dev\/latest\/reference\/cli\/run\/<\/loc>\n {2}<\/url>/);
  assert.equal(sitemap.match(/<url>/g).length, 3);
  assert.match(sitemap, /<\/urlset>\n$/);
});

test('escapes page URLs for XML', () => {
  assert.match(renderSitemap([{ slug: "a&b/it's" }], ''), /<loc>https:\/\/docs.wendy.dev\/a&amp;b\/it&apos;s\/<\/loc>/);
});

test('writes sitemap.xml and a robots.txt that points to the root sitemap', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'wendy-sitemap-'));
  try {
    await writeSitemap(root, '/latest', [{ slug: 'faq' }]);
    assert.equal(await readFile(path.join(root, 'robots.txt'), 'utf8'),
      'User-agent: *\nAllow: /\n\nSitemap: https://docs.wendy.dev/sitemap.xml\n');
    assert.match(await readFile(path.join(root, 'sitemap.xml'), 'utf8'), /<loc>https:\/\/docs.wendy.dev\/latest\/faq\/<\/loc>/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test('dates files by their last first-parent commit and skips shallow boundaries', async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'wendy-sitemap-git-'));
  const repo = path.join(root, 'repo');
  const git = (cwd, date, ...args) => execFileSync('git', [
    '-c', 'user.name=Docs Test', '-c', 'user.email=docs-test@example.com', '-c', 'commit.gpgsign=false',
    '-c', 'core.hooksPath=/dev/null', ...args,
  ], { cwd, env: { ...process.env, GIT_AUTHOR_DATE: date, GIT_COMMITTER_DATE: date }, stdio: 'pipe' });
  try {
    execFileSync('git', ['init', '-q', '-b', 'main', repo]);
    await writeFile(path.join(repo, 'a.md'), 'a\n');
    await writeFile(path.join(repo, 'old.md'), 'old\n');
    git(repo, '2026-01-01T00:00:00+02:00', 'add', '.');
    git(repo, '2026-01-01T00:00:00+02:00', 'commit', '-q', '-m', 'add a and old');
    git(repo, '2026-01-02T00:00:00+02:00', 'checkout', '-q', '-b', 'topic');
    await writeFile(path.join(repo, 'b.md'), 'b\n');
    git(repo, '2026-01-02T00:00:00+02:00', 'add', 'b.md');
    git(repo, '2026-01-02T00:00:00+02:00', 'commit', '-q', '-m', 'add b on a branch');
    git(repo, '2026-01-05T00:00:00+02:00', 'checkout', '-q', 'main');
    await writeFile(path.join(repo, 'a.md'), 'a2\n');
    git(repo, '2026-01-03T00:00:00+02:00', 'commit', '-q', '-am', 'update a');
    git(repo, '2026-01-05T00:00:00+02:00', 'merge', '-q', '--no-ff', '-m', 'merge topic', 'topic');

    const dates = gitLastModified(repo);
    assert.equal(dates.get('a.md'), '2026-01-03T00:00:00+02:00');
    assert.equal(dates.get('old.md'), '2026-01-01T00:00:00+02:00');
    // The branch commit is older than the merge; the page went live at the merge.
    assert.equal(dates.get('b.md'), '2026-01-05T00:00:00+02:00');

    const shallow = path.join(root, 'shallow');
    execFileSync('git', ['clone', '-q', '--depth', '2', `file://${repo}`, shallow], { stdio: 'pipe' });
    const shallowDates = gitLastModified(shallow);
    assert.equal(shallowDates.get('b.md'), '2026-01-05T00:00:00+02:00');
    // Files seen only in the boundary commit have no known date.
    assert.equal(shallowDates.has('a.md'), false);
    assert.equal(shallowDates.has('old.md'), false);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
