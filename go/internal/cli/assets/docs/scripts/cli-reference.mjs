import { execFileSync } from 'node:child_process';
import { access, mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

const text = (value = '') => value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('{', '&#123;').replaceAll('}', '&#125;');
const cell = (value = '') => text(value).replaceAll('|', '\\|').replaceAll('\n', ' ');
export const commandRoute = (name) => `reference/cli${name === 'wendy' ? '' : '/' + name.split(' ').slice(1).join('/')}`;

// A command with no subcommands is a page, not an expandable folder.
export function commandPages(command, commands) {
  return ['index', ...commands
    .filter((c) => c.path.split(' ').slice(0, -1).join(' ') === command.path)
    .map((c) => {
      const hasChildren = commands.some((child) => child.path.split(' ').slice(0, -1).join(' ') === c.path);
      return `${hasChildren ? '' : '...'}${c.path.split(' ').at(-1)}`;
    })];
}

export function commandFlags(command, commands) {
  const flags = new Map();
  const parts = command.path.split(' ');
  for (let i = 1; i <= parts.length; i++) {
    const parent = commands.find((item) => item.path === parts.slice(0, i).join(' '));
    for (const flag of parent?.persistent_flags ?? []) flags.set(flag.name, flag);
  }
  for (const flag of command.local_flags ?? []) flags.set(flag.name, flag);
  return [...flags.values()].sort((a, b) => a.name.localeCompare(b.name));
}

export function renderCommand(command, commands, detailsURL) {
  const parent = command.path.split(' ').slice(0, -1).join(' ');
  const syntax = `${parent ? parent + ' ' : ''}${command.use} [flags]`;
  const children = commands.filter((item) => item.path.split(' ').slice(0, -1).join(' ') === command.path);
  const examples = command.path === 'wendy run'
    ? 'wendy run\nwendy run --watch\nwendy run --device my-robot\nwendy run --detach'
    : command.path === 'wendy init'
      ? 'wendy init\nwendy init hello-wendy --target wendyos --language node --template simple-api --var PORT=3000 --assistant skip --git-init no'
      : command.example || `${command.path} --help`;
  const flags = commandFlags(command, commands);
  const args = command.use.split(/\s+/).slice(1).join(' ');
  const details = detailsURL ? `[Command behavior and detailed examples](${detailsURL})` : 'Use the command help for additional guidance.';
  const output = command.path === 'wendy run'
    ? 'An attached run streams build progress and app logs. When readiness succeeds and a URL can be inferred, it prints a line such as:\n\n```text\nApp reachable at http://127.0.0.1:3000\n```\n\nThe address depends on the target. `--detach` skips log streaming, host readiness checks, and browser opening.'
    : 'Output depends on the operation and selected target. ' + details + '.';
  return `---\ntitle: ${JSON.stringify(command.path)}\ndescription: ${JSON.stringify(command.short || 'Wendy CLI command reference.')}\n---\n\n## Syntax\n\n\`\`\`sh\n${syntax}\n\`\`\`\n\n## Examples\n\n\`\`\`sh\n${examples}\n\`\`\`\n\n## Arguments\n\n${args ? `Argument notation from the CLI: \`${args}\`. Angle brackets mark required values; square brackets mark optional values. See Details for command-specific validation.` : children.length ? 'Choose a subcommand from See also.' : 'No positional arguments are declared in the command synopsis.'}\n\n## Flags\n\nFlags below include inherited options. Types and defaults come from the CLI source. An empty default means no value is configured.\n\n| Flag | Type | Default | Description |\n| --- | --- | --- | --- |\n${flags.map((f) => `| \`--${f.name}\`${f.shorthand ? ` / \`-${f.shorthand}\`` : ''} | ${cell(f.type)} | ${f.default ? '`' + cell(f.default) + '`' : '(empty)'} | ${cell(f.usage)}${f.no_opt_default && f.type !== 'bool' ? ` Value when used without an argument: \`${cell(JSON.stringify(f.no_opt_default))}\`.` : ''}${f.deprecated ? ' Deprecated: ' + cell(f.deprecated) : ''} |`).join('\n')}\n\n## Output\n\n${output}\n\n## JSON output\n\nThe global \`--json\` option requests machine-readable output. The CLI also selects JSON mode when no interactive terminal is attached. Support and payloads are command-specific; this flag does not guarantee a single JSON document for commands that stream logs or invoke build tools. Consult Details before parsing command output.\n\n## Exit codes\n\nThe CLI entry point returns \`0\` on success and for its user-cancelled or default-cleared results. Other returned errors exit with \`1\` and print a diagnostic to stderr. Some operations may terminate through subprocess or signal handling; do not infer application health from the CLI exit code alone.\n\n## Common errors\n\nCheck argument values and flag combinations against the synopsis above. For discovery, build, port, and readiness failures, see [deployment troubleshooting](/docs/errors). Command-specific constraints are in Details.\n\n## See also\n\n${parent ? `- [${parent}](/docs/${commandRoute(parent)})\n` : ''}${children.map((c) => `- [${c.path}](/docs/${commandRoute(c.path)})\n`).join('')}- [CLI conventions and configuration](/docs/reference)\n\n## Details\n\n${command.deprecated ? 'Deprecated: ' + text(command.deprecated) + '\n\n' : ''}${command.aliases?.length ? 'Aliases: ' + command.aliases.map((a) => '`' + a + '`').join(', ') + '.\n\n' : ''}${text(command.long || '')}\n\n${details}\n`;
}

export async function writeCLIReference(docsRoot, contentRoot, publicRoot, normalize) {
  const snapshot = execFileSync('go', ['run', './cmd/api-surface-snapshot'], {
    cwd: path.resolve(docsRoot, '../../../..'),
    env: { ...process.env, CGO_ENABLED: '1' },
    encoding: 'utf8', maxBuffer: 20 * 1024 * 1024,
  });
  const { commands } = JSON.parse(snapshot);
  await mkdir(path.join(publicRoot, 'reference'), { recursive: true });
  await writeFile(path.join(publicRoot, 'reference/cli.json'), snapshot);
  for (const command of commands) {
    const route = commandRoute(command.path);
    const file = `${route}/index.mdx`;
    const oldPath = `clients/wendy-cli/commands/${command.path.split(' ').slice(1).join('/')}.md`;
    let detailsURL;
    try {
      await access(path.join(docsRoot, oldPath));
      detailsURL = `/docs/advanced/${oldPath.slice(0, -3)}`;
    } catch { /* Not every command has a hand-written explanation. */ }
    await mkdir(path.join(contentRoot, route), { recursive: true });
    await writeFile(path.join(contentRoot, file), normalize(renderCommand(command, commands, detailsURL), file));
    await writeFile(path.join(contentRoot, route, 'meta.json'), JSON.stringify({
      title: command.path === 'wendy' ? 'Wendy CLI' : command.path.split(' ').at(-1),
      pages: commandPages(command, commands),
    }, null, 2));
  }
}
