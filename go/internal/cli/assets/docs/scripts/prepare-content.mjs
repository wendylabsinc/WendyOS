import { writeCLIReference } from './cli-reference.mjs';
import { publishMarkdown } from './publish-markdown.mjs';
import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const docsRoot = path.resolve(fileURLToPath(new URL('..', import.meta.url)));
const contentRoot = path.join(docsRoot, 'content', 'docs');
const publicRoot = path.join(docsRoot, 'public');
const basePath = (process.env.NEXT_PUBLIC_BASE_PATH || '').replace(/\/$/, '');
const skipDirs = new Set([
  '.git',
  '.next',
  '.source',
  'app',
  'components',
  'content',
  'export',
  'lib',
  'node_modules',
  'out',
  'public',
  'screenshots',
  'scripts',
]);
const publicAssetExtensions = new Set([
  '.gif',
  '.ico',
  '.js',
  '.jpeg',
  '.jpg',
  '.json',
  '.mp4',
  '.pdf',
  '.png',
  '.svg',
  '.txt',
  '.webm',
  '.webp',
  '.zip',
]);
const appFiles = new Set(['package-lock.json', 'package.json', 'tsconfig.json']);

await rm(contentRoot, { recursive: true, force: true });
await rm(publicRoot, { recursive: true, force: true });
await mkdir(contentRoot, { recursive: true });
await mkdir(publicRoot, { recursive: true });

const markdownFiles = [];
const metaFiles = [];
const assetFiles = [];
const routeDirsBySourceDir = new Map();

async function walk(dir) {
  const entries = await readdir(dir, { withFileTypes: true });

  for (const entry of entries) {
    if (entry.name === 'AGENTS.md' || entry.name === 'CLAUDE.md') continue;
    if (entry.name.startsWith('.') && entry.name !== '.gitignore') continue;
    if (entry.isDirectory() && skipDirs.has(entry.name)) continue;

    const absolutePath = path.join(dir, entry.name);
    const relativePath = path.relative(docsRoot, absolutePath);

    if (entry.isDirectory()) {
      await walk(absolutePath);
    } else if (entry.isFile() && isMarkdownFile(entry.name)) {
      markdownFiles.push({ absolutePath, relativePath });
    } else if (entry.isFile() && entry.name === 'meta.json') {
      metaFiles.push({ absolutePath, relativePath });
    } else if (entry.isFile() && shouldPublishAsset(relativePath)) {
      assetFiles.push({ absolutePath, relativePath });
    }
  }
}

await walk(docsRoot);

for (const file of markdownFiles) {
  const targetRelativePath = normalizeMarkdownPath(file.relativePath);
  const targetPath = path.join(contentRoot, targetRelativePath);
  const raw = normalizeMarkdown(await readFile(file.absolutePath, 'utf8'), targetRelativePath);

  addRouteDirForSource(file.relativePath, targetRelativePath);
  await mkdir(path.dirname(targetPath), { recursive: true });
  await writeFile(targetPath, withFrontmatter(raw, targetRelativePath), 'utf8');
}

for (const file of metaFiles) {
  const targetPath = path.join(contentRoot, file.relativePath);
  const raw = await readFile(file.absolutePath, 'utf8');

  await mkdir(path.dirname(targetPath), { recursive: true });
  await writeFile(targetPath, raw, 'utf8');
}

await writeAdvancedIndexPage();
await writeAdvancedMeta();
await writeCLIReference(docsRoot, contentRoot, publicRoot, normalizeMarkdown);
await writePublicAsset(path.join(publicRoot, 'reference/wendy.schema.json'),
  await readFile(path.resolve(docsRoot, '../../../shared/appconfig/wendy.schema.json')));
await publishMarkdown(contentRoot, publicRoot, basePath);

