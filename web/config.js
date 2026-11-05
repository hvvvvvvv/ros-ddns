import { api, isQuietError } from '/assets/api.js';
import { el, button, emptyState, banner, setText, toast, ask, rememberConfigSecrets } from '/assets/dom.js';
import { clone, getAt, setAt, pathLabel, canonicalPath, normalizeErrors, createField, readOnlyField, section } from '/assets/forms.js';

const durationPattern = /^(?:0|(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h)(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))*)$/;
const rosDurationPattern = /^(?:0|(?:\d+(?:\.\d+)?)|(?:\d+(?::\d+(?:\.\d+)?){1,2})|(?:\d+[wdhms](?:\d+[wdhms])*))$/;
const isReference = value => typeof value === 'string' && value.includes('${');
const durationCheck = value => !value || isReference(value) || durationPattern.test(value) ? '' : '请输入时长，例如 10s、5m、1h30m 或 0。';
const rosDurationCheck = value => !value || isReference(value) || rosDurationPattern.test(value) ? '' : '请输入 RouterOS 时长，例如 1h30m、00:10:00 或 600。';
const grid = (...children) => el('div', { class: 'form-grid' }, ...children);

export class Config {
  constructor(host, itemsHost, { onApplied } = {}) {
    this.onApplied = onApplied;
    this.draft = null;
    this.fields = new Map();
    this.summaries = new Map();
    this.addressToggles = new Map();
    this.domainAddButtons = new Map();
    this.addressCache = [];
    this.template = 'ipv4';
    this.busy = false;
    this.dirty = false;
    this.needsReload = false;
    this.path = el('code', { class: 'config-path', text: '等待载入' });
    this.errors = el('div', { class: 'banner error validation-errors', hidden: true, tabIndex: -1, attrs: { role: 'alert' } });
    this.itemsErrors = el('div', { class: 'banner error validation-errors', hidden: true, tabIndex: -1, attrs: { role: 'alert' } });
    this.notice = el('div', { class: 'banner neutral', hidden: true, attrs: { role: 'status' } });
    this.itemsNotice = el('div', { class: 'banner neutral', hidden: true, attrs: { role: 'status' } });
    this.form = el('form', { noValidate: true });
    this.itemsForm = el('form', { noValidate: true });
    this.form.addEventListener('submit', event => { event.preventDefault(); this.save(); });
    this.itemsForm.addEventListener('submit', event => { event.preventDefault(); this.save(); });
    this.reload = button('重新加载', () => this.load());
    this.configActions = this.actionBar();
    this.itemsActions = this.actionBar();
    this.actionBars = [this.configActions, this.itemsActions];
    host.replaceChildren(el('div', { class: 'config-source' }, el('div', {}, el('span', { class: 'caption', text: '当前配置文件' }), this.path), this.reload),
      this.errors, this.notice, this.form);
    itemsHost.replaceChildren(this.itemsErrors, this.itemsNotice, this.itemsForm);
    const loading = () => emptyState('正在读取配置', '配置只通过表单编辑；未显示的字段会原样保留。');
    this.form.replaceChildren(loading());
    this.itemsForm.replaceChildren(loading());
    this.setBusy(false);
  }

  actionBar() {
    const validate = button('校验配置', () => this.validate());
    const save = el('button', { type: 'submit', class: 'button primary', text: '保存并应用' });
    const hint = el('span', { class: 'caption', text: '所有更改通过服务端校验后应用。' });
    const node = el('div', { class: 'config-actions' }, hint, el('div', { class: 'inline-actions' }, validate, save));
    return { node, validate, save, hint };
  }

  get providers() { return this.draft?.dns?.providers || {}; }
  get items() { return Array.isArray(this.draft?.items) ? this.draft.items : []; }

