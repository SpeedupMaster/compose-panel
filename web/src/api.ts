// Thin client for the panel's JSON API.

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export function isAbortError(err: unknown): boolean {
  return (err instanceof DOMException || err instanceof Error) && err.name === 'AbortError'
}

// Sanitizes non-JSON error bodies (such as reverse-proxy 502 HTML pages) so raw markup is never
// rendered directly into UI alerts or toasts.
export function sanitizeErrorText(text: string, status: number, statusText?: string): string {
  const trimmed = text.trimStart()
  if (!trimmed) return statusText || `HTTP ${status}`
  if (trimmed.startsWith('<')) return `HTTP ${status} ${statusText || '网关错误'}`.trim()
  return text
}

export function parseResponseBody(text: string, status: number, statusText?: string): { data: any; errorMessage: string } {
  if (!text) return { data: null, errorMessage: statusText || `HTTP ${status}` }
  try {
    const data = JSON.parse(text)
    const msg = (data && typeof data.error === 'string' && data.error) || statusText || `HTTP ${status}`
    return { data, errorMessage: msg }
  } catch {
    const msg = sanitizeErrorText(text, status, statusText)
    return { data: { error: msg }, errorMessage: msg }
  }
}

type Listener = () => void
const unauthorized = new Set<Listener>()
export function onUnauthorized(fn: Listener) {
  unauthorized.add(fn)
  return () => unauthorized.delete(fn)
}

function notifyUnauthorized(path: string) {
  if (!path.startsWith('/auth/') && !path.startsWith('/setup')) {
    unauthorized.forEach((f) => f())
  }
}

