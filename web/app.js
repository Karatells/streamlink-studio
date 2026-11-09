import { languages, defaultLanguage } from './languages.config.js';

/* ------------------------------------------------------------ helpers */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const enc = encodeURIComponent;

function fmtSize(b) {
  if (b >= 1e9) return (b / 1e9).toFixed(2) + ' GB';
  if (b >= 1e6) return (b / 1e6).toFixed(1) + ' MB';
  if (b >= 1e3) return (b / 1e3).toFixed(0) + ' KB';
  return b + ' B';
}
function fmtDur(sec) {
  sec = Math.max(0, Math.floor(sec));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return [h, m, s].map((n) => String(n).padStart(2, '0')).join(':');
}
function fmtTime(sec) { // HH:MM:SS.mmm
  sec = Math.max(0, sec || 0);
  const ms = Math.round((sec % 1) * 1000);
  const whole = Math.floor(sec) + (ms === 1000 ? 1 : 0);
  return fmtDur(whole) + '.' + String(ms === 1000 ? 0 : ms).padStart(3, '0');
}
function parseTime(str) { // "HH:MM:SS.ms" | "MM:SS" | "SS.ms"
  const parts = String(str).trim().split(':').map(Number);
  if (!parts.length || parts.some((n) => Number.isNaN(n))) return NaN;
  return parts.reduce((acc, n) => acc * 60 + n, 0);
}
function ymd(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

async function api(method, path, body, raw) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.body = raw ? body : JSON.stringify(body);
    opt.headers['Content-Type'] = raw ? 'text/plain' : 'application/json';
  }
  const r = await fetch('api' + path, opt);
  if (r.status === 401) { location.href = 'login.html'; throw new Error('unauthorized'); }
  if (r.status === 204) return null;
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

function toast(msg, isErr, href) {
  const el = document.createElement('div');
  el.className = 'toast' + (isErr ? ' err' : '') + (href ? ' link' : '');
  el.textContent = msg;
  if (href) el.onclick = () => { location.hash = href; el.remove(); };
  $('#toasts').append(el);
  setTimeout(() => el.remove(), isErr ? 7000 : 3500);
}

/* Platform badges. Official logos are trademarked, so the default is a neutral monogram;
   a file <data>/icons/<platform>.svg|png|webp (e.g. twitch.svg) replaces it automatically. */
const PLATFORM_HUE = { twitch: 265, youtube: 2, kick: 108, niconico: 30, tiktok: 335, facebook: 215, vimeo: 195, dailymotion: 220, trovo: 150, bilibili: 200 };
const PLATFORM_MONO = { twitch: 'Tw', youtube: 'YT', kick: 'K', niconico: 'Nn', tiktok: 'TT', facebook: 'Fb' };
function platformBadge(name, big) {
  const n = String(name || '').toLowerCase();
  if (!n) return '';
  const file = state.icons?.[n];
  if (file) return `<img class="pico${big ? ' lg' : ''}" src="icons/${encodeURIComponent(file)}" alt="${esc(n)}" title="${esc(n)}">`;
  let hue = PLATFORM_HUE[n];
  if (hue === undefined) { hue = 0; for (const ch of n) hue = (hue * 31 + ch.charCodeAt(0)) % 360; }
  const mono = PLATFORM_MONO[n] || (n.charAt(0).toUpperCase() + n.charAt(1));
  return `<span class="pbadge${big ? ' lg' : ''}" style="--h:${hue}" title="${esc(n)}" aria-label="${esc(n)}">${esc(mono)}</span>`;
}
const guard = (fn) => async (...a) => { try { return await fn(...a); } catch (e) { toast(e.message, true); } };

/* --------------------------------------------------------------- i18n */
let dict = {}, fallback = {}, lang = defaultLanguage;
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
async function setLang(code) {
  lang = code;
  localStorage.setItem('lang', code);
  document.documentElement.lang = code;
  dict = code === defaultLanguage ? fallback : await loadLocale(code);
  $('#lang').value = code;
  renderShell();
  route();
}

