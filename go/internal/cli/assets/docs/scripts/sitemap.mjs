import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import { pageUrl } from './publish-markdown.mjs';

// Crawlers only read robots.txt at the site root. The deploy workflow copies
// the latest/ robots.txt and sitemap.xml there on stable releases.
export const robotsTxt = 'User-agent: *\nAllow: /\n\nSitemap: https://docs.wendy.dev/sitemap.xml\n';

const xml = (value) => value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&apos;');

// Maps each file under dir, by path relative to dir, to the committer date of
// the last first-parent commit that changed it, so a page merged from a branch
// is dated when it reached main. A shallow clone's boundary commits list every
// file as added, so files seen only there get no date. Without git, the map is empty.
export function gitLastModified(dir) {
  const git = (...args) => execFileSync('git', args, {
    cwd: dir, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, stdio: ['ignore', 'pipe', 'ignore'],
  });
  let log;
  let boundary = new Set();
  try {
    log = git('-c', 'core.quotePath=false', 'log', '--first-parent', '--diff-merges=first-parent', '--no-renames',
      '--name-only', '--relative', '--format=%x01%H %cI', '--', '.');
    if (git('rev-parse', '--is-shallow-repository').trim() === 'true') {
      boundary = new Set(readFileSync(path.resolve(dir, git('rev-parse', '--git-path', 'shallow').trim()), 'utf8').split('\n'));
    }
  } catch {
    return new Map();
  }
  const dates = new Map();
  for (const commit of log.split('\x01').slice(1)) {
    const [header, ...files] = commit.split('\n');
    const [sha, date] = header.split(' ');
    if (boundary.has(sha)) continue;
    for (const file of files) if (file && !dates.has(file)) dates.set(file, date);
  }
  return dates;
}

// Pages without a lastmod are listed without <lastmod> rather than with a guessed date.
export function renderSitemap(pages, basePath) {
  const urls = pages.map(({ slug, lastmod }) => `  <url>\n    <loc>${xml(encodeURI(pageUrl(basePath, slug)))}</loc>\n`
    + `${lastmod ? `    <lastmod>${lastmod}</lastmod>\n` : ''}  </url>\n`);
  return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.join('')}</urlset>\n`;
}

export async function writeSitemap(publicRoot, basePath, pages) {
  await writeFile(path.join(publicRoot, 'sitemap.xml'), renderSitemap(pages, basePath));
  await writeFile(path.join(publicRoot, 'robots.txt'), robotsTxt);
}