  async load({ force = false, keepNotice = false } = {}) {
    if (this.busy || api.locked) return false;
    if (this.dirty && !force && !await ask({ title: '重新加载配置？', message: '这会丢弃当前未保存的更改，并读取服务端的最新配置。', confirm: '丢弃并重新加载', danger: true })) return false;
    this.setBusy(true, '正在读取…');
    try {
      const result = await api.json('/api/v1/config');
      this.accept(result);
      if (!keepNotice) this.setNotice('');
      return true;
    } catch (error) {
      if (!isQuietError(error)) this.showErrors([{ message: error.message }], '无法读取配置');
      if (!this.draft) {
        const failed = () => emptyState('配置未加载', '请检查连接，然后点击「重新加载」。');
        this.form.replaceChildren(failed());
        this.itemsForm.replaceChildren(failed());
      }
      return false;
    } finally { this.setBusy(false); }
  }

  accept(result) {
    if (!result?.data || typeof result.data !== 'object' || Array.isArray(result.data) || typeof result.hash !== 'string') throw new Error('配置响应必须包含 data 对象与 hash 字符串。');
    rememberConfigSecrets(result.data);
    this.draft = clone(result.data);
    this.hash = result.hash;
    this.baseline = JSON.stringify(this.draft);
    this.addressCache = [];
    this.needsReload = false;
    setText(this.path, result.path || '—');
    this.clearErrors();
    this.updateDirty();
    this.render();
  }

  setNotice(message, tone = 'neutral') {
    banner(this.notice, message, tone);
    banner(this.itemsNotice, message, tone);
  }

  setBusy(busy, label = '') {
    this.busy = busy;
    if (this.configFieldset) this.configFieldset.disabled = busy;
    if (this.itemsFieldset) this.itemsFieldset.disabled = busy;
    this.reload.disabled = busy;
    for (const bar of this.actionBars) {
      bar.validate.disabled = busy || !this.draft;
      bar.save.disabled = busy || !this.draft || this.needsReload;
      setText(bar.save, busy ? label || '处理中…' : '保存并应用');
    }
    this.form.setAttribute('aria-busy', String(busy));
    this.itemsForm.setAttribute('aria-busy', String(busy));
  }

  updateDirty() {
    this.dirty = this.draft != null && JSON.stringify(this.draft) !== this.baseline;
    for (const [id, fallback] of [['config-dirty', '配置'], ['items-config-status', '同步项目']]) {
      const badge = document.getElementById(id);
      if (!badge) continue;
      setText(badge, this.dirty ? '未保存' : this.draft ? '已载入' : '尚未加载');
      badge.className = `badge ${this.dirty ? 'warning' : 'neutral'}`;
      badge.setAttribute('aria-label', `${fallback}：${this.dirty ? '有未保存更改' : this.draft ? '已载入' : '尚未加载'}`);
    }
    for (const bar of this.actionBars) {
      setText(bar.hint, this.needsReload ? '请重新加载以取得最新配置版本。' : this.dirty ? '有未保存的更改' : '所有更改通过服务端校验后应用。');
    }
    document.getElementById('dirty-dot').hidden = !this.dirty;
  }

  change(path, value, { remove = false, rerender = false } = {}) {
    this.draft = setAt(this.draft, path, value, remove);
    this.clearErrors();
    this.setNotice('');
    this.updateDirty();
    if (rerender) this.render(path);
    else this.updateSummaries();
  }

  field(path, spec) {
    const { fallback, validator, removeEmpty = false, ...options } = spec;
    const field = createField({ ...options, value: getAt(this.draft, path) ?? fallback,
      onChange: value => this.change(path, value, { remove: (options.type === 'number' || removeEmpty) && value === '' && !options.required }) });
    field.path = path;
    field.label = spec.label;
    field.validator = validator;
    this.fields.set(canonicalPath(path), field);
    return field.node;
  }

  duration(path, label, fallback, example, help = '') {
    return this.field(path, { label, fallback, placeholder: example, removeEmpty: true, help: `${help ? `${help} ` : ''}例如 ${example}；单位 ns / us / ms / s / m / h。`, validator: durationCheck });
  }

  rosDuration(path, label, fallback, example, help = '') {
    return this.field(path, { label, fallback, placeholder: example, removeEmpty: false, help: `${help ? `${help} ` : ''}例如 ${example}；支持单位、HH:MM:SS 或秒数。`, validator: rosDurationCheck });
  }

