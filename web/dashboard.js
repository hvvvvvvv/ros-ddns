import { api, isQuietError } from '/assets/api.js';
import { el, button, emptyState, banner, setText, timeText, toneOf, toast, redactObject, copyButton } from '/assets/dom.js';
import { store } from '/assets/store.js';

function valueText(value) { return value == null || value === '' ? '—' : value; }

function metric(label, value, caption) {
  const strong = el('strong', { text: value });
  return { strong, node: el('div', { class: 'metric' }, el('span', { class: 'metric-label', text: label }), strong, el('span', { class: 'caption', text: caption })) };
}

function detail(label, value, mono = false) {
  return el('div', { class: 'detail' }, el('dt', { text: label }), el('dd', { class: mono ? 'mono' : '', text: valueText(value) }));
}

function withCopy(value, label, className = '') {
  const text = el('span', { class: className, text: valueText(value) });
  const copy = copyButton(value, label);
  return copy ? el('div', { class: 'copy-value' }, text, copy) : text;
}

export function statusItems(snapshot) {
  if (Array.isArray(snapshot?.items)) return snapshot.items;
  if (snapshot?.items && typeof snapshot.items === 'object') return Object.entries(snapshot.items).map(([name, item]) => ({ name, ...item }));
  return [];
}

export class Dashboard {
  constructor(host) {
    this.host = host;
    this.cards = new Map();
    this.meta = {};
    this.snapshot = null;
    this.running = false;
    this.alert = el('div', { class: 'banner error', hidden: true, attrs: { role: 'alert' } });
    this.total = metric('同步项目', '—', 'ITEMS');
    this.health = metric('服务状态', '连接中', 'SERVICE');
    this.uptime = metric('运行时间', '—', 'UPTIME');
    this.path = el('code', { class: 'config-path', text: '—' });
    this.grid = el('div', { class: 'item-grid' }, emptyState('正在连接服务', '状态快照每 2 秒更新；页面隐藏时暂停轮询。'));
    host.replaceChildren(this.alert, el('div', { class: 'metrics' }, this.total.node, this.health.node, this.uptime.node),
      el('div', { class: 'section-heading dashboard-section-heading' }, el('div', {}, el('h2', { text: '同步项目' }),
        el('p', { class: 'help' }, '配置文件 ', this.path)), el('span', { class: 'caption', text: '每 2 秒刷新 · 隐藏时暂停' })), this.grid);
  }

  start() {
    if (this.running) return;
    this.running = true;
    this.life = new AbortController();
    this.cycle();
  }

  stop() { this.running = false; this.life?.abort(); clearTimeout(this.timer); }

  async cycle() {
    clearTimeout(this.timer);
    const life = this.life;
    if (!this.running || document.hidden || api.locked) return;
    await this.refresh();
    if (this.running && life === this.life && !document.hidden) this.timer = setTimeout(() => this.cycle(), 2000);
  }

  visibilityChanged() {
    if (document.hidden) clearTimeout(this.timer);
    else if (this.running) this.cycle();
  }

  async refresh() {
    if (api.locked) return;
    if (this.refreshing) return this.refreshing;
    const life = this.life;
    this.refreshing = (async () => {
      const options = life ? { signal: life.signal } : {};
      try {
        const snapshot = await api.json('/api/v1/status', options);
        if (life && life !== this.life) return;
        this.render(snapshot);
        banner(this.alert, '');
        store.set({ connected: true });
        setText(document.getElementById('dashboard-updated'), `更新于 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`);
      } catch (error) {
        if (!isQuietError(error)) {
          banner(this.alert, `${error.message} 正在自动重试；下方保留上次状态。`);
          store.set({ connected: false });
          setText(document.getElementById('ready-badge'), '连接中断');
          document.getElementById('ready-badge').className = 'badge error';
        }
      }
      if (!api.locked && !life?.signal.aborted && (!this.metaAt || Date.now() - this.metaAt > 30000)) {
        this.metaAt = Date.now();
        // Optional metadata must not hold up the two-second status loop.
        api.json('/api/v1/meta', options).then(meta => {
          if (life && life !== this.life) return;
          this.meta = meta || {};
          this.metaReceivedAt = Date.now();
          if (this.snapshot) this.renderGlobals(this.snapshot);
        }).catch(error => {
          // Meta is optional; status may carry the same global fields.
          if (error.status === 404) this.metaAt = Infinity;
        });
      }
    })();
    try { await this.refreshing; } finally { this.refreshing = null; }
  }