for (const file of assetFiles) {
  const raw = await readFile(file.absolutePath);

  await writePublicAsset(path.join(contentRoot, file.relativePath), raw);
  await writePublicAsset(path.join(publicRoot, file.relativePath), raw);

  for (const routeDir of routeDirsBySourceDir.get(path.dirname(file.relativePath)) ?? []) {
    await writePublicAsset(path.join(publicRoot, routeDir, path.basename(file.relativePath)), raw);
  }
}

function normalizeMarkdownPath(relativePath) {
  const extension = path.extname(relativePath).toLowerCase();
  let targetPath = relativePath;

  if (path.basename(relativePath).toLowerCase() === 'readme.md') {
    targetPath = path.join(path.dirname(relativePath), 'index.md');
  }

  if (extension === '.md') {
    return path.join('advanced', targetPath);
  }

  return targetPath;
}

function isMarkdownFile(filename) {
  const extension = path.extname(filename).toLowerCase();

  return extension === '.md' || extension === '.mdx';
}

function addRouteDirForSource(sourceRelativePath, targetRelativePath) {
  const sourceDir = path.dirname(sourceRelativePath);
  const routeDir =
    path.basename(targetRelativePath) === 'index.md'
      ? path.dirname(targetRelativePath)
      : targetRelativePath.slice(0, -path.extname(targetRelativePath).length);

  if (routeDir === '.') return;

  const routeDirs = routeDirsBySourceDir.get(sourceDir) ?? new Set();
  routeDirs.add(routeDir);
  routeDirsBySourceDir.set(sourceDir, routeDirs);
}

async function writePublicAsset(targetPath, raw) {
  await mkdir(path.dirname(targetPath), { recursive: true });
  await writeFile(targetPath, raw);
}

function shouldPublishAsset(relativePath) {
  const extension = path.extname(relativePath).toLowerCase();

  if (appFiles.has(relativePath)) return false;

  return publicAssetExtensions.has(extension);
}

function withFrontmatter(raw, relativePath) {
  if (raw.trimStart().startsWith('---')) return raw;

  const title = inferTitle(raw, relativePath);
  const description = inferDescription(raw);

  return `---\ntitle: ${JSON.stringify(title)}\ndescription: ${JSON.stringify(description)}\n---\n\n${raw}`;
}

function normalizeMarkdown(raw, targetRelativePath) {
  return raw
    .replace(/^```bitbake\b/gm, '```ini')
    .replace(/\]\(\/docs\/([^)#\s]*)(#[^)]+)?\)/g, (_match, targetPath, hash = '') => {
      return `](${relativeFromPage(targetRelativePath, targetPath, hash)})`;
    })
    .replace(/href="\/docs\/([^"#]*)(#[^"]+)?"/g, (_match, targetPath, hash = '') => {
      return `href="${relativeFromPage(targetRelativePath, targetPath, hash)}"`;
    })
    .replace(/\]\(\/((?:icons|images|videos)\/[^)#\s]+)(#[^)]+)?\)/g, (_match, assetPath, hash = '') => {
      return `](${relativeAssetImport(targetRelativePath, assetPath, hash)})`;
    })
    .replace(/src="\/((?:icons|images|videos)\/[^"]+)"/g, (_match, assetPath) => {
      return `src="${absoluteAssetPath(assetPath)}"`;
    });
}

function relativeFromPage(fromRelativePath, targetPath, hash = '') {
  const rootPrefix = routeSegments(fromRelativePath).length === 0 ? './' : '../'.repeat(routeSegments(fromRelativePath).length);
  const normalizedTarget = targetPath.replace(/^\/+/, '').replace(/\/$/, '');
  // The site is exported with `trailingSlash: true`, so doc routes are canonical
  // as `.../slug/`. Emit the trailing slash here too — without it a raw `<a href>`
  // to a slugless path makes GCS 301 to `.../index.html`, which the fumadocs router
  // can't match (sidebar stays collapsed / unhighlighted).
  const targetWithSlash = normalizedTarget ? `${normalizedTarget}/` : '';

  return `${rootPrefix}${targetWithSlash}${hash}`;
}