  render(focusPath) {
    if (!this.draft) return;
    const activePath = focusPath || [...this.fields.values()].find(field => field.input === document.activeElement)?.path;
    this.fields.clear(); this.summaries.clear(); this.addressToggles.clear(); this.domainAddButtons.clear();
    this.configFieldset = el('fieldset', { class: 'config-fields', disabled: this.busy }, el('legend', { class: 'sr-only', text: '服务配置' }));
    this.configFieldset.append(this.baseSection(), this.routerSection(), this.providersSection());
    this.itemsFieldset = el('fieldset', { class: 'config-fields', disabled: this.busy }, el('legend', { class: 'sr-only', text: '同步项目' }));
    this.itemsFieldset.append(this.itemsSection());
    this.form.replaceChildren(this.configFieldset, this.configActions.node);
    this.itemsForm.replaceChildren(this.itemsFieldset, this.itemsActions.node);
    this.updateSummaries();
    this.setBusy(this.busy);
    if (activePath) this.fields.get(canonicalPath(activePath))?.input.focus({ preventScroll: true });
  }

  baseSection() {
    return section('01 / 基础设置', '控制轮询、同步节奏与状态存储。留空的可选项使用服务端默认值。', [
      grid(this.field(['log', 'level'], { label: '日志级别', type: 'select', options: ['debug', 'info', 'warn', 'error'], fallback: 'info' }),
        readOnlyField('日志格式', this.draft.log?.format || '服务端默认', '只读 · 需重启进程生效'),
        this.field(['state', 'path'], { label: '状态文件', placeholder: './data/state.json', help: '留空表示仅存储在内存中。' }),
        this.duration(['runtime', 'pollInterval'], '地址轮询间隔', '10s', '10s'),
        this.duration(['runtime', 'stableFor'], '地址稳定等待时间', '0s', '5s', '0 表示立即接受新地址。'),
        this.duration(['runtime', 'reconcileTimeout'], '单次同步超时', '30s', '30s'),
        this.duration(['runtime', 'verifyInterval'], '完整校验间隔', '10m', '10m', '0 表示禁用周期校验。'),
        this.duration(['runtime', 'maxBackoff'], '最大退避时间', '5m', '5m'),
        this.field(['runtime', 'syncOnStart'], { label: '启动后立即同步', type: 'checkbox', fallback: true, help: '首次成功读取地址后强制执行一次同步。' })),
      el('div', { class: 'readonly-panel' }, el('h3', { text: 'API 访问设置' }), el('p', { class: 'help', text: '以下字段只读，原值随配置保留。修改需重启进程生效。' }),
        grid(readOnlyField('监听地址 · api.listen', this.draft.api?.listen || '未设置'),
          readOnlyField('访问令牌 · api.token', this.draft.api?.token ? '已设置' : '未设置'))),
    ]);
  }

  routerSection() {
    return section('02 / RouterOS', '连接 RouterOS REST API。URL 支持基础路径，服务端自动附加 /rest。存在启用项目时必须填写 URL 和用户名；没有启用项目时可以留空。', [grid(
      this.field(['routeros', 'url'], { label: '路由器 URL', placeholder: 'https://192.168.88.1', help: '例如 https://192.168.88.1；有启用项目时必填。', validator: value => {
        if (!String(value).trim() || isReference(value)) return '';
        try { const url = new URL(value); return ['http:', 'https:'].includes(url.protocol) ? '' : 'URL 需使用 http 或 https。'; } catch { return '请输入完整的 RouterOS URL。'; }
      } }),
      this.field(['routeros', 'username'], { label: '用户名', placeholder: 'ddns-api', help: '有启用项目时必填。' }),
      this.field(['routeros', 'password'], { label: '密码', secret: true, help: '支持现有环境变量引用；不会显示在日志中。' }),
      this.duration(['routeros', 'timeout'], '请求超时', '10s', '10s'),
      this.field(['routeros', 'tls', 'caFile'], { label: 'CA 证书文件', placeholder: '/path/to/ca.pem', help: '服务端文件路径；留空使用系统证书。' }),
      this.field(['routeros', 'tls', 'insecureSkipVerify'], { label: '跳过 TLS 证书验证', type: 'checkbox', fallback: false, help: '用于自签名证书；使用受信任证书时关闭。' }),
    )]);
  }

