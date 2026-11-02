import { api, isQuietError } from '/assets/api.js';
import { ask, dismissDialog, banner, setText, toast } from '/assets/dom.js';
import { store } from '/assets/store.js';
import { Dashboard } from '/assets/dashboard.js';
import { Config } from '/assets/config.js';
import { Logs } from '/assets/logs.js';
import { Notifications } from '/assets/notifications.js';

const dashboard = new Dashboard(document.getElementById('dashboard-content'));
const logs = new Logs(document.getElementById('logs-content'));
const notifications = new Notifications(document.getElementById('notifications-content'));
const config = new Config(document.getElementById('config-content'), document.getElementById('items-config-content'), { onApplied: snapshot => {
  if (snapshot?.items) dashboard.render(snapshot);
  dashboard.refresh();
} });
const login = document.getElementById('login-dialog');
const loginForm = document.getElementById('login-form');
const loginInput = document.getElementById('login-token');
const loginError = document.getElementById('login-error');
const loginCancel = document.getElementById('login-cancel');
const globalBanner = document.getElementById('global-banner');
const routes = new Set(['dashboard', 'items', 'config', 'logs', 'notifications']);
let route = '';
let navigating = false;
let loginRequired = false;
let loggingIn = false;

function stopLive() {
  dashboard.stop(); logs.feed.stop(); notifications.feed.stop();
}

function startLive() {
  if (api.locked) return;
  dashboard.start(); logs.feed.start(); notifications.feed.start();
  if ((route === 'config' || route === 'items') && !config.draft) config.load();
  document.getElementById('logout').hidden = !api.token;
}

function openLogin(required = false) {
  if (required) { loginRequired = true; stopLive(); }
  dismissDialog();
  loginCancel.hidden = loginRequired;
  if (!login.open) {
    loginInput.value = '';
    loginError.hidden = true;
    login.showModal();
    loginInput.focus();
  }
}

api.addEventListener('unauthorized', () => {
  store.set({ connected: false });
  document.getElementById('logout').hidden = true;
  setText(document.getElementById('ready-badge'), '需要登录');
  document.getElementById('ready-badge').className = 'badge warning';
  openLogin(true);
  loginError.hidden = false;
  setText(loginError, '服务需要有效的访问令牌，请重新连接。');
});

async function connect(anonymous = false) {
  if (loggingIn) return;
  const token = anonymous ? '' : loginInput.value.trim();
  if (!anonymous && !token) {
    loginError.hidden = false; setText(loginError, '请输入令牌，或选择无令牌连接。'); loginInput.focus(); return;
  }
  if (/[\s\x00-\x1f\x7f]/.test(token)) {
    loginError.hidden = false; setText(loginError, '令牌不能包含空白或控制字符。'); return;
  }
  loggingIn = true;
  loginRequired = true;
  loginCancel.hidden = true;
  stopLive();
  loginInput.value = '';
  loginError.hidden = true;
  for (const control of loginForm.elements) control.disabled = true;
  try {
    api.setToken(token);
    const snapshot = await api.json('/api/v1/status');
    dashboard.render(snapshot);
    loginRequired = false;
    login.close();
    banner(globalBanner, '');
    startLive();
  } catch (error) {
    loginError.hidden = false;
    setText(loginError, error.status === 401 ? '令牌无效或已过期，请重试。' : '连接失败。请检查服务连接与浏览器会话存储后重试。');
  } finally {
    loggingIn = false;
    for (const control of loginForm.elements) control.disabled = false;
    if (login.open) loginInput.focus();
  }
}

loginForm.addEventListener('submit', event => { event.preventDefault(); connect(); });
document.getElementById('login-anonymous').addEventListener('click', () => connect(true));
loginCancel.addEventListener('click', () => { if (!loginRequired) { loginInput.value = ''; login.close(); } });
login.addEventListener('cancel', event => {
  if (loginRequired || loggingIn) event.preventDefault();
  else loginInput.value = '';
});
document.getElementById('token-button').addEventListener('click', () => openLogin());
document.getElementById('logout').addEventListener('click', async () => {
  if (config.dirty && !await ask({ title: '退出当前会话？', message: '退出会清除令牌与当前未保存的配置编辑。', confirm: '退出并丢弃编辑', danger: true })) return;
  stopLive();
  api.setToken('');
  config.reset(); logs.reset(); notifications.reset(); dashboard.reset();
  document.getElementById('toasts').replaceChildren();
  api.lock();
});

async function navigate() {
  const requested = location.hash.replace(/^#\/?/, '');
  const next = routes.has(requested) ? requested : 'dashboard';
  if (navigating) { history.replaceState(null, '', `#/${route || 'dashboard'}`); return; }
  if (next === route) { history.replaceState(null, '', `#/${route}`); return; }
  navigating = true;
  if ((route === 'config' || route === 'items') && config.dirty) {
    history.replaceState(null, '', `#/${route}`);
    const leave = await ask({ title: '配置尚未保存', message: '离开此页后，本次编辑仍会保留。关闭或刷新浏览器前，请先保存配置。', confirm: '保留编辑并离开' });
    if (!leave) { navigating = false; return; }
  }
  const initial = !route;
  route = next;
  history.replaceState(null, '', `#/${route}`);
  for (const name of routes) document.getElementById(`view-${name}`).hidden = name !== route;
  for (const link of document.querySelectorAll('[data-route]')) {
    if (link.dataset.route === route) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
  store.set({ route });
  document.title = `ros-ddns · ${{ dashboard: '运行概览', items: '同步项目', config: '配置管理', logs: '实时日志', notifications: '事件通知' }[route]}`;
  if ((route === 'config' || route === 'items') && !config.draft && !api.locked) config.load();
  if (route === 'logs') logs.render();
  if (!initial) {
    window.scrollTo({ top: 0, behavior: 'instant' });
    document.getElementById(`title-${route}`).focus({ preventScroll: true });
  }
  navigating = false;
}

window.addEventListener('hashchange', navigate);
window.addEventListener('beforeunload', event => { if (config.dirty) { event.preventDefault(); event.returnValue = ''; } });
window.addEventListener('pagehide', stopLive);
window.addEventListener('pageshow', event => { if (event.persisted && !loginRequired) startLive(); });
document.addEventListener('visibilitychange', () => dashboard.visibilityChanged());
window.addEventListener('online', () => { if (!loginRequired) dashboard.cycle(); });
document.querySelector('.skip-link').addEventListener('click', event => { event.preventDefault(); document.getElementById('main').focus(); });

document.getElementById('refresh').addEventListener('click', async event => {
  if (api.locked) { openLogin(true); return; }
  const control = event.currentTarget;
  control.disabled = true;
  try {
    if (route === 'config' || route === 'items') await config.load();
    else if (route === 'logs') await logs.feed.refresh();
    else if (route === 'notifications') await notifications.feed.refresh();
    else await dashboard.refresh();
  } catch (error) { if (!isQuietError(error)) toast(error.message, 'error'); }
  finally { control.disabled = false; }
});


// Anonymous access is attempted first when sessionStorage has no token. A
// dialog is opened only by an actual 401 or an explicit user action.
navigate();
startLive();
