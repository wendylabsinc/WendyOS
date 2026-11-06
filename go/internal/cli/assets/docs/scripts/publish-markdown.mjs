import { mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

const frontmatterBlock = /^---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)/;

export const pageSlug = (relative) => relative.replaceAll(path.sep, '/').replace(/\.mdx?$/, '').replace(/(^|\/)index$/, '');
// The site is exported with `trailingSlash: true`, so page URLs end in a slash.
export const pageUrl = (basePath, slug) => `https://docs.wendy.dev${basePath}/${slug}${slug ? '/' : ''}`;

// Reads a one-line frontmatter value; the prep script writes quoted values with JSON.stringify.
export function frontmatterValue(raw, key) {
  const value = raw.match(frontmatterBlock)?.[1].match(new RegExp(`^${key}:[ \\t]*(.+)$`, 'm'))?.[1].trim();
  if (!value) return '';
  if (value.startsWith('"')) {
    try { return JSON.parse(value); } catch { /* Not JSON; strip the quotes below. */ }
  }
  return value.replace(/^(["'])(.*)\1$/, '$2');
}

// Generated CLI reference and contributor or internal process pages go under
// "## Optional", which the llms.txt format (https://llmstxt.org) reserves for
// content agents can skip. Product pages under advanced/ stay in the index.
const optionalPages = /^(reference\/cli|advanced\/(development|incident_response|RELEASES|wendy-data-platform-demo|wendy-os-publisher))(\/|$)/;
// local-mesh-radio is a user guide that lives in the contributor directory.
export const isOptional = (slug) => optionalPages.test(slug) && slug !== 'advanced/development/local-mesh-radio';

// Publishes each page's Markdown at markdown/<slug>.md and at its page URL plus
// .md (index.md for the homepage), then writes llms.txt and llms-full.txt.
// Returns the pages listed in llms.txt: pages without a body, such as legacy
// redirect stubs, are left out.
export async function publishMarkdown(contentRoot, publicRoot, basePath) {
  const pages = [];
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = path.join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (/\.mdx?$/.test(entry.name)) {
        const slug = pageSlug(path.relative(contentRoot, file));
        const raw = await readFile(file, 'utf8');
        const title = frontmatterValue(raw, 'title') || slug;
        const description = frontmatterValue(raw, 'description').replace(/\s+/g, ' ');
        for (const target of [path.join(publicRoot, 'markdown', `${slug || 'index'}.md`), path.join(publicRoot, `${slug || 'index'}.md`)]) {
          await mkdir(path.dirname(target), { recursive: true });
          await writeFile(target, raw);
        }
        pages.push({ title, description, raw, slug, hasBody: raw.replace(frontmatterBlock, '').trim() !== '' });
      }
    }
  }
  await walk(contentRoot);
  pages.sort((a, b) => a.slug.localeCompare(b.slug));
  const listed = pages.filter((p) => p.hasBody);
  const url = (slug) => pageUrl(basePath, slug);
  // Links go to the Markdown at the page URL plus .md, as in the llms.txt examples.
  const entry = (p) => `- [${p.title}](https://docs.wendy.dev${basePath}/${p.slug || 'index'}.md)${p.description ? `: ${p.description}` : ''}`;
  const section = (heading, filter) => `## ${heading}\n\n${listed.filter(filter).map(entry).join('\n')}`;
  const isAdvanced = (slug) => /^advanced(\/|$)/.test(slug);
  const index = '# Wendy documentation\n\n'
    + '> Wendy is an open source operating system and developer toolchain for robots, drones, and edge AI devices such as NVIDIA Jetson and Raspberry Pi. The `wendy` CLI builds, deploys, and debugs containerized apps on a device over USB-C, the local network, or Wendy Cloud.\n\n'
    + 'Start with the quickstart, then retrieve the specific command or hardware guide you need. CLI flags are generated from source. Native Mac and ESP32 workflows differ from Linux containers. Page links below return Markdown: `<page>.md` is the source of the page at `<page>/`.\n\n'
    + `- [Quickstart](${url('')})\n- [CLI commands and options JSON](https://docs.wendy.dev${basePath}/reference/cli.json)\n- [App JSON Schema](https://docs.wendy.dev${basePath}/reference/wendy.schema.json)\n\n`
    + `${section('Docs', (p) => !isOptional(p.slug) && !isAdvanced(p.slug))}\n\n`
    + `${section('Detailed reference', (p) => !isOptional(p.slug) && isAdvanced(p.slug))}\n\n`
    + section('Optional', (p) => isOptional(p.slug));
  await writeFile(path.join(publicRoot, 'llms.txt'), index + '\n');
  await writeFile(path.join(publicRoot, 'llms-full.txt'), pages.map((p) => `# ${p.title}\nSource: ${url(p.slug)}\n\n${p.raw}`).join('\n\n---\n\n'));
  return listed;
}
