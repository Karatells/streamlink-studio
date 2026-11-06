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

function toast(msg, isErr) {
  const el = document.createElement('div');
  el.className = 'toast' + (isErr ? ' err' : '');
  el.textContent = msg;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), isErr ? 7000 : 3500);
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
let state = { channels: [], active: [], jobs: [], counts: { channels: 0, recordings: 0 } };
let settings = {};
let recs = [];
let current = null; // current view object

function renderShell() {
  const names = { channels: t('nav_channels') + ` (${state.counts.channels})`, recordings: t('nav_recordings') + ` (${state.counts.recordings})`, settings: t('nav_settings') };
  $$('#nav a').forEach((a) => (a.textContent = names[a.dataset.route]));
  applyTheme();
}

async function refresh() {
  if (document.hidden) return;
  try {
    state = await api('GET', '/state');
    renderShell();
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
          <span class="badge recording">LIVE</span><b>${esc(c.display_name)}</b>
          <span class="muted">${esc(c.quality || settings.default_quality || 'best')}</span>
          ${c.auth_active ? `<span class="badge ok">🔑 ${esc(t('auth_active'))}</span>` : ''}
          <span class="muted small">${esc(t('recording_for'))} ${fmtDur((Date.now() - new Date(c.recording_since)) / 1000)}</span>
          <span class="grow"></span>
          <button class="btn sm danger" data-act="stop" data-id="${c.id}">■ ${esc(t('stop'))}</button></div>`).join('')
      : `<div class="muted">${esc(t('no_active'))}</div>`;
    $('#list').innerHTML = state.channels.length
      ? state.channels.map((c) => `<div class="card ch">
          <div class="head"><div class="avatar">${esc(c.display_name.slice(0, 1))}</div>
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
    if (r.live) box.textContent = '🟢 ' + t('test_live', { streams: r.streams.join(', ') || 'best' }) + (r.title ? ` — ${r.title}` : '') + (r.plugin ? ` [${r.plugin}]` : '');
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
const filter = { q: '', streamer: '', from: '', to: '', sort: 'date_desc' };
const NATIVE = ['mp4', 'm4v', 'webm', 'mov'];

function recordingsView() {
  $('#view').innerHTML = `
    <div class="row filters" style="margin-bottom:12px">
      <input type="search" id="q" class="grow" placeholder="${esc(t('rec_search'))}" value="${esc(filter.q)}">
      <select id="st"></select>
      <select id="so">${['date_desc', 'date_asc', 'name', 'streamer', 'size'].map((k) => `<option value="${k}" ${filter.sort === k ? 'selected' : ''}>${esc(t('sort_' + k))}</option>`).join('')}</select>
      <label class="row small muted">${esc(t('rec_from'))} <input type="date" id="df" value="${filter.from}" style="width:auto"></label>
      <label class="row small muted">${esc(t('rec_to'))} <input type="date" id="dt" value="${filter.to}" style="width:auto"></label>
    </div>
    <div class="card" style="padding:4px 8px"><div id="tbl"></div></div>
    <div class="muted small" id="sum" style="margin-top:8px"></div>
    <div id="jobs-wrap"></div>`;
  const render = () => {
    const q = filter.q.toLowerCase();
    const rows = recs.filter((r) =>
      (!q || r.name.toLowerCase().includes(q)) && (!filter.streamer || r.streamer === filter.streamer) &&
      (!filter.from || ymd(new Date(r.started)) >= filter.from) && (!filter.to || ymd(new Date(r.started)) <= filter.to));
    const cmp = {
      date_desc: (a, b) => new Date(b.started) - new Date(a.started),
      date_asc: (a, b) => new Date(a.started) - new Date(b.started),
      name: (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }),
      streamer: (a, b) => a.streamer.localeCompare(b.streamer) || new Date(b.started) - new Date(a.started),
      size: (a, b) => b.size - a.size,
    }[filter.sort] || (() => 0);
    rows.sort(cmp);
    $('#tbl').innerHTML = rows.length ? `<table><thead><tr><th>${esc(t('col_name'))}</th><th>${esc(t('col_streamer'))}</th>
      <th class="num">${esc(t('col_size'))}</th><th class="num">${esc(t('col_date'))}</th><th></th></tr></thead><tbody>
      ${rows.map((r) => `<tr><td class="name">${esc(r.name)} ${r.recording ? `<span class="badge recording">${esc(t('rec_recording_now'))}</span>` : ''}</td>
        <td data-label="${esc(t('col_streamer'))}">${esc(r.streamer)}${r.platform ? ` <span class="muted small">· ${esc(r.platform)}</span>` : ''}</td><td class="num" data-label="${esc(t('col_size'))}">${fmtSize(r.size)}</td>
        <td class="num" data-label="${esc(t('col_date'))}">${new Date(r.started).toLocaleString()}</td>
        <td><div class="actions">
          <button class="btn sm" data-a="play" data-n="${esc(r.name)}">▶ ${esc(t('rec_play'))}</button>
          ${r.ext !== 'mp4' && !r.recording ? `<button class="btn sm" data-a="conv" data-n="${esc(r.name)}">${esc(t('rec_convert'))}</button>` : ''}
          ${r.recording ? '' : `<button class="btn sm" data-a="trim" data-n="${esc(r.name)}">✂ ${esc(t('rec_trim'))}</button>`}
          <a class="btn sm" href="api/recordings/${enc(r.name)}/download" download>⬇</a>
          ${r.recording ? '' : `<button class="btn sm danger" data-a="del" data-n="${esc(r.name)}">🗑</button>`}
        </div></td></tr>`).join('')}</tbody></table>` : `<div class="empty">${esc(t('rec_none'))}</div>`;
    $('#sum').textContent = t('rec_total', { count: rows.length, size: fmtSize(rows.reduce((a, r) => a + r.size, 0)) });
  };
  const renderStreamers = () => {
    const sel = $('#st');
    const names = [...new Set(recs.map((r) => r.streamer))].sort();
    if (filter.streamer && !names.includes(filter.streamer)) filter.streamer = '';
    const html = `<option value="">${esc(t('rec_all_streamers'))}</option>` + names.map((n) => `<option ${n === filter.streamer ? 'selected' : ''} value="${esc(n)}">${esc(n)}</option>`).join('');
    if (sel.innerHTML !== html) sel.innerHTML = html;
  };
  const renderJobs = () => {
    const jobs = state.jobs;
    $('#jobs-wrap').innerHTML = jobs.length ? `<h2>${esc(t('jobs_title'))}</h2><div class="card">${jobs.slice(0, 12).map((j) => {
      const label = j.status === 'running' ? t('job_running') : j.status === 'done' ? t('job_done') : t('job_failed');
      const pct = j.progress >= 0 ? Math.round(j.progress * 100) : null;
      return `<div class="active-row"><b>${esc(t('job_' + j.type))}</b><span class="muted small">${esc(j.input)} → ${esc(j.output)}</span><span class="grow"></span>
        ${j.status === 'running' ? `<div class="progress ${pct === null ? 'indet' : ''}"><i style="width:${pct ?? 35}%"></i></div><span class="small">${pct === null ? '' : pct + '%'}</span>`
        : `<span class="badge ${j.status === 'done' ? 'ok' : 'error'}">${esc(label)}</span>`}
        ${j.error ? `<span class="errtxt">${esc(j.error)}</span>` : ''}</div>`;
    }).join('')}</div>` : '';
  };
  $('#q').oninput = (e) => { filter.q = e.target.value; render(); };
  $('#st').onchange = (e) => { filter.streamer = e.target.value; render(); };
  $('#so').onchange = (e) => { filter.sort = e.target.value; render(); };
  $('#df').onchange = (e) => { filter.from = e.target.value; render(); };
  $('#dt').onchange = (e) => { filter.to = e.target.value; render(); };
  $('#tbl').addEventListener('click', guard(async (e) => {
    const b = e.target.closest('[data-a]');
    if (!b) return;
    const name = b.dataset.n;
    const rec = recs.find((r) => r.name === name);
    if (b.dataset.a === 'play') playerModal(rec);
    if (b.dataset.a === 'trim') trimModal(rec);
    if (b.dataset.a === 'conv') { await api('POST', `/recordings/${enc(name)}/convert`); refresh(); }
    if (b.dataset.a === 'del') {
      if (!confirm(t('confirm_delete_file', { name }))) return;
      await api('DELETE', `/recordings/${enc(name)}`);
      toast(t('deleted')); refresh();
    }
  }));
  const update = async () => {
    recs = await api('GET', '/recordings');
    renderStreamers(); render(); renderJobs();
  };
  update().catch(() => {});
  return { update };
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
function settingsView() {
  $('#view').innerHTML = `<div class="loading muted">${esc(t('loading'))}</div>`;
  (async () => {
    settings = await api('GET', '/settings');
    const ver = await api('GET', '/version');
    const tool = (name, x) => `<span class="badge ${x.found ? 'ok' : 'error'}">${esc(name)}: ${x.found ? esc(t('tool_found')) + ' — ' + esc(x.version || '') : esc(t('tool_missing'))}</span>`;
    $('#view').innerHTML = `<div style="max-width:720px;display:flex;flex-direction:column;gap:18px">
      <h2 style="margin-bottom:-6px">${esc(t('set_title'))}</h2>
      <fieldset class="card"><legend>${esc(t('set_global_auth'))}</legend>
        <div class="field"><label>${esc(t('set_twitch_token'))}</label>
          <div class="row"><input type="password" id="s-token" class="grow" autocomplete="off" placeholder="${settings.has_twitch_token ? esc(t('token_keep')) : ''}">
          ${settings.has_twitch_token ? `<button class="btn sm danger" id="s-token-clear">${esc(t('remove_cookies'))}</button>` : ''}</div></div>
        <div class="field"><label>${esc(t('set_global_cookies'))}</label>
          <div class="row"><span class="badge">${settings.has_cookies ? '🟢 ' + esc(t('cookie_loaded')) : '⚪ ' + esc(t('cookie_none'))}</span>
            <button class="btn sm" id="s-ck-up">${esc(t('upload_cookies'))}</button>
            ${settings.has_cookies ? `<button class="btn sm danger" id="s-ck-rm">${esc(t('remove_cookies'))}</button>` : ''}
            <input type="file" id="s-ck-file" accept=".txt,text/plain" hidden></div></div>
        <div class="field"><label>${esc(t('set_proxy'))}</label><input type="text" id="s-proxy" value="${esc(settings.proxy)}" placeholder="http://user:pass@host:port"></div>
      </fieldset>
      <fieldset class="card"><legend>${esc(t('set_defaults'))}</legend>
        <div class="two">
          <div class="field"><label>${esc(t('set_def_quality'))}</label><input type="text" id="s-q" value="${esc(settings.default_quality)}"></div>
          <div class="field"><label>${esc(t('set_def_interval'))}</label><input type="number" min="15" id="s-iv" value="${settings.default_interval}"></div>
        </div>
        <div class="field"><label>${esc(t('set_def_template'))}</label><input type="text" id="s-tpl" value="${esc(settings.default_template)}"><span class="muted small">${esc(t('ch_template_hint'))}</span></div>
        <label class="check"><input type="checkbox" id="s-remux" ${settings.remux_after ? 'checked' : ''}> ${esc(t('set_remux'))}</label>
      </fieldset>
      <fieldset class="card"><legend>${esc(t('set_tools'))}</legend>
        <div class="row">${tool('streamlink', ver.streamlink)} ${tool('ffmpeg', ver.ffmpeg)}</div>
        <div class="two">
          <div class="field"><label>${esc(t('set_streamlink_path'))}</label><input type="text" id="s-sl" value="${esc(settings.streamlink_bin)}" placeholder="streamlink"></div>
          <div class="field"><label>${esc(t('set_ffmpeg_path'))}</label><input type="text" id="s-ff" value="${esc(settings.ffmpeg_bin)}" placeholder="ffmpeg"></div>
        </div><div class="muted small">${esc(t('set_tools_hint'))}</div>
      </fieldset>
      <div class="row"><button class="btn primary" id="s-save">${esc(t('save'))}</button>
        <span class="grow"></span><span class="muted small">${esc(t('set_about'))}: Streamlink Studio ${esc(ver.version)} · ${esc(ver.os)}/${esc(ver.arch)} ·
        <a href="https://github.com/Karatells/streamlink-studio" target="_blank" rel="noopener noreferrer">GitHub</a></span></div></div>`;
    const v = (id) => $(id).value;
    const reload = guard(async () => settingsView());
    $('#s-save').onclick = guard(async () => {
      const b = { proxy: v('#s-proxy'), default_quality: v('#s-q'), default_interval: parseInt(v('#s-iv'), 10) || 60,
        default_template: v('#s-tpl'), remux_after: $('#s-remux').checked, streamlink_bin: v('#s-sl'), ffmpeg_bin: v('#s-ff') };
      if (v('#s-token')) b.twitch_token = v('#s-token');
      settings = await api('PUT', '/settings', b);
      toast(t('saved')); settingsView();
    });
    const clr = $('#s-token-clear');
    if (clr) clr.onclick = guard(async () => { await api('PUT', '/settings', { twitch_token: '' }); reload(); });
    $('#s-ck-up').onclick = () => $('#s-ck-file').click();
    $('#s-ck-file').onchange = guard(async (e) => {
      const f = e.target.files[0];
      if (f) { await api('PUT', '/settings/cookies', await f.text(), true); toast(t('saved')); reload(); }
    });
    const rm = $('#s-ck-rm');
    if (rm) rm.onclick = guard(async () => { await api('DELETE', '/settings/cookies'); reload(); });
  })().catch((e) => toast(e.message, true));
  return {};
}

/* -------------------------------------------------------------- router */
const views = { channels: channelsView, recordings: recordingsView, settings: settingsView };
function route() {
  const name = (location.hash.match(/^#\/(\w+)/) || [])[1];
  const key = views[name] ? name : 'channels';
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === key));
  closeModal();
  current = views[key]();
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
