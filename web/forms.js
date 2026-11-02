import { el, button, uid, rememberSecret, setText } from '/assets/dom.js';

export const clone = value => structuredClone(value);
export const getAt = (object, path) => path.reduce((value, key) => value != null && Object.hasOwn(value, key) ? value[key] : undefined, object);

// Copy only the path being edited. Sibling fields, including unknown fields,
// survive unchanged. Computed own keys also support unusual provider names.
export function setAt(object, path, value, remove = false) {
  if (!path.length) return value;
  const [key, ...rest] = path;
  const result = Array.isArray(object) ? [...object] : { ...object };
  if (!rest.length && remove) delete result[key];
  else Object.defineProperty(result, key, { value: rest.length ? setAt(getAt(object, [key]), rest, value, remove) : value,
    writable: true, enumerable: true, configurable: true });
  return result;
}

export function pathLabel(path) {
  return path.reduce((text, key) => typeof key === 'number' ? `${text}[${key}]` : `${text}${text ? '.' : ''}${key}`, '');
}

export function canonicalPath(path) {
  if (Array.isArray(path)) return path.map(String).join('.');
  return String(path || '').replace(/^data[./]/, '').replace(/\[(\d+)\]/g, '.$1').replace(/^\//, '').replace(/\//g, '.');
}

export function normalizeErrors(errors) {
  if (!errors) return [];
  if (typeof errors === 'string') {
    const match = errors.match(/^((?:items|dns|runtime|routeros|log|api|state)[^:\s]*)\s*:\s*(.+)$/s);
    return [{ path: match?.[1] || '', message: match?.[2] || errors }];
  }
  if (Array.isArray(errors)) return errors.flatMap(normalizeErrors);
  if (typeof errors === 'object') {
    if (errors.message || errors.error) return [{ path: errors.field || errors.path || '', message: String(errors.message || errors.error) }];
    return Object.entries(errors).flatMap(([path, value]) => normalizeErrors(value).map(error => ({ ...error, path: error.path || path })));
  }
  return [{ path: '', message: String(errors) }];
}

export function createField({ label, value, type = 'text', help = '', placeholder = '', required = false,
  options = [], min, max, step, onChange, secret = false, compact = false, readOnly = false }) {
  const id = uid();
  const helpId = `${id}-help`;
  const errorId = `${id}-error`;
  const input = type === 'select' ? el('select') : el('input', { type: secret ? 'password' : type });
  input.id = id;
  input.required = required;
  input.autocomplete = 'off';
  if (type !== 'checkbox') input.spellcheck = false;
  input.setAttribute('aria-describedby', `${helpId} ${errorId}`);
  if (type === 'select') {
    const choices = options.map(option => typeof option === 'string' ? { value: option, label: option } : option);
    if (value != null && !choices.some(option => String(option.value) === String(value))) {
      choices.unshift({ value, label: `${value}（当前值）` });
    }
    for (const option of choices) input.append(el('option', { value: option.value, text: option.label, disabled: option.disabled || false }));
  }
  if (type === 'checkbox') input.checked = Boolean(value);
  else input.value = value == null ? '' : String(value);
  if (secret) rememberSecret(input.value);
  if (placeholder) input.placeholder = placeholder;
  if (min !== undefined) input.min = min;
  if (max !== undefined) input.max = max;
  if (step !== undefined) input.step = step;
  if (readOnly) {
    if (type === 'select') input.disabled = true;
    else input.readOnly = true;
  }
  const labelNode = el('label', { class: 'field-label', htmlFor: id }, label,
    required ? el('span', { class: 'required', text: ' *', attrs: { 'aria-label': '必填' } }) : null);
  const error = el('p', { id: errorId, class: 'inline-error', hidden: true });
  const hint = el('p', { id: helpId, class: 'help', text: help, hidden: !help });
  const control = el('div', { class: secret ? 'secret-control' : 'input-control' }, input);
  if (secret) {
    const reveal = button('显示', () => {
      input.type = input.type === 'password' ? 'text' : 'password';
      setText(reveal, input.type === 'password' ? '显示' : '隐藏');
      reveal.setAttribute('aria-pressed', String(input.type === 'text'));
      reveal.setAttribute('aria-label', `${input.type === 'password' ? '显示' : '隐藏'}${label}`);
    }, 'reveal');
    reveal.setAttribute('aria-label', `显示${label}`);
    reveal.setAttribute('aria-pressed', 'false');
    control.append(reveal);
  }
  const node = el('div', { class: `field${compact ? ' compact' : ''}${type === 'checkbox' ? ' checkbox-field' : ''}` },
    ...(type === 'checkbox' ? [control, el('div', {}, labelNode, hint, error)] : [labelNode, control, hint, error]));
  input.addEventListener(type === 'checkbox' || type === 'select' ? 'change' : 'input', () => {
    const next = type === 'checkbox' ? input.checked : type === 'number' && input.value !== '' ? input.valueAsNumber : input.value;
    if (secret) rememberSecret(next);
    onChange?.(next, input);
  });
  return {
    node, input,
    setError(message) {
      error.hidden = !message;
      setText(error, message || '');
      if (message) input.setAttribute('aria-invalid', 'true');
      else input.removeAttribute('aria-invalid');
    },
  };
}

export function readOnlyField(label, value, help = '') {
  return el('div', { class: 'field' }, el('span', { class: 'field-label', text: label }),
    el('div', { class: 'readonly-value', text: value || '未设置' }), help ? el('p', { class: 'help', text: help }) : null);
}

export function section(title, description, children, actions) {
  return el('section', { class: 'panel form-section' },
    el('div', { class: 'section-heading' }, el('div', {}, el('h2', { text: title }), description ? el('p', { class: 'help', text: description }) : null), actions),
    ...children);
}
