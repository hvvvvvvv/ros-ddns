import { el, button, emptyState, setText, timeText, display, redactObject, toast } from '/assets/dom.js';
import { createField } from '/assets/forms.js';
import { LiveFeed } from '/assets/stream.js';

const LIMIT = 1000;

export class Logs {
  constructor(host) {
    this.host = host;
    this.entries = [];
    this.buffer = [];
    this.seen = new Set();
    this.lastSeq = '';
    this.paused = false;
    this.autoScroll = true;
    this.dropped = 0;
    this.level = '';
    this.query = '';
    this.list = el('div', { class: 'log-list', tabIndex: 0, attrs: { 'aria-label': '日志内容' } });
    this.summary = el('span', { class: 'caption', attrs: { role: 'status' } });
    this.pauseButton = button('暂停显示', () => this.togglePause());
    this.pauseButton.setAttribute('aria-pressed', 'false');
    const level = createField({ label: '日志级别', type: 'select', value: '', options: [
      { value: '', label: '全部级别' }, 'debug', 'info', 'warn', 'error',
    ], onChange: value => { this.level = value; this.render(); } });
    const search = createField({ label: '搜索日志', type: 'search', placeholder: '消息、属性或时间…', onChange: value => { this.query = value.toLowerCase(); this.scheduleRender(); } });
    const scroll = createField({ label: '自动滚动', type: 'checkbox', value: true, onChange: value => { this.autoScroll = value; if (value) this.scroll(); } });
    const copy = button('复制筛选结果', () => this.copy());
    const clear = button('清空本地', () => {
      this.entries = []; this.buffer = []; this.dropped = 0;
      // Keep the sequence cursor and dedup set: clearing must not replay history.
      this.render();
    }, 'subtle');
    host.replaceChildren(el('div', { class: 'panel' },
      el('div', { class: 'toolbar log-toolbar' }, level.node, search.node, this.pauseButton, scroll.node, copy, clear),
      el('div', { class: 'list-meta' }, this.summary, el('span', { class: 'caption', text: '最多保留 1,000 条 · 暂停期间继续接收' })), this.list));
    this.feed = new LiveFeed({
      kind: 'logs', cursor: () => this.lastSeq, metaPath: '/api/v1/meta',
      onRestart: () => this.restartLocal(),
      // Ask for the minimum level so historical debug entries are available;
      // the page-level filter is exact and does not lose retained debug logs.
      historyPath: after => `/api/v1/logs?limit=200&level=debug${after !== '' ? `&after=${encodeURIComponent(after)}` : ''}`,
      streamPath: after => `/api/v1/logs/stream${after !== '' ? `?after=${encodeURIComponent(after)}` : ''}`,
      onEntries: entries => this.ingest(entries),
      onState: (text, tone) => {
        const badge = document.getElementById('logs-connection');
        setText(badge, text); badge.className = `badge ${tone}`;
      },
    });
    this.render();
  }

  ingest(entries) {
    const ordered = entries.filter(entry => entry && entry.seq != null).sort((a, b) => {
      try { return BigInt(a.seq) < BigInt(b.seq) ? -1 : BigInt(a.seq) > BigInt(b.seq) ? 1 : 0; } catch { return 0; }
    });
    for (const entry of ordered) {
      const seq = String(entry.seq);
      if (this.seen.has(seq)) continue;
      this.seen.add(seq);
      if (this.seen.size > LIMIT * 3) this.seen.delete(this.seen.values().next().value);
      try { if (this.lastSeq === '' || BigInt(seq) > BigInt(this.lastSeq)) this.lastSeq = seq; }
      catch { continue; }
      // Redact at ingestion and again on render/copy in case credentials become
      // known later when configuration is opened.
      const clean = redactObject(entry);
      if (this.paused) {
        this.buffer.push(clean);
        if (this.buffer.length > LIMIT) { this.buffer.shift(); this.dropped++; }
      } else {
        this.entries.push(clean);
        if (this.entries.length > LIMIT) this.entries.shift();
      }
    }
    this.scheduleRender();
  }

