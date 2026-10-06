export type Obj = Readonly<Record<string, unknown>>

export function isObj(value: unknown): value is Obj {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function obj(value: unknown): Obj | undefined {
  return isObj(value) ? value : undefined
}

export function str(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

export function num(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

export function arr(value: unknown): readonly unknown[] {
  return Array.isArray(value) ? value : []
}

/** A JSON object from text; a leading UTF-8 BOM is ignored. */
export function parseJson(text: string): Obj | undefined {
  try {
    return obj(JSON.parse(text.replace(/^\uFEFF/, '')))
  } catch {
    return undefined
  }
}

/**
 * The JSON object a text starts with, and the text after it; undefined when the
 * text starts with anything else. Wendy MCP results can carry more text after
 * their JSON, such as the CLI's one-time update notice.
 */
export function leadingJson(text: string): { readonly value: Obj; readonly rest: string } | undefined {
  const source = text.replace(/^\uFEFF/, '')
  const start = source.search(/\S/)
  if (start === -1 || source[start] !== '{') return undefined
  let depth = 0
  let inString = false
  let escaped = false
  for (let i = start; i < source.length; i++) {
    const c = source[i]
    if (inString) {
      if (escaped) escaped = false
      else if (c === '\\') escaped = true
      else if (c === '"') inString = false
    } else if (c === '"') {
      inString = true
    } else if (c === '{' || c === '[') {
      depth += 1
    } else if (c === '}' || c === ']') {
      depth -= 1
      if (depth === 0) {
        const value = parseJson(source.slice(start, i + 1))
        return value === undefined ? undefined : { value, rest: source.slice(i + 1) }
      }
    }
  }
  return undefined
}