  providersSection() {
    const add = button('+ 添加服务商', () => this.addProvider(), 'small'); add.id = 'add-provider';
    const names = Object.keys(this.providers);
    const cards = names.map(name => {
      const prefix = ['dns', 'providers', name];
      return el('article', { class: 'editor-card provider-card' },
        el('div', { class: 'editor-card-heading' }, el('div', { class: 'inline-actions' }, el('h3', { text: name }), el('span', { class: 'badge neutral', text: 'Aliyun DNS' })),
          el('div', { class: 'inline-actions' }, button('重命名', () => this.renameProvider(name), 'subtle small'), button('移除', () => this.removeProvider(name), 'subtle danger-text small'))),
        grid(readOnlyField('服务商类型', this.providers[name]?.type || 'aliyun', '当前支持 aliyun。'),
          this.field([...prefix, 'endpoint'], { label: 'API endpoint', fallback: 'alidns.aliyuncs.com', placeholder: 'alidns.aliyuncs.com' }),
          this.field([...prefix, 'regionId'], { label: '地域 Region ID', fallback: 'cn-hangzhou', placeholder: 'cn-hangzhou' }),
          this.field([...prefix, 'accessKeyId'], { label: 'AccessKey ID', secret: true, help: '可以稍后填写；缺失时同步会失败并重试。' }),
          this.field([...prefix, 'accessKeySecret'], { label: 'AccessKey Secret', secret: true, help: '可以稍后填写；缺失时同步会失败并重试。' }),
          this.field([...prefix, 'securityToken'], { label: 'Security Token', secret: true, help: '可选，仅使用临时凭据时填写。' }),
          this.duration([...prefix, 'timeout'], '请求超时', '10s', '10s'),
          this.field([...prefix, 'qps'], { label: '请求速率 QPS', type: 'number', min: 1, step: 1, fallback: 5, help: '单位：请求 / 秒，必须为整数且不少于 1。默认 5。' })));
    });
    return section(`03 / DNS 服务商 · ${names.length}`, '服务商名称用于关联「同步项目」页面中的域名记录。凭据仅在表单中编辑。',
      cards.length ? cards : [emptyState('尚未添加 DNS 服务商', '添加 Aliyun DNS 凭据，再将域名关联到服务商。')], add);
  }

  itemsSection() {
    const template = createField({ label: '新项目模板', type: 'select', value: this.template, options: [
      { value: 'ipv4', label: 'IPv4 · A 记录' }, { value: 'ipv6', label: 'IPv6 · AAAA 记录' }, { value: 'eui64', label: 'IPv6 · EUI-64 地址' },
    ], onChange: value => { this.template = value; } });
    const add = button('+ 添加项目', () => this.addItem(), 'small'); add.id = 'add-item';
    return section(`04 / 同步项目 · ${this.items.length}`, '一个来源地址可以同步到多个域名，也可以写入 RouterOS 地址列表。', [
      el('div', { class: 'template-bar' }, template.node, add),
      ...(this.items.length ? this.items.map((item, index) => this.itemCard(item, index)) : [emptyState('尚未配置同步项目', '选择模板并添加第一个项目。')]),
    ]);
  }

