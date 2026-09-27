// Runs the generated client against a stubbed fetch and live connection: ambient path
// parameters, mutation query parameters, onError and ApiError.body. `bun run` it after
// generating into src/api.
import { effectScope } from 'vue'
import { archiveTodos, getPage, live, purgeTodos } from './api/api'
import { ApiError, config, connection } from './api/runtime'
import { useLive } from './api/vue'

function check(ok: boolean, what: string): void {
  if (!ok) throw new Error('failed: ' + what)
}

const calls: string[] = []
let answer = (): Response => new Response(null, { status: 204 })
Object.assign(globalThis, {
  window: { location: { origin: 'http://test' } },
  fetch: (url: URL, init: RequestInit): Promise<Response> => {
    calls.push(`${init.method} ${url.pathname}${url.search}`)
    return Promise.resolve(answer())
  },
})

config.ambient = () => ({ tenant: 'acme' })
await getPage()
await getPage({ tenant: 'o/ther' })
await archiveTodos({ title: 'x' }, { before: 'b' })
check(
  calls.join(' | ') === 'GET /api/v1/tenants/acme/page | GET /api/v1/tenants/o%2Fther/page | POST /api/v1/tenants/acme/archive?before=b',
  calls.join(' | '),
)

config.ambient = () => ({})
const missing = await getPage().then(
  () => undefined,
  (e: unknown) => e,
)
check(missing instanceof Error && missing.message.includes('tenant'), 'a missing ambient parameter rejects, naming it')

const seen: string[] = []
config.onError = (error, request) => seen.push(`${error.name}:${error instanceof ApiError} ${request.method} ${request.path}`)
answer = () => new Response(JSON.stringify({ code: 'conflict', message: 'Taken', extra: 1 }), { status: 409 })
const conflict = await purgeTodos({ tenant: 'acme' }).then(
  () => undefined,
  (e: unknown) => e,
)
check(conflict instanceof ApiError && conflict.code === 'conflict' && (conflict.body as { extra?: number }).extra === 1, 'ApiError keeps the body')
answer = () => new Response('<html>Bad gateway</html>', { status: 502 })
const gateway = await purgeTodos({ tenant: 'acme' }).then(
  () => undefined,
  (e: unknown) => e,
)
check(gateway instanceof ApiError && gateway.status === 502 && gateway.body === '<html>Bad gateway</html>', 'a non-JSON error body is an ApiError')
check(seen.join(' | ') === 'Error:true DELETE /tenants/acme/todos | Error:true DELETE /tenants/acme/todos', seen.join(' | '))

const subscribed: string[] = []
connection.subscribe = (_target, params) => {
  subscribed.push(JSON.stringify(params))
  return () => {}
}
config.ambient = () => ({ tenant: 'acme' })
const scope = effectScope()
scope.run(() => {
  useLive(live.page)
  useLive(live.page, { tenant: 'other' })
})
scope.stop()
check(subscribed.join(' | ') === '{"tenant":"acme"} | {"tenant":"other"}', subscribed.join(' | '))
console.log('behavior ok')