/* -------------------------------------------------------------- theme */
const THEMES = ['dark', 'light', 'auto'];
const THEME_ICON = { dark: '🌙', light: '☀️', auto: '🌓' };
function applyTheme() {
  const th = localStorage.getItem('theme') || 'dark';
  const dark = th === 'dark' || (th === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.toggle('dark', dark);
  $('#theme').textContent = THEME_ICON[th];
  $('#theme').title = t('theme_' + th);
  $('#logout').title = t('logout');
  $('#logout').setAttribute('aria-label', t('logout'));
  const mt = document.querySelector('meta[name=theme-color]');
  if (mt) mt.content = dark ? '#0f111a' : '#ffffff';
}
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', applyTheme);

/* -------------------------------------------------------------- state */
let state = { channels: [], active: [], jobs: [], counts: { channels: 0, recordings: 0, saved: 0 }, update: { available: false } };
let settings = {};
let recs = [];
let current = null; // current view object

function renderShell() {
  const names = {
    channels: [t('nav_channels'), state.counts.channels],
    recordings: [t('nav_recordings'), state.counts.recordings],
    saved: [t('nav_saved'), state.counts.saved || 0],
    settings: [t('nav_settings'), null],
  };
  const upd = state.update?.available;
  $$('#nav a').forEach((a) => {
    const [label, count] = names[a.dataset.route];
    // label and count are separate spans: on narrow screens the count always drops to its own line
    a.innerHTML = `<span class="nl">${esc(label)}</span>${count === null ? '' : ` <span class="nc">(${count})</span>`}`;
    if (a.dataset.route === 'settings' && upd) a.insertAdjacentHTML('beforeend', '<i class="dot"></i>');
  });
  const unread = state.notifications?.unread || 0;
  const badge = $('#bell-badge');
  if (badge) { badge.hidden = !unread; badge.textContent = unread > 99 ? '99+' : String(unread); }
  const bell = $('#bell');
  if (bell) { bell.title = t('nav_notifications'); bell.setAttribute('aria-label', t('nav_notifications')); bell.classList.toggle('active', location.hash.startsWith('#/notifications')); }
  const bn = $('#update-banner');
  if (bn) {
    bn.hidden = !upd || /^#\/settings\/updates/.test(location.hash);
    if (!bn.hidden) bn.innerHTML = `<span>⬆ ${esc(t('upd_banner', { version: state.update.version }))}</span> <a class="btn sm primary" href="#/settings/updates">${esc(t('upd_banner_view'))}</a>`;
  }
  applyTheme();
}

/* toasts for events that happened while the page was open */
let seenNote = null;
async function announceNew() {
  const last = state.notifications?.last_id ?? 0;
  if (seenNote === null) { seenNote = last; return; }
  if (last <= seenNote) return;
  const from = seenNote;
  seenNote = last;
  if (location.hash.startsWith('#/notifications')) return;
  try {
    const d = await api('GET', '/notifications');
    d.items.filter((n) => n.id > from && n.type !== 'job_started').slice(0, 3).reverse()
      .forEach((n) => toast(notifIcon(n) + ' ' + notifText(n), n.level === 'error', notifLink(n)));
  } catch { /* ignore */ }
}

async function refresh() {
  if (document.hidden) return;
  try {
    state = await api('GET', '/state');
    renderShell();
    announceNew();
    if (current?.update) await current.update();
  } catch { /* server temporarily unreachable */ }
}

/* -------------------------------------------------------------- modal */
let modalCleanup = null;
function openModal({ title, body, footer, wide, onClose }) {
  closeModal();
  const root = $('#modal-root');
  root.innerHTML = `<div class="overlay"><div class="modal${wide ? ' wide' : ''}" role="dialog" aria-modal="true">
    <header><span>${esc(title)}</span><button class="btn sm" data-close>✕</button></header>
    <div class="body">${body}</div><footer>${footer || ''}</footer></div></div>`;
  const overlay = $('.overlay', root);
  let downOnOverlay = false;
  overlay.addEventListener('mousedown', (e) => (downOnOverlay = e.target === overlay));
  overlay.addEventListener('click', (e) => { if (e.target === overlay && downOnOverlay) closeModal(); });
  $$('[data-close]', root).forEach((b) => b.addEventListener('click', closeModal));
  modalCleanup = onClose || null;
  return $('.modal', root);
}
function closeModal() {
  if (modalCleanup) { try { modalCleanup(); } catch { /* ignore */ } modalCleanup = null; }
  $('#modal-root').innerHTML = '';
}
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

/* ------------------------------------------------------ channels view */
function statusBadge(ch) {
  return `<span class="badge ${esc(ch.status)}">${esc(t('status_' + ch.status))}</span>`;
}
function authLine(ch) {
  if (ch.plugin === 'twitch') {
    const has = ch.auth_mode === 'custom' ? ch.has_token : settings.has_twitch_token;
    return `${t('token_label')}: ${has ? '🟢 ' + t('token_set') : '⚪ ' + t('token_none')}`;
  }
  const has = ch.has_cookies || settings.has_cookies;
  return `${t('cookie_label')}: ${has ? '🟢 ' + t('cookie_loaded') : '⚪ ' + t('cookie_none')}`;
}

function channelsView() {
  const el = $('#view');
  el.innerHTML = `<div id="cv">
    <div class="row"><button class="btn primary" id="add">${esc(t('add_channel'))}</button></div>
    <h2>${esc(t('active_recordings'))}</h2><div class="card" id="active"></div>
    <h2>${esc(t('channel_list'))}</h2><div class="grid" id="list"></div></div>`;
  $('#add').onclick = () => channelModal(null);
  $('#cv').addEventListener('click', guard(async (e) => {
    const b = e.target.closest('[data-act]');
    if (!b) return;
    const id = b.dataset.id;
    const ch = state.channels.find((c) => c.id === id);
    switch (b.dataset.act) {
      case 'edit': channelModal(ch); break;
      case 'stop': await api('POST', `/channels/${id}/stop`); refresh(); break;
      case 'record': await api('POST', `/channels/${id}/record`); refresh(); break;
      case 'check': await api('POST', `/channels/${id}/check`); toast(t('check_now')); break;
    }
  }));
  const update = () => {
    const act = state.active;
    $('#active').innerHTML = act.length
      ? act.map((c) => `<div class="active-row">
          <span class="badge recording">LIVE</span>${platformBadge(c.plugin)}<b>${esc(c.display_name)}</b>
          <span class="muted">${esc(c.recording_quality || c.quality || settings.default_quality || 'best')}</span>
          ${c.auth_active ? `<span class="badge ok">🔑 ${esc(t('auth_active'))}</span>` : ''}
          <span class="muted small">${esc(t('recording_for'))} ${fmtDur((Date.now() - new Date(c.recording_since)) / 1000)}</span>
          <span class="grow"></span>
          <button class="btn sm danger" data-act="stop" data-id="${c.id}">■ ${esc(t('stop'))}</button></div>`).join('')
      : `<div class="muted">${esc(t('no_active'))}</div>`;
    $('#list').innerHTML = state.channels.length
      ? state.channels.map((c) => `<div class="card ch">
          <div class="head">${platformBadge(c.plugin, true) || `<div class="avatar">${esc(c.display_name.slice(0, 1))}</div>`}
            <div style="min-width:0;flex:1"><div class="name" title="${esc(c.url)}">${esc(c.display_name)}</div>
            <div class="muted small">${esc(c.plugin)}</div></div>${statusBadge(c)}</div>
          <div class="kv"><span>${esc(t('auto_rec'))}: <b>${c.auto_record ? t('on') : t('off')}</b></span><span>${esc(authLine(c))}</span></div>
          ${c.title ? `<div class="small muted" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(c.title)}">${esc(c.title)}</div>` : ''}
          ${c.error ? `<div class="errtxt">${esc(c.error)}</div>` : ''}
          <div class="muted small">${esc(t('last_check'))}: ${c.last_check ? new Date(c.last_check).toLocaleTimeString() : esc(t('never'))}</div>
          <div class="row" style="margin-top:auto">
            <button class="btn sm" data-act="edit" data-id="${c.id}">⚙ ${esc(t('settings_btn'))}</button>
            <a class="btn sm" href="${esc(c.url)}" target="_blank" rel="noopener noreferrer">${esc(t('watch_stream'))}</a>
            ${c.status === 'recording' ? `<button class="btn sm danger" data-act="stop" data-id="${c.id}">■ ${esc(t('stop'))}</button>`
              : c.status === 'live' ? `<button class="btn sm primary" data-act="record" data-id="${c.id}">● ${esc(t('record_now'))}</button>`
              : `<button class="btn sm" data-act="check" data-id="${c.id}" title="${esc(t('check_now'))}">↻</button>`}
          </div></div>`).join('')
      : `<div class="empty card" style="grid-column:1/-1">${esc(t('no_channels'))}</div>`;
  };
  return { update };
}

function channelModal(ch) {
  const isNew = !ch;
  ch = ch || { url: '', name: '', quality: '', auto_record: true, check_interval: 0, filename_template: '', auth_mode: 'global',
    disable_ads: true, low_latency: false, proxy: '', headers: '', email: '', extra_args: '' };
  let cookiesAction = null; // null | {text} | 'remove'
  const body = `
    <div class="field"><label>${esc(t('ch_url'))}</label><input type="text" id="f-url" value="${esc(ch.url)}" placeholder="https://twitch.tv/streamer"></div>
    <div class="two">
      <div class="field"><label>${esc(t('ch_name'))}</label><input type="text" id="f-name" value="${esc(ch.name)}" placeholder="${esc(t('ch_name_hint'))}"></div>
      <div class="field"><label>${esc(t('ch_quality'))}</label><input type="text" id="f-quality" value="${esc(ch.quality)}" placeholder="${esc(settings.default_quality || 'best')}" title="${esc(t('ch_quality_hint'))}"></div>
    </div>
    <div class="muted small" style="margin-top:-6px">${esc(t('ch_quality_hint'))}</div>
    <label class="check"><input type="checkbox" id="f-auto" ${ch.auto_record ? 'checked' : ''}> ${esc(t('ch_auto'))}</label>
    <fieldset id="twitch-box"><legend>${esc(t('ch_auth'))} (Twitch)</legend>
      <label class="check"><input type="radio" name="authmode" value="global" ${ch.auth_mode !== 'custom' ? 'checked' : ''}> ${esc(t('auth_global'))}</label>
      <label class="check"><input type="radio" name="authmode" value="custom" ${ch.auth_mode === 'custom' ? 'checked' : ''}> ${esc(t('auth_custom'))}</label>
      <div class="field" id="token-box"><label>${esc(t('oauth_token'))}</label>
        <input type="password" id="f-token" autocomplete="off" placeholder="${ch.has_token ? esc(t('token_keep')) : ''}"></div>
      <label class="check"><input type="checkbox" id="f-ads" ${ch.disable_ads ? 'checked' : ''}> ${esc(t('disable_ads'))}</label>
      <label class="check"><input type="checkbox" id="f-ll" ${ch.low_latency ? 'checked' : ''}> ${esc(t('low_latency'))}</label>
    </fieldset>
    <fieldset><legend>${esc(t('cookies_file'))}</legend>
      <div class="row"><span id="cookie-state" class="badge"></span>
        <button class="btn sm" type="button" id="cookie-up">${esc(t('upload_cookies'))}</button>
        <button class="btn sm danger" type="button" id="cookie-rm">${esc(t('remove_cookies'))}</button>
        <input type="file" id="cookie-file" accept=".txt,text/plain" hidden></div>
      <div class="muted small">${esc(t('cookies_hint'))}</div>
    </fieldset>
    <details><summary>${esc(t('advanced'))}</summary><div class="body" style="padding:0;gap:12px">
      <div class="two">
        <div class="field"><label>${esc(t('ch_interval'))}</label><input type="number" id="f-interval" min="0" value="${ch.check_interval || 0}" title="${esc(t('ch_interval_hint'))}"></div>
        <div class="field"><label>${esc(t('proxy'))}</label><input type="text" id="f-proxy" value="${esc(ch.proxy)}" placeholder="http://user:pass@host:port"></div>
      </div>
      <div class="field"><label>${esc(t('ch_template'))}</label><input type="text" id="f-tpl" value="${esc(ch.filename_template)}" placeholder="${esc(settings.default_template || '')}"><span class="muted small">${esc(t('ch_template_hint'))}</span></div>
      <div class="field"><label>${esc(t('headers'))} (${esc(t('headers_hint'))})</label><textarea id="f-headers">${esc(ch.headers)}</textarea></div>
      <div class="two">
        <div class="field"><label>${esc(t('email'))}</label><input type="text" id="f-email" value="${esc(ch.email)}" autocomplete="off"></div>
        <div class="field"><label>${esc(t('password'))}</label><input type="password" id="f-pass" autocomplete="new-password" placeholder="${ch.has_password ? esc(t('token_keep')) : ''}"></div>
      </div>
      <div class="field"><label>${esc(t('extra_args'))}</label><input type="text" id="f-extra" value="${esc(ch.extra_args)}" placeholder="--stream-segment-threads 2"></div>
    </div></details>
    <div id="testres" class="result" hidden></div>`;
  const footer = `${isNew ? '' : `<button class="btn danger" id="del" style="margin-right:auto">${esc(t('delete'))}</button>`}
    <button class="btn" id="test">${esc(t('test_stream'))}</button><button class="btn primary" id="save">${esc(t('save'))}</button>`;
  const m = openModal({ title: isNew ? t('add_channel').replace(/^\+\s*/, '') : `${t('edit_channel')}: ${ch.display_name}`, body, footer });

  const val = (id) => $(id, m).value;
  const platform = () => {
    const u = val('#f-url').toLowerCase();
    if (/(^|[/.])twitch\.tv/.test(u)) return 'twitch';
    return 'other';
  };
  const syncUI = () => {
    $('#twitch-box', m).hidden = platform() !== 'twitch';
    $('#token-box', m).hidden = (m.querySelector('input[name=authmode]:checked')?.value) !== 'custom';
    const has = cookiesAction === 'remove' ? false : cookiesAction ? true : ch.has_cookies;
    const cs = $('#cookie-state', m);
    cs.textContent = has ? '🟢 ' + t('cookie_loaded') : '⚪ ' + t('cookie_none');
    $('#cookie-rm', m).hidden = !has;
  };
  syncUI();
  $('#f-url', m).addEventListener('input', syncUI);
  $$('input[name=authmode]', m).forEach((r) => r.addEventListener('change', syncUI));
  $('#cookie-up', m).onclick = () => $('#cookie-file', m).click();
  $('#cookie-rm', m).onclick = () => { cookiesAction = 'remove'; syncUI(); };
  $('#cookie-file', m).onchange = async (e) => {
    const f = e.target.files[0];
    if (f) { cookiesAction = { text: await f.text() }; syncUI(); }
  };

  const collect = () => {
    const b = {
      url: val('#f-url'), name: val('#f-name'), quality: val('#f-quality'),
      auto_record: $('#f-auto', m).checked,
      auth_mode: m.querySelector('input[name=authmode]:checked').value,
      disable_ads: $('#f-ads', m).checked, low_latency: $('#f-ll', m).checked,
      check_interval: parseInt(val('#f-interval'), 10) || 0, proxy: val('#f-proxy'),
      filename_template: val('#f-tpl'), headers: val('#f-headers'), email: val('#f-email'), extra_args: val('#f-extra'),
    };
    if (val('#f-token')) b.oauth_token = val('#f-token'); // secrets only when changed
    if (val('#f-pass')) b.password = val('#f-pass');
    return b;
  };

  $('#test', m).onclick = guard(async () => {
    const box = $('#testres', m);
    box.hidden = false; box.textContent = t('testing');
    const body = collect();
    if (!isNew) body.id = ch.id;
    const r = await api('POST', '/channels/test', body);
    if (r.live) box.textContent = '🟢 ' + t('test_live', { streams: r.streams.join(', ') || 'best' }) + (r.title ? ` — ${r.title}` : '') + (r.plugin ? ` [${r.plugin}]` : '')
      + (r.resolved ? '\n' + t('test_resolved', { resolved: r.resolved, requested: r.requested }) : '');
    else if (r.auth_error) box.textContent = '🔴 ' + t('test_auth_error', { error: r.error });
    else if (r.error) box.textContent = '🔴 ' + t('test_error', { error: r.error });
    else box.textContent = '⚪ ' + t('test_offline');
  });
  $('#save', m).onclick = guard(async () => {
    const saved = isNew ? await api('POST', '/channels', collect()) : await api('PUT', `/channels/${ch.id}`, collect());
    if (cookiesAction === 'remove') { if (!isNew && ch.has_cookies) await api('DELETE', `/channels/${saved.id}/cookies`); }
    else if (cookiesAction) await api('PUT', `/channels/${saved.id}/cookies`, cookiesAction.text, true);
    closeModal(); toast(t('saved')); refresh();
  });
  if (!isNew) $('#del', m).onclick = guard(async () => {
    if (!confirm(t('confirm_delete_channel', { name: ch.display_name }))) return;
    await api('DELETE', `/channels/${ch.id}`);
    closeModal(); toast(t('deleted')); refresh();
  });
}

/* ---------------------------------------------------- recordings view */
const NATIVE = ['mp4', 'm4v', 'webm', 'mov']; // containers browsers can play natively

const filters = {
  all: { q: '', streamer: '', from: '', to: '', sort: 'date_desc' },
  saved: { q: '', streamer: '', from: '', to: '', sort: 'date_desc' },
};

function dayLabel(key) { // key = yyyy-mm-dd
  const [y, m, d] = key.split('-').map(Number);
  const date = new Date(y, m - 1, d);
  const now = new Date();
  if (key === ymd(now)) return t('rec_today');
  const yest = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
  if (key === ymd(yest)) return t('rec_yesterday');
  const opt = { weekday: 'long', day: 'numeric', month: 'long' };
  if (y !== now.getFullYear()) opt.year = 'numeric';
  const s = date.toLocaleDateString(lang, opt);
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function recTabs(active) {
  const running = state.jobs.filter((j) => j.status === 'running').length;
  return `<div class="tabs" role="tablist">
    <a role="tab" class="tab${active === 'list' ? ' active' : ''}" href="#/recordings">${esc(t('rec_tab_list'))}</a>
    <a role="tab" class="tab${active === 'jobs' ? ' active' : ''}" href="#/recordings/jobs">${esc(t('rec_tab_jobs'))}<span class="tab-badge" id="jt-badge" ${running ? '' : 'hidden'}>${running}</span></a></div>`;
}

const fmtJobTime = (d) => new Date(d).toLocaleString([], { hour: '2-digit', minute: '2-digit', day: '2-digit', month: '2-digit' });

function jobsView() {
  $('#view').innerHTML = `${recTabs('jobs')}<div id="jobs-wrap"></div>`;
  let sig = '';
  const update = () => {
    const s = JSON.stringify(state.jobs);
    if (s === sig) return;
    sig = s;
    const jobs = state.jobs;
    $('#jobs-wrap').innerHTML = jobs.length ? jobs.map((j) => {
      const pct = j.progress >= 0 ? Math.round(j.progress * 100) : null;
      const label = j.status === 'running' ? t('job_running') : j.status === 'done' ? t('job_done') : t('job_failed');
      return `<div class="card job ${j.status}">
        <div class="job-head"><b>${esc(t('job_' + j.type))}</b><span class="badge ${j.status === 'done' ? 'ok' : j.status === 'error' ? 'error' : ''}">${esc(label)}</span>
          <span class="grow"></span><span class="muted small">${esc(fmtJobTime(j.started))}</span></div>
        <div class="job-files"><div class="muted small">${esc(t('job_from'))}</div><div class="fname">${esc(j.input)}</div>
          <div class="muted small">${esc(t('job_to'))}</div><div class="fname">${esc(j.output)}</div></div>
        ${j.status === 'running' ? `<div class="row"><div class="progress ${pct === null ? 'indet' : ''}" style="flex:1"><i style="width:${pct ?? 35}%"></i></div><span class="small">${pct === null ? '' : pct + '%'}</span></div>` : ''}
        ${j.error ? `<div class="errtxt">${esc(j.error)}</div>` : ''}</div>`;
    }).join('') : `<div class="card"><div class="empty">${esc(t('jobs_none'))}</div></div>`;
  };
  update();
  return { update };
}

function recordingsView(savedMode, sub) {
  if (!savedMode && sub === 'jobs') return jobsView();
  const filter = savedMode ? filters.saved : filters.all;
  const sel = new Set();          // selected file names
  const collapsed = new Set();    // collapsed group keys
  let visible = [];               // names in display order (for shift-click ranges)
  let lastIdx = -1;
  let lastSig = '';
  $('#view').innerHTML = `${savedMode ? '' : recTabs('list')}
    <div class="row filters" style="margin-bottom:8px">
      <input type="search" id="q" class="grow" placeholder="${esc(t('rec_search'))}" value="${esc(filter.q)}">
      <select id="st"></select>
      <select id="so">${['date_desc', 'date_asc', 'name', 'streamer', 'size'].map((k) => `<option value="${k}" ${filter.sort === k ? 'selected' : ''}>${esc(t('sort_' + k))}</option>`).join('')}</select>
      <label class="row small muted">${esc(t('rec_from'))} <input type="date" id="df" value="${filter.from}" style="width:auto"></label>
      <label class="row small muted">${esc(t('rec_to'))} <input type="date" id="dt" value="${filter.to}" style="width:auto"></label>
    </div>
    <div id="chips" class="chips"></div>
    <div id="tbl"></div>
    <div class="muted small" id="sum" style="margin-top:8px"></div>
    <div id="bulk" class="bulkbar" hidden></div>`;

  const byName = () => new Map(recs.map((r) => [r.name, r]));
  const selectable = (r) => !r.recording;
  const selRecs = () => { const m = byName(); return [...sel].map((n) => m.get(n)).filter(Boolean); };

  const filtered = () => {
    const q = filter.q.toLowerCase();
    const rows = recs.filter((r) =>
      (!q || r.name.toLowerCase().includes(q) || r.streamer.toLowerCase().includes(q)) && (!filter.streamer || r.streamer === filter.streamer) &&
      (!filter.from || ymd(new Date(r.started)) >= filter.from) && (!filter.to || ymd(new Date(r.started)) <= filter.to));
    const cmp = {
      date_desc: (a, b) => new Date(b.started) - new Date(a.started),
      date_asc: (a, b) => new Date(a.started) - new Date(b.started),
      name: (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }),
      streamer: (a, b) => a.streamer.localeCompare(b.streamer) || new Date(b.started) - new Date(a.started),
      size: (a, b) => b.size - a.size,
    }[filter.sort] || (() => 0);
    return rows.sort(cmp);
  };

  // date sorts -> one group per day, "streamer" sort -> one group per streamer, others -> flat
  const groupsOf = (rows) => {
    if (filter.sort !== 'date_desc' && filter.sort !== 'date_asc' && filter.sort !== 'streamer') return [{ key: '', label: '', rows }];
    const map = new Map();
    for (const r of rows) {
      const key = filter.sort === 'streamer' ? r.streamer : ymd(new Date(r.started));
      if (!map.has(key)) map.set(key, { key, label: filter.sort === 'streamer' ? r.streamer : dayLabel(key), rows: [] });
      map.get(key).rows.push(r);
    }
    return [...map.values()];
  };

  const rowHtml = (r) => `<tr class="${sel.has(r.name) ? 'selected' : ''}">
    <td class="sel"><input type="checkbox" data-sel="${esc(r.name)}" aria-label="${esc(t('rec_select'))}" ${sel.has(r.name) ? 'checked' : ''} ${selectable(r) ? '' : 'disabled'}></td>
    <td class="name">${esc(r.name)} ${r.recording ? `<span class="badge recording">${esc(t('rec_recording_now'))}</span>` : ''}</td>
    <td data-label="${esc(t('col_streamer'))}"><button class="nick${filter.streamer === r.streamer ? ' on' : ''}" data-a="streamer" data-n="${esc(r.streamer)}" title="${esc(t('rec_filter_streamer'))}">${esc(r.streamer)}</button>${r.platform ? ` ${platformBadge(r.platform)}` : ''}</td>
    <td class="num" data-label="${esc(t('col_size'))}">${fmtSize(r.size)}</td>
    <td class="num" data-label="${esc(t('col_date'))}">${new Date(r.started).toLocaleString()}</td>
    <td><div class="actions">
      <button class="btn sm" data-a="play" data-n="${esc(r.name)}">▶ ${esc(t('rec_play'))}</button>
      ${r.ext !== 'mp4' && !r.recording ? `<button class="btn sm" data-a="conv" data-n="${esc(r.name)}">${esc(t('rec_convert'))}</button>` : ''}
      ${r.recording ? '' : `<button class="btn sm" data-a="trim" data-n="${esc(r.name)}">✂ ${esc(t('rec_trim'))}</button>`}
      ${r.recording ? '' : `<button class="btn sm${r.saved ? ' on' : ''}" data-a="${r.saved ? 'unsave' : 'save'}" data-n="${esc(r.name)}">${r.saved ? '★ ' + esc(t('rec_unsave')) : '☆ ' + esc(t('rec_save'))}</button>`}
      <a class="btn sm" href="api/recordings/${enc(r.name)}/download" download>⬇</a>
      ${r.recording ? '' : `<button class="btn sm danger" data-a="del" data-n="${esc(r.name)}">🗑</button>`}
    </div></td></tr>`;

  const render = () => {
    const rows = filtered();
    visible = [];
    const groups = groupsOf(rows);
    $('#tbl').innerHTML = rows.length ? groups.map((g) => {
      const open = !collapsed.has(g.key);
      if (open) g.rows.forEach((r) => visible.push(r.name));
      const sels = g.rows.filter(selectable);
      const nSel = sels.filter((r) => sel.has(r.name)).length;
      const head = g.label ? `<div class="grp-head" data-grp="${esc(g.key)}">
        <input type="checkbox" data-grpsel="${esc(g.key)}" aria-label="${esc(t('rec_select_group'))}" ${sels.length ? '' : 'disabled'} ${nSel && nSel === sels.length ? 'checked' : ''}>
        <button class="grp-title" data-a="toggle" data-g="${esc(g.key)}" aria-expanded="${open}"><span class="chev">${open ? '▾' : '▸'}</span> <b>${esc(g.label)}</b></button>
        <span class="muted small">${esc(t('rec_group_info', { count: g.rows.length, size: fmtSize(g.rows.reduce((a, r) => a + r.size, 0)) }))}</span>
        ${nSel ? `<span class="badge">${esc(t('rec_n_selected', { count: nSel }))}</span>` : ''}</div>` : '';
      const table = open ? `<div class="card tblcard" style="padding:4px 8px"><table><thead><tr><th class="sel"></th><th>${esc(t('col_name'))}</th><th>${esc(t('col_streamer'))}</th>
        <th class="num">${esc(t('col_size'))}</th><th class="num">${esc(t('col_date'))}</th><th></th></tr></thead><tbody>${g.rows.map(rowHtml).join('')}</tbody></table></div>` : '';
      return `<section class="grp">${head}${table}</section>`;
    }).join('') : `<div class="card"><div class="empty">${esc(t(savedMode ? 'rec_saved_none' : 'rec_none'))}</div></div>`;
    // indeterminate state of the group checkboxes
    $$('[data-grpsel]').forEach((cb) => {
      const g = groups.find((x) => x.key === cb.dataset.grpsel);
      const sels = g ? g.rows.filter(selectable) : [];
      const n = sels.filter((r) => sel.has(r.name)).length;
      cb.indeterminate = n > 0 && n < sels.length;
    });
    $('#sum').textContent = t('rec_total', { count: rows.length, size: fmtSize(rows.reduce((a, r) => a + r.size, 0)) });
    renderChips(); renderBulk();
  };

  const renderChips = () => {
    $('#chips').innerHTML = filter.streamer
      ? `<span class="chip">👤 ${esc(filter.streamer)} <button data-a="clear-streamer" aria-label="${esc(t('close'))}">✕</button></span>` : '';
  };

  const renderBulk = () => {
    const items = selRecs();
    const bar = $('#bulk');
    bar.hidden = items.length === 0;
    if (!items.length) { bar.innerHTML = ''; return; }
    const total = items.reduce((a, r) => a + r.size, 0);
    bar.innerHTML = `<span class="bulk-info"><b>${esc(t('rec_n_selected', { count: items.length }))}</b> · ${fmtSize(total)}</span>
      <span class="grow"></span>
      <button class="btn sm" data-b="selall">${esc(t('rec_select_all'))}</button>
      <button class="btn sm" data-b="merge" ${items.length < 2 ? 'disabled' : ''} title="${esc(items.length < 2 ? t('rec_merge_need_two') : '')}">⛓ ${esc(t('rec_merge'))}</button>
      <button class="btn sm" data-b="${savedMode ? 'unsave' : 'save'}">${savedMode ? '★ ' + esc(t('rec_unsave')) : '☆ ' + esc(t('rec_save'))}</button>
      <button class="btn sm danger" data-b="del">🗑 ${esc(t('delete'))}</button>
      <button class="btn sm" data-b="clear">✕ ${esc(t('rec_clear_sel'))}</button>`;
  };

  const renderStreamers = () => {
    const sel_ = $('#st');
    const names = [...new Set(recs.map((r) => r.streamer))].sort();
    if (filter.streamer && !names.includes(filter.streamer)) filter.streamer = '';
    const html = `<option value="">${esc(t('rec_all_streamers'))}</option>` + names.map((n) => `<option ${n === filter.streamer ? 'selected' : ''} value="${esc(n)}">${esc(n)}</option>`).join('');
    if (sel_.innerHTML !== html) sel_.innerHTML = html;
  };

  const renderJobsBadge = () => {
    const el = $('#jt-badge');
    if (!el) return;
    const running = state.jobs.filter((j) => j.status === 'running').length;
    el.hidden = !running; el.textContent = running;
  };

  const setStreamer = (name) => { filter.streamer = name; renderStreamers(); render(); };
  $('#q').oninput = (e) => { filter.q = e.target.value; render(); };
  $('#st').onchange = (e) => setStreamer(e.target.value);
  $('#so').onchange = (e) => { filter.sort = e.target.value; render(); };
  $('#df').onchange = (e) => { filter.from = e.target.value; render(); };
  $('#dt').onchange = (e) => { filter.to = e.target.value; render(); };
  $('#chips').addEventListener('click', (e) => { if (e.target.closest('[data-a=clear-streamer]')) setStreamer(''); });

  // selection: single, shift-range and whole group
  let shift = false; // the change event carries no modifier keys: take them from the click before it
  $('#tbl').addEventListener('click', (e) => { shift = !!e.shiftKey; }, true);
  $('#tbl').addEventListener('change', (e) => {
    const cb = e.target;
    if (cb.dataset.sel !== undefined) {
      const name = cb.dataset.sel;
      const idx = visible.indexOf(name);
      if (shift && lastIdx >= 0 && idx >= 0) {
        const [a, b] = [Math.min(lastIdx, idx), Math.max(lastIdx, idx)];
        const m = byName();
        for (const n of visible.slice(a, b + 1)) if (m.get(n) && selectable(m.get(n))) cb.checked ? sel.add(n) : sel.delete(n);
      } else cb.checked ? sel.add(name) : sel.delete(name);
      lastIdx = idx;
      render();
    } else if (cb.dataset.grpsel !== undefined) {
      const g = groupsOf(filtered()).find((x) => x.key === cb.dataset.grpsel);
      if (g) g.rows.filter(selectable).forEach((r) => (cb.checked ? sel.add(r.name) : sel.delete(r.name)));
      render();
    }
  });
  $('#tbl').addEventListener('click', guard(async (e) => {
    const b = e.target.closest('[data-a]');
    if (!b) return;
    const name = b.dataset.n;
    const rec = recs.find((r) => r.name === name);
    switch (b.dataset.a) {
      case 'toggle': collapsed.has(b.dataset.g) ? collapsed.delete(b.dataset.g) : collapsed.add(b.dataset.g); render(); break;
      case 'streamer': setStreamer(filter.streamer === name ? '' : name); break;
      case 'play': playerModal(rec); break;
      case 'trim': trimModal(rec); break;
      case 'conv': await api('POST', `/recordings/${enc(name)}/convert`); refresh(); break;
      case 'save': case 'unsave':
        await api('POST', '/recordings/save', { names: [name], saved: b.dataset.a === 'save' });
        toast(t(b.dataset.a === 'save' ? 'rec_saved_toast' : 'rec_unsaved_toast')); refresh(); update(); break;
      case 'del':
        if (!confirm(t('confirm_delete_file', { name }))) return;
        await api('DELETE', `/recordings/${enc(name)}`);
        sel.delete(name); toast(t('deleted')); refresh(); update(); break;
    }
  }));

  const mergeModal = (items) => {
    const ordered = [...items].sort((a, b) => new Date(a.started) - new Date(b.started));
    const exts = new Set(ordered.map((r) => r.ext));
    const streamers = new Set(ordered.map((r) => r.streamer));
    const total = ordered.reduce((a, r) => a + r.size, 0);
    const mixed = exts.size > 1;
    const m = openModal({
      title: t('rec_merge_title'),
      body: `<div class="muted small">${esc(t('rec_merge_hint'))}</div>
        <ol class="mergelist">${ordered.map((r) => `<li><span class="name">${esc(r.name)}</span><span class="muted small">${esc(new Date(r.started).toLocaleString())} · ${fmtSize(r.size)}</span></li>`).join('')}</ol>
        ${mixed ? `<div class="result error">${esc(t('rec_merge_mixed'))}</div>` : ''}
        ${streamers.size > 1 ? `<div class="result">${esc(t('rec_merge_streamers'))}</div>` : ''}
        <label class="check mdel"><input type="checkbox" id="mdel"> <span>${esc(t('rec_merge_delete'))}<br><span class="muted small">${esc(t('rec_merge_delete_hint'))}</span></span></label>
        <div class="muted small">${esc(t('rec_merge_total', { count: ordered.length, size: fmtSize(total) }))}</div>`,
      footer: `<button class="btn" data-close>${esc(t('cancel'))}</button><button class="btn primary" id="mgo" ${mixed ? 'disabled' : ''}>⛓ ${esc(t('rec_merge'))}</button>`,
    });
    $('#mgo', m).onclick = guard(async () => {
      await api('POST', '/recordings/merge', { names: ordered.map((r) => r.name), delete_sources: $('#mdel', m).checked });
      closeModal(); sel.clear(); toast(t('rec_merge_started')); refresh(); update();
    });
  };

  $('#bulk').addEventListener('click', guard(async (e) => {
    const b = e.target.closest('[data-b]');
    if (!b) return;
    const items = selRecs();
    const names = items.map((r) => r.name);
    switch (b.dataset.b) {
      case 'clear': sel.clear(); render(); break;
      case 'selall': filtered().filter(selectable).forEach((r) => sel.add(r.name)); render(); break;
      case 'merge': mergeModal(items); break;
      case 'save': case 'unsave':
        await api('POST', '/recordings/save', { names, saved: b.dataset.b === 'save' });
        sel.clear(); toast(t(b.dataset.b === 'save' ? 'rec_saved_toast_n' : 'rec_unsaved_toast_n', { count: names.length })); refresh(); update(); break;
      case 'del': {
        const hasSaved = items.some((r) => r.saved);
        if (!confirm(t('confirm_delete_many', { count: names.length, size: fmtSize(items.reduce((a, r) => a + r.size, 0)) }) + (hasSaved && !savedMode ? '' : hasSaved ? '\n\n' + t('confirm_delete_saved') : ''))) return;
        const res = await api('POST', '/recordings/delete', { names });
        sel.clear();
        toast(t('rec_deleted_n', { count: res.deleted.length }) + (res.failed.length ? ' · ' + t('rec_delete_failed', { count: res.failed.length }) : ''), res.failed.length > 0);
        refresh(); update(); break;
      }
    }
  }));

  const update = async () => {
    const all = await api('GET', '/recordings');
    recs = all.filter((r) => !!r.saved === !!savedMode);
    const sig = JSON.stringify(recs);
    const names = new Set(recs.map((r) => r.name));
    for (const n of [...sel]) if (!names.has(n)) sel.delete(n); // files that vanished
    renderStreamers(); renderJobsBadge();
    if (sig !== lastSig) { lastSig = sig; render(); }
  };
  update().catch(() => {});
  return { update: () => { renderJobsBadge(); return update(); } };
}

function playerModal(rec) {
  const note = !NATIVE.includes(rec.ext);
  const m = openModal({
    title: rec.name, wide: true,
    body: `<video controls autoplay src="api/recordings/${enc(rec.name)}/stream"></video>
      <div class="result" id="pnote" ${note ? '' : 'hidden'}>${esc(t('rec_not_native'))}</div>`,
    footer: `${note ? `<button class="btn" id="pconv">${esc(t('rec_convert'))}</button>` : ''}
      <a class="btn" href="api/recordings/${enc(rec.name)}/download" download>⬇ ${esc(t('download'))}</a><button class="btn" data-close>${esc(t('close'))}</button>`,
    onClose: () => { const v = $('video'); if (v) { v.pause(); v.removeAttribute('src'); v.load(); } },
  });
  $('video', m).onerror = () => { $('#pnote', m).hidden = false; };
  const c = $('#pconv', m);
  if (c) c.onclick = guard(async () => { await api('POST', `/recordings/${enc(rec.name)}/convert`); closeModal(); refresh(); });
}

function trimModal(rec) {
  const m = openModal({
    title: `${t('trim_title')}: ${rec.name}`, wide: true,
    body: `<video controls preload="metadata" src="api/recordings/${enc(rec.name)}/stream"></video>
      <div class="result" id="tnote" hidden>${esc(t('rec_not_native'))}</div>
      <div class="timeline" id="tl"><div class="track"></div><div class="sel"></div><div class="head"></div>
        <input type="range" id="rin" min="0" max="1" step="0.01" value="0"><input type="range" id="rout" min="0" max="1" step="0.01" value="1"></div>
      <div class="trimbar">
        <div class="trimrow"><span class="lbl">${esc(t('trim_in'))}</span><input type="text" class="timeinput" id="tin" value="00:00:00.000">
          <button class="btn sm" id="setin">${esc(t('trim_set_in'))}</button></div>
        <div class="trimrow"><span class="lbl">${esc(t('trim_out'))}</span><input type="text" class="timeinput" id="tout" value="00:00:00.000">
          <button class="btn sm" id="setout">${esc(t('trim_set_out'))}</button></div>
      </div>
      <div class="muted small">${esc(t('trim_duration'))}: <b id="tdur">–</b></div>
      <div class="field"><span class="lbl">${esc(t('trim_mode'))}</span>
        <label class="check"><input type="radio" name="tmode" value="fast" checked> ${esc(t('trim_fast'))}</label>
        <label class="check"><input type="radio" name="tmode" value="accurate"> ${esc(t('trim_accurate'))}</label></div>`,
    footer: `<button class="btn" data-close>${esc(t('cancel'))}</button><button class="btn primary" id="texp" disabled>✂ ${esc(t('trim_export'))}</button>`,
    onClose: () => { const v = $('video'); if (v) { v.pause(); v.removeAttribute('src'); v.load(); } },
  });
  const v = $('video', m), rin = $('#rin', m), rout = $('#rout', m), tin = $('#tin', m), tout = $('#tout', m);
  let dur = 0, a = 0, b = 0;
  const paint = () => {
    const sel = $('.sel', m);
    sel.style.left = (a / dur) * 100 + '%';
    sel.style.width = ((b - a) / dur) * 100 + '%';
    rin.value = a; rout.value = b;
    tin.value = fmtTime(a); tout.value = fmtTime(b);
    $('#tdur', m).textContent = fmtTime(b - a);
  };
  v.addEventListener('loadedmetadata', () => {
    dur = isFinite(v.duration) ? v.duration : 0;
    if (!dur) { $('#tnote', m).hidden = false; return; }
    rin.max = rout.max = dur;
    a = 0; b = dur; paint();
    $('#texp', m).disabled = false;
  });
  v.addEventListener('error', () => { $('#tnote', m).hidden = false; });
  v.addEventListener('timeupdate', () => { if (dur) $('.head', m).style.left = (v.currentTime / dur) * 100 + '%'; });
  rin.oninput = () => { a = Math.min(parseFloat(rin.value), b - 0.1); paint(); };
  rout.oninput = () => { b = Math.max(parseFloat(rout.value), a + 0.1); paint(); };
  const fromText = (inp, isIn) => () => {
    const s = parseTime(inp.value);
    if (!Number.isNaN(s)) { if (isIn) a = Math.max(0, Math.min(s, b - 0.1)); else b = Math.min(dur, Math.max(s, a + 0.1)); }
    paint();
  };
  tin.onchange = fromText(tin, true); tout.onchange = fromText(tout, false);
  $('#setin', m).onclick = () => { a = Math.min(v.currentTime, b - 0.1); paint(); };
  $('#setout', m).onclick = () => { b = Math.max(v.currentTime, a + 0.1); paint(); };
  $('#tl', m).addEventListener('click', (e) => {
    if (!dur || e.target.tagName === 'INPUT') return;
    const r = e.currentTarget.getBoundingClientRect();
    v.currentTime = Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * dur;
  });
  $('#texp', m).onclick = guard(async () => {
    const mode = m.querySelector('input[name=tmode]:checked').value;
    await api('POST', `/recordings/${enc(rec.name)}/trim`, { start: a, end: b, mode });
    closeModal(); toast(t('trim_started')); refresh();
  });
}

/* ------------------------------------------------------ settings view */
const SET_TABS = ['general', 'auth', 'notifications', 'tools', 'stats', 'updates'];
const settingsTabLabel = (k) => t('set_tab_' + k);

function settingsView(tab) {
  tab = SET_TABS.includes(tab) ? tab : 'general';
  const alive = { stop: false, timers: [] };
  const view = { destroy() { alive.stop = true; alive.timers.forEach(clearTimeout); } };
  $('#view').innerHTML = `<div class="settings">
    <h2>${esc(t('set_title'))}</h2>
    <div class="tabs" role="tablist">${SET_TABS.map((k) => `<a role="tab" href="#/settings/${k}" class="tab${k === tab ? ' active' : ''}" aria-selected="${k === tab}">${esc(settingsTabLabel(k))}${k === 'updates' && state.update?.available ? '<i class="dot"></i>' : ''}</a>`).join('')}</div>
    <div id="stab" class="stab"><div class="loading muted">${esc(t('loading'))}</div></div></div>`;
  const body = $('#stab');
  (async () => {
    settings = await api('GET', '/settings');
    if (alive.stop) return;
    if (tab === 'updates') return updatesTab(body, alive);
    if (tab === 'stats') return statsTab(body, alive);
    if (tab === 'notifications') return notificationsTab(body, alive);
    const ver = await api('GET', '/version');
    if (alive.stop) return;
    const v = (id) => $(id).value;
    const saveBar = `<div class="row"><button class="btn primary" id="s-save">${esc(t('save'))}</button></div>`;
    const save = (collect) => ($('#s-save').onclick = guard(async () => {
      const b = collect();
      settings = await api('PUT', '/settings', b);
      toast(t('saved')); settingsView(tab);
    }));
    const reload = guard(async () => settingsView(tab));
    if (tab === 'general') {
      body.innerHTML = `<fieldset class="card"><legend>${esc(t('set_defaults'))}</legend>
        <div class="two">
          <div class="field"><label>${esc(t('set_def_quality'))}</label><input type="text" id="s-q" value="${esc(settings.default_quality)}"><span class="muted small">${esc(t('ch_quality_hint'))}</span></div>
          <div class="field"><label>${esc(t('set_def_interval'))}</label><input type="number" min="15" id="s-iv" value="${settings.default_interval}"></div>
        </div>
        <div class="field"><label>${esc(t('set_def_template'))}</label><input type="text" id="s-tpl" value="${esc(settings.default_template)}"><span class="muted small">${esc(t('ch_template_hint'))}</span></div>
        <label class="check"><input type="checkbox" id="s-remux" ${settings.remux_after ? 'checked' : ''}> ${esc(t('set_remux'))}</label>
      </fieldset>${saveBar}`;
      save(() => ({ default_quality: v('#s-q'), default_interval: parseInt(v('#s-iv'), 10) || 60, default_template: v('#s-tpl'), remux_after: $('#s-remux').checked }));
    } else if (tab === 'auth') {
      body.innerHTML = `<fieldset class="card"><legend>${esc(t('set_global_auth'))}</legend>
        <div class="field"><label>${esc(t('set_twitch_token'))}</label>
          <div class="row"><input type="password" id="s-token" class="grow" autocomplete="off" placeholder="${settings.has_twitch_token ? esc(t('token_keep')) : ''}">
          ${settings.has_twitch_token ? `<button class="btn sm danger" id="s-token-clear">${esc(t('remove_cookies'))}</button>` : ''}</div></div>
        <div class="field"><label>${esc(t('set_global_cookies'))}</label>
          <div class="row"><span class="badge">${settings.has_cookies ? '🟢 ' + esc(t('cookie_loaded')) : '⚪ ' + esc(t('cookie_none'))}</span>
            <button class="btn sm" id="s-ck-up">${esc(t('upload_cookies'))}</button>
            ${settings.has_cookies ? `<button class="btn sm danger" id="s-ck-rm">${esc(t('remove_cookies'))}</button>` : ''}
            <input type="file" id="s-ck-file" accept=".txt,text/plain" hidden></div></div>
        <div class="field"><label>${esc(t('set_proxy'))}</label><input type="text" id="s-proxy" value="${esc(settings.proxy)}" placeholder="http://user:pass@host:port"></div>
      </fieldset>${saveBar}`;
      save(() => { const b = { proxy: v('#s-proxy') }; if (v('#s-token')) b.twitch_token = v('#s-token'); return b; });
      const clr = $('#s-token-clear');
      if (clr) clr.onclick = guard(async () => { await api('PUT', '/settings', { twitch_token: '' }); reload(); });
      $('#s-ck-up').onclick = () => $('#s-ck-file').click();
      $('#s-ck-file').onchange = guard(async (e) => {
        const f = e.target.files[0];
        if (f) { await api('PUT', '/settings/cookies', await f.text(), true); toast(t('saved')); reload(); }
      });
      const rm = $('#s-ck-rm');
      if (rm) rm.onclick = guard(async () => { await api('DELETE', '/settings/cookies'); reload(); });
    } else {
      const tool = (name, x) => `<span class="badge ${x.found ? 'ok' : 'error'}">${esc(name)}: ${x.found ? esc(t('tool_found')) + ' — ' + esc(x.version || '') : esc(t('tool_missing'))}</span>`;
      body.innerHTML = `<fieldset class="card"><legend>${esc(t('set_tools'))}</legend>
        <div class="row">${tool('streamlink', ver.streamlink)} ${tool('ffmpeg', ver.ffmpeg)}</div>
        <div class="two">
          <div class="field"><label>${esc(t('set_streamlink_path'))}</label><input type="text" id="s-sl" value="${esc(settings.streamlink_bin)}" placeholder="streamlink"></div>
          <div class="field"><label>${esc(t('set_ffmpeg_path'))}</label><input type="text" id="s-ff" value="${esc(settings.ffmpeg_bin)}" placeholder="ffmpeg"></div>
        </div><div class="muted small">${esc(t('set_tools_hint'))}</div>
      </fieldset><div class="row">${'<button class="btn primary" id="s-save">' + esc(t('save')) + '</button>'}</div>`;
      save(() => ({ streamlink_bin: v('#s-sl'), ffmpeg_bin: v('#s-ff') }));
    }
  })().catch((e) => toast(e.message, true));
  return view;
}

/* ---------------------------------------------------- statistics tab */
function fmtUptime(sec) {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  return d ? t('st_uptime_d', { d, h }) : h ? t('st_uptime_h', { h, m }) : t('st_uptime_m', { m });
}

const platformIcon = (name) => (PLATFORM_HUE[String(name).toLowerCase()] !== undefined || state.icons?.[String(name).toLowerCase()]) ? platformBadge(name) + ' ' : '';

/* Settings → Notifications: Gotify delivery */
function notificationsTab(body, alive) {
  const types = ['stream_live', 'stream_ended', 'rec_failed', 'auth_error', 'job_started', 'job_done', 'job_failed', 'update_available'];
  const sel = new Set(settings.gotify_types || types);
  const fmt = (d) => new Date(d).toLocaleString();
  const gl = settings.gotify_lang || lang;
  body.innerHTML = `<fieldset class="card"><legend>Gotify</legend>
    <div class="muted small" style="margin-bottom:8px">${esc(t('gotify_hint'))}</div>
    <label class="check"><input type="checkbox" id="g-on" ${settings.gotify_on ? 'checked' : ''}> ${esc(t('gotify_enable'))}</label>
    <div class="two" style="margin-top:10px">
      <div class="field"><label>${esc(t('gotify_url'))}</label><input type="url" id="g-url" value="${esc(settings.gotify_url)}" placeholder="https://gotify.example.com" autocomplete="off" inputmode="url"></div>
      <div class="field"><label>${esc(t('gotify_token'))}</label>
        <div class="row"><input type="password" id="g-token" class="grow" autocomplete="off" placeholder="${settings.has_gotify_token ? esc(t('token_keep')) : ''}">
        ${settings.has_gotify_token ? `<button class="btn sm danger" id="g-token-clear">${esc(t('remove_cookies'))}</button>` : ''}</div>
        <span class="muted small">${esc(t('gotify_token_hint'))}</span></div>
    </div>
    <div class="field"><label>${esc(t('gotify_lang'))}</label>
      <select id="g-lang">${languages.map((l) => `<option value="${l.code}" ${l.code === gl ? 'selected' : ''}>${esc(l.name)}</option>`).join('')}</select></div>
    <div class="field"><label>${esc(t('gotify_types'))}</label>
      <div class="row" style="gap:8px;margin-bottom:4px"><button class="btn sm" id="g-all" type="button">${esc(t('gotify_all'))}</button><button class="btn sm" id="g-none" type="button">${esc(t('gotify_none'))}</button></div>
      <div id="g-types">${types.map((k) => `<label class="check"><input type="checkbox" data-type="${k}" ${sel.has(k) ? 'checked' : ''}> ${notifIcon({ type: k })} ${esc(t('ntf_type_' + k))}</label>`).join('')}</div>
      <span class="muted small">${esc(t('gotify_types_hint'))}</span></div>
    <div id="g-status" class="muted small"></div>
  </fieldset>
  <div class="row"><button class="btn primary" id="g-save">${esc(t('save'))}</button><button class="btn" id="g-test">${esc(t('gotify_test'))}</button></div>`;

  const v = (id) => $(id).value.trim();
  const collect = () => {
    const b = { gotify_on: $('#g-on').checked, gotify_url: v('#g-url'), gotify_lang: $('#g-lang').value,
      gotify_types: $$('#g-types [data-type]').filter((c) => c.checked).map((c) => c.dataset.type) };
    if (v('#g-token')) b.gotify_token = v('#g-token');
    return b;
  };
  const status = async () => {
    try {
      const s = await api('GET', '/gotify');
      if (alive.stop) return;
      $('#g-status').textContent = s.last_error ? '⚠ ' + t('gotify_last_error') + ' (' + fmt(s.last_error_at) + '): ' + s.last_error
        : s.last_ok ? '✓ ' + t('gotify_last_ok') + ': ' + fmt(s.last_ok) : '';
    } catch { /* ignore */ }
  };
  status();
  $('#g-all').onclick = () => $$('#g-types [data-type]').forEach((c) => { c.checked = true; });
  $('#g-none').onclick = () => $$('#g-types [data-type]').forEach((c) => { c.checked = false; });
  $('#g-save').onclick = guard(async () => {
    settings = await api('PUT', '/settings', collect());
    toast(t('saved')); notificationsTab(body, alive);
  });
  const clr = $('#g-token-clear');
  if (clr) clr.onclick = guard(async () => {
    settings = await api('PUT', '/settings', { gotify_on: false, gotify_token: '' });
    toast(t('saved')); notificationsTab(body, alive);
  });
  $('#g-test').onclick = guard(async () => {
    const b = collect();
    await api('POST', '/gotify/test', { url: b.gotify_url, token: b.gotify_token, lang: b.gotify_lang });
    toast(t('gotify_test_ok')); status();
  });
}

function statsTab(body, alive) {
  const bars = (items, total) => items.length ? `<div class="hbars">${items.map((g) => `<div class="hbar">
      <div class="hbar-top"><span class="hbar-name">${platformIcon(g.name)}${esc(g.name)}</span><span class="muted small">${esc(t('st_files_n', { count: g.count }))} · ${fmtSize(g.size)}</span></div>
      <div class="meter"><i style="width:${total ? Math.max(2, (g.size / total) * 100) : 0}%"></i></div></div>`).join('')}</div>` : `<div class="muted">${esc(t('st_no_data'))}</div>`;

  async function load() {
    const s = await api('GET', '/stats');
    if (alive.stop) return;
    const d = s.disk, lib = s.library, sys = s.system;
    const pct = d && d.total ? Math.round((d.used / d.total) * 100) : 0;
    const level = pct >= 90 ? 'bad' : pct >= 75 ? 'warn' : 'ok';
    const libPct = d && d.total ? Math.min(100, (lib.size / d.total) * 100) : 0;
    const otherPct = d && d.total ? Math.max(0, pct - libPct) : 0;
    const maxDay = Math.max(1, ...s.days.map((x) => x.size));
    const memUsed = sys.mem_total ? sys.mem_total - sys.mem_avail : 0;
    const left = s.days_left > 0 ? (s.days_left > 3650 ? t('st_days_left_many') : t('st_days_left', { days: Math.floor(s.days_left) })) : t('st_days_left_unknown');
    body.innerHTML = `
      <fieldset class="card"><legend>${esc(t('st_disk'))}</legend>
        ${d ? `<div class="diskbar ${level}" role="img" aria-label="${pct}%"><i class="lib" style="width:${libPct}%"></i><i class="other" style="width:${otherPct}%"></i></div>
        <div class="disk-legend">
          <span><i class="sw lib"></i>${esc(t('st_recordings'))}: <b>${fmtSize(lib.size)}</b></span>
          <span><i class="sw other"></i>${esc(t('st_other_used'))}: <b>${fmtSize(Math.max(0, d.used - lib.size))}</b></span>
          <span><i class="sw free"></i>${esc(t('st_free'))}: <b>${fmtSize(d.free)}</b></span></div>
        <div class="muted small">${esc(t('st_disk_line', { used: fmtSize(d.used), total: fmtSize(d.total), pct }))} · ${esc(left)}</div>
        <div class="muted small">${esc(lib.recordings_dir)}</div>` : `<div class="muted">${esc(t('st_no_data'))}</div>`}
      </fieldset>
      <div class="tiles">
        <div class="tile"><div class="tile-n">${lib.count}</div><div class="muted small">${esc(t('st_tile_files'))}</div><div class="small">${fmtSize(lib.size)}</div></div>
        <div class="tile"><div class="tile-n">${lib.saved_count}</div><div class="muted small">${esc(t('st_tile_saved'))}</div><div class="small">${fmtSize(lib.saved_size)}</div></div>
        <div class="tile"><div class="tile-n">${s.channels.recording}<span class="muted"> / ${s.channels.total}</span></div><div class="muted small">${esc(t('st_tile_channels'))}</div><div class="small">${esc(t('st_recording_now'))}</div></div>
        <div class="tile"><div class="tile-n">${fmtSize(s.week_size)}</div><div class="muted small">${esc(t('st_tile_week'))}</div><div class="small">${esc(t('st_per_day', { size: fmtSize(s.week_size / 7) }))}</div></div>
      </div>
      <fieldset class="card"><legend>${esc(t('st_activity'))}</legend>
        <div class="daychart">${s.days.map((x) => `<div class="daycol" title="${esc(x.date)}: ${esc(t('st_files_n', { count: x.count }))}, ${fmtSize(x.size)}">
          <div class="daybar" style="height:${x.size ? Math.max(4, (x.size / maxDay) * 100) : 0}%"></div></div>`).join('')}</div>
        <div class="row between small muted"><span>${esc(s.days[0].date)}</span><span>${esc(t('st_max_day', { size: fmtSize(maxDay === 1 ? 0 : maxDay) }))}</span><span>${esc(s.days[13].date)}</span></div>
      </fieldset>
      <div class="two">
        <fieldset class="card"><legend>${esc(t('st_by_streamer'))}</legend>${bars(s.streamers, s.streamers[0]?.size)}</fieldset>
        <fieldset class="card"><legend>${esc(t('st_by_platform'))}</legend>${bars(s.platforms, s.platforms[0]?.size)}</fieldset>
      </div>
      <fieldset class="card"><legend>${esc(t('st_largest'))}</legend>
        ${s.largest.length ? `<ol class="mergelist">${s.largest.map((r) => `<li><span class="name">${esc(r.name)}</span><span class="muted small">${esc(r.streamer)} · ${fmtSize(r.size)}</span></li>`).join('')}</ol>` : `<div class="muted">${esc(t('st_no_data'))}</div>`}
      </fieldset>
      <fieldset class="card"><legend>${esc(t('st_system'))}</legend>
        <dl class="kv">
          <dt>${esc(t('st_uptime_app'))}</dt><dd>${esc(fmtUptime(sys.uptime_app))}</dd>
          <dt>${esc(t('st_uptime_host'))}</dt><dd>${sys.uptime_host ? esc(fmtUptime(sys.uptime_host)) : '—'}</dd>
          <dt>${esc(t('st_load'))}</dt><dd>${sys.load1.toFixed(2)} (${esc(t('st_cpus', { n: sys.cpus }))})</dd>
          ${sys.mem_total ? `<dt>${esc(t('st_memory'))}</dt><dd>${fmtSize(memUsed)} / ${fmtSize(sys.mem_total)}</dd>` : ''}
          <dt>${esc(t('st_app_memory'))}</dt><dd>${fmtSize(sys.app_mem)}</dd>
          <dt>${esc(t('st_data_size'))}</dt><dd>${fmtSize(lib.data_size)}</dd>
        </dl>
      </fieldset>
      <div class="row"><button class="btn" id="st-refresh">${esc(t('st_refresh'))}</button><span class="muted small">${esc(t('st_auto'))}</span></div>`;
    $('#st-refresh').onclick = guard(load);
    const id = setTimeout(() => load().catch(() => {}), 15000);
    alive.timers.push(id);
  }
  load().catch((e) => toast(e.message, true));
}

/* ------------------------------------------------------- updates tab */
function updatesTab(body, alive) {
  let st = null, pollTimer = null;
  const later = (fn, ms) => { const id = setTimeout(fn, ms); alive.timers.push(id); return id; };
  const fmtDate = (d) => (d ? new Date(d).toLocaleString() : t('never'));
  const chLabel = (c) => t('upd_ch_' + c);
  const applying = () => ['downloading', 'verifying', 'installing', 'restarting'].includes(st.apply?.state);

  async function load() { st = await api('GET', '/update'); if (!alive.stop) draw(); }

  function draw() {
    if (alive.stop || !st) return;
    const cur = st.current, chk = st.check, ap = st.apply || {};
    const latest = chk?.latest;
    const installedDiffers = st.channel !== cur.installed_channel;
    let status = '';
    if (applying() || ap.state === 'restart_manual') {
      const labels = { downloading: t('upd_state_downloading', { percent: ap.percent || 0 }), verifying: t('upd_state_verifying'), installing: t('upd_state_installing'), restarting: t('upd_state_restarting'), restart_manual: t('upd_manual_restart') };
      status = `<div class="upd-card"><div class="upd-title">${esc(labels[ap.state])}</div>
        ${ap.state === 'downloading' ? `<div class="progress"><i style="width:${ap.percent || 0}%"></i></div>` : ap.state === 'restart_manual' ? '' : '<div class="progress indeterminate"><i></i></div>'}</div>`;
    } else if (!chk) {
      status = `<div class="muted">${esc(t('upd_not_checked'))}</div>`;
    } else if (chk.error) {
      status = `<div class="upd-card bad">${esc(t('upd_check_failed'))}: ${esc(chk.error)}</div>`;
    } else if (chk.available) {
      status = `<div class="upd-card avail"><div class="upd-title">⬆ ${esc(t('upd_available', { version: latest.tag }))}</div>
        <div class="muted small">${latest.published && !String(latest.published).startsWith('0001') ? esc(t('upd_published')) + ': ' + esc(fmtDate(latest.published)) + ' · ' : ''}<a href="${esc(latest.url)}" target="_blank" rel="noopener noreferrer">${esc(t('upd_release_page'))}</a></div>
        ${latest.notes ? `<details class="notes"><summary>${esc(t('upd_notes'))}</summary><pre>${esc(latest.notes)}</pre></details>` : ''}
        ${ap.state === 'error' ? `<div class="err">${esc(t('upd_error'))}: ${esc(ap.error)}</div>` : ''}
        ${st.can_update ? `<div class="row"><button class="btn primary" id="u-apply">${esc(t('upd_apply'))}</button></div>`
          : `<div class="warn-box"><b>${esc(t('upd_cannot_title'))}</b> ${esc(t('upd_reason_' + st.reason))}<div class="muted small">${esc(t('upd_run_command'))}</div>
             <div class="cmd"><code id="u-cmd">${esc(st.command)}</code><button class="btn sm" id="u-copy">${esc(t('upd_copy'))}</button></div></div>`}
      </div>`;
    } else {
      status = `<div class="upd-card ok">✓ ${esc(t('upd_up_to_date'))}</div>`;
    }
    body.innerHTML = `<fieldset class="card"><legend>${esc(t('set_tab_updates'))}</legend>
      <dl class="kv">
        <dt>${esc(t('upd_current'))}</dt><dd>${esc(cur.version)}</dd>
        ${cur.commit ? `<dt>${esc(t('upd_commit'))}</dt><dd><code>${esc(cur.commit.slice(0, 7))}</code></dd>` : ''}
        <dt>${esc(t('upd_installed_channel'))}</dt><dd>${esc(chLabel(cur.installed_channel))}</dd>
        <dt>${esc(t('upd_last_checked'))}</dt><dd>${esc(chk ? fmtDate(chk.checked_at) : t('never'))}</dd>
      </dl>
      <div class="field"><label>${esc(t('upd_channel'))}</label>
        <select id="u-ch">${['stable', 'beta', 'nightly'].map((c) => `<option value="${c}"${st.channel === c ? ' selected' : ''}>${esc(chLabel(c))}${c === cur.installed_channel ? ' ★' : ''}</option>`).join('')}</select>
        <span class="muted small">${esc(t('upd_channel_hint'))}${installedDiffers ? ' ' + esc(t('upd_channel_differs', { channel: chLabel(cur.installed_channel) })) : ''}</span></div>
      <label class="check"><input type="checkbox" id="u-auto" ${settings.update_auto ? 'checked' : ''}> ${esc(t('upd_auto'))}</label>
      <div class="row"><button class="btn" id="u-check" ${applying() ? 'disabled' : ''}>${esc(t('upd_check'))}</button></div>
    </fieldset>${status}`;
    $('#u-check').onclick = guard(async () => {
      const b = $('#u-check'); b.disabled = true; b.textContent = t('upd_checking');
      st = await api('POST', '/update/check'); draw();
    });
    $('#u-ch').onchange = guard(async (e) => {
      settings = await api('PUT', '/settings', { update_channel: e.target.value });
      st = await api('POST', '/update/check'); draw(); refresh();
    });
    $('#u-auto').onchange = guard(async (e) => { settings = await api('PUT', '/settings', { update_auto: e.target.checked }); toast(t('saved')); });
    const ap_ = $('#u-apply');
    if (ap_) ap_.onclick = guard(async () => {
      const n = state.active?.length || 0;
      if (!confirm(t('upd_apply_confirm', { version: latest.tag }) + (n ? '\n\n' + t('upd_apply_recordings', { count: n }) : ''))) return;
      st = await api('POST', '/update/apply'); draw(); watch();
    });
    const cp = $('#u-copy');
    if (cp) cp.onclick = async () => {
      try { await navigator.clipboard.writeText(st.command); toast(t('upd_copied')); }
      catch { const r = document.createRange(); r.selectNodeContents($('#u-cmd')); getSelection().removeAllRanges(); getSelection().addRange(r); }
    };
  }

  // follow the progress; after the restart wait for the new version and reload
  function watch() {
    clearTimeout(pollTimer);
    const tick = async () => {
      if (alive.stop) return;
      try {
        const prevVersion = st.current.version, prevCommit = st.current.commit;
        if (st.apply?.state === 'restarting' || st._down) {
          const v = await fetch('api/version', { cache: 'no-store' });
          if (v.ok) {
            const j = await v.json();
            if (j.version !== prevVersion || (j.commit || '') !== (prevCommit || '')) { location.reload(); return; }
          }
        } else {
          const next = await api('GET', '/update');
          next._down = false; st = next; draw();
        }
      } catch {
        // the server is restarting: keep waiting for it to come back
        st._down = true;
        if (st.apply) st.apply.state = 'restarting';
        draw();
      }
      if (['downloading', 'verifying', 'installing', 'restarting'].includes(st.apply?.state) || st._down) pollTimer = later(tick, 1200);
    };
    pollTimer = later(tick, 800);
  }

  load().then(() => { if (st && applying()) watch(); }).catch((e) => toast(e.message, true));
}

/* ------------------------------------------------- notifications page */
const NOTIF_ICON = { stream_live: '🔴', stream_ended: '⏹', rec_failed: '⚠️', auth_error: '🔑', job_started: '⚙️', job_done: '✅', job_failed: '❌', update_available: '⬆️' };
const NOTIF_GROUP = { stream_live: 'streams', stream_ended: 'streams', job_started: 'jobs', job_done: 'jobs', job_failed: 'jobs', rec_failed: 'system', auth_error: 'system', update_available: 'system' };
const notifIcon = (n) => NOTIF_ICON[n.type] || '🔔';

function notifText(n) {
  const p = n.params || {};
  return t('ntf_' + n.type, {
    channel: p.channel || '', title: p.title || '', error: p.error || '', input: p.input || '', output: p.output || '',
    kind: p.kind ? t('job_' + p.kind) : '', version: p.version || '', channel_type: p.channel ? t('upd_ch_' + p.channel) : '',
  }) + (n.type === 'stream_live' && p.recording ? ' · ' + t('ntf_recording_started') : '');
}

function notifLink(n) {
  switch (n.type) {
    case 'job_started': case 'job_failed': return '#/recordings/jobs';
    case 'job_done': return '#/recordings';
    case 'update_available': return '#/settings/updates';
    default: return '#/channels';
  }
}

function notificationsView() {
  let items = [], lastRead = 0, firstUnread = null, sig = '', markTimer = null;
  const f = { group: 'all' };
  const view = { destroy() { clearTimeout(markTimer); } };
  $('#view').innerHTML = `<div class="notif-page">
    <div class="row between"><h2 style="margin:0">${esc(t('ntf_title'))}</h2>
      <div class="row"><button class="btn sm" id="n-read">✓ ${esc(t('ntf_mark_read'))}</button><button class="btn sm danger" id="n-clear">🗑 ${esc(t('ntf_clear'))}</button></div></div>
    <div class="chips" id="n-filter"></div>
    <div id="n-list"></div>
    <details class="card" id="n-prefs"><summary>⚙ ${esc(t('ntf_prefs'))}</summary><div class="muted small" style="margin:6px 0">${esc(t('ntf_prefs_hint'))}</div><div id="n-prefs-body"></div></details></div>`;

  const renderFilter = () => {
    $('#n-filter').innerHTML = ['all', 'streams', 'jobs', 'system'].map((g) =>
      `<button class="chipbtn${f.group === g ? ' on' : ''}" data-g="${g}">${esc(t('ntf_group_' + g))}</button>`).join('');
  };
  const render = () => {
    const rows = items.filter((n) => f.group === 'all' || NOTIF_GROUP[n.type] === f.group);
    if (!rows.length) { $('#n-list').innerHTML = `<div class="card"><div class="empty">${esc(t('ntf_none'))}</div></div>`; return; }
    const groups = new Map();
    for (const n of rows) {
      const k = ymd(new Date(n.time));
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(n);
    }
    $('#n-list').innerHTML = [...groups].map(([k, list]) => `<section class="grp"><div class="grp-head"><b>${esc(dayLabel(k))}</b><span class="muted small">${list.length}</span></div>
      <div class="card ntf-card">${list.map((n) => `<a class="ntf l-${n.level}${n.id > firstUnread ? ' unread' : ''}" href="${notifLink(n)}">
        <span class="ntf-ico">${n.platform ? platformBadge(n.platform, true) : `<span class="ntf-emoji">${notifIcon(n)}</span>`}</span>
        <span class="ntf-main"><span class="ntf-text">${esc(notifText(n))}</span>
          <span class="muted small">${n.platform ? esc(n.platform) + ' · ' : ''}${esc(new Date(n.time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }))}</span></span>
        ${n.id > firstUnread ? '<i class="udot"></i>' : ''}</a>`).join('')}</div></section>`).join('');
  };
  const renderPrefs = () => {
    const off = new Set(settings.notify_off || []);
    $('#n-prefs-body').innerHTML = ['stream_live', 'stream_ended', 'rec_failed', 'auth_error', 'job_started', 'job_done', 'job_failed', 'update_available']
      .map((k) => `<label class="check"><input type="checkbox" data-type="${k}" ${off.has(k) ? '' : 'checked'}> ${notifIcon({ type: k })} ${esc(t('ntf_type_' + k))}</label>`).join('');
  };
  const scheduleRead = () => {
    clearTimeout(markTimer);
    if (!items.some((n) => n.id > lastRead)) return;
    markTimer = setTimeout(async () => {
      try { await api('POST', '/notifications/read'); lastRead = items.length ? items[0].id : lastRead; refresh(); } catch { /* ignore */ }
    }, 1500);
  };

  $('#n-filter').addEventListener('click', (e) => { const b = e.target.closest('[data-g]'); if (b) { f.group = b.dataset.g; renderFilter(); render(); } });
  $('#n-read').onclick = guard(async () => { await api('POST', '/notifications/read'); firstUnread = Infinity; render(); refresh(); });
  $('#n-clear').onclick = guard(async () => {
    if (!confirm(t('ntf_clear_confirm'))) return;
    await api('DELETE', '/notifications'); items = []; render(); refresh();
  });
  $('#n-prefs-body').addEventListener('change', guard(async () => {
    const off = $$('#n-prefs-body [data-type]').filter((c) => !c.checked).map((c) => c.dataset.type);
    settings = await api('PUT', '/settings', { notify_off: off }); toast(t('saved'));
  }));

  const update = async () => {
    const d = await api('GET', '/notifications');
    const s = JSON.stringify(d.items) + d.last_read;
    if (firstUnread === null) firstUnread = d.last_read; // items newer than this are shown as "new"
    lastRead = d.last_read;
    if (s !== sig) { sig = s; items = d.items; render(); }
    scheduleRead();
  };
  renderFilter();
  api('GET', '/settings').then((st) => { settings = st; renderPrefs(); }).catch(() => {});
  update().catch((e) => toast(e.message, true));
  view.update = update;
  return view;
}

/* -------------------------------------------------------------- router */
const views = { channels: channelsView, recordings: (a) => recordingsView(false, a), saved: () => recordingsView(true), settings: settingsView, notifications: notificationsView };
function route() {
  const m = location.hash.match(/^#\/(\w+)(?:\/(\w+))?/) || [];
  const name = m[1];
  const key = views[name] ? name : 'channels';
  if (current?.destroy) current.destroy();
  renderShell();
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === key));
  closeModal();
  current = views[key](m[2]);
  if (current.update) current.update();
}
window.addEventListener('hashchange', route);

/* ---------------------------------------------------------------- init */
(async function init() {
  fallback = await loadLocale(defaultLanguage);
  const sel = $('#lang');
  sel.innerHTML = languages.map((l) => `<option value="${l.code}">${esc(l.name)}</option>`).join('');
  sel.onchange = () => setLang(sel.value);
  $('#theme').onclick = () => {
    const cur = localStorage.getItem('theme') || 'dark';
    localStorage.setItem('theme', THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length]);
    applyTheme();
  };
  try { // show the sign-out button only when the panel is password protected
    const a = await (await fetch('api/auth/status')).json();
    if (a.auth_required) {
      if (!a.authenticated) { location.href = 'login.html'; return; }
      $('#logout').hidden = false;
      $('#logout').onclick = async () => { try { await fetch('api/logout', { method: 'POST' }); } finally { location.href = 'login.html'; } };
    }
  } catch { /* ignore */ }
  try { settings = await api('GET', '/settings'); } catch { /* ignore */ }
  try { state = await api('GET', '/state'); } catch { /* ignore */ }
  await setLang(detectLang());
  setInterval(refresh, 3000);
  document.addEventListener('visibilitychange', refresh);
})();