  itemCard(item, index) {
    const prefix = ['items', index];
    const title = el('h3', { text: item.name || `项目 ${index + 1}` });
    const summary = el('p', { class: 'help' });
    this.summaries.set(index, { title, summary });
    const addressEnabled = createField({ label: '同步 RouterOS 地址列表', type: 'checkbox', value: Boolean(item.addressList), help: '可选，可与 DNS 同步同时启用。', onChange: value => {
      if (!value) this.addressCache[index] = clone(this.items[index].addressList);
      this.change([...prefix, 'addressList'], value ? this.addressCache[index] || { list: '', timeout: '', comment: '' } : undefined, { remove: !value, rerender: true });
      this.addressToggles.get(index)?.focus({ preventScroll: true });
    } });
    this.addressToggles.set(index, addressEnabled.input);
    const sourceFields = grid(
      this.field([...prefix, 'name'], { label: '项目名称', required: true, placeholder: 'home-ipv4' }),
      this.field([...prefix, 'source', 'interface'], { label: '来源接口', required: true, placeholder: 'pppoe-out1', help: 'RouterOS 中的接口名称。' }),
      this.field([...prefix, 'source', 'family'], { label: '地址族', type: 'select', options: [{ value: 'ipv4', label: 'IPv4 → A' }, { value: 'ipv6', label: 'IPv6 → AAAA' }], fallback: 'ipv4' }),
      this.field([...prefix, 'source', 'eui64-mac'], { label: 'EUI-64 MAC', placeholder: '00:1A:2B:3C:4D:5E', help: '仅 IPv6：使用所选地址的前 64 位与此 MAC 推导接口 ID。留空使用来源地址。', validator: value => !value || isReference(value) || /^(?:[\da-f]{2}[:-]){5}[\da-f]{2}$/i.test(value) ? '' : '请输入 6 组十六进制 MAC 地址，例如 00:1A:2B:3C:4D:5E。' }),
      this.field([...prefix, 'source', 'select', 'scope'], { label: '地址范围', type: 'select', options: [{ value: 'public', label: 'public · 公网地址' }, { value: 'any', label: 'any · 任意地址' }], fallback: 'public' }),
      this.field([...prefix, 'source', 'select', 'prefix'], { label: '地址前缀', placeholder: '240e:399:45ce::/48', help: '可选，用 CIDR 前缀筛选多个地址。' }),
      this.field([...prefix, 'source', 'select', 'index'], { label: '地址索引', type: 'number', min: 0, step: 1, fallback: 0, help: '从 0 开始；默认选择第一个匹配地址。' }),
      this.field([...prefix, 'disabled'], { label: '停用此项目', type: 'checkbox', fallback: false, help: '保留配置，停止自动同步。' }),
    );
    const domainRows = (Array.isArray(item.domains) ? item.domains : []).map((domain, row) => this.domainRow(index, row));
    const domainTable = el('div', { class: 'table-scroll domain-editor' }, el('table', { class: 'data-table', attrs: { 'aria-label': `项目 ${index + 1} 的域名配置` } },
      el('thead', {}, el('tr', {}, ['服务商', '主域名', '主机记录', 'TTL / 秒', '线路', '操作'].map(text => el('th', { text, attrs: { scope: 'col' } })))),
      el('tbody', {}, domainRows)));
    const addDomain = button('+ 添加域名', () => {
      const domains = [...(this.items[index].domains || []), this.newDomain()];
      this.change([...prefix, 'domains'], domains, { rerender: true });
      this.fields.get(canonicalPath([...prefix, 'domains', domains.length - 1, 'domain']))?.input.focus();
    }, 'small');
    this.domainAddButtons.set(index, addDomain);
    return el('article', { class: 'editor-card item-editor' },
      el('div', { class: 'editor-card-heading' }, el('div', {}, title, summary), el('div', { class: 'inline-actions' },
        button('复制', () => this.copyItem(index), 'subtle small'), button('移除', () => this.removeItem(index), 'subtle danger-text small'))),
      sourceFields,
      el('div', { class: 'section-heading subsection-heading' }, el('h4', { text: '域名记录' }), addDomain),
      domainRows.length ? domainTable : el('p', { class: 'compact-empty muted', text: '暂无域名记录。仅同步地址列表时可留空。' }),
      !Object.keys(this.providers).length ? el('p', { class: 'help warning-text', text: '尚无可选服务商，请先在「配置管理」页面的「DNS 服务商」分组中添加服务商。' }) : null,
      el('div', { class: 'address-editor' }, addressEnabled.node, item.addressList ? grid(
        this.field([...prefix, 'addressList', 'list'], { label: '列表名称', required: true, placeholder: 'ddns-home-ipv4' }),
        this.rosDuration([...prefix, 'addressList', 'timeout'], '条目过期时间', '', '1h', '留空为静态条目；非空时 RouterOS 重启会清除动态条目。'),
        this.field([...prefix, 'addressList', 'comment'], { label: '条目备注', placeholder: `ros-ddns:${item.name || '项目名称'}`, help: '默认使用 ros-ddns:项目名称。' }),
      ) : null));
  }

