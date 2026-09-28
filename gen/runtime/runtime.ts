import type { Error as WireError } from './types'

/** A live resource: its wire name, whether it is a list, and (phantom) its data and params. */
export interface Target<TData, TParams> {
  name: string
  list: boolean
  /** Path parameters useLive fills from config.ambient() when the params leave them out. */
  ambient?: string[]
  __data?: TData
  __params?: TParams
}

export function target<TData, TParams>(name: string, list: boolean, ambient?: string[]): Target<TData, TParams> {
  return ambient ? { name, list, ambient } : { name, list }
}

/** The server answered with an error body. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public fields?: Record<string, string>,
    /** The parsed error body as the server sent it. */
    public body?: unknown,
  ) {
    super(message)
  }
}

/** The request never reached the server: offline, DNS, connection refused. */
export class NetworkError extends Error {}

export interface Config {
  baseUrl: string
  /** Defaults to the same host as the page, below baseUrl. */
  wsUrl: string
  headers: () => Record<string, string>
  /** Values for the ambient path parameters (e.g. the current tenant) a call leaves out. */
  ambient: () => Record<string, string | undefined>
  onUnauthorized: () => void
  /** Sees every failed request right before it throws, after onUnauthorized for a 401. */
  onError?: (error: ApiError | NetworkError, request: { method: string; path: string }) => void
}

export const config: Config = {
  baseUrl: '/api/v1',
  wsUrl: '',
  headers: () => ({}),
  ambient: () => ({}),
  onUnauthorized: () => {},
}

/** An ambient path parameter: the call's value, else config.ambient()'s, URI-encoded. */
export function ambientParam(name: string, value: unknown): string {
  const resolved = value === undefined || value === null || value === '' ? config.ambient()[name] : String(value)
  if (!resolved) throw new Error(`The path parameter ${name} is missing`)
  return encodeURIComponent(resolved)
}

function failed<E extends ApiError | NetworkError>(error: E, method: string, path: string): E {
  config.onError?.(error, { method, path })
  return error
}

export async function request<T>(
  method: string,
  path: string,
  init: { query?: Record<string, unknown>; body?: unknown } = {},
): Promise<T> {
  const url = new URL(config.baseUrl + path, window.location.origin)
  for (const [key, value] of Object.entries(init.query ?? {})) {
    if (value === undefined || value === null) continue
    // A list goes as a repeated key: ?status=a&status=b.
    for (const item of Array.isArray(value) ? (value as unknown[]) : [value]) url.searchParams.append(key, String(item))
  }
  const headers: Record<string, string> = { ...config.headers() }
  if (init.body !== undefined) headers['Content-Type'] = 'application/json'
  let response: Response
  try {
    response = await fetch(url, {
      method,
      credentials: 'include',
      headers,
      ...(init.body !== undefined ? { body: JSON.stringify(init.body) } : {}),
    })
  } catch (e) {
    throw failed(new NetworkError(String(e)), method, path)
  }
  if (response.status === 204) return undefined as T
  let text: string
  try {
    text = await response.text()
  } catch (e) {
    // The connection died while the body was on its way.
    throw failed(new NetworkError(String(e)), method, path)
  }
  let data: unknown
  try {
    data = text ? JSON.parse(text) : undefined
  } catch (e) {
    // A proxy's HTML error page is still an ApiError; a success must be JSON.
    if (response.ok) throw e
    data = text
  }
  if (!response.ok) {
    if (response.status === 401) config.onUnauthorized()
    // livewire answers {code, message, fields}; auth middleware in front of it often {error, message}.
    const body = (data ?? {}) as Partial<WireError> & { error?: string }
    const error = new ApiError(response.status, body.code ?? body.error ?? 'internal', body.message ?? response.statusText, body.fields, data)
    throw failed(error, method, path)
  }
  return data as T
}

export type LiveStatus = 'connecting' | 'live' | 'offline'
export type PushKind = 'snapshot' | 'diff' | 'replace' | 'error'

interface Diff<T> {
  upserts: T[]
  removes: string[]
  order?: string[]
}

/** Applies a list push to the current items. */
export function applyPush<T extends { id: unknown }>(current: T[] | undefined, kind: PushKind, payload: unknown): T[] {
  if (kind === 'snapshot' || !current) return payload as T[]
  const diff = payload as Diff<T>
  const byId = new Map(current.map((item) => [String(item.id), item]))
  for (const item of diff.upserts) byId.set(String(item.id), item)
  for (const id of diff.removes) byId.delete(id)
  const order = diff.order ?? current.map((item) => String(item.id))
  return order.filter((id) => byId.has(id)).map((id) => byId.get(id) as T)
}

interface Subscription {
  target: Target<unknown, unknown>
  params: Record<string, string>
  serverId?: string
  onPush: (kind: PushKind, payload: unknown) => void
  onError: (error: ApiError) => void
}

interface ResponseFrame {
  id: string
  ok: boolean
  subscription?: string
  error?: WireError
}

interface PushFrame {
  sub: string
  seq: number
  kind: PushKind
  payload: unknown
}