function relativeAssetImport(fromRelativePath, targetPath, hash = '') {
  const fromDir = path.posix.dirname(fromRelativePath.replaceAll(path.sep, '/'));
  const normalizedTarget = targetPath.replace(/^\/+/, '').replace(/\/$/, '');
  const relativePath = path.posix.relative(fromDir, normalizedTarget);
  const importPath = relativePath.startsWith('.') ? relativePath : `./${relativePath}`;

  return `${importPath}${hash}`;
}

function absoluteAssetPath(targetPath) {
  const normalizedTarget = targetPath.replace(/^\/+/, '').replace(/\/$/, '');

  return `${basePath}/${normalizedTarget}`;
}

function routeSegments(relativePath) {
  const extension = path.extname(relativePath);
  const withoutExtension = relativePath.slice(0, -extension.length);

  return withoutExtension
    .split(path.sep)
    .filter((segment) => segment && segment !== 'index');
}

function inferTitle(raw, relativePath) {
  const heading = raw.match(/^#\s+(.+)$/m)?.[1]?.trim();
  if (heading) return stripMarkdown(heading);

  const basename = path.basename(relativePath, path.extname(relativePath));
  const dirname = path.basename(path.dirname(relativePath));
  const name = basename === 'index' ? dirname : basename;

  return toTitle(name);
}

function inferDescription(raw) {
  const withoutCode = raw.replace(/```[\s\S]*?```/g, '');
  const paragraph = withoutCode
    .split(/\n{2,}/)
    .map((part) => part.trim())
    .find((part) => part && !part.startsWith('#') && !part.startsWith('|') && !part.startsWith('---'));

  if (!paragraph) return 'WendyOS developer documentation.';

  return stripMarkdown(paragraph).replace(/\s+/g, ' ').slice(0, 180);
}

function stripMarkdown(value) {
  return value
    .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')
    .replace(/[`*_>#]/g, '')
    .trim();
}

function toTitle(value) {
  return value
    .replace(/[-_]+/g, ' ')
    .replace(/\b\w/g, (char) => char.toUpperCase())
    .replace(/\bCli\b/g, 'CLI')
    .replace(/\bGpu\b/g, 'GPU')
    .replace(/\bGrpc\b/g, 'gRPC')
    .replace(/\bMcp\b/g, 'MCP')
    .replace(/\bOci\b/g, 'OCI')
    .replace(/\bPki\b/g, 'PKI');
}

async function writeAdvancedIndexPage() {
  const targetPath = path.join(contentRoot, 'advanced', 'index.md');
  const body = `---\ntitle: "Detailed reference"\ndescription: "Generated and low-level WendyOS reference documentation."\n---\n\n# Detailed reference\n\nBehavioral explanations and lower-level WendyOS documentation live here.\n\n## Reference areas\n\n- [Wendy CLI](./clients/wendy-cli/global-flags.md)\n- [App configuration](./apps/wendy.json.md)\n- [Wendy Cloud](./cloud/requirements.md)\n- [WendyOS internals](./wendyos/requirements.md)\n- [Development](./development/)\n`;

  await writeFile(targetPath, body, 'utf8');
}

async function writeAdvancedMeta() {
  const targetPath = path.join(contentRoot, 'advanced', 'meta.json');
  const body = JSON.stringify(
    {
      title: 'Detailed reference',
      pages: [
        'index',
        'clients',
        'apps',
        'cloud',
        'wendyos',
        'debugging',
        'development',
        'architecture',
        'pki',
        'vscode',
        'wendy-lite',
        'wendy-os-publisher',
        'Examples',
        'RELEASES',
        'entitlements',
        'roadmap',
      ],
    },
    null,
    2,
  );

  await mkdir(path.dirname(targetPath), { recursive: true });
  await writeFile(targetPath, `${body}\n`, 'utf8');
}