  filtered() {
    return this.entries.filter(entry => (!this.level || String(entry.level).toLowerCase() === this.level)
      && (!this.query || display(redactObject(entry)).toLowerCase().includes(this.query)));
  }

  togglePause() {
    this.paused = !this.paused;
    if (!this.paused) {
      this.entries = [...this.entries, ...this.buffer].slice(-LIMIT);
      this.buffer = [];
      if (this.dropped) toast(`暂停缓冲区已满，较早的 ${this.dropped} 条日志已丢弃。`, 'warning');
      this.dropped = 0;
    }
    setText(this.pauseButton, this.paused ? '继续显示' : '暂停显示');
    this.pauseButton.setAttribute('aria-pressed', String(this.paused));
    this.render();
  }

  scheduleRender() {
    if (this.renderTimer) return;
    this.renderTimer = setTimeout(() => { this.renderTimer = null; this.render(); }, 120);
  }

  render() {
    const visible = this.filtered();
    setText(this.summary, `${visible.length} / ${this.entries.length} 条${this.paused ? ` · 已暂停，缓冲 ${this.buffer.length} 条${this.dropped ? `，丢弃 ${this.dropped} 条` : ''}` : ''}`);
    const oldScroll = this.list.scrollTop;
    const fragment = document.createDocumentFragment();
    for (const entry of visible) {
      const level = String(entry.level || 'info').toLowerCase();
      const tone = ['error', 'fatal'].includes(level) ? 'error' : ['warn', 'warning'].includes(level) ? 'warning' : 'neutral';
      fragment.append(el('article', { class: `log-row ${tone}` },
        el('time', { class: 'log-time', text: timeText(entry.time), attrs: { datetime: entry.time || '' } }),
        el('span', { class: `log-level ${tone}`, text: level }),
        el('div', { class: 'log-content' }, el('p', { class: 'log-message', text: entry.message }),
          entry.attrs != null && Object.keys(Object(entry.attrs)).length ? el('pre', { class: 'log-attrs', text: redactObject(entry.attrs) }) : null),
        el('span', { class: 'log-seq', text: `#${entry.seq}` })));
    }
    this.list.replaceChildren(visible.length ? fragment : emptyState(this.entries.length ? '没有匹配的日志' : '等待日志', '日志会在连接后自动显示。'));
    if (this.autoScroll && !this.paused) this.scroll();
    else this.list.scrollTop = oldScroll;
  }

  scroll() { this.list.scrollTop = this.list.scrollHeight; }

  async copy() {
    const entries = this.filtered();
    if (!entries.length) { toast('当前没有可复制的日志。'); return; }
    const text = entries.map(entry => `${timeText(entry.time)} [${entry.level}] ${entry.message}${entry.attrs ? ` ${JSON.stringify(redactObject(entry.attrs))}` : ''}`).join('\n');
    try {
      await navigator.clipboard.writeText(display(text));
      toast(`已复制 ${entries.length} 条日志。`, 'success');
    } catch {
      // Clipboard API may be unavailable on an HTTP LAN origin. Let the user
      // copy a selection manually; do not use a hidden editable text area.
      const range = document.createRange(); range.selectNodeContents(this.list);
      const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range);
      this.list.focus();
      toast('浏览器不允许自动复制，已选中日志，请按 Ctrl+C（macOS 为 ⌘C）。', 'warning');
    }
  }

  restartLocal() {
    this.entries = []; this.buffer = []; this.seen.clear(); this.lastSeq = ''; this.dropped = 0;
    this.render();
  }

  reset() {
    this.feed.reset();
    this.entries = []; this.buffer = []; this.seen.clear(); this.lastSeq = ''; this.dropped = 0;
    if (this.paused) this.togglePause(); else this.render();
  }
}