export async function api<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { 'X-Panel': '1' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  let res: Response
  try {
    res = await fetch('/api' + path, {
      method,
      headers,
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch (e) {
    if (isAbortError(e)) throw e
    throw new ApiError(0, '网络错误，无法连接面板')
  }
  const text = await res.text()
  const { data, errorMessage } = parseResponseBody(text, res.status, res.statusText)
  if (!res.ok) {
    if (res.status === 401) notifyUnauthorized(path)
    throw new ApiError(res.status, errorMessage)
  }
  return data as T
}

export const get = <T = any>(p: string) => api<T>('GET', p)
export const post = <T = any>(p: string, b: unknown = {}) => api<T>('POST', p, b)
export const put = <T = any>(p: string, b: unknown = {}) => api<T>('PUT', p, b)
export const del = <T = any>(p: string) => api<T>('DELETE', p)

export interface StreamLinesOptions {
  resetOnReconnect?: boolean
}

// Server-Sent Events stream of text lines; returns a function that closes it.
// When EventSource reconnects after a network drop, onReset fires before replayed lines arrive
// unless opts.resetOnReconnect is explicitly false (used for cursor-resumed streams via Last-Event-ID).
export function streamLines(
  path: string,
  onLine: (l: string) => void,
  onEnd?: (data: string) => void,
  onReset?: () => void,
  opts?: StreamLinesOptions,
): () => void {
  const es = new EventSource('/api' + path, { withCredentials: true })
  const resetOnReconnect = opts?.resetOnReconnect ?? true
  let opens = 0
  es.onopen = () => {
    if (opens++ > 0 && resetOnReconnect) onReset?.()
  }
  es.onmessage = (e) => onLine(e.data)
  es.addEventListener('end', (e) => {
    es.close()
    onEnd?.((e as MessageEvent).data)
  })
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) onEnd?.('')
  }
  return () => es.close()
}

// ---------- formatting ----------

export function bytes(n?: number | null): string {
  if (n == null || isNaN(n)) return '—'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return (i === 0 ? v.toFixed(0) : v < 10 ? v.toFixed(1) : v.toFixed(0)) + ' ' + u[i]
}

export function ago(ts?: number | string | null): string {
  if (!ts) return '—'
  const t = typeof ts === 'string' ? Date.parse(ts) / 1000 : ts
  if (!t || t < 0) return '—'
  const d = Date.now() / 1000 - t
  if (d < 0) return in_(-d)
  if (d < 60) return '刚刚'
  if (d < 3600) return Math.floor(d / 60) + ' 分钟前'
  if (d < 86400) return Math.floor(d / 3600) + ' 小时前'
  if (d < 86400 * 30) return Math.floor(d / 86400) + ' 天前'
  return dateTime(t)
}

function in_(d: number) {
  if (d < 3600) return Math.max(1, Math.floor(d / 60)) + ' 分钟后'
  if (d < 86400) return Math.floor(d / 3600) + ' 小时后'
  return Math.floor(d / 86400) + ' 天后'
}

export function dateTime(ts?: number | null): string {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export function duration(sec: number): string {
  if (sec < 60) return Math.round(sec) + ' 秒'
  if (sec < 3600) return Math.floor(sec / 60) + ' 分 ' + Math.round(sec % 60) + ' 秒'
  if (sec < 86400) return Math.floor(sec / 3600) + ' 小时 ' + Math.floor((sec % 3600) / 60) + ' 分'
  return Math.floor(sec / 86400) + ' 天'
}

export function pct(a: number, b: number) {
  if (!b) return 0
  return Math.min(100, Math.round((a / b) * 100))
}

function xhrUpload<T = any>({
  method,
  path,
  body,
  headers,
  onProgress,
  signal,
  networkError,
}: {
  method: 'PUT' | 'POST'
  path: string
  body: Blob | FormData
  headers?: Record<string, string>
  onProgress?: (pct: number) => void
  signal?: AbortSignal
  networkError: string
}): Promise<T> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('请求已取消', 'AbortError'))
      return
    }
    const xhr = new XMLHttpRequest()
    const onAbort = () => xhr.abort()
    const cleanup = () => signal?.removeEventListener('abort', onAbort)
    signal?.addEventListener('abort', onAbort, { once: true })

    xhr.open(method, '/api' + path)
    xhr.withCredentials = true
    xhr.setRequestHeader('X-Panel', '1')
    if (headers) {
      for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v)
    }
    if (onProgress) {
      xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(Math.round((e.loaded / e.total) * 100))
    }
    xhr.onload = () => {
      cleanup()
      const { data, errorMessage } = parseResponseBody(xhr.responseText, xhr.status, xhr.statusText)
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(data as T)
      } else {
        if (xhr.status === 401) notifyUnauthorized(path)
        reject(new ApiError(xhr.status, errorMessage))
      }
    }
    xhr.onerror = () => {
      cleanup()
      reject(new ApiError(0, networkError))
    }
    xhr.onabort = () => {
      cleanup()
      reject(new DOMException('请求已取消', 'AbortError'))
    }
    xhr.send(body)
  })
}

// Uploads a raw file body (PUT/POST) with progress; resolves with the JSON response.
export function upload<T = any>(
  method: 'PUT' | 'POST',
  path: string,
  file: Blob,
  onProgress?: (pct: number) => void,
  signal?: AbortSignal,
): Promise<T> {
  return xhrUpload<T>({
    method,
    path,
    body: file,
    headers: { 'Content-Type': 'application/octet-stream' },
    onProgress,
    signal,
    networkError: '网络错误，上传失败',
  })
}

// Uploads a multipart FormData payload with progress; resolves with the JSON response.
export function uploadForm<T = any>(
  path: string,
  form: FormData,
  onProgress?: (pct: number) => void,
  extraHeaders?: Record<string, string>,
  signal?: AbortSignal,
): Promise<T> {
  return xhrUpload<T>({
    method: 'POST',
    path,
    body: form,
    headers: extraHeaders,
    onProgress,
    signal,
    networkError: '网络错误，请求失败',
  })
}

export function qs(params: Record<string, string | number | boolean | undefined | null>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') q.set(k, String(v))
  const s = q.toString()
  return s ? '?' + s : ''
}

export function downloadCSV(name: string, columns: string[], rows: unknown[][]) {
  const esc = (v: unknown) => {
    const s = v === null || v === undefined ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v)
    return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s
  }
  const text = '\uFEFF' + [columns, ...rows].map((r) => r.map(esc).join(',')).join('\r\n')
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/csv;charset=utf-8' }))
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(a.href), 1000)
}