/** One WebSocket for the whole app: reconnects with backoff and resubscribes everything. */
export class LiveConnection {
  status: LiveStatus = 'offline'
  private socket: WebSocket | undefined
  private subscriptions = new Map<number, Subscription>()
  private byServerId = new Map<string, number>()
  private pending = new Map<string, (frame: ResponseFrame) => void>()
  private statusListeners = new Set<(status: LiveStatus) => void>()
  private lastId = 0
  private attempt = 0
  private retryTimer: ReturnType<typeof setTimeout> | undefined
  private pingTimer: ReturnType<typeof setInterval> | undefined
  private lastMessage = 0

  constructor() {
    if (typeof window === 'undefined') return
    const reconnectNow = () => {
      if (this.retryTimer && this.subscriptions.size > 0) {
        clearTimeout(this.retryTimer)
        this.retryTimer = undefined
        this.connect()
      }
    }
    window.addEventListener('online', reconnectNow)
    document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' && reconnectNow())
  }

  onStatus(listener: (status: LiveStatus) => void): () => void {
    this.statusListeners.add(listener)
    listener(this.status)
    return () => this.statusListeners.delete(listener)
  }

  subscribe(
    target: Target<unknown, unknown>,
    params: Record<string, string>,
    onPush: Subscription['onPush'],
    onError: Subscription['onError'],
  ): () => void {
    const id = ++this.lastId
    const subscription: Subscription = { target, params, onPush, onError }
    this.subscriptions.set(id, subscription)
    if (this.socket?.readyState === WebSocket.OPEN) this.sendSubscribe(id, subscription)
    else if (!this.socket && !this.retryTimer) this.connect()
    return () => {
      this.subscriptions.delete(id)
      if (subscription.serverId) {
        this.byServerId.delete(subscription.serverId)
        this.send({ id: String(++this.lastId), op: 'unsubscribe', subscription: subscription.serverId })
      }
    }
  }

  private setStatus(status: LiveStatus) {
    this.status = status
    for (const listener of this.statusListeners) listener(status)
  }

  private url(): string {
    if (config.wsUrl) return config.wsUrl
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    return `${protocol}//${window.location.host}${config.baseUrl}/ws`
  }

  private connect() {
    this.setStatus('connecting')
    const socket = new WebSocket(this.url())
    this.socket = socket
    socket.onopen = () => {
      this.attempt = 0
      this.setStatus('live')
      for (const [id, subscription] of this.subscriptions) this.sendSubscribe(id, subscription)
      this.lastMessage = Date.now()
      // Every ping is answered, so silence means a half-open socket (a phone that switched
      // networks): close it and let the reconnect take over.
      this.pingTimer = setInterval(() => {
        if (Date.now() - this.lastMessage > 60_000) {
          socket.close()
          return
        }
        this.send({ id: String(++this.lastId), op: 'ping' })
      }, 25_000)
    }
    socket.onmessage = (event) => {
      this.lastMessage = Date.now()
      this.receive(JSON.parse(String(event.data)) as ResponseFrame | PushFrame)
    }
    socket.onclose = (event) => {
      clearInterval(this.pingTimer)
      this.socket = undefined
      this.byServerId.clear()
      this.pending.clear()
      for (const subscription of this.subscriptions.values()) delete subscription.serverId
      this.setStatus('offline')
      if (event.code === 4401) config.onUnauthorized()
      if (this.subscriptions.size === 0) return
      const delay = Math.min(30_000, 1000 * 2 ** this.attempt++) * (0.75 + Math.random() / 2)
      this.retryTimer = setTimeout(() => {
        this.retryTimer = undefined
        this.connect()
      }, delay)
    }
  }

  private receive(frame: ResponseFrame | PushFrame) {
    if ('sub' in frame) {
      const id = this.byServerId.get(frame.sub)
      const subscription = id === undefined ? undefined : this.subscriptions.get(id)
      if (!subscription) return
      if (frame.kind === 'error') {
        const error = frame.payload as WireError
        this.subscriptions.delete(id as number)
        subscription.onError(new ApiError(403, error.code, error.message))
        return
      }
      subscription.onPush(frame.kind, frame.payload)
      return
    }
    this.pending.get(frame.id)?.(frame)
    this.pending.delete(frame.id)
  }

  private sendSubscribe(id: number, subscription: Subscription) {
    const requestId = String(++this.lastId)
    this.pending.set(requestId, (frame) => {
      const serverId = frame.subscription
      if (!frame.ok || serverId === undefined) {
        const error = frame.error
        subscription.onError(new ApiError(0, error?.code ?? 'internal', error?.message ?? 'Subscription failed'))
        return
      }
      if (!this.subscriptions.has(id)) {
        this.send({ id: String(++this.lastId), op: 'unsubscribe', subscription: serverId })
        return
      }
      subscription.serverId = serverId
      this.byServerId.set(serverId, id)
    })
    this.send({ id: requestId, op: 'subscribe', target: subscription.target.name, params: subscription.params })
  }

  private send(frame: object) {
    if (this.socket?.readyState === WebSocket.OPEN) this.socket.send(JSON.stringify(frame))
  }
}

export const connection = new LiveConnection()
