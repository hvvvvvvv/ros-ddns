const secrets = new Set();
let nextId = 0;
export const uid = (prefix = 'field') => `${prefix}-${++nextId}`;

export function rememberSecret(value) {
  if (typeof value === 'string' && value.length) secrets.add(value);
}

export function rememberConfigSecrets(data) {
  const visit = (value, key = '') => {
    if (/password|secret|token|accesskeyid|authorization/i.test(key)) {
      if (typeof value === 'string') rememberSecret(value);
      return;
    }
    if (value && typeof value === 'object') {
      for (const [childKey, child] of Object.entries(value)) visit(child, childKey);
    }
  };
  visit(data);
}

// All display and clipboard text shares the same redaction boundary. Form input
// values are set separately; API tokens are never placed in configuration inputs.
export function display(value) {
  let text = value == null ? '' : typeof value === 'object' ? JSON.stringify(value) : String(value);
  for (const secret of [...secrets].sort((a, b) => b.length - a.length)) {
    text = text.split(secret).join('[REDACTED]');
  }
  return text
    .replace(/\bBearer\s+[^\s,"'<>]+/gi, 'Bearer [REDACTED]')
    .replace(/((?:password|accessKeyId|accessKeySecret|securityToken|api[._-]?token|authorization)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)/gi, '$1[REDACTED]');
}

export function redactObject(value) {
  if (Array.isArray(value)) return value.map(redactObject);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key,
      /password|secret|token|accesskeyid|authorization/i.test(key) ? '[REDACTED]' : redactObject(entry)]));
  }
  return typeof value === 'string' ? display(value) : value;
}

export function el(tag, options = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(options)) {
    if (key === 'text') node.textContent = display(value);
    else if (key === 'class') node.className = value;
    else if (key === 'on') {
      for (const [event, handler] of Object.entries(value)) node.addEventListener(event, handler);
    } else if (key === 'attrs') {
      for (const [name, attr] of Object.entries(value)) if (attr != null) node.setAttribute(name, attr);
    } else if (value != null) node[key] = value;
  }
  for (const child of children.flat(Infinity)) {
    if (child != null) node.append(child instanceof Node ? child : document.createTextNode(display(child)));
  }
  return node;
}

export const button = (text, handler, className = '') => el('button', {
  type: 'button', class: `button ${className}`, text, on: { click: handler },
});

export function setText(node, value) {
  const text = display(value);
  if (node.textContent !== text) node.textContent = text;
}

export function emptyState(title, detail = '') {
  return el('div', { class: 'empty-state' }, el('span', { class: 'empty-icon', text: '◌', attrs: { 'aria-hidden': 'true' } }),
    el('h3', { text: title }), el('p', { text: detail }));
}

export function banner(node, message, tone = 'error') {
  node.hidden = !message;
  node.className = `banner ${tone}`;
  setText(node, message || '');
}

export function timeText(value) {
  if (!value || String(value).startsWith('0001-')) return '—';
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? display(value) : date.toLocaleString('zh-CN', { hour12: false });
}

export function toneOf(value) {
  const key = String(value || '').toLowerCase();
  if (/error|fail|fatal|degrad|backoff/.test(key)) return 'error';
  if (/recover|success|ready|healthy|synced|idle|running/.test(key)) return 'success';
  if (/warn|pending|stabil|wait|sync|poll|reconcil/.test(key)) return 'warning';
  return 'neutral';
}

export function toast(message, tone = 'neutral', title = '') {
  const host = document.getElementById('toasts');
  const content = el('div', {}, title ? el('strong', { text: title }) : null, el('p', { text: message }));
  const node = el('div', { class: `toast ${tone}` }, content);
  let timer;
  const remove = () => { clearTimeout(timer); node.remove(); };
  const dismiss = button('×', remove, 'icon-button');
  dismiss.setAttribute('aria-label', '关闭提示');
  node.append(dismiss);
  const schedule = () => { clearTimeout(timer); timer = setTimeout(remove, tone === 'error' ? 12000 : 6500); };
  node.addEventListener('pointerenter', () => clearTimeout(timer));
  node.addEventListener('pointerleave', schedule);
  node.addEventListener('focusin', () => clearTimeout(timer));
  node.addEventListener('focusout', schedule);
  while (host.children.length >= 4) host.firstElementChild.remove();
  host.append(node);
  schedule();
}

