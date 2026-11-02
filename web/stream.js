import { api, isQuietError } from '/assets/api.js';

// Streaming fetch is used instead of EventSource so every connection uses the
// same Authorization header as ordinary API calls. Handles LF, CRLF, lone CR,
// split UTF-8 characters, multiline data, comments, and arbitrary chunk edges.
export async function readSSE(response, { signal, onEvent, onActivity = () => {} }) {
  if (!response.body || !response.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) {
    throw new Error('服务未返回事件流。');
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let event = 'message';
  let id = '';
  let data = [];
  let size = 0;
  const processLine = line => {
    if (line === '') {
      if (data.length) onEvent({ event, id, data: data.join('\n') });
      event = 'message'; data = []; size = 0;
      return;
    }
    if (line.startsWith(':')) return;
    const colon = line.indexOf(':');
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'data') { data.push(value); size += value.length; }
    else if (field === 'event') event = value;
    else if (field === 'id' && !value.includes('\0')) id = value;
    if (size > 1048576) throw new Error('单个事件超出读取上限。');
  };
  const drain = final => {
    let start = 0;
    for (let i = 0; i < buffer.length; i++) {
      const character = buffer[i];
      if (character !== '\r' && character !== '\n') continue;
      if (character === '\r' && i === buffer.length - 1 && !final) break;
      processLine(buffer.slice(start, i));
      if (character === '\r' && buffer[i + 1] === '\n') i++;
      start = i + 1;
    }
    buffer = buffer.slice(start);
    if (buffer.length > 1048576) throw new Error('事件流行超出读取上限。');
  };
  const cancel = () => { reader.cancel().catch(() => {}); };
  signal?.addEventListener('abort', cancel, { once: true });
  try {
    while (!signal?.aborted) {
      const { value, done } = await reader.read();
      if (done) {
        buffer += decoder.decode();
        drain(true);
        // An unterminated event is intentionally not dispatched (SSE rules).
        break;
      }
      onActivity();
      buffer += decoder.decode(value, { stream: true });
      drain(false);
    }
  } finally {
    signal?.removeEventListener('abort', cancel);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

// The contract permits collection payloads without prescribing a wrapper.
// Accept a bare array or a named collection; reject other payloads visibly.
export function collection(data, kind) {
  if (Array.isArray(data)) return data;
  for (const key of [kind, 'entries', 'events', 'items']) if (Array.isArray(data?.[key])) return data[key];
  throw new Error('历史数据格式不正确。');
}

export class LiveFeed {
  constructor({ historyPath, streamPath, kind, onEntries, onState, onRestart = null, metaPath = '/api/v1/meta', cursor = () => '' }) {
    Object.assign(this, { historyPath, streamPath, kind, onEntries, onState, onRestart, metaPath, cursor });
    this.running = false;
    this.initialized = false;
    this.historyReady = false;
    this.failures = 0;
    this.startedAt = '';
  }

  async start() {
    if (this.running) return;
    this.running = true;
    this.life = new AbortController();
    const life = this.life;
    this.onState('正在连接', 'neutral');
    this.epochTimer = setInterval(() => {
      if (this.running && !api.locked) this.checkEpoch();
    }, 10000);
    if (await this.checkEpoch()) return;
    await this.refresh();
    if (this.running && life === this.life && !api.locked) this.connect(life);
  }

  // A restarted daemon starts its in-memory log/notification sequence at zero.
  // The cursor from the previous process would otherwise hide every new event.
  // `/meta.startedAt` is the process generation marker. On a change, abort the
  // whole feed generation (history request, stream, fallback and retry timers)
  // and start a clean generation; merely clearing a cursor is racy because an
  // old in-flight response can restore a stale cursor afterwards.
  async checkEpoch() {
    if (!this.running || !this.metaPath) return false;
    const life = this.life;
    try {
      const meta = await api.json(this.metaPath, { signal: life.signal });
      if (!this.running || life !== this.life) return false;
      const startedAt = typeof meta?.startedAt === 'string' ? meta.startedAt : '';
      if (!startedAt) return false;
      if (this.startedAt && this.startedAt !== startedAt) {
        this.startedAt = startedAt;
        this.restartGeneration();
        return true;
      }
      this.startedAt = startedAt;
    } catch {
      // Metadata is best-effort; the normal stream/history retry path remains.
    }
    return false;
  }

  restartGeneration() {
    const wasRunning = this.running;
    this.stop();
    this.initialized = false;
    this.historyReady = false;
    this.failures = 0;
    this.onRestart?.();
    if (wasRunning) {
      // Start after the current call stack so aborted handlers finish first.
      queueMicrotask(() => {
        if (!this.running) this.start();
      });
    }
  }

  stop() {
    this.running = false;
    this.fallback = false;
    this.life?.abort();
    this.connection?.abort();
    clearInterval(this.epochTimer);
    clearTimeout(this.retryTimer);
    clearTimeout(this.pollTimer);
    this.onState('连接已暂停', 'neutral');
  }

  reset() { this.stop(); this.initialized = false; this.historyReady = false; this.failures = 0; }

  async refresh() {
    if (!this.running) return;
    const life = this.life;
    if (this.pollLife === life) return this.pollPromise;
    this.pollLife = life;
    this.pollPromise = this.fetchHistory(life);
    try { await this.pollPromise; }
    finally { if (this.pollLife === life) { this.pollLife = null; this.pollPromise = null; } }
  }

  async fetchHistory(life) {
    try {
      const payload = await api.json(this.historyPath(this.cursor()), { signal: life.signal });
      if (!this.running || life !== this.life) return;
      this.onEntries(collection(payload, this.kind), { initial: !this.historyReady });
      this.historyReady = true;
      this.initialized = true;
      if (this.fallback) this.onState('轮询中 · 3 秒 / 次', 'warning');
    } catch (error) {
      if (!isQuietError(error) && this.running) this.onState('历史加载失败 · 自动重试', 'error');
    }
  }

  startFallback(life) {
    if (this.fallback) return;
    this.fallback = true;
    const poll = async () => {
      if (!this.running || life !== this.life || !this.fallback) return;
      if (await this.checkEpoch()) return;
      await this.refresh();
      if (this.running && life === this.life && this.fallback) this.pollTimer = setTimeout(poll, 3000);
    };
    poll();
  }

  async connect(life) {
    if (!this.running || life !== this.life || api.locked) return;
    this.connection = new AbortController();
    const connection = this.connection;
    const signal = AbortSignal.any([life.signal, connection.signal]);
    const started = Date.now();
    let watchdog;
    const armWatchdog = () => {
      clearTimeout(watchdog);
      watchdog = setTimeout(() => connection.abort(), 45000);
    };
    armWatchdog();
    try {
      const response = await api.request(this.streamPath(this.cursor()), { stream: true, signal });
      if (!response.body || !response.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) {
        await response.body?.cancel();
        throw new Error('不支持事件流。');
      }
      // Bind the connection to the process generation at the server. A restart
      // between the /meta check and this request must not leave the old cursor
      // active for the new process's stream.
      const streamEpoch = response.headers.get('x-ros-ddns-started-at') || '';
      if (streamEpoch) {
        if (this.startedAt && streamEpoch !== this.startedAt) {
          this.startedAt = streamEpoch;
          this.restartGeneration();
          return;
        }
        this.startedAt = streamEpoch;
      }
      this.onState('实时连接', 'success');
      this.fallback = false;
      clearTimeout(this.pollTimer);
      await readSSE(response, {
        signal,
        onActivity: armWatchdog,
        onEvent: event => {
          if (life !== this.life || !this.running) return;
          if (event.event === 'heartbeat' || event.event === 'ping') return;
          let entry;
          try { entry = JSON.parse(event.data); }
          catch { throw new Error('事件流包含无效 JSON。'); }
          if (!entry || typeof entry !== 'object' || Array.isArray(entry)) throw new Error('事件数据格式不正确。');
          // If history never loaded, retained SSE entries must not look like new
          // notifications (which would cause a toast replay storm). This flag is
          // set only by a successful history request.
          this.onEntries([entry], { initial: !this.historyReady });
        },
      });
    } catch (error) {
      if (error.status === 401) return;
      // Connection errors, unsupported SSE and idle connections all fall back.
    } finally {
      clearTimeout(watchdog);
    }
    if (!this.running || life !== this.life || api.locked) return;
    if (Date.now() - started >= 10000) this.failures = 0;
    const delay = Math.min(30000, 1000 * 2 ** Math.min(this.failures++, 5));
    this.onState(`连接中断 · ${Math.ceil(delay / 1000)} 秒后重连`, 'warning');
    this.startFallback(life);
    this.retryTimer = setTimeout(async () => {
      // Notifications have no documented cursor parameter. Backfill history on
      // reconnect and deduplicate by id; log requests resume at the last seq.
      // A daemon restart changes startedAt; abort the old generation first.
      if (await this.checkEpoch()) return;
      await this.refresh();
      this.connect(life);
    }, delay);
  }
}
