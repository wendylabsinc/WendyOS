import { mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

export async function publishMarkdown(contentRoot, publicRoot, basePath) {
  const pages = [];
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = path.join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (/\.mdx?$/.test(entry.name)) {
        const relative = path.relative(contentRoot, file).replaceAll(path.sep, '/');
        const slug = relative.replace(/\.mdx?$/, '').replace(/(^|\/)index$/, '');
        const raw = await readFile(file, 'utf8');
        const title = raw.match(/^title:\s*(.+)$/m)?.[1]?.replace(/^"|"$/g, '') || slug;
        const target = path.join(publicRoot, 'markdown', `${slug || 'index'}.md`);
        await mkdir(path.dirname(target), { recursive: true });
        await writeFile(target, raw);
        pages.push({ title, raw, slug });
      }
    }
  }
  await walk(contentRoot);
  pages.sort((a, b) => a.slug.localeCompare(b.slug));
  const url = (slug) => `https://docs.wendy.dev${basePath}/${slug}${slug ? '/' : ''}`;
  const index = '# Wendy documentation\n\nStart with the quickstart, then retrieve the specific command or hardware guide you need. CLI flags are generated from source. Native Mac and ESP32 workflows differ from Linux containers.\n\n'
    + `- [Quickstart](${url('')})\n- [CLI commands and options JSON](https://docs.wendy.dev${basePath}/reference/cli.json)\n- [App JSON Schema](https://docs.wendy.dev${basePath}/reference/wendy.schema.json)\n\n## Pages\n\n`
    + pages.map((p) => `- [${p.title}](${url(p.slug)}): [Markdown](https://docs.wendy.dev${basePath}/markdown/${p.slug || 'index'}.md)`).join('\n');
  await writeFile(path.join(publicRoot, 'llms.txt'), index + '\n');
  await writeFile(path.join(publicRoot, 'llms-full.txt'), pages.map((p) => `# ${p.title}\nSource: ${url(p.slug)}\n\n${p.raw}`).join('\n\n---\n\n'));
}