export async function copyText(value) {
  const text = display(value);
  if (!text) throw new Error('copy text is empty');
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall back to the selection-based API below (e.g. non-secure HTTP).
    }
  }
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', '');
  area.setAttribute('aria-hidden', 'true');
  area.style.position = 'fixed';
  area.style.inset = '0';
  area.style.opacity = '0';
  area.style.pointerEvents = 'none';
  document.body.append(area);
  area.focus();
  area.select();
  let copied = false;
  try { copied = document.execCommand('copy'); } finally { area.remove(); }
  if (!copied) throw new Error('copy command failed');
}

function copyIcon() {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const addPath = (d) => {
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', d);
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '2');
    path.setAttribute('stroke-linecap', 'round');
    path.setAttribute('stroke-linejoin', 'round');
    svg.append(path);
  };
  addPath('M9 9V7a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2h-2');
  addPath('M5 9a2 2 0 0 1 2-2h6a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2z');
  return svg;
}

export function copyButton(value, label) {
  if (!display(value)) return null;
  const node = button('', async () => {
    try {
      await copyText(value);
      toast(`${label}已复制。`, 'success');
    } catch {
      toast(`复制${label}失败，请手动选择文本。`, 'error');
    }
  }, 'small subtle copy-button');
  node.append(copyIcon());
  node.title = `复制${label}`;
  node.setAttribute('aria-label', `复制${label}`);
  return node;
}

let activeDialog;
export function dismissDialog() {
  if (activeDialog) activeDialog();
}

export function ask({ title, message, confirm = '确认', danger = false, input, validate }) {
  // A single modal also prevents repeated hash changes from stacking prompts.
  if (activeDialog) return Promise.resolve(input === undefined ? false : null);
  return new Promise(resolve => {
    const previous = document.activeElement;
    const titleId = uid('dialog-title');
    const descriptionId = uid('dialog-description');
    const dialog = el('dialog', { class: 'modal', attrs: { 'aria-labelledby': titleId, 'aria-describedby': descriptionId } });
    const form = el('form', {}, el('h2', { id: titleId, text: title }), el('p', { id: descriptionId, class: 'muted', text: message }));
    const field = input === undefined ? null : el('input', { type: 'text', value: input, required: true, maxLength: 128, attrs: { 'aria-label': title, autocomplete: 'off' } });
    const error = el('p', { class: 'inline-error', hidden: true, attrs: { role: 'alert' } });
    const finish = value => {
      activeDialog = null;
      dialog.close();
      dialog.remove();
      if (previous?.isConnected) previous.focus();
      resolve(value);
    };
    activeDialog = () => finish(input === undefined ? false : null);
    const cancel = button('取消', activeDialog, 'subtle');
    const submit = el('button', { type: 'submit', class: `button ${danger ? 'danger' : 'primary'}`, text: confirm });
    form.append(...[field, error, el('div', { class: 'modal-actions' }, cancel, submit)].filter(Boolean));
    form.addEventListener('submit', event => {
      event.preventDefault();
      const value = field ? field.value.trim() : true;
      const issue = field && validate ? validate(value) : '';
      if (issue) { error.hidden = false; setText(error, issue); field.focus(); return; }
      finish(value);
    });
    dialog.addEventListener('cancel', event => { event.preventDefault(); activeDialog(); });
    dialog.append(form);
    document.body.append(dialog);
    dialog.showModal();
    (field || cancel).focus();
    if (field) field.select();
  });
}