  domainRow(index, row) {
    const prefix = ['items', index, 'domains', row];
    const names = Object.keys(this.providers);
    const options = [{ value: '', label: '请选择服务商' }, ...names.map(name => ({ value: name, label: name }))];
    const cell = node => el('td', {}, node);
    return el('tr', {},
      cell(this.field([...prefix, 'provider'], { label: '服务商', type: 'select', compact: true, options, required: true, validator: value => Object.hasOwn(this.providers, value) ? '' : '请选择现有的 DNS 服务商。' })),
      cell(this.field([...prefix, 'domain'], { label: '主域名', compact: true, required: true, placeholder: 'example.com' })),
      cell(this.field([...prefix, 'rr'], { label: '主机记录', compact: true, required: true, placeholder: '@ 或 home' })),
      cell(this.field([...prefix, 'ttl'], { label: 'TTL（秒）', compact: true, type: 'number', min: 1, step: 1, fallback: 600, placeholder: '600' })),
      cell(this.field([...prefix, 'line'], { label: '线路', compact: true, fallback: 'default', placeholder: 'default' })),
      cell(button('移除', async () => {
        if (!await ask({ title: '移除域名记录？', message: '仅移除此项目的配置绑定；不会直接删除 DNS 服务商中的远端记录。保存后生效。', confirm: '移除记录', danger: true })) return;
        this.change(['items', index, 'domains'], this.items[index].domains.filter((_, current) => current !== row), { rerender: true });
        const next = Math.min(row, this.items[index].domains.length - 1);
        (this.fields.get(canonicalPath(['items', index, 'domains', next, 'provider']))?.input || this.domainAddButtons.get(index))?.focus();
      }, 'subtle danger-text small')));
  }

  updateSummaries() {
    for (const [index, refs] of this.summaries) {
      const item = this.items[index];
      if (!item) continue;
      setText(refs.title, item.name || `项目 ${index + 1}`);
      const names = [...new Set((item.domains || []).map(domain => domain.provider).filter(Boolean))];
      setText(refs.summary, `${item.disabled ? '已停用 · ' : ''}${String(item.source?.family || 'ipv4').toUpperCase()} · 服务商：${names.join('、') || '未选择'} · ${(item.domains || []).length} 条域名记录`);
    }
  }

  uniqueName(base, names) { let name = base; let suffix = 2; while (names.includes(name)) name = `${base}-${suffix++}`; return name; }
  newDomain() { return { provider: Object.keys(this.providers)[0] || '', domain: '', rr: '@', ttl: 600, line: 'default' }; }

  async addProvider() {
    const name = await ask({ title: '添加 DNS 服务商', message: '设置一个唯一名称，域名记录将通过此名称关联服务商。', input: this.uniqueName('ali-main', Object.keys(this.providers)), confirm: '添加服务商',
      validate: value => !value ? '请填写服务商名称。' : Object.hasOwn(this.providers, value) ? '服务商名称已存在。' : '' });
    if (name === null) return;
    this.change(['dns', 'providers', name], { type: 'aliyun', endpoint: 'alidns.aliyuncs.com', regionId: 'cn-hangzhou', accessKeyId: '', accessKeySecret: '', securityToken: '', timeout: '10s', qps: 5 }, { rerender: true });
    this.fields.get(canonicalPath(['dns', 'providers', name, 'accessKeyId']))?.input.focus();
  }

  async renameProvider(name) {
    const next = await ask({ title: '重命名服务商', message: '所有引用此服务商的域名记录会一并更新，保存后生效。', input: name, confirm: '重命名',
      validate: value => !value ? '请填写服务商名称。' : value !== name && Object.hasOwn(this.providers, value) ? '服务商名称已存在。' : '' });
    if (next === null || next === name) return;
    this.draft = setAt(this.draft, ['dns', 'providers'], Object.fromEntries(Object.entries(this.providers).map(([key, value]) => [key === name ? next : key, value])));
    this.change(['items'], this.items.map(item => Array.isArray(item.domains) ? { ...item, domains: item.domains.map(domain => domain.provider === name ? { ...domain, provider: next } : domain) } : item), { rerender: true });
    this.fields.get(canonicalPath(['dns', 'providers', next, 'endpoint']))?.input.focus();
  }

