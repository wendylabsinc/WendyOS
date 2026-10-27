import { realpath } from 'node:fs/promises';
import { resolve, sep } from 'node:path';

// Check both lexical traversal and the real symlink destination before reading.
export async function resolveServedFile(base, relative) {
  const directory = await realpath(base);
  const candidate = resolve(directory, `.${relative}`);
  if (!candidate.startsWith(directory + sep)) throw new Error('Outside package');
  const file = await realpath(candidate);
  if (!file.startsWith(directory + sep)) throw new Error('Outside package');
  return file;
}
