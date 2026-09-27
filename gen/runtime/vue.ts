import { onScopeDispose, ref, shallowRef, toValue, watch, type MaybeRefOrGetter, type Ref } from 'vue'
import { applyPush, config, connection, type ApiError, type LiveStatus, type Target } from './runtime'

function wireParams(params: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries((params ?? {}) as Record<string, unknown>)) {
    if (value !== undefined && value !== null) out[key] = String(value)
  }
  return out
}

/** The target's ambient parameters from config.ambient(), for explicit params to override. */
function ambientParams(names: string[] = []): Record<string, string> {
  const values = config.ambient()
  return wireParams(Object.fromEntries(names.map((name) => [name, values[name]])))
}

/**
 * Subscribes to a live target for as long as the calling scope lives. `stale` is true
 * until the first push and whenever the connection is down; with `cache` the last data is
 * kept in localStorage and shown (stale) before the socket answers, e.g. offline. The
 * target's ambient parameters come from config.ambient() unless params set them; a reactive
 * config.ambient() resubscribes when it changes.
 */
export function useLive<TData, TParams>(
  target: Target<TData, TParams>,
  params?: MaybeRefOrGetter<TParams | undefined>,
  options: { cache?: boolean } = {},
) {
  const data = shallowRef<TData>()
  const error = shallowRef<ApiError>()
  const stale = ref(true)
  let stop: (() => void) | undefined

  watch(
    () => JSON.stringify({ ...ambientParams(target.ambient), ...wireParams(toValue(params)) }),
    (key) => {
      stop?.()
      const cacheKey = `livewire:${target.name}:${key}`
      if (options.cache) {
        const cached = localStorage.getItem(cacheKey)
        data.value = cached ? (JSON.parse(cached) as TData) : undefined
      }
      stale.value = true
      stop = connection.subscribe(
        target as Target<unknown, unknown>,
        JSON.parse(key) as Record<string, string>,
        (kind, payload) => {
          data.value = (target.list ? applyPush(data.value as { id: unknown }[] | undefined, kind, payload) : payload) as TData
          stale.value = false
          error.value = undefined
          if (options.cache) localStorage.setItem(cacheKey, JSON.stringify(data.value))
        },
        (e) => {
          error.value = e
        },
      )
    },
    { immediate: true },
  )
  const offStatus = connection.onStatus((status) => {
    if (status !== 'live') stale.value = true
  })
  onScopeDispose(() => {
    stop?.()
    offStatus()
  })
  return { data, error, stale }
}

/** The connection state, for an online/offline indicator. */
export function useLiveStatus(): Ref<LiveStatus> {
  const status = ref<LiveStatus>(connection.status)
  const off = connection.onStatus((s) => (status.value = s))
  onScopeDispose(off)
  return status
}