  async removeProvider(name) {
    const count = this.items.reduce((total, item) => total + (item.domains || []).filter(domain => domain.provider === name).length, 0);
    if (!await ask({ title: '移除 DNS 服务商？', message: count ? `${count} 条域名记录正在使用此服务商。移除后需为这些记录重新选择服务商，才能保存。` : '这会从配置中移除此服务商，保存后生效。', confirm: '移除服务商', danger: true })) return;
    this.change(['dns', 'providers', name], undefined, { remove: true, rerender: true });
    document.getElementById('add-provider')?.focus();
  }

  async addItem() {
    const family = this.template === 'ipv4' ? 'ipv4' : 'ipv6';
    const name = await ask({ title: '添加同步项目', message: `基于 ${this.template === 'eui64' ? 'IPv6 EUI-64' : family.toUpperCase()} 模板创建。请为项目设置唯一名称。`, input: this.uniqueName(`home-${family}`, this.items.map(item => item.name)), confirm: '添加项目',
      validate: value => !value ? '请填写项目名称。' : this.items.some(item => item.name === value) ? '项目名称已存在。' : '' });
    if (name === null) return;
    const source = { interface: '', family, select: { scope: 'public', prefix: '', index: 0 } };
    if (this.template === 'eui64') source['eui64-mac'] = '';
    const index = this.items.length;
    this.change(['items'], [...this.items, { name, disabled: false, source, domains: [this.newDomain()] }], { rerender: true });
    this.fields.get(canonicalPath(['items', index, 'source', 'interface']))?.input.focus();
  }

  copyItem(index) {
    const item = clone(this.items[index]);
    item.name = this.uniqueName(`${item.name || 'item'}-copy`, this.items.map(entry => entry.name));
    // A copied item starts disabled so it cannot accidentally compete for the
    // same records before its source and destinations have been reviewed.
    item.disabled = true;
    const next = this.items.length;
    this.change(['items'], [...this.items, item], { rerender: true });
    this.fields.get(canonicalPath(['items', next, 'name']))?.input.focus();
    toast('已复制项目。副本默认停用，请检查来源与目标记录后启用。');
  }

  async removeItem(index) {
    if (!await ask({ title: '移除同步项目？', message: '项目会在保存后停止同步。此操作只修改配置，不直接删除远端记录。', confirm: '移除项目', danger: true })) return;
    this.addressCache.splice(index, 1);
    this.change(['items'], this.items.filter((_, current) => current !== index), { rerender: true });
    document.getElementById('add-item')?.focus();
  }

  clearErrors() {
    for (const node of [this.errors, this.itemsErrors]) {
      node.hidden = true;
      node.replaceChildren();
    }
    for (const field of this.fields.values()) field.setError('');
  }

  showErrors(errors, title = '请检查以下配置项') {
    this.clearErrors();
    const entries = normalizeErrors(errors);
    if (!entries.length) entries.push({ path: '', message: '服务端未提供具体校验信息。' });
    const render = node => {
      const list = el('ul');
      for (const error of entries) {
        const field = this.fields.get(canonicalPath(error.path));
        if (field) field.setError(error.message);
        const label = `${error.path ? `${Array.isArray(error.path) ? pathLabel(error.path) : error.path}：` : ''}${error.message}`;
        list.append(el('li', {}, field ? button(label, () => { field.input.focus(); field.input.scrollIntoView({ block: 'center' }); }, 'error-link') : el('span', { text: label })));
      }
      node.replaceChildren(el('strong', { text: title }), list);
      node.hidden = false;
    };
    render(this.errors);
    render(this.itemsErrors);
    const active = document.getElementById('view-items')?.hidden === false ? this.itemsErrors : this.errors;
    active.focus({ preventScroll: true });
    active.scrollIntoView({ block: 'nearest' });
  }

