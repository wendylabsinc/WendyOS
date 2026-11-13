import type { On, RenderElement } from 'claude-code'

import { CONTAINERS_RUNNING, INSPECT_HEALTHY, mcpJson, RUN_STARTED, STATUS_CONNECTED, WENDY_JSON } from './fixtures'

export type RunAnswer =
  | { readonly kind: 'result'; readonly json: object; readonly isError?: true; readonly suffix?: string }
  | { readonly kind: 'deny'; readonly reason: string }

/** What the engine and the Wendy MCP server answer beneath the plugin; tests change it between steps. */
export type World = {
  status: object | 'missing'
  inspect: object | 'missing'
  containers: object | 'missing'
  servers: readonly string[]
  files: Record<string, string>
  run: RunAnswer
  /** Text the server appends, once, to the next answer: the CLI update notice. */
  notice: string
  readonly calls: string[]
  readonly inspectArgs: unknown[]
  readonly toasts: string[]
}

export function standIn(on: On, init: Partial<Pick<World, 'status' | 'inspect' | 'containers' | 'servers' | 'files' | 'run' | 'notice'>> = {}): World {
  const world: World = {
    status: STATUS_CONNECTED,
    inspect: INSPECT_HEALTHY,
    containers: CONTAINERS_RUNNING,
    servers: ['wendy'],
    files: { '/proj/wendy.json': WENDY_JSON },
    run: { kind: 'result', json: RUN_STARTED },
    notice: '',
    ...init,
    calls: [],
    inspectArgs: [],
    toasts: [],
  }
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('session.cwd', () => ({ value: '/proj' }))
  on('fs.read', ($, e) => {
    const text = world.files[e.path]
    return text === undefined ? { deny: `ENOENT: no such file or directory, open '${e.path}'` } : { value: text }
  })
  on('ui.toast', ($, e) => {
    world.toasts.push(e.text)
    return { value: undefined }
  })
  on('mcp.call', ($, e) => {
    world.calls.push(`${e.server}/${e.tool}`)
    if (e.tool === 'app_inspect') world.inspectArgs.push(e.args)
    const answer = !world.servers.includes(e.server)
      ? 'missing'
      : e.tool === 'wendy_status'
        ? world.status
        : e.tool === 'app_inspect'
          ? world.inspect
          : e.tool === 'container_list'
            ? world.containers
            : 'missing'
    if (answer === 'missing') return { deny: `no connected MCP tool "${e.tool}" on a server named "${e.server}"` }
    const result = mcpJson(answer)
    if (world.notice === '') return { value: result }
    const notice = world.notice
    world.notice = ''
    return { value: { ...result, content: [...result.content, { type: 'text', text: notice }] } }
  })
  // The kit draws nothing beneath the plugins; this stands in for the engine's own (empty) band.
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return h(Text, {}, ENGINE_BAND) as RenderElement
  })
  on('tool.call', { tool: /^mcp__.+__run$/ }, () => {
    const run = world.run
    if (run.kind === 'deny') return { deny: run.reason }
    const text = JSON.stringify(run.json) + (run.suffix ?? '')
    return (run.isError === true ? { result: [], text, isError: true } : { result: [], text }) as never
  })
  return world
}

/** What the stand-in engine draws when the mod passes. */
export const ENGINE_BAND = '(engine band)'

export const BAND = {
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: false, maxRows: 4, bodyColumns: 100, scroll: { offset: 0, bodyRows: 4 }, view: {} },
} as const

export const START = { cwd: '/proj', surface: 'terminal', isInteractive: true } as const

export const SURFACES = ['terminal', 'desktop'] as const
