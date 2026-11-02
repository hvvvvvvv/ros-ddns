import { el, emptyState, setText, timeText, toneOf, toast, redactObject } from '/assets/dom.js';
import { LiveFeed } from '/assets/stream.js';

export class Notifications {
  constructor(host) {
    this.entries = [];
    this.seen = new Set();
    this.lastId = '';
    this.summary = el('span', { class: 'caption', attrs: { role: 'status' } });
    this.list = el('div', { class: 'notification-list' });
    host.replaceChildren(el('div', { class: 'panel' }, el('div', { class: 'toolbar' }, this.summary), this.list));
    this.feed = new LiveFeed({
      kind: 'notifications', metaPath: '/api/v1/meta',
      onRestart: () => this.restartLocal(),
      cursor: () => this.lastId,
      historyPath: () => '/api/v1/notifications?limit=200',
      streamPath: after => `/api/v1/notifications/stream${after !== '' ? `?after=${encodeURIComponent(after)}` : ''}`,
      onEntries: (entries, options) => this.ingest(entries, options),
      onState: (text, tone) => {
        const badge = document.getElementById('notifications-connection');
        setText(badge, text); badge.className = `badge ${tone}`;
      },
    });
    this.render();
  }

  ingest(entries, { initial = false } = {}) {
    const fresh = [];
    for (const entry of entries) {
      if (!entry || entry.id == null) continue;
      try { if (this.lastId === '' || BigInt(entry.id) > BigInt(this.lastId)) this.lastId = String(entry.id); } catch { /* Keep the previous cursor. */ }
      if (this.seen.has(String(entry.id))) continue;
      this.seen.add(String(entry.id));
      if (this.seen.size > 3000) this.seen.delete(this.seen.values().next().value);
      const clean = redactObject(entry);
      fresh.push(clean);
      this.entries.push(clean);
    }
    this.entries.sort((a, b) => (Date.parse(b.time) || 0) - (Date.parse(a.time) || 0));
    this.entries = this.entries.slice(0, 500);
    if (!initial) {
      for (const entry of fresh.slice(-3)) toast(entry.message || entry.kind, this.tone(entry), entry.item || '系统事件');
      if (fresh.length > 3) toast(`另有 ${fresh.length - 3} 条新事件，请查看通知中心。`);
    }
    this.render();
  }

  tone(entry) {
    // A recovery may retain the original incident's severity.
    if (/recover|resolv|success|restor/i.test(String(entry.kind))) return 'success';
    return toneOf(entry.severity || entry.kind || entry.phase);
  }

  render() {
    setText(this.summary, `${this.entries.length} 条事件`);
    const fragment = document.createDocumentFragment();
    for (const entry of this.entries) {
      const tone = this.tone(entry);
      fragment.append(el('article', { class: `notification-row ${tone}` },
        el('span', { class: `event-marker ${tone}`, text: tone === 'success' ? '✓' : tone === 'error' ? '!' : '·', attrs: { 'aria-hidden': 'true' } }),
        el('div', { class: 'notification-body' }, el('div', { class: 'event-meta' },
          el('strong', { text: entry.item || '系统' }),
          el('span', { class: `badge ${tone}`, text: entry.kind || 'event' }),
          entry.severity ? el('span', { class: 'caption', text: entry.severity }) : null,
          entry.phase ? el('span', { class: 'caption', text: entry.phase }) : null),
          el('p', { class: 'event-message', text: entry.message }),
          el('time', { class: 'caption', text: timeText(entry.time), attrs: { datetime: entry.time || '' } }))));
    }
    this.list.replaceChildren(this.entries.length ? fragment : emptyState('暂无事件', '同步异常和恢复事件将在这里汇总。'));
  }

  restartLocal() {
    // The daemon restarted and its in-memory notification IDs were reset.
    // Discard previous-process state and start a new cursor/history generation.
    this.entries = []; this.seen.clear(); this.lastId = '';
    this.render();
  }

  reset() { this.feed.reset(); this.entries = []; this.seen.clear(); this.lastId = ''; this.render(); }
}