  renderGlobals(snapshot) {
    const ready = snapshot.ready ?? this.meta.ready;
    const label = ready === true ? 'Ready' : ready === false ? '未就绪' : '状态未知';
    const badge = document.getElementById('ready-badge');
    setText(badge, label); badge.className = `badge ${ready === true ? 'success' : 'warning'}`;
    setText(this.total.strong, statusItems(snapshot).length);
    setText(this.health.strong, ready === true ? '已就绪' : ready === false ? '启动中' : '—');
    setText(this.path, snapshot.configPath ?? this.meta.configPath ?? '—');
    const rawUptime = snapshot.uptimeSeconds ?? snapshot.uptime ?? this.meta.uptimeSeconds ?? this.meta.uptime;
    let seconds;
    if (typeof rawUptime === 'number') {
      seconds = rawUptime;
      if (snapshot.uptimeSeconds == null && snapshot.uptime == null) seconds += (Date.now() - (this.metaReceivedAt || Date.now())) / 1000;
    } else if (rawUptime == null && (snapshot.startedAt || this.meta.startedAt)) seconds = (Date.now() - Date.parse(snapshot.startedAt || this.meta.startedAt)) / 1000;
    if (Number.isFinite(seconds)) {
      const total = Math.max(0, Math.floor(seconds));
      setText(this.uptime.strong, total >= 86400 ? `${Math.floor(total / 86400)}天 ${Math.floor(total % 86400 / 3600)}时`
        : total >= 3600 ? `${Math.floor(total / 3600)}时 ${Math.floor(total % 3600 / 60)}分` : `${Math.floor(total / 60)}分 ${total % 60}秒`);
    } else setText(this.uptime.strong, valueText(rawUptime));
  }

  render(payload) {
    const snapshot = payload?.snapshot || payload;
    if (!snapshot || typeof snapshot !== 'object') throw new Error('状态快照格式不正确。');
    this.snapshot = snapshot;
    this.renderGlobals(snapshot);
    const items = statusItems(snapshot);
    const retained = new Set();
    if (items.length && !this.cards.size) this.grid.replaceChildren();
    for (const [index, item] of items.entries()) {
      const key = String(item.name ?? index);
      retained.add(key);
      let card = this.cards.get(key);
      if (!card) { card = this.createCard(key); this.cards.set(key, card); this.grid.append(card.node); }
      this.updateCard(card, item);
    }
    for (const [key, card] of this.cards) {
      if (!retained.has(key)) { card.node.remove(); this.cards.delete(key); }
    }
    if (!items.length) this.grid.replaceChildren(emptyState('还没有同步项目', '在配置管理中添加项目，开始同步地址与解析记录。'));
  }

  createCard(name) {
    const title = el('h3', { text: name });
    const subtitle = el('p', { class: 'caption' });
    const phase = el('span', { class: 'badge neutral' });
    const details = el('dl', { class: 'item-details' });
    const domains = el('div', { class: 'table-scroll' });
    const address = el('div', { class: 'address-list' });
    const error = el('p', { class: 'banner error', hidden: true });
    const footer = el('div', { class: 'item-footer' });
    const sync = button('立即同步', async () => {
      if (sync.disabled) return;
      sync.disabled = true; setText(sync, '提交中…');
      try {
        const response = await api.request(`/api/v1/items/${encodeURIComponent(name)}/sync`, { method: 'POST' });
        if (response.status === 202) toast(`${name} 的同步请求已进入队列。`, 'success');
        else toast(`同步请求已接收（HTTP ${response.status}）。`);
        await this.refresh();
      } catch (failure) {
        if (!isQuietError(failure)) toast(failure.status === 404 ? '项目已不存在，请刷新状态或检查配置。' : failure.message, 'error');
      } finally { sync.disabled = false; setText(sync, '立即同步'); }
    }, 'small');
    const node = el('article', { class: 'panel item-card' },
      el('div', { class: 'item-card-heading' }, el('div', {}, title, subtitle), el('div', { class: 'inline-actions' }, phase, sync)), details,
      el('div', { class: 'item-subheading', text: 'DNS 解析记录' }), domains, address, error, footer);
    return { node, title, subtitle, phase, details, domains, address, error, footer, sync };
  }

