import type { Error as WireError } from './types'

/** A live resource: its wire name, whether it is a list, and (phantom) its data and params. */
export interface Target<TData, TParams> {
  name: string
  list: boolean
  __data?: TData
  __params?: TParams
}

export function target<TData, TParams>(name: string, list: boolean): Target<TData, TParams> {
  return { name, list }
}

/** The server answered with an error body. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public fields?: Record<string, string>,
  ) {
    super(message)
  }
}

/** The request never reached the server: offline, DNS, connection refused. */
export class NetworkError extends Error {}

export const config = {
  baseUrl: '/api/v1',
  /** Defaults to the same host as the page, below baseUrl. */
  wsUrl: '',
  headers: (): Record<string, string> => ({}),
  onUnauthorized: (): void => {},
}

export async function request<T>(
  method: string,
  path: string,
  init: { query?: Record<string, unknown>; body?: unknown } = {},
): Promise<T> {
  const url = new URL(config.baseUrl + path, window.location.origin)
  for (const [key, value] of Object.entries(init.query ?? {})) {
    if (value !== undefined && value !== null) url.searchParams.set(key, String(value))
  }
  const headers: Record<string, string> = { ...config.headers() }
  if (init.body !== undefined) headers['Content-Type'] = 'application/json'
  let response: Response
  try {
    response = await fetch(url, {
      method,
      credentials: 'include',
      headers,
      body: init.body !== undefined ? JSON.stringify(init.body) : undefined,
    })
  } catch (e) {
    throw new NetworkError(String(e))
  }
  if (response.status === 204) return undefined as T
  const text = await response.text()
  const data: unknown = text ? JSON.parse(text) : undefined
  if (!response.ok) {
    if (response.status === 401) config.onUnauthorized()
    // go-basicauth answers {error, message}; livewire {code, message, fields}.
    const body = (data ?? {}) as Partial<WireError> & { error?: string }
    throw new ApiError(response.status, body.code ?? body.error ?? 'internal', body.message ?? response.statusText, body.fields)
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
  private socket?: WebSocket
  private subscriptions = new Map<number, Subscription>()
  private byServerId = new Map<string, number>()
  private pending = new Map<string, (frame: ResponseFrame) => void>()
  private statusListeners = new Set<(status: LiveStatus) => void>()
  private lastId = 0
  private attempt = 0
  private retryTimer?: ReturnType<typeof setTimeout>
  private pingTimer?: ReturnType<typeof setInterval>

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
      this.pingTimer = setInterval(() => this.send({ id: String(++this.lastId), op: 'ping' }), 25_000)
    }
    socket.onmessage = (event) => this.receive(JSON.parse(String(event.data)) as ResponseFrame | PushFrame)
    socket.onclose = (event) => {
      clearInterval(this.pingTimer)
      this.socket = undefined
      this.byServerId.clear()
      this.pending.clear()
      for (const subscription of this.subscriptions.values()) subscription.serverId = undefined
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
      if (!frame.ok) {
        const error = frame.error
        subscription.onError(new ApiError(0, error?.code ?? 'internal', error?.message ?? 'Subscription failed'))
        return
      }
      if (!this.subscriptions.has(id)) {
        this.send({ id: String(++this.lastId), op: 'unsubscribe', subscription: frame.subscription })
        return
      }
      subscription.serverId = frame.subscription
      this.byServerId.set(frame.subscription as string, id)
    })
    this.send({ id: requestId, op: 'subscribe', target: subscription.target.name, params: subscription.params })
  }

  private send(frame: object) {
    if (this.socket?.readyState === WebSocket.OPEN) this.socket.send(JSON.stringify(frame))
  }
}

export const connection = new LiveConnection()
