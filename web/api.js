import { rememberSecret } from '/assets/dom.js';

const TOKEN_KEY = 'ros-ddns.token';

export class ApiError extends Error {
  constructor(status, data = null, message = '') {
    super(message || ({ 401: '需要有效的访问令牌。', 404: '请求的资源不存在。', 409: '配置已被外部修改，请重新加载。', 422: '配置校验未通过。' }[status] || `请求失败（HTTP ${status}）。`));
    this.name = 'ApiError';
    this.status = status;
    this.data = data;
  }
}

class ApiClient extends EventTarget {
  constructor() {
    super();
    this.locked = false;
    this.session = new AbortController();
    rememberSecret(this.token);
  }

  get token() {
    try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch { return ''; }
  }

  setToken(value) {
    this.session.abort();
    this.session = new AbortController();
    if (value) {
      // Do not fall back to persistent storage or an in-memory-only login.
      try { sessionStorage.setItem(TOKEN_KEY, value); } catch { throw new Error('浏览器无法使用 sessionStorage，请允许会话存储后重试。'); }
      rememberSecret(value);
    } else {
      try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* Anonymous mode is still available. */ }
    }
    this.locked = false;
  }

  lock() {
    if (this.locked) return;
    this.locked = true;
    try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* Storage may be disabled. */ }
    this.session.abort();
    this.dispatchEvent(new Event('unauthorized'));
  }

  async request(path, options = {}) {
    if (!path.startsWith('/api/v1/') || path.includes('://')) throw new Error('仅支持当前服务的 API 请求。');
    if (this.locked) throw new ApiError(401);
    const { stream = false, body, signal: externalSignal, ...init } = options;
    const session = this.session;
    const signals = [session.signal];
    if (externalSignal) signals.push(externalSignal);
    if (!stream) signals.push(AbortSignal.timeout(15000));
    const headers = new Headers(init.headers);
    headers.set('Accept', stream ? 'text/event-stream' : 'application/json');
    if (this.token) headers.set('Authorization', `Bearer ${this.token}`);
    if (body !== undefined) headers.set('Content-Type', 'application/json');
    let response;
    try {
      response = await fetch(path, { ...init, headers, body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.any(signals), cache: 'no-store', credentials: 'same-origin', redirect: 'error' });
    } catch (error) {
      if (error.name === 'TimeoutError') throw new Error('请求超时，请检查服务连接。');
      if (error.name === 'AbortError') throw error;
      throw new Error('无法连接到服务，请检查网络与服务状态。');
    }
    if (session !== this.session) throw new DOMException('会话已切换', 'AbortError');
    if (response.status === 401) {
      this.lock();
      throw new ApiError(401);
    }
    if (!response.ok) {
      let data = null;
      try { data = await response.json(); } catch { /* Never print a raw error response. */ }
      throw new ApiError(response.status, data);
    }
    return response;
  }

  async json(path, options) {
    const session = this.session;
    const response = await this.request(path, options);
    if (response.status === 204) return null;
    try {
      const result = await response.json();
      if (session !== this.session || session.signal.aborted) throw new DOMException('会话已切换', 'AbortError');
      return result;
    }
    catch (error) {
      if (error.name === 'AbortError') throw error;
      throw new Error('服务返回了无法读取的 JSON 数据。');
    }
  }
}

export const api = new ApiClient();
export const isQuietError = error => error?.status === 401 || error?.name === 'AbortError';