  localErrors() {
    const errors = [];
    for (const field of this.fields.values()) {
      const path = field.path;
      if (path[0] === 'items' && this.items[path[1]]?.disabled && path[2] !== 'name') continue;
      const value = field.input.type === 'checkbox' ? field.input.checked : field.input.value;
      let message = '';
      if (field.input.required && !String(value).trim()) message = `请填写${field.label}。`;
      else if (field.input.type === 'number' && value !== '' && (!Number.isFinite(Number(value)) || !field.input.validity.valid)) message = '请输入范围内的有效数值。';
      else if (field.validator) message = field.validator(value);
      if (message) errors.push({ path, message });
    }
    if (this.items.some(item => !item.disabled)) {
      for (const [path, label] of [[['routeros', 'url'], '路由器 URL'], [['routeros', 'username'], '用户名']]) {
        const field = this.fields.get(canonicalPath(path));
        if (!field || !String(field.input.value).trim()) errors.push({ path, message: `存在启用项目时必须填写${label}。` });
      }
    }
    const seen = new Set();
    this.items.forEach((item, index) => {
      if (seen.has(item.name)) errors.push({ path: ['items', index, 'name'], message: '项目名称必须唯一。' });
      seen.add(item.name);
      if (!item.disabled && !item.domains?.length && !item.addressList) errors.push({ path: ['items', index, 'name'], message: '至少添加一条域名记录或启用地址列表。' });
      if (!item.disabled && item.source?.['eui64-mac'] && item.source?.family !== 'ipv6') errors.push({ path: ['items', index, 'source', 'eui64-mac'], message: 'EUI-64 MAC 仅适用于 IPv6。' });
    });
    return errors;
  }

  async validate() {
    if (this.busy || !this.draft) return;
    const errors = this.localErrors();
    if (errors.length) { this.showErrors(errors); return; }
    this.setBusy(true, '校验中…');
    try {
      const result = await api.json('/api/v1/config/validate', { method: 'POST', body: { data: this.draft } });
      if (result?.valid === true) { this.clearErrors(); this.setNotice('配置校验通过。尚未保存或应用。', 'success'); }
      else this.showErrors(result?.errors);
    } catch (error) {
      if (!isQuietError(error)) this.showErrors(error.data?.errors || [{ message: error.message }]);
    } finally { this.setBusy(false); }
  }

  async save() {
    if (this.busy || !this.draft || this.needsReload) return;
    const errors = this.localErrors();
    if (errors.length) { this.showErrors(errors); return; }
    this.setBusy(true, '正在应用…');
    let conflict = false;
    try {
      const result = await api.json('/api/v1/config', { method: 'PUT', body: { baseHash: this.hash, data: this.draft } });
      this.baseline = JSON.stringify(this.draft);
      this.needsReload = true;
      this.updateDirty(); this.clearErrors();
      // Success feedback comes from the config_applied event/toast.
      this.setNotice('');
      this.onApplied?.(result?.snapshot);
      try { this.accept(await api.json('/api/v1/config')); }
      catch (error) {
        if (!isQuietError(error)) this.showErrors([{ message: '配置已应用，但未能读取新的配置版本。请重新加载后再保存。' }]);
      }
    } catch (error) {
      if (error.status === 409) {
        conflict = true;
        this.showErrors([{ message: '配置已被外部修改。当前编辑仍保留；重新加载会丢弃这些更改。' }], '配置版本冲突');
      } else if (!isQuietError(error)) this.showErrors(error.data?.errors || [{ message: error.message }]);
    } finally { this.setBusy(false); }
    if (conflict && await ask({ title: '配置已被外部修改', message: '当前更改尚未保存。是否丢弃本地更改并载入最新配置？选择取消可继续保留当前编辑。', confirm: '重新加载', danger: true })) await this.load({ force: true });
  }

  reset() {
    this.draft = null; this.hash = ''; this.baseline = ''; this.addressCache = []; this.fields.clear(); this.summaries.clear(); this.addressToggles.clear(); this.domainAddButtons.clear();
    this.needsReload = false; this.updateDirty(); this.clearErrors(); this.setNotice(''); setText(this.path, '等待连接');
    const disconnected = () => emptyState('请先连接', '连接后可以读取和编辑服务配置。');
    this.form.replaceChildren(disconnected());
    this.itemsForm.replaceChildren(disconnected());
    this.setBusy(false);
  }
}