  updateCard(card, item) {
    const family = item.family ?? item.source?.family;
    const network = item.interface ?? item.source?.interface;
    setText(card.title, item.name || '未命名项目');
    setText(card.subtitle, `${String(family || '—').toUpperCase()} / ${network || '—'}`);
    setText(card.phase, item.phase || 'unknown');
    card.phase.className = `badge ${toneOf(item.phase)}`;
    card.details.replaceChildren(detail('来源地址', item.sourceAddress, true), detail('EUI-64 MAC', item.eui64Mac, true), detail('记录值', item.recordValue, true));
    const domains = Array.isArray(item.domains) ? item.domains : [];
    if (domains.length) {
      const columns = [['provider', '服务商'], ['fqdn', '完整域名'], ['type', '类型'], ['value', '记录值'], ['recordId', '记录 ID'], ['ttl', 'TTL / 秒'], ['line', '线路'], ['updatedAt', '更新时间'], ['lastError', '最近错误']];
      card.domains.replaceChildren(el('table', { class: 'data-table', attrs: { 'aria-label': `${item.name} 的解析记录` } },
        el('thead', {}, el('tr', {}, columns.map(([, label]) => el('th', { text: label, attrs: { scope: 'col' } })))),
        el('tbody', {}, domains.map(domain => el('tr', {}, columns.map(([key, label]) => {
          const copy = key === 'fqdn' || key === 'value';
          let content;
          if (copy) content = withCopy(domain[key], label, 'mono');
          else if (key === 'updatedAt') content = timeText(domain[key]);
          else content = valueText(redactObject(domain[key]));
          return el('td', { class: key === 'lastError' && domain[key] ? 'text-error' : '' }, content);
        }))))));
    } else card.domains.replaceChildren(el('p', { class: 'muted compact-empty', text: '未配置 DNS 记录' }));
    if (item.addressList) {
      const list = item.addressList;
      const entries = Array.isArray(list.entries) ? list.entries : list.entries == null ? [] : [list.entries];
      card.address.replaceChildren(el('div', { class: 'item-subheading', text: 'RouterOS 地址列表' }),
        el('div', { class: 'address-list-meta' }, el('strong', { text: valueText(list.list) }), el('span', { class: 'caption', text: `更新于 ${timeText(list.updatedAt)}` })),
        entries.length ? el('ul', { class: 'address-entries mono' }, entries.map(entry => el('li', { text: redactObject(entry) }))) : el('p', { class: 'help', text: '暂无条目' }),
        ...(list.lastError ? [el('p', { class: 'inline-error', text: list.lastError })] : []));
    } else card.address.replaceChildren();
    banner(card.error, item.lastError || '');
    card.footer.replaceChildren(el('span', { text: `上次同步 ${timeText(item.lastSyncAt ?? item.lastSync)}` }), el('span', { text: `下次轮询 ${timeText(item.nextPollAt)}` }));
  }

  reset() {
    this.stop(); this.snapshot = null; this.meta = {}; this.metaAt = 0; this.cards.clear();
    this.grid.replaceChildren(emptyState('请先连接', '连接后将自动载入运行状态。'));
    for (const metric of [this.total, this.health, this.uptime]) setText(metric.strong, '—');
    setText(this.path, '—'); banner(this.alert, '');
    setText(document.getElementById('dashboard-updated'), '等待连接');
    setText(document.getElementById('ready-badge'), '未连接');
    document.getElementById('ready-badge').className = 'badge neutral';
  }
}
