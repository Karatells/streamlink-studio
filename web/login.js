import { languages, defaultLanguage } from './languages.config.js';
import { registerSW } from './pwa.js';

const $ = (s) => document.querySelector(s);
let dict = {}, fallback = {};

async function loadLocale(code) {
  try { const r = await fetch(`locales/${code}.json`); return r.ok ? await r.json() : {}; } catch { return {}; }
}
function t(key, vars) {
  let s = dict[key] ?? fallback[key] ?? key;
  if (vars) s = s.replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m));
  return s;
}
function detectLang() {
  const saved = localStorage.getItem('lang');
  if (saved && languages.some((l) => l.code === saved)) return saved;
  const nav = (navigator.language || '').toLowerCase().split('-')[0];
  return languages.some((l) => l.code === nav) ? nav : defaultLanguage;
}
function translate() {
  document.querySelectorAll('[data-i18n]').forEach((el) => { el.textContent = t(el.dataset.i18n); });
  setEye(false);
  applyTheme();
  if (lastError) showError(lastError.key, lastError.vars); // keep the message in the new language
}
async function setLang(code) {
  localStorage.setItem('lang', code);
  document.documentElement.lang = code;
  dict = code === defaultLanguage ? fallback : await loadLocale(code);
  $('#lang').value = code;
  translate();
}

/* theme (same storage key as the main app) */
const THEMES = ['dark', 'light', 'auto'];
const ICON = { dark: '🌙', light: '☀️', auto: '🌓' };
function applyTheme() {
  const th = localStorage.getItem('theme') || 'dark';
  const dark = th === 'dark' || (th === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.toggle('dark', dark);
  $('#theme').textContent = ICON[th];
  $('#theme').title = t('theme_' + th);
  const mt = document.querySelector('meta[name=theme-color]');
  if (mt) mt.content = dark ? '#0f111a' : '#ffffff';
}
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', applyTheme);

/* show / hide password */
function setEye(show) {
  $('#p').type = show ? 'text' : 'password';
  $('#eye-on').hidden = show;
  $('#eye-off').hidden = !show;
  $('#eye').setAttribute('aria-pressed', String(show));
  $('#eye').setAttribute('aria-label', t(show ? 'login_hide' : 'login_show'));
}

/* errors */
let lastError = null;
function showError(key, vars) {
  lastError = { key, vars };
  const box = $('#err');
  box.textContent = t(key, vars);
  box.hidden = false;
  box.classList.remove('shake'); void box.offsetWidth; box.classList.add('shake');
}
function clearError() { lastError = null; $('#err').hidden = true; }

function busy(on) {
  $('#go').disabled = on;
  $('#go .spin').hidden = !on;
}

async function submit(e) {
  e.preventDefault();
  clearError();
  const username = $('#u').value.trim();
  const password = $('#p').value;
  if (!username || !password) { showError('login_empty'); (username ? $('#p') : $('#u')).focus(); return; }
  busy(true);
  try {
    const r = await fetch('api/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password, remember: $('#remember').checked }),
    });
    if (r.ok) { location.replace('./'); return; }
    const data = await r.json().catch(() => ({}));
    if (r.status === 429) showError('login_locked', { minutes: Math.max(1, Math.ceil((data.retry_after || 300) / 60)) });
    else if (r.status === 401) { showError('login_error'); $('#p').value = ''; $('#p').focus(); }
    else showError('login_network');
  } catch {
    showError('login_network');
  } finally {
    busy(false);
  }
}

(async function init() {
  // already signed in (or no password configured) -> straight to the panel
  try {
    const a = await (await fetch('api/auth/status', { cache: 'no-store' })).json();
    if (!a.auth_required || a.authenticated) { location.replace('./'); return; }
  } catch { /* show the form anyway */ }

  fallback = await loadLocale(defaultLanguage);
  const sel = $('#lang');
  sel.innerHTML = languages.map((l) => `<option value="${l.code}">${l.name}</option>`).join('');
  sel.onchange = () => setLang(sel.value);
  $('#theme').onclick = () => {
    const cur = localStorage.getItem('theme') || 'dark';
    localStorage.setItem('theme', THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length]);
    applyTheme();
  };
  $('#eye').onclick = () => setEye($('#p').type === 'password');
  $('#form').addEventListener('submit', submit);
  $('#u').addEventListener('input', clearError);
  $('#p').addEventListener('input', clearError);
  registerSW();
  await setLang(detectLang());
  document.body.classList.add('ready');
})();
