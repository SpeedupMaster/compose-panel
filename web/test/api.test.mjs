import test from 'node:test'
import assert from 'node:assert/strict'
import { ApiError, api, onUnauthorized, parseResponseBody, sanitizeErrorText, streamLines } from '../src/api.ts'

test('sanitizeErrorText strips HTML proxy pages and preserves plain text', () => {
  assert.equal(sanitizeErrorText('<html><body>502 Bad Gateway</body></html>', 502, 'Bad Gateway'), 'HTTP 502 Bad Gateway')
  assert.equal(sanitizeErrorText('  \n<!DOCTYPE html><html>502</html>', 502, ''), 'HTTP 502 网关错误')
  assert.equal(sanitizeErrorText('rate limit exceeded', 429, 'Too Many Requests'), 'rate limit exceeded')
  assert.equal(sanitizeErrorText('   ', 500, 'Internal Server Error'), 'Internal Server Error')
  assert.equal(sanitizeErrorText('', 503, ''), 'HTTP 503')
})

test('parseResponseBody handles JSON, HTML gateway errors, and empty bodies', () => {
  assert.deepEqual(parseResponseBody('{"error":"token expired"}', 401, 'Unauthorized'), {
    data: { error: 'token expired' },
    errorMessage: 'token expired',
  })
  assert.deepEqual(parseResponseBody('{"ok":true}', 200, 'OK'), {
    data: { ok: true },
    errorMessage: 'OK',
  })
  assert.deepEqual(parseResponseBody('<html>504 Gateway Timeout</html>', 504, ''), {
    data: { error: 'HTTP 504 网关错误' },
    errorMessage: 'HTTP 504 网关错误',
  })
  assert.deepEqual(parseResponseBody('', 204, 'No Content'), {
    data: null,
    errorMessage: 'No Content',
  })
})

test('api wraps fetch network failures as ApiError(0) and notifies on 401', async () => {
  const origFetch = globalThis.fetch
  let unauthCount = 0
  const off = onUnauthorized(() => { unauthCount++ })
  try {
    globalThis.fetch = async () => {
      throw new TypeError('Failed to fetch')
    }
    await assert.rejects(
      () => api('GET', '/projects'),
      (err) => err instanceof ApiError && err.status === 0 && err.message === '网络错误，无法连接面板',
    )

    globalThis.fetch = async () => new Response('<html>502</html>', { status: 502, statusText: 'Bad Gateway' })
    await assert.rejects(
      () => api('GET', '/projects'),
      (err) => err instanceof ApiError && err.status === 502 && err.message === 'HTTP 502 Bad Gateway',
    )

    globalThis.fetch = async () => new Response('{"error":"unauthorized"}', { status: 401 })
    await assert.rejects(() => api('GET', '/auth/me'), (err) => err instanceof ApiError && err.status === 401)
    assert.equal(unauthCount, 0)

    await assert.rejects(() => api('GET', '/projects'), (err) => err instanceof ApiError && err.status === 401)
    assert.equal(unauthCount, 1)
  } finally {
    off()
    globalThis.fetch = origFetch
  }
})

test('streamLines invokes onReset only on subsequent reconnects and closes on end', () => {
  const origES = globalThis.EventSource
  const instances = []
  class FakeEventSource {
    static OPEN = 1
    static CLOSED = 2
    readyState = 1
    closed = false
    onopen = null
    onmessage = null
    onerror = null
    listeners = new Map()
    constructor(url, opts) {
      this.url = url
      this.opts = opts
      instances.push(this)
    }
    addEventListener(type, fn) {
      this.listeners.set(type, fn)
    }
    close() {
      this.closed = true
      this.readyState = FakeEventSource.CLOSED
    }
    emit(type, data) {
      this.listeners.get(type)?.({ data })
    }
  }
  globalThis.EventSource = FakeEventSource
  try {
    const lines = []
    let resets = 0
    let ended = null
    const close = streamLines(
      '/jobs/j1/log',
      (l) => lines.push(l),
      (d) => { ended = d },
      () => {
        resets++
        lines.length = 0
      },
    )
    assert.equal(instances.length, 1)
    const es = instances[0]
    assert.equal(es.url, '/api/jobs/j1/log')

    // Initial open should NOT trigger onReset.
    es.onopen?.()
    es.onmessage?.({ data: 'line-1' })
    es.onmessage?.({ data: 'line-2' })
    assert.equal(resets, 0)
    assert.deepEqual(lines, ['line-1', 'line-2'])

    // Reconnect open MUST trigger onReset so replayed lines do not duplicate.
    es.onopen?.()
    assert.equal(resets, 1)
    assert.deepEqual(lines, [])
    es.onmessage?.({ data: 'line-1' })
    es.onmessage?.({ data: 'line-2' })
    es.onmessage?.({ data: 'line-3' })
    assert.deepEqual(lines, ['line-1', 'line-2', 'line-3'])

    es.emit('end', 'success')
    assert.equal(ended, 'success')
    assert.equal(es.closed, true)
    close()
  } finally {
    globalThis.EventSource = origES
  }
})

test('streamLines skips onReset on reconnect when resetOnReconnect is false', () => {
  const origES = globalThis.EventSource
  const instances = []
  class FakeEventSource {
    static OPEN = 1
    static CLOSED = 2
    readyState = 1
    closed = false
    onopen = null
    onmessage = null
    onerror = null
    listeners = new Map()
    constructor(url, opts) {
      this.url = url
      this.opts = opts
      instances.push(this)
    }
    addEventListener(type, fn) {
      this.listeners.set(type, fn)
    }
    close() {
      this.closed = true
      this.readyState = FakeEventSource.CLOSED
    }
  }
  globalThis.EventSource = FakeEventSource
  try {
    const lines = []
    let resets = 0
    const close = streamLines(
      '/containers/c1/logs?tail=500&follow=1',
      (l) => lines.push(l),
      undefined,
      () => {
        resets++
        lines.length = 0
      },
      { resetOnReconnect: false },
    )
    const es = instances[0]
    es.onopen?.()
    es.onmessage?.({ data: '2026-11-27T01:00:01Z first' })
    // Reconnect should NOT reset buffer when resetOnReconnect is false.
    es.onopen?.()
    es.onmessage?.({ data: '2026-11-27T01:00:02Z second' })
    assert.equal(resets, 0)
    assert.deepEqual(lines, ['2026-11-27T01:00:01Z first', '2026-11-27T01:00:02Z second'])
    close()
  } finally {
    globalThis.EventSource = origES
  }
})
