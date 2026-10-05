'use strict';
// FolderManager フロントエンド(ビルド不要の素のJS)

// ===== 共通ユーティリティ =====
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmtNum = n => (n ?? 0).toLocaleString('ja-JP');
function fmtSize(b) {
  if (b == null) return '';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = b;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}
function fmtDate(t) {
  if (!t || t <= 0) return '';
  const d = new Date(t * 1000), p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}/${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
const pct = (a, b) => b > 0 ? a / b * 100 : 0;
const srcLabel = m => ({ excel: 'Excel取込', scan: 'フォルダスキャン', merge: 'DB統合' }[m.source] || m.source || '');
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch (e) { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch (e) { } },
};

async function api(path, body) {
  const opt = { method: body ? 'POST' : 'GET', headers: { 'X-Token': TOKEN } };
  if (body) { opt.body = JSON.stringify(body); opt.headers['Content-Type'] = 'application/json'; }
  const r = await fetch(path, opt);
  const j = await r.json().catch(() => ({ error: r.statusText }));
  if (!r.ok) {
    if (j.error === 'NEED_CODE') { CodeGate.prompt(); throw Object.assign(new Error('作業者コードが必要です'), { quiet: true }); }
    throw new Error(j.error || r.statusText);
  }
  return j;
}
function toast(msg, err) {
  const d = document.createElement('div');
  d.textContent = msg;
  if (err) d.className = 'err';
  $('#toast').appendChild(d);
  setTimeout(() => d.remove(), err ? 7000 : 3500);
}
const guard = fn => async (...a) => { try { return await fn(...a); } catch (e) { if (!e.quiet) toast(e.message, true); } };
function download(path, params) {
  const q = new URLSearchParams(params || {});
  q.set('token', TOKEN);
  location.href = path + '?' + q;
}
function modal(html, onOk, okLabel = 'OK', cancelLabel = 'キャンセル') {
  const m = $('#modal');
  m.innerHTML = `<div class="box">${html}<div class="foot"><button data-x>${cancelLabel}</button>${onOk ? `<button class="primary" data-ok>${okLabel}</button>` : ''}</div></div>`;
  m.hidden = false;
  const close = () => { m.hidden = true; m.innerHTML = ''; };
  $('[data-x]', m).onclick = close;
  if (onOk) $('[data-ok]', m).onclick = guard(async () => { if (await onOk(m) !== false) close(); });
  m.close = close;
  return m;
}
const closeModal = () => { const m = $('#modal'); m.hidden = true; m.innerHTML = ''; };

// ===== 状態 =====
const S = {
  state: null, settings: null, rules: null, checks: [], code: '',
  dbv: 0,     // DBを開き直すたびに増える(各画面の再初期化判定)
  planv: 0,   // アクションが変わるたびに増える(画面の再読み込み判定)
  tags: [], owners: [],
  sel: [], selGrid: null, view: 'import',
  vTarget: 'root', vTargetPath: '',
};

// フラグ(Go側 fsdb/flags.go と同じ値)
const FL = { badchar: 1, trailing: 2, reserved: 4, access: 8, empty: 16, temp: 32, copy: 64, version: 128, single: 256, dup: 512 };
const MIG = FL.badchar | FL.trailing | FL.reserved | FL.access;
const ACT = { delete: '削除', move: '移動', hold: '保留' };

function warnings(r) {
  const st = S.settings || { oldYears: 3, pathLimit: 250, deepDepth: 8, manyFiles: 500 };
  const w = [], f = r.f;
  const add = (c, l, t) => w.push({ c, l, t });
  if (f & FL.badchar) add('mig', '禁止文字', 'SharePointで使えない文字(\\ / : * ? " < > | # %)を含む');
  if (f & FL.trailing) add('mig', '末尾.空白', '名前の末尾が「.」または半角スペース');
  if (f & FL.reserved) add('mig', '予約語', 'CON, PRN, AUX, NUL, COM1-9, LPT1-9');
  if (f & FL.access) add('mig', '🔒アクセス不可', (r.err || 'スキャン時にアクセスできなかった') + (r.dir ? '(中身・サイズは不明)' : ''));
  if (r.pl > st.pathLimit) add('mig', 'パス長', `相対パスが${st.pathLimit}文字超(${r.pl}文字)`);
  if (f & FL.empty) add('org', '空', '空フォルダ');
  if (f & FL.temp) add('org', '一時', '一時・システムファイル');
  if (f & FL.dup) add('org', '重複?', '同名・同サイズのファイルが他にもある');
  if (f & FL.copy) add('org', 'コピー名', '「- コピー」「(1)」「新しいフォルダー」等');
  if (f & FL.version) add('org', '版管理名', '旧/old/bk/_v2/最新 等');
  if (f & FL.single) add('org', '1フォルダのみ', '中身がフォルダ1つだけ(階層を浅くできる候補)');
  if (isDeep(r)) add('org', '深い', `階層${r.d}(${st.deepDepth}階層以上で警告)`);
  if (r.dir && r.cc > st.manyFiles) add('org', '項目過多', `直下に${r.cc}項目`);
  if (!r.dir && r.m > 0 && r.m < Date.now() / 1000 - st.oldYears * 365.25 * 86400) add('org', `${st.oldYears}年超`, `更新から${st.oldYears}年以上`);
  if (r.r5s) w.unshift({ c: 'r5s', l: '⛔5S外れ', t: r.r5s });
  return w;
}
const warnHTML = r => warnings(r).map(x => `<span class="b ${x.c}" title="${esc(x.t)}">${esc(x.l)}</span>`).join('');
const isDeep = r => r.d >= (S.settings?.deepDepth ?? 8);
const eff = r => r.act || r.ia || '';
const rowClass = r => {
  const e = eff(r);
  const base = (r.f & MIG) || (r.pl > (S.settings?.pathLimit ?? 250)) ? 'mig' : isDeep(r) ? 'deep' : (r.dir ? 'dir' : '');
  return base + (e === 'delete' ? ' del' : e === 'move' ? ' mov' : e === 'hold' ? ' hold' : '') + (r.r5s ? ' r5s' : '') + (r.tf === 2 ? ' tfpath' : '');
};
const vroot = () => S.rules?.rootName || '整理後';
const vpathLabel = p => p ? vroot() + '\\' + p : vroot();

function actHTML(r) {
  if (r.act === 'delete') return `<span class="actb delete">削除</span>${memoMark(r)}`;
  if (r.act === 'hold') return `<span class="actb hold">⏸ 保留</span>${memoMark(r)}`;
  if (r.act === 'move') return `<span class="actb move">移動</span> <span title="${esc(vpathLabel(r.vpath))}">→ ${esc(vpathLabel(r.vpath))}</span>${r.nn ? ` <span class="muted">名前: ${esc(r.nn)}</span>` : ''}${memoMark(r)}`;
  if (r.ia) return `<span class="actb inh" title="親フォルダに設定されたアクションが及んでいます">親で${ACT[r.ia]}</span>${r.ivpath ? ` <span class="muted">→ ${esc(vpathLabel(r.ivpath))}</span>` : ''}${memoMark(r)}`;
  return memoMark(r);
}
const memoMark = r => r.memo ? ` <span title="${esc(r.memo)}">📝</span>` : '';
// 担当は4段(部 / 課 / 担当 / 担当者)。保存形式は \x1f 区切り(区切りの無い旧形式は「担当」の段)
const OWNER_LV = ['部', '課', '担当', '担当者'];
const ownerFields = s => { if (!s) return ['', '', '', '']; const a = s.split('\x1f'); return a.length === 1 ? ['', '', a[0], ''] : [...a, '', '', '', ''].slice(0, 4); };
const ownerLabel = f => f.map((v, i) => v ? `${OWNER_LV[i]}: ${v}` : '').filter(Boolean).join(' / ');
// 実際の担当(空欄の段は親フォルダから引き継ぐ)を表示。この項目に設定した段は太字、引き継いだ段は灰色
const ownerHTML = r => {
  const eff = ownerFields(r.iown), own = ownerFields(r.own);
  if (!eff.some(Boolean)) return '';
  return `<span title="${esc(eff.map((v, i) => v ? `${OWNER_LV[i]}: ${v}${own[i] ? '' : '(親フォルダから)'}` : '').filter(Boolean).join('\n'))}">${eff.map((v, i) => v ? `<span class="${own[i] ? 'own' : 'iown'}">${esc(v)}</span>` : '').filter(Boolean).join('<span class="iown"> / </span>')}</span>`;
};
const tagsHTML = tags => (tags || []).map(t => `<span class="tag">${esc(t)}</span>`).join('');
function dueHTML(due) {
  if (!due) return '';
  const over = due < new Date().toISOString().slice(0, 10);
  return `<span class="due ${over ? 'over' : ''}" title="${over ? '期限切れ' : '期限'}">⏳${esc(due)}</span>`;
}

// ===== 仮想スクロールのグリッド =====
const Drag = { on: false, rows: null, ghost: null, target: null, tgrid: null };

class Grid {
  constructor(host, o) {
    this.o = Object.assign({ rh: 24, keyOf: r => r.id }, o);
    this.host = host;
    host.tabIndex = 0;
    host.innerHTML = '<div class="g-scroll"><div class="g-head"></div><div class="g-spacer"></div><div class="g-rows"></div></div><div class="g-empty" hidden></div>';
    this.sc = $('.g-scroll', host); this.head = $('.g-head', host);
    this.spacer = $('.g-spacer', host); this.rowsEl = $('.g-rows', host); this.empty = $('.g-empty', host);
    this.sel = new Map(); this.anchor = -1; this.cursor = -1;
    this.src = { count: 0, get: () => null };
    this.cols = this.o.columns;
    host.grid = this;
    this.renderHead();
    this.sc.addEventListener('scroll', () => this.schedule());
    new ResizeObserver(() => this.schedule()).observe(this.sc);
    this.rowsEl.addEventListener('mousedown', e => this.onDown(e));
    host.addEventListener('keydown', e => this.onKey(e));
    // 右クリック: 未選択の行なら選択し直してからメニュー(行の外ならメニューのみ)
    this.sc.addEventListener('contextmenu', e => {
      if (!this.o.onContext) return;
      e.preventDefault();
      const i = this.idxOf(e.target), r = i >= 0 ? this.src.get(i) : null;
      if (r && !this.sel.has(this.key(r))) {
        this.sel.clear(); this.sel.set(this.key(r), r); this.anchor = this.cursor = i;
        this.render();
        if (!Pick.on) this.o.onSelect?.(this.selected());
      }
      this.host.focus({ preventScroll: true });
      this.o.onContext(r ? this.selected() : [], e, r);
    });
  }
  key(r) { return this.o.keyOf(r); }
  renderHead() {
    const w = this.cols.reduce((a, c) => a + c.w, 0);
    this.width = w;
    this.head.style.width = this.rowsEl.style.width = w + 'px';
    this.head.innerHTML = this.cols.map((c, k) => {
      const s = this.o.sort && this.o.sort.key === c.key ? (this.o.sort.desc ? ' ▼' : ' ▲') : '';
      return `<div class="g-hc ${c.sort ? 'sortable' : ''} ${c.cls || ''}" data-k="${k}" style="width:${c.w}px" title="${esc(c.tip || c.label)}">${esc(c.label)}${s}<span class="rz"></span></div>`;
    }).join('');
    $$('.g-hc', this.head).forEach(h => {
      const c = this.cols[+h.dataset.k];
      h.addEventListener('click', e => {
        if (e.target.classList.contains('rz') || !c.sort) return;
        const desc = this.o.sort && this.o.sort.key === c.key ? !this.o.sort.desc : !!c.descFirst;
        this.o.sort = { key: c.key, desc };
        this.renderHead();
        this.o.onSort?.(this.o.sort);
      });
      $('.rz', h).addEventListener('mousedown', e => {
        e.preventDefault(); e.stopPropagation();
        const x0 = e.clientX, w0 = c.w;
        const mv = ev => { c.w = Math.max(30, w0 + ev.clientX - x0); this.renderHead(); this.render(); };
        const up = () => { removeEventListener('mousemove', mv); removeEventListener('mouseup', up); };
        addEventListener('mousemove', mv); addEventListener('mouseup', up);
      });
    });
  }
  setSource(src, keep) {
    this.src = src;
    if (!keep) { this.sc.scrollTop = 0; this.sel.clear(); this.anchor = this.cursor = -1; }
    else this.remap();
    this.refresh();
  }
  // データを読み直した後、選択を新しい行オブジェクトに付け替える
  remap() {
    if (!this.sel.size || !this.src.forEach) return;
    const next = new Map();
    this.src.forEach(r => { const k = this.key(r); if (this.sel.has(k)) next.set(k, r); });
    for (const [k, r] of this.sel) if (!next.has(k)) next.set(k, r);
    this.sel = next;
  }
  setEmpty(msg) { this.emptyMsg = msg; }
  refresh() {
    this.spacer.style.height = this.src.count * this.o.rh + 'px';
    this.spacer.style.width = this.width + 'px';
    this.empty.hidden = this.src.count > 0 || !this.emptyMsg;
    this.empty.textContent = this.emptyMsg || '';
    this.render();
  }
  schedule() { if (!this.raf) this.raf = requestAnimationFrame(() => { this.raf = 0; this.render(); }); }
  render() {
    const rh = this.o.rh, n = this.src.count;
    const top = this.sc.scrollTop, h = this.sc.clientHeight;
    const s = Math.max(0, Math.floor(top / rh) - 5), e = Math.min(n, Math.ceil((top + h) / rh) + 5);
    this.src.ensure?.(s, e, () => this.schedule());
    let html = '';
    for (let i = s; i < e; i++) {
      const r = this.src.get(i);
      if (!r) { html += `<div class="g-row loading" style="top:${i * rh}px;width:${this.width}px">読み込み中…</div>`; continue; }
      const cls = (this.o.rowClass?.(r) || '') + (this.sel.has(this.key(r)) ? ' sel' : '') + (i === this.cursor ? ' cur' : '') + (Drag.target === r ? ' drop' : '');
      html += `<div class="g-row ${cls}" data-i="${i}" style="top:${i * rh}px">`;
      for (const c of this.cols) html += `<div class="g-cell ${c.cls || ''}" style="width:${c.w}px">${c.render(r)}</div>`;
      html += '</div>';
    }
    this.rowsEl.innerHTML = html;
  }
  idxOf(el) { const r = el?.closest?.('.g-row'); return r && r.dataset.i ? +r.dataset.i : -1; }
  selected() { return [...this.sel.values()]; }
  emitSel() { this.render(); this.o.onSelect?.(this.selected()); }
  onDown(e) {
    if (e.button === 2) return; // 右クリックは contextmenu で処理
    const i = this.idxOf(e.target); if (i < 0) return;
    const r = this.src.get(i); if (!r) return;
    e.preventDefault(); // 行の再描画でフォーカス・ダブルクリックが失われないよう自前で処理する
    this.host.focus({ preventScroll: true });
    if (e.target.closest('.tw')) { this.o.onToggle?.(r, i); return; }
    if (e.detail === 2 && !e.shiftKey && !e.ctrlKey) { this.o.onOpen?.(r, i); return; }
    const already = this.sel.has(this.key(r));
    const plain = !e.shiftKey && !e.ctrlKey && !e.metaKey;
    if (e.shiftKey && this.anchor >= 0) {
      this.sel.clear();
      const [a, b] = [Math.min(this.anchor, i), Math.max(this.anchor, i)];
      for (let j = a; j <= b; j++) { const x = this.src.get(j); if (x) this.sel.set(this.key(x), x); }
    } else if (e.ctrlKey || e.metaKey) {
      already ? this.sel.delete(this.key(r)) : this.sel.set(this.key(r), r);
      this.anchor = i;
    } else if (!(already && this.o.draggable && this.sel.size > 1)) { // 複数選択中の行を掴んだ場合はそのままドラッグできる
      this.sel.clear(); this.sel.set(this.key(r), r); this.anchor = i;
    }
    this.cursor = i;
    this.emitSel();
    if (e.button !== 0) return;
    if (this.o.draggable && this.sel.size && !Pick.on) this.armDrag(e, plain && already ? r : null);
  }
  // ドラッグ&ドロップ(ブラウザ標準のドラッグは行の再描画で途切れるため自前で実装)
  armDrag(e, clickedSel) {
    const x0 = e.clientX, y0 = e.clientY;
    let started = false;
    const mv = ev => {
      if (!started) {
        if (Math.abs(ev.clientX - x0) + Math.abs(ev.clientY - y0) < 6) return;
        started = true;
        Object.assign(Drag, { on: true, rows: this.selected(), target: null, tgrid: null });
        Drag.ghost = document.createElement('div');
        Drag.ghost.id = 'dragghost';
        document.body.appendChild(Drag.ghost);
      }
      Drag.ghost.style.left = ev.clientX + 14 + 'px';
      Drag.ghost.style.top = ev.clientY + 10 + 'px';
      const el = document.elementFromPoint(ev.clientX, ev.clientY);
      const host = el?.closest?.('.grid');
      let t = null, g = null;
      if (host?.grid?.o.canDrop) {
        g = host.grid;
        const r = g.src.get(g.idxOf(el));
        if (r && g.o.canDrop(r, Drag.rows)) t = r;
      }
      if (t !== Drag.target) { const old = Drag.tgrid; Drag.target = t; Drag.tgrid = g; old?.render(); g?.render(); }
      Drag.ghost.textContent = t ? `${Drag.rows.length}件 → ${t.uuid === 'root' ? vroot() : t.n}` : `${Drag.rows.length}件を移動(右の仮想フォルダへ)`;
    };
    const up = () => {
      removeEventListener('mousemove', mv); removeEventListener('mouseup', up);
      if (!started) {
        if (clickedSel && this.sel.size > 1) { this.sel.clear(); this.sel.set(this.key(clickedSel), clickedSel); this.emitSel(); }
        return;
      }
      Drag.ghost.remove();
      const t = Drag.target, g = Drag.tgrid, rows = Drag.rows;
      Object.assign(Drag, { on: false, target: null, rows: null, tgrid: null });
      g?.render();
      if (t) g.o.onDrop(t, rows);
    };
    addEventListener('mousemove', mv); addEventListener('mouseup', up);
  }
  moveTo(i, extend) {
    const n = this.src.count; if (!n) return;
    i = Math.max(0, Math.min(n - 1, i));
    const r = this.src.get(i); if (!r) return;
    if (!extend) { this.sel.clear(); this.anchor = i; }
    this.sel.set(this.key(r), r);
    this.cursor = i;
    const rh = this.o.rh, top = i * rh, vh = this.sc.clientHeight - 26;
    if (top < this.sc.scrollTop) this.sc.scrollTop = top;
    else if (top + rh > this.sc.scrollTop + vh) this.sc.scrollTop = top + rh - vh;
    this.emitSel();
  }
  onKey(e) {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
    const i = this.cursor, r = i >= 0 ? this.src.get(i) : null;
    const page = Math.floor((this.sc.clientHeight - 26) / this.o.rh);
    const k = e.key;
    if (k === 'ArrowDown') this.moveTo(i + 1, e.shiftKey);
    else if (k === 'ArrowUp') this.moveTo(i - 1, e.shiftKey);
    else if (k === 'PageDown') this.moveTo(i + page, e.shiftKey);
    else if (k === 'PageUp') this.moveTo(i - page, e.shiftKey);
    else if (k === 'Home') this.moveTo(0);
    else if (k === 'End') this.moveTo(this.src.count - 1);
    else if (k === 'Enter' && r) this.o.onOpen?.(r, i);
    else if ((k === 'ArrowRight' || k === 'ArrowLeft') && r && this.o.onArrow) this.o.onArrow(r, i, k === 'ArrowRight');
    else if (k === 'a' && (e.ctrlKey || e.metaKey) && this.src.rows) {
      this.sel.clear(); this.src.rows.forEach(x => this.sel.set(this.key(x), x)); this.emitSel();
    } else if (this.o.onKeyAction && this.sel.size && !e.ctrlKey && this.o.onKeyAction(k, e)) { /* 処理済み */ }
    else return;
    e.preventDefault();
  }
  focusKey(key) {
    for (let i = 0; i < this.src.count; i++) {
      const r = this.src.get(i);
      if (r && this.key(r) === key) { this.moveTo(i); this.sc.scrollTop = Math.max(0, i * this.o.rh - this.sc.clientHeight / 3); this.host.focus(); return true; }
    }
    return false;
  }
}

class ArraySource {
  constructor(rows) { this.rows = rows; }
  get count() { return this.rows.length; }
  get(i) { return this.rows[i]; }
  forEach(fn) { this.rows.forEach(fn); }
}

// サーバー側ページング(検索結果など数十万行でも軽い)
class PagedSource {
  constructor(urlFn, onTotal) { this.urlFn = urlFn; this.onTotal = onTotal; this.pages = new Map(); this.loading = new Set(); this.count = 0; this.ps = 500; }
  async init() { await this.load(0); return this; }
  get(i) { const p = this.pages.get(Math.floor(i / this.ps)); return p ? p[i % this.ps] : null; }
  ensure(s, e, cb) {
    for (let p = Math.floor(s / this.ps); p <= Math.floor(Math.max(s, e - 1) / this.ps); p++)
      if (!this.pages.has(p) && !this.loading.has(p) && p * this.ps < this.count) this.load(p).then(cb, err => toast(err.message, true));
  }
  async load(p) {
    this.loading.add(p);
    try {
      const r = await api(this.urlFn(p * this.ps, this.ps));
      this.pages.set(p, r.rows); this.count = r.total; this.onTotal?.(r.total);
    } finally { this.loading.delete(p); }
  }
  forEach(fn) { for (const rows of this.pages.values()) rows.forEach(fn); }
}

// 遅延読み込みのツリー(現在のフォルダ構成)
class TreeModel {
  constructor(o) { this.o = o || {}; this.rows = []; }
  get count() { return this.rows.length; }
  get(i) { return this.rows[i]; }
  forEach(fn) { this.rows.forEach(fn); }
  key(r) { return r.id; }
  q() { return (this.o.dirs ? '&dirs=1' : '') + (this.o.hide ? '&hide=1' : '') + (this.o.owner ? '&owner=' + encodeURIComponent(this.o.owner) : '') + (this.o.tf ? '&tf=' + this.o.tf : ''); }
  hasKids(r) { return r.dir && (this.o.dirs ? r.dc > 0 : r.cc > 0); }
  prep(r, parent) {
    r._anc = parent ? parent._anc + (parent.d >= 1 ? (parent._last ? '　' : '┃') : '') : '';
    r._has = this.hasKids(r);
    r._open = false;
    r._ps = parent ? parent.s : r.s; // 親フォルダのサイズ(サイズゲージの基準)
  }
  markLast(list, pkey = r => r.p) {
    const last = new Map();
    for (const r of list) last.set(pkey(r), this.key(r));
    for (const r of list) r._last = last.get(pkey(r)) === this.key(r);
  }
  async loadRoot() { return (await api('/api/node?id=1')).node; }
  async fetchKids(r) { return api(`/api/children?id=${r.id}${this.q()}`); }
  async load() {
    const root = await this.loadRoot();
    root._last = true; this.prep(root, null);
    this.rows = [root];
    await this.expand(0);
  }
  indexOf(key) { return this.rows.findIndex(r => this.key(r) === key); }
  async expand(i) {
    const r = this.rows[i];
    if (!r || !r._has || r._open) return;
    const kids = await this.fetchKids(r);
    this.markLast(kids, () => 0);
    kids.forEach(k => this.prep(k, r));
    r._open = true;
    this.rows = this.rows.slice(0, i + 1).concat(kids, this.rows.slice(i + 1));
  }
  collapse(i) {
    const r = this.rows[i];
    let j = i + 1;
    while (j < this.rows.length && this.rows[j].d > r.d) j++;
    this.rows.splice(i + 1, j - i - 1);
    r._open = false;
  }
  async expandDepth(i, depth) {
    const r = this.rows[i];
    if (!r._has) return;
    const sub = await api(`/api/subtree?id=${r.id}&depth=${depth}${this.q()}`);
    this.collapse(i);
    this.markLast(sub);
    const byId = new Map([[r.id, r]]);
    for (const x of sub) { this.prep(x, byId.get(x.p)); byId.set(x.id, x); }
    for (const x of sub) if (x._has && x.d < r.d + depth) x._open = true;
    r._open = true;
    this.rows = this.rows.slice(0, i + 1).concat(sub, this.rows.slice(i + 1));
  }
  // 開いていたフォルダを保ったまま読み直す(アクション変更後の表示更新用)
  async reloadKeep() {
    const open = new Set(this.rows.filter(r => r._open).map(r => this.key(r)));
    const root = await this.loadRoot();
    root._last = true; this.prep(root, null);
    this.rows = [root];
    for (let i = 0; i < this.rows.length; i++) if (open.has(this.key(this.rows[i]))) await this.expand(i);
  }
  async reveal(id) {
    const { node, ancestors } = await api('/api/node?id=' + id);
    for (const a of ancestors) {
      const i = this.indexOf(a.id);
      if (i < 0) return -1;
      await this.expand(i);
    }
    return this.indexOf(node.id);
  }
}

// 整理後のフォルダ構成(仮想ツリー)。行 = 仮想フォルダ(vdir) または移動して配置された実フォルダ/ファイル
class VTreeModel extends TreeModel {
  key(r) { return r.key; }
  hasKids(r) { return r.kind === 'vdir' ? r.cc > 0 : r.kind === 'dir' && r.node.cc > 0; }
  async loadRoot() { return vrow(await api('/api/vnode?uuid=root')); }
  async fetchKids(r) {
    const list = r.kind === 'vdir' ? await api('/api/vchildren?uuid=' + encodeURIComponent(r.uuid)) : await api(`/api/vreal?id=${r.node.id}&depth=${r.d}`);
    return list.map(vrow);
  }
  async revealV(uuid) {
    const all = await api('/api/vtree');
    const chain = [];
    for (let v = all.find(x => x.uuid === uuid); v && v.uuid !== 'root'; v = all.find(x => x.uuid === v.parent)) chain.unshift(v.uuid);
    const root = this.indexOf('v:root');
    if (root >= 0 && chain.length) await this.expand(root);
    for (const u of chain.slice(0, -1)) { const i = this.indexOf('v:' + u); if (i >= 0) await this.expand(i); }
    return this.indexOf('v:' + uuid);
  }
}
function vrow(v) {
  const r = Object.assign({}, v);
  if (r.node) { // 実フォルダ/ファイルはノードの情報も行に持たせる(パネル・警告表示用)
    for (const k of ['id', 'dir', 'x', 'm', 'f', 'pl', 'act', 'ia', 'nn', 'due', 'memo', 'tags', 'path', 'ivpath', 'ed', 'p', 'e', 'cc', 'dc', 'rem', 'own', 'iown', 'r5s', 'err']) r[k] = r.node[k];
    r.nvpath = r.node.vpath;
    r.realD = r.node.d;
  }
  return r;
}

function nameCell(r, tree, label) {
  const icon = r.kind === 'vlink' ? '<span class="lnk" title="ショートカット(ダブルクリックでリンク先へ)">↪</span>' : r.kind === 'vdir' ? '🗂' : (r.f & FL.access) ? '🔒' : r.dir ? (r._open ? '📂' : '📁') : fileIcon(r.x);
  const nm = `<span class="nm" title="${esc(r.path || r.n)}">${esc(label ?? r.n)}</span>`;
  if (!tree) return `<span class="ico">${icon}</span>${nm}`;
  const conn = r.d === 0 ? '' : (r._last ? '┗' : '┣');
  const tw = r._has ? `<span class="tw">${r._open ? '▼' : '▶'}</span>` : '<span class="tw"></span>';
  return `<span class="tl">${r._anc}${conn}</span>${tw}<span class="ico">${icon}</span>${nm}`;
}
function fileIcon(x) {
  if (/^(xlsx?|xlsm|csv)$/.test(x)) return '📊';
  if (/^(docx?|rtf|txt|md)$/.test(x)) return '📝';
  if (/^(pptx?)$/.test(x)) return '📽';
  if (x === 'pdf') return '📕';
  if (/^(jpe?g|png|gif|bmp|tiff?|heic|svg)$/.test(x)) return '🖼';
  if (/^(zip|7z|rar|lzh|gz|tar)$/.test(x)) return '🗜';
  if (/^(mp4|mov|avi|wmv|mp3|wav|m4a)$/.test(x)) return '🎞';
  return '📄';
}
function gaugeHTML(p, label, title) {
  return `<div class="gauge" title="${esc(title || '')}"><div class="tr"><i style="width:${Math.min(100, p).toFixed(2)}%"></i></div><span>${label}</span></div>`;
}

// 列定義(画面ごとに組み合わせて使う)
const COL = {
  depth: { key: 'depth', label: '階層', w: 44, cls: 'num', sort: true, tip: 'ルート(選択フォルダ)を0とした階層の深さ', render: r => isDeep(r) ? `<b class="deepnum" title="${S.settings?.deepDepth ?? 8}階層以上">${r.d}</b>` : r.d },
  kind: { key: 'kind', label: '種別', w: 64, render: r => r.dir ? 'フォルダ' : 'ファイル', sort: true },
  ext: { key: 'ext', label: '拡張子', w: 60, render: r => esc(r.x), sort: true },
  mtime: { key: 'mtime', label: '更新日時', w: 124, render: r => fmtDate(r.m), sort: true, descFirst: true },
  size: { key: 'size', label: 'サイズ', w: 80, cls: 'num', render: r => r.dir && (r.f & FL.access) ? `<span class="muted" title="${esc(r.err || 'アクセス不可')}">不明</span>` : fmtSize(r.s), sort: true, descFirst: true },
  warn: { key: 'warn', label: '警告・整理のヒント', w: 200, render: warnHTML },
  action: { key: 'action', label: 'アクション(移動先)', w: 250, render: actHTML, sort: true },
  tags: { key: 'tags', label: 'タグ', w: 140, render: r => tagsHTML(r.tags) },
  owner: { key: 'owner', label: '担当(部/課/担当/担当者)', w: 180, render: ownerHTML, sort: true, tip: '担当(フォルダに設定すると配下に引き継がれます)' },
  due: { key: 'due', label: '期限', w: 96, render: r => dueHTML(r.due), sort: true },
  memo: { key: 'memo', label: 'メモ', w: 180, render: r => esc(r.memo) },
  editor: { key: 'editor', label: '作業者', w: 100, render: r => esc(r.ed), sort: true },
  path: { key: 'path', label: 'フルパス', w: 460, render: r => `<span title="${esc(r.path)}">${esc(r.path)}</span>`, sort: true },
};
const cols = (...ks) => ks.map(k => typeof k === 'string' ? { ...COL[k] } : k);

// ===== アクションの操作(すべての画面で共通) =====
const Plan = {
  ids: rows => rows.filter(r => typeof r.id === 'number' && r.kind !== 'vdir').map(r => r.id),
  async delete(rows) {
    const ids = this.ids(rows); if (!ids.length) return;
    const r = await api('/api/plan/delete', { ids });
    toast(`${fmtNum(r.applied)}件に「削除」を設定しました`);
    await afterEdit();
  },
  async hold(rows) {
    const ids = this.ids(rows); if (!ids.length) return;
    const r = await api('/api/plan/hold', { ids });
    toast(`${fmtNum(r.applied)}件を「保留」にしました`);
    await afterEdit();
  },
  async owner(rows, fields) {
    const ids = this.ids(rows); if (!ids.length) return;
    const r = await api('/api/plan/owner', { ids, owner: fields });
    const l = ownerLabel(fields);
    toast(l ? `${fmtNum(r.applied)}件の担当を「${l}」にしました` : `${fmtNum(r.applied)}件の担当を解除しました`);
    await loadOwners();
    await afterEdit();
  },
  async clear(rows) {
    const ids = this.ids(rows); if (!ids.length) return;
    const r = await api('/api/plan/clear', { ids });
    toast(`${fmtNum(r.applied)}件のアクションを解除しました`);
    await afterEdit();
  },
  // 移動先が決まっていなければ、仮想フォルダの選択ダイアログを出す
  async move(rows, target) {
    const ids = this.ids(rows); if (!ids.length) return;
    if (!target) target = await VPicker.pick(`${ids.length}件の移動先を選択`);
    if (!target) return;
    const r = await api('/api/plan/move', { ids, target });
    showReport(r, '移動');
    await afterEdit();
  },
  async fields(rows, f) {
    const ids = this.ids(rows); if (!ids.length) return;
    const r = await api('/api/plan/fields', { ids, ...f });
    if (r.issues?.length) showReport(r, '名前変更');
    else toast('保存しました');
    await afterEdit();
  },
  async tags(rows, add, remove) {
    const ids = this.ids(rows); if (!ids.length) return;
    await api('/api/tags', { ids, add, remove });
    add.forEach(t => S.tags.includes(t) || S.tags.push(t)); updateTagList();
    await afterEdit();
  },
};
// 5Sルールのチェック結果を表示
function showReport(r, verb) {
  const blocked = (r.issues || []).filter(i => i.blocks?.length), warned = (r.issues || []).filter(i => !i.blocks?.length && i.warns?.length);
  if (!blocked.length && !warned.length) return toast(`${fmtNum(r.applied)}件に「${verb}」を設定しました`);
  const li = (i, cls) => `<div class="issue ${cls}"><b>${esc(i.name)}</b><ul>${[...(i.blocks || []), ...(i.warns || [])].map(m => `<li>${esc(m)}</li>`).join('')}</ul></div>`;
  modal(`<h2>${verb}: ${fmtNum(r.applied)}件を設定しました</h2>
    ${blocked.length ? `<h3>整理ルール(5S)に合わないため設定しなかった項目: ${blocked.length}件</h3>${blocked.slice(0, 50).map(i => li(i, 'block')).join('')}` : ''}
    ${warned.length ? `<h3>警告(設定はしています): ${warned.length}件</h3>${warned.slice(0, 50).map(i => li(i, '')).join('')}` : ''}
    <p class="hint">ルールは「⚙ オプション」→「整理後の構成ルール(5S)」で変更できます。</p>`, null, '', '閉じる');
}
function showIssue(is, verb) {
  if (!is || (!is.blocks?.length && !is.warns?.length)) return false;
  const msgs = [...(is.blocks || []), ...(is.warns || [])];
  toast(`${verb}${is.blocks?.length ? 'できません' : '(警告)'}: ${msgs.join(' / ')}`, !!is.blocks?.length);
  return !!is.blocks?.length;
}

async function afterEdit() {
  S.planv++;
  const v = Views[S.view];
  if (v?.afterEdit) { await v.afterEdit(); v.planv = S.planv; }
  if (S.selGrid) { S.selGrid.remap(); S.sel = S.selGrid.selected(); }
}

// ===== ファイルを既定のアプリで開く(「これって何だっけ？」をすぐ確認する) =====
// プログラムやスクリプトはダブルクリックで誤って実行しないよう確認する
const RISKY = /^(exe|com|bat|cmd|msi|msp|ps1|psm1|vbs|vbe|js|jse|wsf|wsh|hta|scr|pif|lnk|reg|jar|cpl|inf|url|application|appref-ms)$/;
async function openFile(r) {
  if (!r || r.dir || r.kind === 'vdir' || typeof r.id !== 'number') return;
  const go = async () => { await api('/api/openfile', { id: r.id }); toast(`「${r.node?.n ?? r.n}」を開きました`); };
  if (RISKY.test(r.x || '')) {
    return modal(`<h2>このファイルを開きますか？</h2><p>「${esc(r.node?.n ?? r.n)}」はプログラムまたはスクリプト(.${esc(r.x)})です。開くと<b>実行されます</b>。</p>
      <p class="hint">中身を確認したいだけの場合は「Windowsで開く」(エクスプローラーで場所を表示)を使ってください。</p>`, go, '実行して開く');
  }
  await go();
}

// ===== 仮想フォルダの選択ダイアログ(移動先を整理後のツリーから選ぶ) =====
const VPicker = {
  pick(title) {
    return new Promise(async resolve => {
      let all;
      try { all = await api('/api/vtree'); } catch (e) { toast(e.message, true); return resolve(null); }
      let cur = 'root';
      const render = m => {
        $('.vpick', m).innerHTML = all.map(v => `<div data-u="${esc(v.uuid)}" class="${v.uuid === cur ? 'on' : ''}" style="padding-left:${6 + v.d * 18}px">🗂 ${esc(v.uuid === 'root' ? vroot() : v.n)} <span class="muted">${v.fc ? fmtNum(v.fc) + '件' : ''}</span></div>`).join('');
        $$('.vpick div', m).forEach(d => {
          d.onclick = () => { cur = d.dataset.u; render(m); };
          d.ondblclick = () => { closeModal(); resolve(d.dataset.u); };
        });
      };
      const m = modal(`<h2>${esc(title)}</h2><p class="hint">整理後のフォルダ構成から移動先を選んでください(ダブルクリックで決定)。</p><div class="vpick"></div>
        <div style="display:flex;gap:6px;margin-top:8px"><input type="text" id="vp-new" placeholder="選択中のフォルダの下に新しいフォルダを作成" style="flex:1"><button id="vp-add">作成</button></div>`,
        () => { resolve(cur); }, 'ここへ移動');
      render(m);
      $('[data-x]', m).addEventListener('click', () => resolve(null));
      const add = guard(async () => {
        const name = $('#vp-new', m).value.trim(); if (!name) return;
        const r = await api('/api/vcreate', { parent: cur, name });
        if (showIssue(r.issue, 'フォルダ作成')) return;
        all = await api('/api/vtree'); cur = r.uuid; $('#vp-new', m).value = ''; render(m);
      });
      $('#vp-add', m).onclick = add;
      $('#vp-new', m).onkeydown = e => { if (e.key === 'Enter') add(); };
    });
  },
};

// ===== 作業者コード(誰の変更かを記録する。複数人で編集するときの統合に使う) =====
const CodeGate = {
  prompt() {
    const stem = (S.state?.db?.path || '').split(/[\\/]/).pop().replace(/\.db$/i, '');
    modal(`<h2>作業者コードが必要です</h2>
      <p class="hint">このDBは「マスター」(作業者コードなし)です。アクションを設定するには、作業者コード(部署名・氏名など。日本語可)が必要です。<br>
      複数人で作業する場合は、<b>作業用コピーを作成</b>して各自で編集し、最後に「ファイル」→「アクションの統合」でまとめます。</p>
      <div class="form" style="grid-template-columns:120px 1fr"><span>作業者コード</span><input type="text" id="cg-code" placeholder="例: 経理部_山田"></div>
      <p class="hint">作業用コピーの保存先: ${esc(S.state?.projectsDir || '')}\\${esc(stem)}_<i>作業者コード</i>.db</p>
      <div style="margin-top:6px"><button id="cg-direct">このDBに直接設定する(1人で使う場合)</button></div>`,
      async m => {
        const code = $('#cg-code', m).value.trim();
        if (!code) { toast('作業者コードを入力してください', true); return false; }
        await api('/api/workcopy', { code });
        await reopened('作業用コピーを作成して開きました。もう一度操作してください');
      }, '作業用コピーを作成して開く(推奨)');
    $('#cg-direct').onclick = guard(async () => {
      const code = $('#cg-code').value.trim();
      if (!code) return toast('作業者コードを入力してください', true);
      await api('/api/setcode', { code });
      closeModal();
      await refreshState();
      toast('作業者コードを設定しました。もう一度操作してください');
    });
    setTimeout(() => $('#cg-code')?.focus(), 50);
  },
};

// ===== 選択状態(右パネルは廃止し、操作は右クリックメニューに集約) =====
const Sel = { set(rows, grid) { S.sel = rows; S.selGrid = grid; } };

// ===== 右クリックメニュー =====
const Menu = {
  // items: {label, icon, onClick, disabled, title} または '-'(区切り線)
  show(x, y, items) {
    this.close();
    const el = document.createElement('div');
    el.id = 'ctxmenu';
    el.innerHTML = items.map((it, i) => it === '-' ? '<hr>' : `<button data-i="${i}" ${it.disabled ? 'disabled' : ''} title="${esc(it.title || '')}"><span class="mi">${it.icon || ''}</span>${esc(it.label)}${it.key ? `<span class="kbd">${esc(it.key)}</span>` : ''}</button>`).join('');
    document.body.appendChild(el);
    const r = el.getBoundingClientRect();
    el.style.left = Math.min(x, innerWidth - r.width - 8) + 'px';
    el.style.top = Math.min(y, innerHeight - r.height - 8) + 'px';
    $$('button[data-i]', el).forEach(b => b.onclick = () => { this.close(); guard(items[+b.dataset.i].onClick)(); });
    setTimeout(() => {
      this.off = e => { if (!e.target.closest('#ctxmenu')) this.close(); };
      addEventListener('mousedown', this.off, true);
      addEventListener('keydown', this.esc = e => { if (e.key === 'Escape') this.close(); });
    });
  },
  close() {
    $('#ctxmenu')?.remove();
    if (this.off) removeEventListener('mousedown', this.off, true);
    if (this.esc) removeEventListener('keydown', this.esc);
    this.off = this.esc = null;
  },
};

// ===== 画面間の移動 =====
const Nav = {
  go(where, r) {
    if (where === 'tree') return show('organize', { reveal: r.id });
    if (where === 'under') return show('list', { under: r.id, underPath: r.path });
    if (where === 'dups') return show('dups', { under: r.id, underPath: r.path });
  },
};
// 実フォルダ/ファイルに共通の閲覧系メニュー項目
function browseItems(r, withTree) {
  const items = [];
  if (r.dir) items.push({ label: '配下を検索', icon: '🔍', onClick: () => Nav.go('under', r) }, { label: '配下の重複候補', icon: '♊', onClick: () => Nav.go('dups', r) });
  if (withTree) items.push({ label: 'ツリーへ移動(整理画面で表示)', icon: '🌳', onClick: () => Nav.go('tree', r) });
  if (!r.dir) items.push({ label: 'ファイルを開く', icon: '📂', key: 'ダブルクリック', onClick: () => openFile(r) });
  items.push({ label: 'プロパティ', icon: 'ℹ', onClick: () => Props.show(r) });
  return items;
}
function actionItems(rows) {
  const nodes = rows.filter(r => r.kind !== 'vdir');
  const anyOwn = nodes.some(r => r.act);
  return [
    { label: `移動…${nodes.length > 1 ? `(${nodes.length}件)` : ''}`, icon: '📦', key: 'M', onClick: () => MoveFlow.start(nodes) },
    { label: `削除${nodes.length > 1 ? `(${nodes.length}件)` : ''}`, icon: '🗑', key: 'Del', onClick: () => Plan.delete(nodes) },
    { label: `保留${nodes.length > 1 ? `(${nodes.length}件)` : ''}`, icon: '⏸', key: 'H', title: '判断を後回しにする(あとで「保留のみ」で一覧できます)', onClick: () => Plan.hold(nodes) },
    { label: '解除', icon: '↺', key: 'Backspace', disabled: !anyOwn, title: anyOwn ? '' : 'アクションが設定されていません(親フォルダの設定は、親フォルダで解除してください)', onClick: () => Plan.clear(nodes.filter(r => r.act)) },
    '-',
    ...ownerItems(nodes),
    { label: 'タグ / メモの編集…', icon: '🏷', onClick: () => EditNotes.show(nodes) },
  ];
}
function ownerItems(nodes) {
  return [{ label: `担当の設定…${nodes.length > 1 ? `(${nodes.length}件)` : ''}`, icon: '👤', title: 'フォルダに設定すると配下すべての担当になります', onClick: () => OwnerEdit.show(nodes) }];
}

// ===== プロパティ =====
const Props = {
  show(r) {
    if (r.kind === 'vdir') return this.showV(r);
    if (r.kind === 'vlink') return modal(`<h2>↪ プロパティ(ショートカット)</h2><div style="font-weight:600">${esc(r.n)}</div>
      <p class="hint">整理後のフォルダ構成の中で、リンク先へ飛ぶための目印です(実際のファイルは作られません)。</p>
      <dl class="props"><dt>リンク先</dt><dd style="word-break:break-all">${esc(r.linkPath || '')}${r.broken ? `<div class="issue">${esc(r.broken)}</div>` : ''}</dd>
      ${r.ed ? `<dt>作成・変更</dt><dd>${esc(r.ed)}</dd>` : ''}</dl>
      <div class="btns"><button data-j>↪ リンク先へ移動</button></div>`, null, '', '閉じる').querySelector('[data-j]').onclick = () => { closeModal(); guard(() => Shortcut.jump(r))(); };
    const n = r.node || r;
    const vp = r.kind ? r.nvpath : r.vpath;
    const st = r.act ? (r.act === 'delete' ? '<span class="actb delete">削除</span>' : `<span class="actb move">移動</span> → ${esc(vpathLabel(vp))}${r.nn ? ` (名前: ${esc(r.nn)})` : ''}`)
      : r.ia ? `<span class="actb inh">親フォルダで${ACT[r.ia]}</span>${r.ivpath ? ' → ' + esc(vpathLabel(r.ivpath)) : ''}` : '<span class="muted">未設定(未処理)</span>';
    const unk = r.dir && (r.f & FL.access);
    const m = modal(`<h2>${r.dir ? '📁' : fileIcon(r.x)} プロパティ</h2>
      <div style="font-weight:600;word-break:break-all">${esc(n.n)}</div><div class="path">${esc(r.path)}</div>
      ${r.err ? `<div class="issue block">🔒 ${esc(r.err)}${r.dir ? '<br>このフォルダの中身・サイズは不明です。' : ''}</div>` : ''}
      <dl class="props"><dt>種類</dt><dd>${r.dir ? 'フォルダ' : 'ファイル' + (r.x ? ` (.${esc(r.x)})` : '')}</dd>
      <dt>サイズ</dt><dd>${unk ? '不明' : `${fmtSize(r.s)} <span class="muted">(${fmtNum(r.s)} バイト)</span>`}</dd>
      <dt>更新日時</dt><dd>${fmtDate(r.m) || '-'}</dd>
      ${r.dir ? `<dt>配下</dt><dd>${unk ? '不明' : `ファイル ${fmtNum(n.fc)} / フォルダ ${fmtNum(n.dc)}`}</dd><dt>未処理</dt><dd>${unk ? '不明' : fmtNum(r.rem) + ' ファイル'}</dd>` : ''}
      <dt>階層 / パス長</dt><dd>${r.realD ?? r.d} / ${r.pl}文字</dd>
      <dt>警告・ヒント</dt><dd>${warnHTML(n.f !== undefined ? n : r) || '<span class="muted">なし</span>'}</dd>
      <dt>整理アクション</dt><dd>${st}${r.ed ? ` <span class="muted">(${esc(r.ed)})</span>` : ''}</dd>
      <dt>期限</dt><dd>${r.due ? dueHTML(r.due) : '<span class="muted">永続(期限なし)</span>'}</dd>
      <dt>担当</dt><dd>${ownerHTML(r) || '<span class="muted">なし</span>'}</dd>
      <dt>タグ</dt><dd>${tagsHTML(r.tags) || '<span class="muted">なし</span>'}</dd>
      ${r.r5s ? `<dt>5Sルール</dt><dd><div class="issue block" style="margin:0">${esc(r.r5s).replace(/\n/g, '<br>')}</div></dd>` : ''}
      <dt>メモ</dt><dd style="white-space:pre-wrap">${esc(r.memo) || '<span class="muted">なし</span>'}</dd></dl>
      <div class="btns">${r.dir ? '' : '<button data-p="open">📂 ファイルを開く</button>'}<button data-p="reveal">Windowsで開く(場所を表示)</button><button data-p="copy">パスをコピー</button></div>`, null, '', '閉じる');
    $$('[data-p]', m).forEach(b => b.onclick = guard(async () => {
      if (b.dataset.p === 'open') return openFile(r);
      if (b.dataset.p === 'copy') { await navigator.clipboard.writeText(r.path); return toast('パスをコピーしました'); }
      await api('/api/reveal', { id: r.id });
    }));
  },
  showV(v) {
    const isRoot = v.uuid === 'root';
    modal(`<h2>🗂 プロパティ(整理後のフォルダ)</h2><div style="font-weight:600">${esc(isRoot ? vroot() : v.n)}</div>
      <div class="path">${esc(vpathLabel(v.vpath))}</div>
      <dl class="props"><dt>実際の場所</dt><dd>${S.rules?.basePath ? esc(S.rules.basePath + (v.vpath ? '\\' + v.vpath : '')) : '<span class="muted">未設定(⚙ オプション)</span>'}</dd>
      <dt>配下のファイル</dt><dd>${fmtNum(v.fc)} (${fmtSize(v.s)})</dd><dt>直下の項目</dt><dd>${fmtNum(v.cc)}</dd><dt>階層</dt><dd>${v.d}</dd>
      ${v.ed ? `<dt>作成・変更</dt><dd>${esc(v.ed)}</dd>` : ''}
      <dt>メモ</dt><dd style="white-space:pre-wrap">${esc(v.memo) || '<span class="muted">なし</span>'}</dd></dl>
      ${v.sim?.length ? `<div class="issue"><b>⚠ 同じ階層に似た名前のフォルダがあります</b><ul>${v.sim.map(x => `<li>${esc(x)}</li>`).join('')}</ul>目的が同じなら1つにまとめることを検討してください。</div>` : ''}
      <div class="btns"><button data-l>ここへ移動した項目を一覧</button></div>`, null, '', '閉じる');
    $('[data-l]').onclick = () => { closeModal(); VOps.list(v); };
  },
};

// タグ入力(Enterでチップを追加)
function tagInput(host, initial) {
  const tags = [...initial];
  const render = () => {
    $('.chips', host).innerHTML = tags.map((t, i) => `<span class="tag">${esc(t)}<a data-x="${i}" title="外す">×</a></span>`).join('') || '<span class="muted">なし</span>';
    $$('[data-x]', host).forEach(a => a.onclick = () => { tags.splice(+a.dataset.x, 1); render(); });
  };
  host.innerHTML = `<div class="chips"></div><div style="display:flex;gap:4px;margin-top:4px"><input list="taglist" placeholder="タグを入力して Enter"><button type="button">追加</button></div>`;
  const inp = $('input', host);
  const add = () => { const t = inp.value.trim(); if (t && !tags.includes(t)) tags.push(t); inp.value = ''; render(); };
  inp.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); add(); } };
  $('button', host).onclick = add;
  render();
  return { get: () => { add(); return tags; } };
}
function dueInput(host, due) {
  host.innerHTML = `<label class="inl"><input type="radio" name="due" value="" ${due ? '' : 'checked'}> 永続(期限なし)</label>
    <label class="inl"><input type="radio" name="due" value="set" ${due ? 'checked' : ''}> 期限を設定 <input type="date" class="dd" value="${esc(due || '')}"></label>`;
  $('.dd', host).onfocus = () => { $('input[value="set"]', host).checked = true; };
  return { get: () => $('input[name="due"]:checked', host).value === 'set' ? $('.dd', host).value : '' };
}

// ===== 移動: ウィンドウで期限・タグ → OK → 右の仮想ツリーから移動先を選ぶ =====
const MoveFlow = {
  start(rows) {
    const nodes = rows.filter(r => typeof r.id === 'number' && r.kind !== 'vdir');
    if (!nodes.length) return;
    if (S.view !== 'organize') return Plan.move(nodes);
    const one = nodes.length === 1 ? nodes[0] : null;
    const union = [...new Set(nodes.flatMap(r => r.tags || []))];
    const m = modal(`<h2>📦 ${nodes.length === 1 ? `「${esc(one.node?.n ?? one.n)}」` : `${nodes.length}件`}を移動</h2>
      <p class="hint">期限とタグを設定して OK を押すと、<b>右の「整理後のフォルダ構成」から移動先のフォルダ(🗂)を選ぶ</b>状態になります(Esc / キャンセルで中止)。</p>
      <h3>期限</h3><div id="mv-due"></div>
      <h3 style="margin-top:10px">タグ(目的・種類)</h3><div id="mv-tags"></div>`,
      () => {
        const due = dueW.get(), tags = tagW.get();
        Pick.start(`${nodes.length}件の移動先を、右の「整理後のフォルダ構成」からクリックして選んでください`, async v => {
          const ids = Plan.ids(nodes);
          const add = tags.filter(t => !union.includes(t)), remove = union.filter(t => !tags.includes(t));
          if (add.length || remove.length) await api('/api/tags', { ids, add, remove }); // タグ必須ルールのため先に設定
          const rep = await api('/api/plan/move', { ids, target: v.uuid });
          const blocked = new Set((rep.issues || []).filter(i => i.blocks?.length).map(i => i.id));
          const moved = ids.filter(id => !blocked.has(id));
          if (moved.length) await api('/api/plan/fields', { ids: moved, due });
          add.forEach(t => S.tags.includes(t) || S.tags.push(t)); updateTagList();
          showReport(rep, '移動');
          await afterEdit();
        });
      }, 'OK(移動先を選ぶ)');
    const dueW = dueInput($('#mv-due', m), one?.due || '');
    const tagW = tagInput($('#mv-tags', m), union);
  },
};

// 右の仮想ツリーから移動先を選ぶ状態(キャンセルするまで他の操作はできない)
const Pick = {
  on: false,
  start(label, onPick) {
    this.on = true; this.onPick = onPick;
    $('#pickbar-text').textContent = label;
    $('#view-organize').classList.add('picking');
    this.esc = e => { if (e.key === 'Escape') this.cancel(); };
    addEventListener('keydown', this.esc);
    Views.organize.vt.host.focus();
  },
  end() {
    this.on = false;
    $('#view-organize').classList.remove('picking');
    removeEventListener('keydown', this.esc);
  },
  cancel() { if (this.on) { this.end(); toast('移動を中止しました'); } },
  async choose(v) {
    if (v.kind !== 'vdir') return toast('移動先には整理後のフォルダ(🗂)を選んでください', true);
    const fn = this.onPick;
    this.end();
    await fn(v);
  },
};

// ===== タグ / メモ(/ 期限)の編集 =====
const EditNotes = {
  show(rows) {
    const nodes = rows.filter(r => typeof r.id === 'number' && r.kind !== 'vdir');
    if (!nodes.length) return;
    const one = nodes.length === 1 ? nodes[0] : null;
    const union = [...new Set(nodes.flatMap(r => r.tags || []))];
    const allMoved = nodes.every(r => r.act === 'move');
    const m = modal(`<h2>🏷 タグ / メモの編集${one ? `: ${esc(one.node?.n ?? one.n)}` : `(${nodes.length}件)`}</h2>
      ${nodes.length > 1 ? '<p class="hint">タグは追加・削除した分だけ全件に反映します。メモは入力した場合だけ全件に設定します。</p>' : ''}
      <h3>タグ(目的・種類)</h3><div id="en-tags"></div>
      ${allMoved ? '<h3 style="margin-top:10px">期限</h3><div id="en-due"></div>' : ''}
      <h3 style="margin-top:10px">メモ</h3><textarea id="en-memo" style="width:100%;height:80px" placeholder="${one ? '判断理由・確認事項など' : '(変更しない)'}">${esc(one?.memo || '')}</textarea>`,
      async () => {
        const ids = Plan.ids(nodes), tags = tagW.get();
        const add = tags.filter(t => !union.includes(t)), remove = union.filter(t => !tags.includes(t));
        if (add.length || remove.length) await api('/api/tags', { ids, add, remove });
        const f = {};
        const memo = $('#en-memo').value;
        if (one ? memo !== (one.memo || '') : memo.trim()) f.memo = memo;
        if (dueW) { const d = dueW.get(); if (!one || d !== (one.due || '')) f.due = d; }
        if (Object.keys(f).length) await api('/api/plan/fields', { ids, ...f });
        add.forEach(t => S.tags.includes(t) || S.tags.push(t)); updateTagList();
        toast('保存しました');
        await afterEdit();
      }, '保存');
    const tagW = tagInput($('#en-tags', m), union);
    const dueW = allMoved ? dueInput($('#en-due', m), one?.due || '') : null;
  },
  showV(v) {
    modal(`<h2>🏷 メモの編集: ${esc(v.n)}</h2><p class="hint">このフォルダの用途・保存ルールなど</p><textarea id="ev-memo" style="width:100%;height:100px">${esc(v.memo || '')}</textarea>`,
      async () => { await api('/api/vmemo', { uuid: v.uuid, memo: $('#ev-memo').value }); toast('保存しました'); await afterEdit(); }, '保存');
  },
};
// ===== 担当の設定(フォルダに設定すると配下に引き継がれる。担当ごとに作業用コピーで判断し、最後に統合する) =====
const OwnerEdit = {
  show(rows) {
    const nodes = rows.filter(r => typeof r.id === 'number' && r.kind !== 'vdir');
    if (!nodes.length) return;
    const one = nodes.length === 1 ? nodes[0] : null;
    // 初期値: 選んだ項目すべてで同じなら、その項目に設定した値
    const owns = nodes.map(r => ownerFields(r.own));
    const cur = OWNER_LV.map((_, i) => owns.every(o => o[i] === owns[0][i]) ? owns[0][i] : '');
    const eff = one ? ownerFields(one.iown) : ['', '', '', ''];
    const vals = i => [...new Set(S.owners.filter(o => o.level === i).map(o => o.value))];
    const m = modal(`<h2>👤 担当の設定${one ? `: ${esc(one.node?.n ?? one.n)}` : `(${nodes.length}件)`}</h2>
      <p class="hint">フォルダに担当を設定すると、<b>配下すべてがその担当</b>になります。<br>
      <b>空欄の段は親フォルダの値を引き継ぎます</b>(例: 上のフォルダに「部」、その中のフォルダに「課」だけを設定)。全段空欄で保存すると解除です。<br>
      整理画面の「担当」で段ごとに絞り込めるので、担当者が自分の範囲だけを見て判断できます。</p>
      <div class="form" style="grid-template-columns:80px 1fr">
        ${OWNER_LV.map((l, i) => `<span>${l}</span><input type="text" data-lv="${i}" list="ownerlist${i}" value="${esc(cur[i])}" placeholder="${!cur[i] && eff[i] ? `空欄 → 親フォルダの「${esc(eff[i])}」` : '空欄可'}">`).join('')}
      </div>
      ${OWNER_LV.map((_, i) => `<datalist id="ownerlist${i}">${vals(i).map(v => `<option value="${esc(v)}">`).join('')}</datalist>`).join('')}
      <div style="margin-top:10px"><button id="ow-clear" ${nodes.some(r => r.own) ? '' : 'disabled'}>担当を解除する(すべて親フォルダの担当に戻す)</button></div>`,
      async mm => {
        const f = $$('[data-lv]', mm).map(x => x.value.trim());
        await Plan.owner(nodes, f);
      }, '設定');
    $('#ow-clear', m).onclick = guard(async () => { closeModal(); await Plan.owner(nodes.filter(r => r.own), []); });
    setTimeout(() => $('[data-lv]', m)?.focus(), 50);
  },
};
async function loadOwners() {
  try { S.owners = await api('/api/owners'); } catch (e) { S.owners = []; }
  Views.organize.ownerOptions?.(); Views.list.ownerOptions?.(); Views.plan.ownerOptions?.();
}
// 担当の絞り込みの選択肢(値は "段:値")。段ごとにグループ化
function ownerOptionsHTML(cur, allLabel) {
  const has = cur === '-' || S.owners.some(o => `${o.level}:${o.value}` === cur);
  return `<option value="">${allLabel}</option>` + OWNER_LV.map((l, i) => {
    const os = S.owners.filter(o => o.level === i);
    return os.length ? `<optgroup label="${l}">${os.map(o => `<option value="${i}:${esc(o.value)}">${l}: ${esc(o.value)}(${fmtNum(o.files)}ファイル)</option>`).join('')}</optgroup>` : '';
  }).join('') + `<option value="-">担当なし(未割り当て)</option>` + (cur && !has ? `<option value="${esc(cur)}">${esc(ownerSpecLabel(cur))}</option>` : '');
}
const ownerSpecLabel = spec => spec === '-' ? '担当なし' : (([l, ...v]) => `${OWNER_LV[+l] || ''}: ${v.join(':')}`)(spec.split(':'));

function updateTagList() { $('#taglist').innerHTML = S.tags.map(t => `<option value="${esc(t)}">`).join(''); }
async function loadTags() { try { S.tags = (await api('/api/tags')).map(t => t.key); updateTagList(); } catch (e) { } }

// ===== ショートカット(整理後の構成の中で、別の場所へ飛ぶための目印。実際のファイルは作らない) =====
const Shortcut = {
  target(r) { return r.kind === 'vdir' ? 'v:' + r.uuid : 'n:' + r.id; },
  label(r) { return r.kind === 'vdir' ? (r.uuid === 'root' ? vroot() : r.n) : (r.nn || r.node?.n || r.n); },
  item(r) {
    return { label: 'ショートカットを作成…', icon: '↪', title: '整理後のフォルダ構成の中に、この項目へ飛ぶショートカットを置きます', onClick: () => this.create(r) };
  },
  create(r) {
    if (S.view !== 'organize') return;
    const target = this.target(r), name0 = `${this.label(r)} - ショートカット`;
    Pick.start(`「${this.label(r)}」へのショートカットを置く場所を、右の「整理後のフォルダ構成」からクリックして選んでください`, async v => {
      const name = prompt('ショートカットの名前', name0);
      if (!name?.trim()) return;
      const res = await api('/api/vlink', { parent: v.uuid, target, name: name.trim() });
      toast('ショートカットを作成しました');
      await afterEdit();
      await Views.organize.focusV(res.uuid);
    });
  },
  // リンク先をツリー上で開いて選択する
  async jump(r) {
    const o = Views.organize, loc = await api('/api/vlocate?target=' + encodeURIComponent(r.link));
    if (loc.left) {
      toast(`リンク先は${loc.reason}。現在のフォルダ構成で表示します`);
      return o.reveal(loc.left);
    }
    if (!loc.placed) return o.focusV(loc.vfolder);
    const m = o.vmodel;
    let i = await m.revealV(loc.vfolder);
    if (i >= 0) await m.expand(i);
    for (const id of [loc.placed, ...(loc.path || [])]) {
      if (id === loc.target) break;
      i = m.indexOf('n:' + id);
      if (i < 0) break;
      await m.expand(i);
    }
    o.vt.refresh();
    if (o.vt.focusKey('n:' + loc.target)) Sel.set(o.vt.selected(), o.vt);
    else toast('リンク先を表示できませんでした', true);
  },
};

// 仮想フォルダの操作
const VOps = {
  // 新しい仮想フォルダを作る(作ったフォルダのIDを返す)。pick=true は移動先選択中(画面の更新は移動後に行う)
  async new(v, pick) {
    const name = prompt(`「${v.uuid === 'root' ? vroot() : v.n}」の下に作るフォルダ名`);
    if (!name?.trim()) return null;
    const r = await api('/api/vcreate', { parent: v.uuid, name: name.trim() });
    if (showIssue(r.issue, 'フォルダを作成')) return null;
    toast('フォルダを作成しました');
    if (pick) return r.uuid;
    await afterEdit();
    if (S.view === 'organize') await Views.organize.focusV(r.uuid);
    return r.uuid;
  },
  // 仮想フォルダを別の仮想フォルダの下へ(右のツリーから移動先を選ぶ)
  move(vdirs) {
    vdirs = vdirs.filter(v => v.uuid !== 'root');
    if (!vdirs.length) return;
    Pick.start(`「${vdirs.map(v => v.n).join('」「')}」の移動先を、右のツリーからクリックして選んでください`, async t => {
      for (const v of vdirs) {
        if (v.uuid === t.uuid) continue;
        const r = await api('/api/vmove', { uuid: v.uuid, parent: t.uuid });
        showIssue(r.issue, `「${v.n}」の移動`);
      }
      await afterEdit();
    });
  },
  async rename(v) {
    const name = prompt('新しい名前', v.uuid === 'root' ? vroot() : v.n);
    if (!name?.trim()) return;
    const r = await api('/api/vrename', { uuid: v.uuid, name: name.trim() });
    if (showIssue(r.issue, '名前変更')) return;
    if (v.uuid === 'root') await refreshState();
    await afterEdit();
  },
  async del(v) {
    if (v.kind === 'vlink') return modal(`<h2>ショートカット「${esc(v.n)}」を削除しますか？</h2><p class="hint">リンク先の項目は変わりません。</p>`,
      async () => { await api('/api/vdelete', { uuid: v.uuid }); toast('ショートカットを削除しました'); await afterEdit(); }, '削除');
    modal(`<h2>仮想フォルダ「${esc(v.n)}」を削除しますか？</h2><p class="hint">配下の仮想フォルダも削除されます。ここへ「移動」を設定していた項目(${fmtNum(v.fc)}ファイル)は、移動の設定が解除され「未処理」に戻ります。</p>`,
      async () => { const r = await api('/api/vdelete', { uuid: v.uuid }); toast(`削除しました(${r.cleared}件の移動を解除)`); await afterEdit(); }, '削除');
  },
  list(v) { show('list', { vparent: v.uuid, vparentPath: vpathLabel(v.vpath) }); },
};

// ===== 画面切り替え =====
const Views = {};
function show(name, arg) {
  if (!['import', 'actmerge'].includes(name) && !S.state?.db) name = 'import';
  S.view = name;
  $$('#nav button').forEach(b => b.classList.toggle('active', b.dataset.view === name));
  $$('.view').forEach(v => v.classList.toggle('active', v.id === 'view-' + name));
  Pick.on && Pick.end();
  Menu.close();
  Sel.set([], null);
  const v = Views[name];
  if (v) {
    let changed = false;
    if (v.dbv !== S.dbv) { v.dbv = S.dbv; v.planv = S.planv; v.reset?.(); }
    else if (v.planv !== S.planv) { v.planv = S.planv; changed = true; }
    guard(async () => { if (changed && v.planChanged) await v.planChanged(); await v.enter(arg || {}); })();
  }
}

async function refreshState() {
  S.state = await api('/api/state');
  S.checks = S.state.checks;
  const db = S.state.db;
  S.settings = db?.settings || null;
  S.rules = db?.rules || null;
  S.code = db?.code || '';
  $('#dbname').textContent = db ? '📄 ' + db.path.split(/[\\/]/).pop() : 'DB未選択';
  $('#dbname').title = db ? db.path : '';
  const cb = $('#codebadge');
  cb.hidden = !db;
  cb.className = S.code ? 'edit' : 'master';
  cb.textContent = S.code ? `✎ 作業者: ${S.code}` : 'マスター(編集には作業者コードが必要)';
  cb.title = S.code ? 'このDBでの変更は、この作業者コードで記録されます' : 'クリックして作業者コードを設定 / 作業用コピーを作成';
  $$('#nav .needdb').forEach(b => b.disabled = !db);
  DeepSetting.sync(false);
  return S.state;
}
function dbOpened() {
  S.dbv++; S.planv++;
  loadTags(); loadOwners();
}
async function reopened(msg) {
  await refreshState(); dbOpened();
  if (msg) toast(msg);
  show(['import', 'actmerge'].includes(S.view) ? 'organize' : S.view);
}
const openDB = guard(async path => {
  if (!path) return;
  await api('/api/open', { path });
  await reopened('DBを開きました');
});

// ===== ファイルメニュー =====
const FileMenu = {
  init() {
    $('#filebtn').onclick = e => { e.stopPropagation(); this.toggle(); };
    document.addEventListener('mousedown', e => { if (!e.target.closest('#filemenu')) this.toggle(false); });
    $$('#filepop [data-cmd]').forEach(b => b.onclick = () => { this.toggle(false); guard(() => this[b.dataset.cmd]())(); });
    addEventListener('keydown', e => { if (e.key === 'o' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); guard(() => this.open())(); } });
    $('#codebadge').onclick = () => { if (!S.code) CodeGate.prompt(); };
  },
  async toggle(on) {
    const pop = $('#filepop');
    on = on ?? pop.hidden;
    pop.hidden = !on;
    $('#filebtn').classList.toggle('open', on);
    if (!on) return;
    const db = !!S.state?.db;
    $$('#filepop [data-cmd="workcopy"], #filepop [data-cmd="setcode"], #filepop [data-cmd="close"]').forEach(b => b.disabled = !db);
    const list = await api('/api/recent').catch(() => []);
    $('#recentlist').innerHTML = list.length ? list.slice(0, 10).map((f, i) => `<button data-recent="${i}" ${f.missing ? 'disabled' : ''} title="${esc(f.path)}"><span>📄 ${esc(f.name)}${f.missing ? ' (見つかりません)' : ''}</span><span class="rp">${esc(f.path)}</span></button>`).join('')
      : '<div class="menusub">(なし)</div>';
    $$('#recentlist [data-recent]').forEach(b => b.onclick = () => { this.toggle(false); openDB(list[+b.dataset.recent].path); });
  },
  async open() {
    let r;
    try { r = await api('/api/dialog', { kind: 'db', initial: S.state?.projectsDir ? S.state.projectsDir + '\\' : '' }); }
    catch (e) { // ダイアログが使えない環境 → パス入力
      return modal('<h2>DBを開く</h2><div class="form" style="grid-template-columns:80px 1fr"><span>パス</span><input type="text" id="op-path"></div>', m => openDB($('#op-path', m).value), '開く');
    }
    if (r.path) return openDB(r.path);
  },
  async workcopy() {
    const stem = S.state.db.path.split(/[\\/]/).pop().replace(/\.db$/i, '');
    const m = modal(`<h2>作業用コピーを作成</h2>
      <p class="hint">今開いているマスターDBをコピーし、<b>作業者コード</b>(部署名・氏名など。日本語可)を付けます。各自がコピーを編集し、最後に「アクションの統合」でマスターにまとめます。</p>
      <div class="form" style="grid-template-columns:110px 1fr auto"><span>作業者コード</span><input type="text" id="wc-code" placeholder="例: 経理部_山田"><span></span>
      <span>保存先</span><input type="text" id="wc-path" placeholder="空欄なら ${esc(stem)}_作業者コード.db(DB保存フォルダ)"><button id="wc-ref">参照…</button></div>`,
      async m => {
        await api('/api/workcopy', { code: $('#wc-code', m).value, path: $('#wc-path', m).value });
        await reopened('作業用コピーを作成して開きました');
      }, '作成して開く');
    $('#wc-ref', m).onclick = guard(async () => {
      const code = $('#wc-code', m).value.trim();
      const r = await api('/api/dialog', { kind: 'savedb', initial: `${S.state.projectsDir}\\${stem}_${code || '作業者'}.db` });
      if (r.path) $('#wc-path', m).value = r.path;
    });
  },
  setcode() { CodeGate.prompt(); },
  actmerge() { show('actmerge'); },
  async close() { await api('/api/close', {}); await refreshState(); S.dbv++; show('import'); },
  quit() { Life.quit(); },
};

// ===== オプション(タブで分類) =====
const MODES = [['block', '禁止'], ['warn', '警告のみ'], ['off', '無効']];
const modeSel = (cls, v) => `<select class="${cls}">${MODES.map(([k, l]) => `<option value="${k}" ${v === k ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
const CATS = [
  ['整理', '不要なファイルを削除する。判断基準を決めて捨てる'],
  ['整頓', '必要なファイルを「あるべき場所」へ格納・命名する。命名/格納ルールを決め、誰が見ても分かる状態にする'],
  ['清掃', '定期的な棚卸し・チェック。旧版・不要なコピーを残さず、最新の状態を保つ'],
  ['清潔', '整理・整頓・清掃のルールを維持し、形式を統一する(名前の付け方・使える文字など)'],
  ['躾', '担当・役割・目的を明確にし、全員が自然に実践できるようにする'],
];
// 名前の条件(Go側 fsdb/rules.go と同じ)
const COND_OPS = [
  ['contains', '名前に次の語のどれかを含む'], ['notcontains', '名前に次の語を含まない'],
  ['chars', '名前に次の文字のどれかを含む'], ['nochars', '名前に次の文字を含まない'],
  ['prefix', '名前が次の語で始まる'], ['suffix', '名前が次の語で終わる'],
  ['format', '名前が決まった形式'], ['regex', '名前が正規表現に一致する'], ['notregex', '名前が正規表現に一致しない'],
  ['maxlen', '名前の文字数が次の数以下'],
];
const MUST_OPS = [['forbid', '置けない(禁止)'], ...COND_OPS, ['haschild', '直下に次のフォルダがある(必須フォルダ)']];
const FORMATS = [['num3', '数字3桁_名前(例: 010_経理)'], ['alnum3', '英数字3文字_名前(例: A01_経理)'], ['any3', '任意の3文字_名前']];
const VAL_HINT = {
  contains: '例: 旧, OLD, ゴミ箱(「,」区切り)', notcontains: '例: 旧, OLD, ゴミ箱(「,」区切り)', chars: '例: #%&', nochars: '例: #%&',
  prefix: '例: 0, 1(「,」区切り)', suffix: '例: 課, 部', regex: '例: ^[0-9]{3}_', notregex: '例: (^|_)old($|_)', maxlen: '例: 30',
  haschild: '例: 規定|SOP, 契約(「,」= すべて必要 /「|」= どれか)',
};
function condTextJS(c) {
  const q = () => '「' + (c.val || '').split(/[,、\n]/).map(x => x.trim()).filter(Boolean).join('」「') + '」';
  switch (c.op) {
    case 'contains': return `名前に${q()}のどれかを含む`;
    case 'notcontains': return `名前に${q()}を含まない`;
    case 'chars': return `名前に文字「${c.val}」のどれかを含む`;
    case 'nochars': return `名前に文字「${c.val}」を含まない`;
    case 'prefix': return `名前が${q()}で始まる`;
    case 'suffix': return `名前が${q()}で終わる`;
    case 'regex': return `名前が正規表現 ${c.val} に一致する`;
    case 'notregex': return `名前が正規表現 ${c.val} に一致しない`;
    case 'format': return `名前が「${(FORMATS.find(f => f[0] === c.val) || [, c.val])[1]}」の形式`;
    case 'maxlen': return `名前が${c.val}文字以内`;
    case 'haschild': return `直下に${q()}のフォルダがある`;
    case 'forbid': return '置けない';
  }
  return '';
}
function describeRule(c) {
  const kind = { dir: 'フォルダ', file: 'ファイル' }[c.kind] || 'フォルダ・ファイル';
  let scope = { eq: `第${c.depth}階層の`, ge: `第${c.depth}階層以下(深い側)の`, le: `第${c.depth}階層までの`, range: `第${c.depth}〜${c.depth2}階層の` }[c.depthOp] || 'すべての';
  if (c.when?.op) scope += `(${condTextJS(c.when)})`;
  return `${scope}${kind}: ${condTextJS(c.must)}${c.must.op === 'forbid' ? '' : 'こと'}`;
}
// 「ファイル整理方針」に沿った初期ルール(Go側 DefaultCustomRules と同じ)
const PRESETS = [
  { id: 'top-name', cat: '整頓', label: '上位階層のフォルダ名は「3桁の番号_名前」', mode: 'warn', kind: 'dir', depthOp: 'le', depth: 3, depth2: 0, when: { op: '', val: '' }, must: { op: 'format', val: 'num3' } },
  { id: 'top-file', cat: '整頓', label: '上位階層(第3階層まで)にはファイルを置かない', mode: 'warn', kind: 'file', depthOp: 'le', depth: 3, depth2: 0, when: { op: '', val: '' }, must: { op: 'forbid', val: '' } },
  { id: 'required', cat: '整頓', label: '各課に「規定／SOP」「契約」フォルダを設置する', mode: 'off', kind: 'dir', depthOp: 'eq', depth: 2, depth2: 0, when: { op: 'contains', val: '課' }, must: { op: 'haschild', val: '規定|SOP, 契約' } },
  { id: 'old', cat: '清掃', label: '旧版・ゴミ箱のようなフォルダを残さない(バージョン履歴で管理)', mode: 'warn', kind: 'dir', depthOp: '', depth: 0, depth2: 0, when: { op: '', val: '' }, must: { op: 'notregex', val: '(^|[^a-z])(old|bk|backup)([^a-z]|$)|旧版|ゴミ箱|ごみ箱|まもなく消去|削除予定' } },
  { id: 'temp', cat: '整理', label: '一時・システムファイルは移動しない', mode: 'warn', kind: 'file', depthOp: '', depth: 0, depth2: 0, when: { op: '', val: '' }, must: { op: 'notregex', val: '^~\\$|\\.tmp$|^thumbs\\.db$|^desktop\\.ini$|^\\.ds_store$' } },
];

const Options = {
  tab: 'general', sub: '整頓',
  open(tab) {
    if (tab) this.tab = tab;
    const db = S.state?.db, m = db?.meta || {}, st = S.settings || {}, ru = S.rules || {};
    const t = store.get('fm-theme', 'auto');
    this.custom = JSON.parse(JSON.stringify(ru.custom || [])).map(c => ({ when: { op: '', val: '' }, depth2: 0, ...c }));
    const tabs = [['general', '表示'], ...(db ? [['db', 'DB・パス'], ['warn', '現在の構成の警告'], ['rules', '5Sルール']] : [])];
    if (!tabs.some(x => x[0] === this.tab)) this.tab = 'general';
    const builtin = (label, input, sel, hint) => `<span>${label}</span><span>${input || ''}</span><span>${sel || ''}</span><span class="hint">${hint || ''}</span>`;
    const md = modal(`<h2>⚙ オプション</h2>
      <div class="tabs" id="op-tabs">${tabs.map(([k, l]) => `<button data-tab="${k}" class="${this.tab === k ? 'on' : ''}">${l}</button>`).join('')}</div>
      <div class="opbody">
      <div class="tabpane" data-pane="general">
        <div class="optsec"><h3>テーマ</h3><span class="seg" id="op-theme">${[['auto', '🌓 自動'], ['light', '☀ ライト'], ['dark', '🌙 ダーク']].map(([k, l]) => `<button data-t="${k}" class="${t === k ? 'on' : ''}">${l}</button>`).join('')}</span>
          <span class="hint">「自動」はWindowsの設定に従います</span></div>
        ${db ? '' : '<p class="hint">DBを開くと、DBの情報と判定ルールを設定できます。</p>'}
      </div>
      ${db ? `<div class="tabpane" data-pane="db">
        <div class="optsec"><h3>開いているDB</h3><table class="t">
        <tr><td>ファイル</td><td class="wrap">${esc(db.path)}</td></tr>
        <tr><td>作業者コード</td><td>${S.code ? esc(S.code) : '<span class="muted">なし(マスター)</span>'}</td></tr>
        <tr><td>対象ルート</td><td class="wrap">${esc(m.root)}</td></tr>
        <tr><td>取得元</td><td>${esc(srcLabel(m))}${m.merged_from ? ' / アクション統合: ' + esc(m.merged_from) : ''}</td></tr>
        <tr><td>データ取得</td><td>${esc(m.scanned_at || m.merged_at || m.built_at || '')}</td></tr>
        <tr><td>項目数</td><td>${fmtNum(+m.node_count)}</td></tr>
        <tr><td>マスターID / 版</td><td class="muted">${esc((m.master_id || '-').slice(0, 8))} / ${esc((m.master_rev || '-').slice(0, 8))}</td></tr>
        ${m.master_path ? `<tr><td>元のマスター</td><td class="wrap">${esc(m.master_path)}</td></tr>` : ''}</table></div>
        ${m.virtual === 'true' ? '' : `<div class="optsec"><h3>パス(SharePoint / OneDrive 同期フォルダ向け)</h3>
        <p class="hint">「記録用のパス」を設定すると、DB内のパスをその形(SharePoint の URL など)で記録します。個人のローカルパスが入らないので、全員が同じパスで作業・統合できます。<br>
        ファイルを開く・実行スクリプトでは、下の「このPCでの実際の場所」に読み替えます。</p>
        <div class="form" style="grid-template-columns:200px 1fr auto">
          <span>記録用のパス</span><input type="text" id="op-alias" value="${esc(m.alias_root || '')}" placeholder="例: https://xxx.sharepoint.com/sites/チーム/Shared Documents"><button id="op-alias-set">変更</button>
          <span>このPCでの実際の場所</span><input type="text" id="op-local" value="${esc(db.localRoot || '')}" placeholder="${esc(m.local_root || '')}" ${m.alias_root ? '' : 'disabled'}><span style="display:flex;gap:4px"><button id="op-local-ref" ${m.alias_root ? '' : 'disabled'}>参照…</button><button id="op-local-set" ${m.alias_root ? '' : 'disabled'}>保存</button></span>
          <span></span><span class="hint" style="grid-column:span 2">スキャンした場所: ${esc(m.local_root || m.root)}(空欄の場合はこの場所を使います。PCごとの設定で、DBには保存しません)</span>
        </div></div>`}
      </div>
      <div class="tabpane" data-pane="warn">
        <p class="hint">現在のフォルダ構成の各行に表示する「警告・整理のヒント」とサマリーの5Sチェックの判定基準です。</p>
        <div class="form" style="grid-template-columns:260px 110px">
        <span>古いファイル(更新から○年以上)</span><input type="number" id="st-old" value="${st.oldYears}" min="1">
        <span>パス文字数の警告(○文字超)</span><input type="number" id="st-path" value="${st.pathLimit}" min="1">
        <span>深い階層(○階層以上で警告)</span><input type="number" id="st-deep" value="${st.deepDepth}" min="1">
        <span>項目過多(直下○項目超)</span><input type="number" id="st-many" value="${st.manyFiles}" min="1"></div>
      </div>
      <div class="tabpane" data-pane="rules">
        <p class="hint">整理後のフォルダ構成(仮想)へ移動・フォルダ作成するときのルールです。「禁止」はルールに合わない操作をできなくし、「警告のみ」は操作はできますが知らせます。<br>
        階層は「整理後のルート」を0、その直下を第1階層と数えます。</p>
        <div class="form" style="grid-template-columns:200px 280px 1fr;margin-bottom:8px">
          <span>整理後のルートの表示名</span><input type="text" id="ru-root" value="${esc(ru.rootName)}"><span></span>
          <span>整理後のルートの実際の場所</span><input type="text" id="ru-base" value="${esc(ru.basePath)}" placeholder="例: \\\\srv\\share\\整理後"><span class="hint">パス長・実行スクリプト用</span>
        </div>
        <div class="tabs sub" id="ru-tabs"></div>
        ${CATS.map(([c, d]) => `<div class="subpane" data-sub="${c}"><div class="catdesc"><b>${c}</b>: ${esc(d)}</div>
          ${c === '整頓' ? `<h3>構成の上限</h3><div class="builtin">
            ${builtin('最大階層(ルート=0)', `<input type="number" id="ru-depth" value="${ru.maxDepth}" min="1">`, modeSel('', ru.depthMode).replace('<select class=""', '<select id="ru-depthm"'), '整理方針: 10階層以下')}
            ${builtin('整理後のフルパス文字数の上限', `<input type="number" id="ru-path" value="${ru.pathLimit}" min="10">`, modeSel('', ru.pathMode).replace('<select class=""', '<select id="ru-pathm"'), '整理後のルートの実際の場所を含む')}
            ${builtin('1フォルダ直下の項目数の上限', `<input type="number" id="ru-items" value="${ru.maxItems}" min="1">`, modeSel('', ru.itemsMode).replace('<select class=""', '<select id="ru-itemsm"'), '1階層あたりのフォルダ数を絞る')}
          </div>` : ''}
          ${c === '清潔' ? `<h3>名前の基本ルール</h3><div class="builtin">${builtin('禁止文字・末尾の.や空白・予約語', '', modeSel('', ru.badName).replace('<select class=""', '<select id="ru-bad"'), 'SharePoint で使えない名前。名前変更で直せます')}</div>` : ''}
          ${c === '清掃' ? `<h3>名前の基本ルール</h3><div class="builtin">${builtin('コピー・版管理的な名前', '', modeSel('', ru.copyName).replace('<select class=""', '<select id="ru-copy"'), '「- コピー」「(1)」「旧」「v2」など。版はSharePointのバージョン履歴で管理')}</div>` : ''}
          ${c === '躾' ? `<h3>基本ルール</h3><div class="builtin">${builtin('タグが無い項目の移動', '', modeSel('', ru.tagRequired).replace('<select class=""', '<select id="ru-tag"'), '目的・種類を明確にする')}</div>` : ''}
          <h3>カスタムルール</h3><div class="rules" data-cat="${c}"></div>
          <div class="btns"><button data-addrule="${c}">＋ ルールを追加</button><button data-preset="${c}" title="ファイル整理方針に沿った初期ルールのうち、まだ無いものを追加">整理方針の初期ルールを追加</button></div>
        </div>`).join('')}
        <div class="subpane" data-sub="適用範囲">
          <div class="catdesc"><b>現在のフォルダ構成への当てはめ</b>: 整理前の構成にも同じルールを当てはめ、合わない項目を色分け(行の左に赤線・「⛔5S外れ」)して、「⛔ 5Sルール外れのみ」で絞り込めるようにします。整理後の構成を考える前の棚卸しや、整理後の定期チェック(5S外れの検知)に使えます。</div>
          <label class="inl"><input type="checkbox" id="ru-apply" ${ru.applyCurrent ? 'checked' : ''}> 現在のフォルダ構成にもルールを当てはめる</label>
          <div class="form" style="grid-template-columns:320px 80px 1fr;margin-top:6px">
            <span>現在のルート(スキャンしたフォルダ)の階層</span><input type="number" id="ru-offset" value="${ru.currentOffset || 0}" min="0" max="20">
            <span class="hint">整理後の構成の第何階層に当たるか。例: 整理後のルート=「整備本部」、スキャンしたフォルダ=「125_整備業務部」なら 1</span>
          </div>
          <p class="hint">当てはめるルール: 最大階層・直下の項目数・禁止文字・コピー/版管理的な名前・カスタムルール(禁止/警告のどちらも)。<br>保存時に全項目を判定し直します(大規模なDBでは数秒かかります)。</p>
        </div>
      </div>` : ''}</div>`,
      db ? async mm => {
        S.settings = await api('/api/settings', { oldYears: +$('#st-old', mm).value, pathLimit: +$('#st-path', mm).value, deepDepth: +$('#st-deep', mm).value, manyFiles: +$('#st-many', mm).value });
        toast('保存しています…');
        S.rules = await api('/api/rules', {
          rootName: $('#ru-root', mm).value, basePath: $('#ru-base', mm).value, maxDepth: +$('#ru-depth', mm).value, depthMode: $('#ru-depthm', mm).value,
          pathLimit: +$('#ru-path', mm).value, pathMode: $('#ru-pathm', mm).value, maxItems: +$('#ru-items', mm).value, itemsMode: $('#ru-itemsm', mm).value,
          badName: $('#ru-bad', mm).value, copyName: $('#ru-copy', mm).value, tagRequired: $('#ru-tag', mm).value,
          custom: this.custom.map(c => ({ ...c, depth: +c.depth || 0, depth2: +c.depth2 || 0 })),
          applyCurrent: $('#ru-apply', mm).checked, currentOffset: +$('#ru-offset', mm).value || 0,
        });
        await refreshState();
        toast('保存しました');
        S.planv++; show(S.view);
      } : null, '保存', '閉じる');
    md.querySelector('.box').classList.add('wide', 'fixed');
    const showTab = k => {
      this.tab = k;
      $$('#op-tabs [data-tab]', md).forEach(b => b.classList.toggle('on', b.dataset.tab === k));
      $$('.tabpane', md).forEach(p => p.hidden = p.dataset.pane !== k);
    };
    $$('#op-tabs [data-tab]', md).forEach(b => b.onclick = () => showTab(b.dataset.tab));
    showTab(this.tab);
    if (db) this.initRules(md);
    if ($('#op-alias-set', md)) {
      $('#op-alias-set', md).onclick = guard(async () => {
        const v = $('#op-alias', md).value.trim();
        if (!confirm(v ? `すべての項目のパスを「${v}」から始まる形に書き換えます。よろしいですか？` : '記録用のパスを解除し、スキャンした場所のパスに戻します。よろしいですか？')) return;
        await api('/api/alias', { alias: v });
        closeModal(); await refreshState(); S.dbv++; toast('パスを書き換えました'); show(S.view);
      });
      $('#op-local-ref', md).onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'folder', initial: $('#op-local', md).value }); if (r.path) $('#op-local', md).value = r.path; });
      $('#op-local-set', md).onclick = guard(async () => { await api('/api/localroot', { local: $('#op-local', md).value }); await refreshState(); toast('このPCでの実際の場所を保存しました'); });
    }
    $$('#op-theme [data-t]', md).forEach(b => b.onclick = () => {
      store.set('fm-theme', b.dataset.t); Theme.apply();
      $$('#op-theme [data-t]', md).forEach(x => x.classList.toggle('on', x === b));
    });
  },
  // ---- 5Sルール(種別ごとのタブ + カスタムルールの編集) ----
  initRules(md) {
    const subs = [...CATS.map(c => c[0]), '適用範囲'];
    const renderTabs = () => {
      $('#ru-tabs', md).innerHTML = subs.map(c => {
        const n = this.custom.filter(r => r.cat === c && r.mode !== 'off').length;
        return `<button data-sub="${c}" class="${this.sub === c ? 'on' : ''}">${c === '適用範囲' ? '🔎 現在の構成への適用' : c}${c !== '適用範囲' && n ? `<span class="cnt">${n}</span>` : ''}</button>`;
      }).join('');
      $$('#ru-tabs [data-sub]', md).forEach(b => b.onclick = () => { this.sub = b.dataset.sub; renderTabs(); });
      $$('.subpane', md).forEach(p => p.hidden = p.dataset.sub !== this.sub);
    };
    this.renderTabs = renderTabs;
    if (!subs.includes(this.sub)) this.sub = '整頓';
    renderTabs();
    $$('[data-addrule]', md).forEach(b => b.onclick = () => {
      this.custom.push({ id: '', cat: b.dataset.addrule, label: '', mode: 'warn', kind: 'dir', depthOp: '', depth: 1, depth2: 3, when: { op: '', val: '' }, must: { op: 'notcontains', val: '' } });
      this.renderRules(md);
    });
    $$('[data-preset]', md).forEach(b => b.onclick = () => {
      const add = PRESETS.filter(p => p.cat === b.dataset.preset && !this.custom.some(c => c.id === p.id));
      if (!add.length) return toast('この種別の初期ルールはすべて追加済みです');
      this.custom.push(...JSON.parse(JSON.stringify(add)));
      this.renderRules(md);
    });
    this.renderRules(md);
  },
  renderRules(md) {
    const opt = (list, v) => list.map(([k, l]) => `<option value="${k}" ${v === k ? 'selected' : ''}>${esc(l)}</option>`).join('');
    const valInput = (cls, c) => c.op === 'format' ? `<select class="${cls}">${opt(FORMATS, c.val)}</select>`
      : c.op === 'forbid' || !c.op ? '' : `<input type="text" class="val ${cls}" value="${esc(c.val)}" placeholder="${esc(VAL_HINT[c.op] || '')}">`;
    for (const host of $$('.rules', md)) {
      const cat = host.dataset.cat;
      const idx = this.custom.map((c, i) => [c, i]).filter(([c]) => c.cat === cat);
      host.innerHTML = idx.length ? idx.map(([c, i]) => `<div class="rule m-${c.mode}" data-i="${i}">
        <div class="rl">${modeSel('r-mode', c.mode)}<input type="text" class="lbl r-label" value="${esc(c.label)}" placeholder="ルール名(任意。違反の表示に使います)">
          <select class="r-cat" title="種別(タブ)">${CATS.map(([k]) => `<option ${c.cat === k ? 'selected' : ''}>${k}</option>`).join('')}</select>
          <button class="mini" data-up title="上へ">↑</button><button class="mini danger" data-del title="削除">×</button></div>
        <div class="rl"><b>対象</b><select class="r-kind">${opt([['dir', 'フォルダ'], ['file', 'ファイル'], ['any', 'フォルダ・ファイル']], c.kind)}</select>
          <select class="r-dop">${opt([['', 'すべての階層'], ['eq', '第n階層'], ['le', '第n階層まで(n以下)'], ['ge', '第n階層以下(n以上・深い側)'], ['range', '第n〜m階層']], c.depthOp)}</select>
          ${c.depthOp ? `<input type="number" class="r-d" value="${c.depth}" min="0" title="n">` : ''}${c.depthOp === 'range' ? `〜<input type="number" class="r-d2" value="${c.depth2}" min="0" title="m">` : ''}
          <select class="r-wop" title="名前による対象の絞り込み(任意)">${opt([['', '名前は問わない'], ...COND_OPS], c.when.op)}</select>${valInput('r-wval', c.when)}</div>
        <div class="rl"><b>要件</b><select class="r-mop">${opt(MUST_OPS, c.must.op)}</select>${valInput('r-mval', c.must)}${c.must.op && c.must.op !== 'forbid' ? '<span class="muted">こと</span>' : ''}</div>
        <div class="rd">→ ${esc(describeRule(c))}</div></div>`).join('')
        : '<p class="muted">この種別のカスタムルールはありません。</p>';
    }
    $$('.rule', md).forEach(el => {
      const c = this.custom[+el.dataset.i];
      const on = (sel, ev, fn) => { const x = $(sel, el); if (x) x.addEventListener(ev, () => { fn(x.value, x); }); };
      const rerender = () => { this.renderRules(md); this.renderTabs(); };
      const desc = () => { $('.rd', el).textContent = '→ ' + describeRule(c); el.className = `rule m-${c.mode}`; };
      on('.r-mode', 'change', v => { c.mode = v; desc(); this.renderTabs(); });
      on('.r-label', 'input', v => { c.label = v; });
      on('.r-cat', 'change', v => { c.cat = v; rerender(); });
      on('.r-kind', 'change', v => { c.kind = v; if (v === 'file' && c.must.op === 'haschild') c.must = { op: 'forbid', val: '' }; rerender(); });
      on('.r-dop', 'change', v => { c.depthOp = v; rerender(); });
      on('.r-d', 'input', v => { c.depth = +v; desc(); });
      on('.r-d2', 'input', v => { c.depth2 = +v; desc(); });
      on('.r-wop', 'change', v => { c.when = { op: v, val: v === 'format' ? 'num3' : '' }; rerender(); });
      on('.r-wval', 'input', v => { c.when.val = v; desc(); });
      on('.r-wval', 'change', v => { c.when.val = v; desc(); });
      on('.r-mop', 'change', v => { c.must = { op: v, val: v === 'format' ? 'num3' : '' }; if (v === 'haschild') c.kind = 'dir'; rerender(); });
      on('.r-mval', 'input', v => { c.must.val = v; desc(); });
      on('.r-mval', 'change', v => { c.must.val = v; desc(); });
      $('[data-del]', el).onclick = () => { this.custom.splice(+el.dataset.i, 1); rerender(); };
      $('[data-up]', el).onclick = () => {
        const i = +el.dataset.i, prev = this.custom.map((x, k) => [x, k]).filter(([x, k]) => k < i && x.cat === c.cat).pop();
        if (prev) { [this.custom[prev[1]], this.custom[i]] = [this.custom[i], this.custom[prev[1]]]; rerender(); }
      };
    });
  },
};
const Theme = {
  apply() {
    const t = store.get('fm-theme', 'auto');
    const dark = t === 'dark' || (t === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  },
  init() { matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => this.apply()); this.apply(); },
};

// ===== 深い階層の警告しきい値(整理画面のツールバー) =====
const DeepSetting = {
  bind(sel) {
    const inp = $(sel);
    this.inp = inp;
    inp.value = S.settings?.deepDepth ?? 8;
    inp.onchange = guard(async () => {
      const v = Math.max(1, Math.floor(+inp.value || 0));
      S.settings = await api('/api/settings', { ...S.settings, deepDepth: v });
      this.sync(true);
      toast(`${v}階層以上を警告色で表示します`);
    });
  },
  sync(render) {
    if (this.inp) this.inp.value = S.settings?.deepDepth ?? 8;
    if (render) for (const g of [Views.organize.src, Views.organize.vt, Views.list.grid]) g?.render();
  },
};

// ===== データ取り込み =====
Views.import = {
  async enter() {
    await refreshState();
    const db = S.state.db;
    $('#home-current').innerHTML = db ? `<h2>開いているDB</h2>
      <div class="path" style="font-family:var(--mono);word-break:break-all">${esc(db.path)}</div>
      <p class="hint">取得元: ${srcLabel(db.meta)} (${esc((db.meta.source_path || '').split('\n').join(' / '))}) ・ 項目数: ${fmtNum(+db.meta.node_count)}</p>
      <button class="primary" onclick="show('organize')">整理を始める</button> <button onclick="show('summary')">サマリーを見る</button>`
      : `<h2>はじめに</h2><p class="hint">下の ①フォルダスキャン でデータを作るか、「ファイル」→「開く」/「最近開いたファイル」で保存済みのDBを開いてください。<br>DBの保存先: ${esc(S.state.projectsDir)}</p>`;
    if (S.state.job && !S.state.job.finished) Job.watch();
  },
};

const Job = {
  timer: 0,
  watch() {
    $('#home-job').hidden = false;
    clearInterval(this.timer);
    this.timer = setInterval(() => this.poll(), 700);
    this.poll();
  },
  poll: guard(async function () {
    const j = await api('/api/job');
    const p = j.total > 0 ? Math.min(100, j.done / j.total * 100) : 0;
    const kind = { scan: 'スキャン', merge: 'DB統合', actmerge: 'アクション統合' }[j.kind] || j.kind;
    const txt = `${kind}: ${j.phase}` + (j.total > 0 ? ` (${p.toFixed(0)}%)` : j.done ? ` ${fmtNum(j.done)}件` : '') + ` ・ 経過 ${j.elapsed}`;
    $('#job-text').textContent = txt;
    $('#job-prog').classList.toggle('indet', !(j.total > 0));
    $('#job-prog i').style.width = p + '%';
    $('#jobmini').hidden = !!j.finished;
    $('#jobmini').textContent = '⏳ ' + txt;
    if (j.finished) {
      clearInterval(Job.timer);
      $('#home-job').hidden = true;
      if (j.error) return toast(j.error, true);
      await refreshState(); dbOpened();
      toast(j.kind === 'actmerge' ? '統合が完了しました' : `完了しました(${fmtNum(j.result?.items)}項目)`);
      (j.result?.warnings || []).forEach(w => toast(w));
      show('summary');
    }
  }),
};

// DBの統合(ツリーのマージ)
const TreeMerge = {
  items: [],
  async add(paths) {
    paths = paths.map(p => p.trim().replace(/^"|"$/g, '')).filter(p => p && !this.items.some(x => x.path.toLowerCase() === p.toLowerCase()));
    if (!paths.length) return;
    const info = await api('/api/dbinfo', { paths });
    info.forEach(i => { if (i.error) toast(`${i.path}: ${i.error}`, true); else this.items.push(i); });
    this.render();
  },
  order() {
    if ($('#mg-prefer').value !== 'newest') return this.items;
    return [...this.items].sort((a, b) => (b.date || '').localeCompare(a.date || ''));
  },
  render() {
    const pri = new Map(this.order().map((x, i) => [x, i + 1]));
    $('#mg-list').innerHTML = this.items.length ? `<tr><th>優先</th><th>DB</th><th>ルート</th><th>取得日時</th><th></th></tr>` +
      this.items.map((x, i) => `<tr><td class="num">${pri.get(x)}</td><td title="${esc(x.path)}">📄 ${esc(x.path.split(/[\\/]/).pop())}</td><td class="wrap">${esc(x.root)}</td><td>${esc(x.date)}</td>
        <td><button class="mini" data-mv="${i},-1" ${i === 0 ? 'disabled' : ''}>↑</button> <button class="mini" data-mv="${i},1" ${i === this.items.length - 1 ? 'disabled' : ''}>↓</button> <button class="mini danger" data-rm="${i}">×</button></td></tr>`).join('')
      : '<tr><td class="muted">統合するDBを2つ以上追加してください。</td></tr>';
    $$('#mg-list [data-mv]').forEach(b => b.onclick = () => {
      const [i, d] = b.dataset.mv.split(',').map(Number);
      [this.items[i], this.items[i + d]] = [this.items[i + d], this.items[i]];
      this.render();
    });
    $$('#mg-list [data-rm]').forEach(b => b.onclick = () => { this.items.splice(+b.dataset.rm, 1); this.render(); });
  },
  init() {
    $('#mg-add').onclick = guard(async () => { await this.add([$('#mg-path').value]); $('#mg-path').value = ''; });
    $('#mg-path').onkeydown = e => { if (e.key === 'Enter') $('#mg-add').click(); };
    $('#mg-ref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'dbmulti' }); await this.add(r.paths || []); });
    $('#mg-recent').onclick = guard(() => pickRecent(paths => this.add(paths)));
    $('#mg-prefer').onchange = () => this.render();
    $('#mg-go').onclick = guard(async () => {
      if (this.items.length < 2) return toast('統合するDBを2つ以上追加してください', true);
      await api('/api/merge', { dbs: this.order().map(x => x.path), db: $('#mg-db').value, prefer: 'order' });
      Job.watch();
    });
    this.render();
  },
};
async function pickRecent(onPick) {
  const list = (await api('/api/recent')).filter(f => !f.missing);
  if (!list.length) return toast('最近開いたファイルがありません', true);
  modal(`<h2>最近開いたファイルから選択</h2><table class="t">${list.map((p, i) => `<tr><td><label><input type="checkbox" data-i="${i}"> 📄 ${esc(p.name)}</label><div class="muted" style="font-size:11px">${esc(p.path)}</div></td><td>${esc(p.modified)}</td></tr>`).join('')}</table>`,
    async m => { await onPick($$('input[data-i]:checked', m).map(c => list[+c.dataset.i].path)); }, '追加');
}

// ===== サマリー(5S) =====
function progressHTML(p) {
  if (!p) return '';
  const done = p.files - p.remFiles - (p.holdFiles || 0);
  return `<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
    <div class="tr" style="width:240px;height:12px;background:var(--track);border-radius:6px;overflow:hidden;display:flex" title="移動 ${fmtNum(p.moveFiles)} / 削除 ${fmtNum(p.delFiles)} / 保留 ${fmtNum(p.holdFiles || 0)} / 未処理 ${fmtNum(p.remFiles)}">
      <i class="seg-move" style="display:block;width:${pct(p.moveFiles, p.files)}%"></i><i class="seg-del" style="display:block;width:${pct(p.delFiles, p.files)}%"></i><i class="seg-hold" style="display:block;width:${pct(p.holdFiles || 0, p.files)}%"></i></div>
    <span>処理済み <b>${pct(done, p.files).toFixed(1)}%</b> (${fmtNum(done)} / ${fmtNum(p.files)} ファイル)</span>
    <span><span class="legend-dot seg-move"></span>移動 ${fmtNum(p.moveFiles)} (${fmtSize(p.moveSize)})</span>
    <span><span class="legend-dot seg-del"></span>削除 ${fmtNum(p.delFiles)} (${fmtSize(p.delSize)})</span>
    ${p.holdFiles ? `<span><a class="legend-hold" title="保留の項目だけを表示"><span class="legend-dot seg-hold"></span>保留 ${fmtNum(p.holdFiles)} (${fmtSize(p.holdSize)})</a></span>` : ''}
    <span><span class="legend-dot" style="background:var(--track);border:1px solid var(--line)"></span>未処理 ${fmtNum(p.remFiles)} (${fmtSize(p.remSize)})</span></div>`;
}
Views.summary = {
  async enter() {
    const el = $('#view-summary');
    if (!this.html) el.innerHTML = '<p class="muted">集計中…(初回は大規模データで数秒かかります)</p>';
    const [sm] = await Promise.all([api('/api/summary'), loadOwners()]);
    S.settings = sm.settings;
    const m = sm.meta, st = sm.settings;
    const groups = {};
    sm.checks.forEach(c => (groups[c.group] ||= []).push(c));
    const gdesc = { '整理': '整理(Seiri): 要らない物を見つける', '整頓': '整頓(Seiton): 探しやすい構造・名前にする', '移行(SharePoint)': '清掃・清潔: 移行や運用の妨げを取り除く' };
    const maxAge = Math.max(1, ...sm.age.map(a => a.count));
    const maxDepth = Math.max(1, ...sm.depth.map(a => a.count));
    const bar = (v, max) => `<div class="meter"><i style="width:${Math.round(v / max * 120)}px"></i></div>`;
    this.html = `
      <div class="cards">
        <div class="stat"><div class="k">ルート</div><div class="v" style="font-size:13px;word-break:break-all">${esc(m.root)}</div></div>
        <div class="stat"><div class="k">フォルダ数</div><div class="v">${fmtNum(sm.folders)}</div></div>
        <div class="stat"><div class="k">ファイル数</div><div class="v">${fmtNum(sm.files)}</div></div>
        <div class="stat"><div class="k">合計サイズ</div><div class="v">${fmtSize(sm.totalSize)}</div></div>
        <div class="stat"><div class="k">最大階層</div><div class="v">${sm.maxDepth}</div></div>
        <div class="stat"><div class="k">データ取得</div><div class="v" style="font-size:13px">${srcLabel(m)}<br>${esc(m.scanned_at || m.merged_at || m.built_at)}</div></div>
      </div>
      <div class="card"><h2>整理の進み具合</h2>${progressHTML(sm.progress)}
        <p><a onclick="show('organize')">整理画面を開く →</a> ・ <a onclick="show('list', {state: 'unhandled'})">未処理の項目を一覧 →</a></p></div>
      <div class="cols2">
        <div class="card"><h2>5S チェック</h2>
          <p class="hint">件数をクリックすると該当項目の一覧を表示します。判定の閾値は <a id="sm-settings">⚙ オプション</a> で変更できます(古い: ${st.oldYears}年 / パス長: ${st.pathLimit}文字 / 深い階層: ${st.deepDepth}階層以上 / 項目過多: ${st.manyFiles})。</p>
          ${Object.entries(groups).map(([g, cs]) => `<div class="check-grp"><h3>${esc(gdesc[g] || g)}</h3><table class="t">
            ${cs.map(c => `<tr class="click" data-check="${c.key}"><td>${esc(c.label)}</td><td class="num">${fmtNum(c.count)} 件</td><td class="num muted">${c.size ? fmtSize(c.size) : ''}</td></tr>`).join('')}</table></div>`).join('')}
        </div>
        <div>
          <div class="card"><h2>容量の大きいフォルダ(第1〜2階層)</h2><table class="t">
            ${sm.topFolders.map(f => `<tr class="click" data-id="${f.id}"><td class="wrap">📁 ${esc(m.virtual === 'true' ? f.path : (f.path.slice(m.root.length) || f.n))}</td><td class="num">${fmtSize(f.s)}</td><td>${bar(f.s, sm.totalSize || 1)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>最終更新からの経過(ファイル)</h2><table class="t"><tr><th>経過</th><th class="num">件数</th><th class="num">サイズ</th><th></th></tr>
            ${sm.age.map(a => `<tr><td>${a.label}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td><td>${bar(a.count, maxAge)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>階層の深さ別の項目数</h2><table class="t"><tr><th>階層</th><th class="num">項目数</th><th class="num">ファイル容量</th><th></th></tr>
            ${sm.depth.map(a => `<tr class="${+a.key >= st.deepDepth ? 'click' : ''}" data-deep="${a.key}"><td>${a.label}${+a.key >= st.deepDepth ? ' <span class="b org">深い</span>' : ''}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td><td>${bar(a.count, maxDepth)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>拡張子 上位10(容量順)</h2><table class="t">
            ${sm.topExts.map(x => `<tr class="click" data-ext="${esc(x.key)}"><td>${esc(x.label)}</td><td class="num">${fmtNum(x.count)} 件</td><td class="num">${fmtSize(x.size)}</td></tr>`).join('')}</table>
            <p><a onclick="show('exts')">すべての拡張子を見る →</a></p></div>
          <div class="card"><h2>作業者別・タグ別・担当別</h2>
            ${sm.editors.length ? `<table class="t"><tr><th>作業者</th><th class="num">設定数</th></tr>${sm.editors.map(a => `<tr><td>${esc(a.key)}</td><td class="num">${fmtNum(a.count)}</td></tr>`).join('')}</table>` : '<p class="muted">まだアクションはありません。</p>'}
            ${sm.tags.length ? `<p>${sm.tags.slice(0, 30).map(t => `<a data-tag="${esc(t.key)}"><span class="tag">${esc(t.key)} ${fmtNum(t.count)}</span></a>`).join('')}</p>` : ''}
            ${S.owners.length ? `<h3 style="margin-top:10px">担当別(担当範囲のファイル数)</h3><table class="t">${S.owners.map(o => `<tr><td class="muted">${OWNER_LV[o.level]}</td><td><a data-owner="${o.level}:${esc(o.value)}" title="整理画面でこの担当の範囲だけを表示">👤 ${esc(o.value)}</a></td><td class="num">${fmtNum(o.files)} ファイル</td></tr>`).join('')}</table>` : ''}
            <p><a onclick="show('plan')">アクション一覧を開く →</a></p></div>
        </div>
      </div>`;
    el.innerHTML = this.html;
    $$('tr[data-check]', el).forEach(tr => tr.onclick = () => show('list', { check: tr.dataset.check }));
    $$('tr[data-ext]', el).forEach(tr => tr.onclick = () => show('list', { ext: tr.dataset.ext, kind: 'file' }));
    $$('tr[data-id]', el).forEach(tr => tr.onclick = () => show('organize', { reveal: +tr.dataset.id }));
    $$('tr.click[data-deep]', el).forEach(tr => tr.onclick = () => show('list', { check: 'deep' }));
    $$('a[data-tag]', el).forEach(a => a.onclick = () => show('list', { tag: a.dataset.tag }));
    $('#sm-settings').onclick = () => Options.open();
    const lh = $('.legend-hold', el); if (lh) lh.onclick = () => show('organize', { filter: 'hold' });
    $$('a[data-owner]', el).forEach(a => a.onclick = () => show('organize', { owner: a.dataset.owner }));
  },
};

// ===== 整理(現在のツリー ⇔ 整理後のフォルダ構成) =====
Views.organize = {
  reset() {
    if (!this.src) this.build();
    $('#tr-owner').value = ''; $('#tr-filter').value = ''; $('#tr-filtered').innerHTML = '';
    this.model = new TreeModel(this.treeOpts());
    this.vmodel = new VTreeModel();
    this.loaded = false;
  },
  build() {
    this.src = new Grid($('#tr-grid'), {
      columns: this.columns(), rowClass, draggable: true,
      onSelect: rows => Sel.set(rows, this.src),
      onToggle: guard((r, i) => this.toggle(this.model, this.src, i)),
      onOpen: guard((r, i) => r.dir ? this.toggle(this.model, this.src, i) : openFile(r)),
      onArrow: guard(async (r, i, right) => {
        if (right && r._has && !r._open) await this.toggle(this.model, this.src, i);
        else if (!right && r._open) await this.toggle(this.model, this.src, i);
        else if (!right && r.d > 0) { const pi = this.model.indexOf(r.p); if (pi >= 0) this.src.moveTo(pi); }
      }),
      onKeyAction: k => {
        if (k === 'Delete') { guard(() => Plan.delete(S.sel))(); return true; }
        if (k === 'm' || k === 'M') { MoveFlow.start(S.sel); return true; }
        if (k === 'Backspace') { guard(() => Plan.clear(S.sel.filter(r => r.act)))(); return true; }
        if (k === 'h' || k === 'H') { guard(() => Plan.hold(S.sel))(); return true; }
        return false;
      },
      onContext: (rows, e, r) => {
        if (!r) return;
        Menu.show(e.clientX, e.clientY, [...actionItems(rows), '-', ...(rows.length === 1 ? [Shortcut.item(r), ...browseItems(r, false)] : [{ label: 'プロパティ(先頭の項目)', icon: 'ℹ', onClick: () => Props.show(r) }])]);
      },
    });
    this.vt = new Grid($('#vt-grid'), {
      keyOf: r => r.key, draggable: true,
      columns: [
        { key: 'name', label: '名前(整理後)', w: 300, render: r => nameCell(r, true, r.uuid === 'root' ? vroot() : r.n) + (r.nn && r.kind !== 'vdir' ? ` <span class="muted" title="元の名前">(元: ${esc(r.node.n)})</span>` : '') },
        { key: 'fc', label: 'ファイル', w: 70, cls: 'num', render: r => r.kind === 'file' || r.kind === 'vlink' ? '' : fmtNum(r.fc) },
        { key: 's', label: 'サイズ', w: 80, cls: 'num', render: r => r.kind === 'vlink' ? '' : fmtSize(r.s) },
        { key: 'tags', label: 'タグ', w: 120, render: r => tagsHTML(r.tags) },
        { key: 'owner', label: '担当', w: 150, render: ownerHTML },
        { key: 'due', label: '期限', w: 96, render: r => dueHTML(r.due) },
        { key: 'warn', label: '警告', w: 220, render: r => r.kind === 'vlink' ? (r.broken ? `<span class="b org" title="${esc(r.broken)}">⚠ ${esc(r.broken)}</span>` : `<span class="muted" title="${esc(r.linkPath)}">↪ ${esc(r.linkPath)}</span>`) : (r.rule?.length ? `<span class="b mig" title="${esc(r.rule.join('\n'))}">⛔ 5Sルール ${r.rule.length > 1 ? r.rule.length : ''}</span>` : '') + (r.sim?.length ? `<span class="b org" title="同じ階層に似た名前: ${esc(r.sim.join(', '))}">⚠ 似た名前 ${r.sim.length}</span>` : '') + (r.kind !== 'vdir' && r.node ? warnHTML({ ...r.node, r5s: '' }) : '') },
      ],
      rowClass: r => (r.rule?.length ? 'mig' : r.kind === 'vdir' ? 'vdir' : r.kind === 'dir' ? 'dir' : r.kind === 'vlink' ? 'vlink' + (r.broken ? ' broken' : '') : '') + (r.sim?.length ? ' sim' : ''),
      onSelect: rows => {
        if (Pick.on) { if (rows.length) guard(() => Pick.choose(rows[0]))(); return; }
        Sel.set(rows, this.vt);
      },
      onToggle: guard((r, i) => this.toggle(this.vmodel, this.vt, i)),
      onOpen: guard((r, i) => Pick.on ? null : r.kind === 'vlink' ? Shortcut.jump(r) : r.kind === 'file' ? openFile(r) : r._has ? this.toggle(this.vmodel, this.vt, i) : null),
      onArrow: guard(async (r, i, right) => { if (r._has && right !== !!r._open) await this.toggle(this.vmodel, this.vt, i); }),
      onKeyAction: k => {
        if (Pick.on) return false;
        const r = S.sel[0];
        if (k === 'F2' && S.sel.length === 1) { guard(() => this.rename(r))(); return true; }
        if (k === 'Delete' || k === 'Backspace') { // 仮想フォルダは削除、配置した項目は移動の解除
          if (S.sel.length === 1 && (r.kind === 'vdir' || r.kind === 'vlink')) { if (r.uuid !== 'root') guard(() => VOps.del(r))(); }
          else guard(() => Plan.clear(S.sel.filter(x => x.kind !== 'vdir' && x.act)))();
          return true;
        }
        if ((k === 'm' || k === 'M') && S.sel.every(x => x.kind !== 'vdir')) { MoveFlow.start(S.sel); return true; }
        return false;
      },
      onContext: (rows, e, r) => this.vMenu(rows, e, r),
      canDrop: (t, rows) => t.kind === 'vdir' && !rows.includes(t),
      onDrop: guard((t, rows) => this.drop(t, rows)),
    });
    this.src.setEmpty('項目がありません');
    $('#tr-collapse').onclick = guard(() => this.loadSrc());
    $('#tr-expand').onclick = guard(async () => {
      const r = this.src.selected()[0] || this.model.rows[0];
      await this.model.expandDepth(this.model.indexOf(r.id), +$('#tr-depth').value);
      this.src.refresh();
    });
    $('#tr-dirs').onchange = $('#tr-hide').onchange = $('#tr-owner').onchange = $('#tr-filter').onchange = guard(() => this.applyFilter());
    this.splitter();
    $('#tr-gauge').value = store.get('fm-gauge2', 'remain');
    $('#tr-gauge').onchange = () => { store.set('fm-gauge2', $('#tr-gauge').value); this.src.cols = this.columns(); this.src.renderHead(); this.src.refresh(); };
    DeepSetting.bind('#tr-deep');
    const go = guard(async () => {
      const p = $('#tr-path').value.trim(); if (!p) return;
      const { id } = await api('/api/find?path=' + encodeURIComponent(p));
      await this.reveal(id);
    });
    $('#tr-go').onclick = go;
    $('#tr-path').onkeydown = e => { if (e.key === 'Enter') go(); };
    $('#vt-new').onclick = guard(() => VOps.new(this.vmodel.rows[0]));
    $('#vt-warn').onclick = guard(() => this.showWarnings());
    $('#vt-rule').onclick = guard(() => this.showRuleViolations());
    $('#tr-rule').onclick = guard(() => this.setFilter($('#tr-filter').value === 'rule' ? '' : 'rule'));
    $('#pick-cancel').onclick = () => Pick.cancel();
  },
  // 仮想ツリーの行 i の親の仮想フォルダ(配置した実フォルダの中なら、その上の仮想フォルダ)
  vParent(r) {
    const rows = this.vmodel.rows;
    let i = rows.indexOf(r), d = r.d;
    for (let k = i - 1; k >= 0; k--) if (rows[k].d < d) { if (rows[k].kind === 'vdir') return rows[k]; d = rows[k].d; }
    return rows[0];
  },
  vMenu(rows, e, r) {
    if (Pick.on) {
      if (!r || r.kind !== 'vdir') return;
      return Menu.show(e.clientX, e.clientY, [
        { label: `「${r.uuid === 'root' ? vroot() : r.n}」へ移動`, icon: '📥', onClick: () => Pick.choose(r) },
        { label: 'この下に新規フォルダを作って移動…', icon: '➕', onClick: async () => { const id = await VOps.new(r, true); if (id) await Pick.choose({ kind: 'vdir', uuid: id }); } },
        '-', { label: '移動を中止', icon: '✖', onClick: () => Pick.cancel() },
      ]);
    }
    if (!r) return Menu.show(e.clientX, e.clientY, [{ label: `新規フォルダ(「${vroot()}」の直下)`, icon: '➕', onClick: () => VOps.new(this.vmodel.rows[0]) }]);
    const vdirs = rows.filter(x => x.kind === 'vdir' || x.kind === 'vlink'), nodes = rows.filter(x => x.kind !== 'vdir' && x.kind !== 'vlink');
    if (rows.length > 1) {
      return Menu.show(e.clientX, e.clientY, vdirs.length && !nodes.length ? [{ label: `移動…(${vdirs.length}件の仮想フォルダ)`, icon: '📦', onClick: () => VOps.move(vdirs) }]
        : [{ label: `移動…(${nodes.length}件)`, icon: '📦', onClick: () => MoveFlow.start(nodes) },
          { label: '解除', icon: '↺', disabled: !nodes.some(x => x.act), onClick: () => Plan.clear(nodes.filter(x => x.act)) },
          { label: 'タグ / メモの編集…', icon: '🏷', onClick: () => EditNotes.show(nodes) }]);
    }
    if (r.kind === 'vlink') {
      return Menu.show(e.clientX, e.clientY, [
        { label: 'リンク先へ移動', icon: '↪', key: 'ダブルクリック', onClick: () => Shortcut.jump(r) },
        '-',
        { label: '名前の変更', icon: '✏', key: 'F2', onClick: () => VOps.rename(r) },
        { label: '移動…', icon: '📦', onClick: () => VOps.move([r]) },
        { label: '削除', icon: '🗑', key: 'Del', onClick: () => VOps.del(r) },
        '-',
        { label: 'プロパティ', icon: 'ℹ', onClick: () => Props.show(r) },
      ]);
    }
    if (r.kind === 'vdir') {
      const root = r.uuid === 'root';
      return Menu.show(e.clientX, e.clientY, [
        { label: '名前の変更', icon: '✏', key: 'F2', onClick: () => this.rename(r) },
        { label: '移動…', icon: '📦', disabled: root, onClick: () => VOps.move([r]) },
        { label: '削除', icon: '🗑', key: 'Del', disabled: root, onClick: () => VOps.del(r) },
        { label: '新規フォルダ(この直下)', icon: '➕', onClick: () => VOps.new(r) },
        Shortcut.item(r),
        '-',
        { label: 'メモの編集…', icon: '🏷', disabled: root, onClick: () => EditNotes.showV(r) },
        { label: 'プロパティ', icon: 'ℹ', onClick: () => Props.show(r) },
      ]);
    }
    const parent = this.vParent(r);
    Menu.show(e.clientX, e.clientY, [
      { label: '名前の変更', icon: '✏', key: 'F2', disabled: r.act !== 'move', title: r.act === 'move' ? '' : '名前変更は、移動を設定した項目(フォルダの中身ではなく、仮想フォルダの直下に置いた項目)に設定できます', onClick: () => this.rename(r) },
      { label: '移動…', icon: '📦', key: 'M', onClick: () => MoveFlow.start([r]) },
      { label: '解除(移動を取り消す)', icon: '↺', key: 'Del', disabled: !r.act, title: r.act ? '' : '親フォルダの移動に含まれています。親フォルダで解除してください', onClick: () => Plan.clear([r]) },
      { label: `新規フォルダ(「${parent.uuid === 'root' ? vroot() : parent.n}」の直下)`, icon: '➕', onClick: () => VOps.new(parent) },
      '-',
      { label: 'タグ / メモの編集…', icon: '🏷', onClick: () => EditNotes.show([r]) },
      ...(r.kind === 'file' ? [{ label: 'ファイルを開く', icon: '📂', key: 'ダブルクリック', onClick: () => openFile(r) }] : []),
      Shortcut.item(r),
      { label: 'ツリーへ移動(現在の場所を表示)', icon: '🌳', onClick: () => this.reveal(r.id) },
      { label: 'プロパティ', icon: 'ℹ', onClick: () => Props.show(r) },
    ]);
  },
  async rename(r) {
    if (r.kind === 'vdir' || r.kind === 'vlink') return VOps.rename(r);
    if (r.act !== 'move') return toast('名前変更は、移動を設定した項目にだけ設定できます', true);
    const name = prompt('移動後の名前(空欄で元の名前に戻す)', r.nn || r.node.n);
    if (name === null) return;
    await Plan.fields([r], { newName: name.trim() === r.node.n ? '' : name.trim() });
  },
  treeOpts() { return { dirs: $('#tr-dirs').checked, hide: $('#tr-hide').checked, owner: $('#tr-owner').value, tf: $('#tr-filter').value }; },
  ownerOptions() { const cur = $('#tr-owner').value; $('#tr-owner').innerHTML = ownerOptionsHTML(cur, 'すべての担当'); $('#tr-owner').value = cur; },
  async setFilter(tf) { $('#tr-filter').value = tf; await this.applyFilter(); },
  async applyFilter() {
    this.model.o = this.treeOpts();
    const o = this.model.o, chips = [];
    if (o.owner) chips.push('👤 ' + ownerSpecLabel(o.owner));
    if (o.tf) chips.push({ hold: '⏸ 保留のみ', unhandled: '未処理のみ', rule: '⛔ 5Sルール外れのみ' }[o.tf]);
    $('#tr-filtered').innerHTML = chips.length ? `<span class="b org" title="該当する項目と、そこまでのフォルダ(薄い色)だけを表示しています">絞り込み: ${chips.map(esc).join(' + ')} <a id="tr-unfilter" title="絞り込みを解除">×</a></span>` : '';
    if (chips.length) $('#tr-unfilter').onclick = guard(async () => { $('#tr-owner').value = ''; $('#tr-filter').value = ''; await this.applyFilter(); });
    await this.model.reloadKeep();
    this.src.setSource(this.model, true);
    if (o.owner || o.tf) { // 絞り込んだら該当箇所が見えるよう、2階層まで開く
      const r0 = this.model.rows[0];
      if (r0 && this.model.rows.length < 400) { await this.model.expandDepth(0, 2); this.src.refresh(); }
    }
    this.src.setEmpty(o.owner || o.tf ? '該当する項目はありません' : '項目がありません');
    this.src.refresh();
  },
  // 左右の境目をドラッグして幅を調整(幅は保存する)
  splitter() {
    const sp = $('#org-split'), src = $('#view-organize .pane.src'), dst = $('#view-organize .pane.dst');
    const set = f => { src.style.flex = `${f} 1 0`; dst.style.flex = `${1 - f} 1 0`; };
    const saved = +store.get('fm-split', 0);
    if (saved > 0.1 && saved < 0.9) set(saved);
    sp.addEventListener('mousedown', e => {
      e.preventDefault();
      const box = sp.parentElement.getBoundingClientRect();
      sp.classList.add('drag'); document.body.classList.add('colresize');
      let f = 0;
      const mv = ev => { f = Math.min(0.85, Math.max(0.15, (ev.clientX - box.left) / box.width)); set(f); };
      const up = () => {
        removeEventListener('mousemove', mv); removeEventListener('mouseup', up);
        sp.classList.remove('drag'); document.body.classList.remove('colresize');
        if (f) store.set('fm-split', f.toFixed(3));
      };
      addEventListener('mousemove', mv); addEventListener('mouseup', up);
    });
    sp.ondblclick = () => { src.style.flex = dst.style.flex = ''; store.set('fm-split', ''); };
  },
  gaugeMode() { return store.get('fm-gauge2', 'remain'); },
  columns() {
    const c = cols({ key: 'name', label: '名前(現在)', w: 400, render: r => nameCell(r, true) }, 'depth', 'size');
    const mode = this.gaugeMode();
    if (mode !== 'off') c.push({
      key: 'gauge', w: 140, label: mode === 'remain' ? '未処理' : mode === 'root' ? 'サイズ比(全体)' : 'サイズ比(親内)',
      tip: mode === 'remain' ? 'フォルダ内でアクションが未設定のファイルの割合' : mode === 'root' ? 'ルート全体のサイズに占める割合' : '1つ上のフォルダのサイズに占める割合',
      render: r => this.gauge(r, mode),
    });
    return c.concat(cols('action', 'owner', 'tags', 'due', 'mtime', 'warn'));
  },
  gauge(r, mode) {
    if (r.dir && (r.f & FL.access)) return `<span class="muted" title="${esc(r.err || 'アクセス不可')}">不明</span>`;
    if (mode === 'remain') {
      if (!r.dir) return eff(r) ? '<span class="muted">処理済み</span>' : '<span>未処理</span>';
      return r.fc ? gaugeHTML(pct(r.rem, r.fc), `${fmtNum(r.rem)}`, `未処理 ${fmtNum(r.rem)} / ${fmtNum(r.fc)} ファイル (${pct(r.rem, r.fc).toFixed(1)}%)`) : '';
    }
    const base = mode === 'root' ? (this.model.rows[0]?.s || 0) : r._ps;
    const p = pct(r.s, base);
    return gaugeHTML(p, (r.s > 0 && p < 0.1 ? '<0.1' : p.toFixed(1)) + '%', `${fmtSize(r.s)} / ${fmtSize(base)}`);
  },
  async loadSrc() { this.model.o = this.treeOpts(); await this.model.load(); this.src.setSource(this.model); },
  async loadV() { await this.vmodel.load(); this.vt.setSource(this.vmodel); },
  async toggle(model, grid, i) {
    const r = model.rows[i];
    if (r._open) model.collapse(i); else await model.expand(i);
    grid.refresh();
  },
  async reveal(id) {
    const i = await this.model.reveal(id);
    this.src.refresh();
    if (i >= 0) this.src.focusKey(id);
    else toast('この項目は「アクション設定済みを非表示」または絞り込みで隠れています', true);
  },
  async focusV(uuid) {
    const i = await this.vmodel.revealV(uuid);
    this.vt.refresh();
    if (i >= 0) { this.vt.focusKey('v:' + uuid); Sel.set(this.vt.selected(), this.vt); }
  },
  async drop(t, rows) {
    const vdirs = rows.filter(r => r.kind === 'vdir' || r.kind === 'vlink'), nodes = rows.filter(r => r.kind !== 'vdir' && r.kind !== 'vlink');
    for (const v of vdirs) {
      const r = await api('/api/vmove', { uuid: v.uuid, parent: t.uuid });
      showIssue(r.issue, 'フォルダの移動');
    }
    if (nodes.length) await Plan.move(nodes, t.uuid);
    else await afterEdit();
  },
  async showWarnings() {
    const w = await api('/api/vwarnings');
    modal(`<h2>⚠ 似た名前のフォルダ(同じ階層)</h2><p class="hint">目的が同じフォルダが別名で作られていないか確認してください(表記ゆれ・一方を含む・よく似た名前を検出。年度など数字だけの違いは除外)。</p>
      <table class="t">${w.map(([a, b]) => `<tr><td>${esc(vpathLabel(a))}</td><td>⇔ ${esc(b)}</td></tr>`).join('')}</table>`, null, '', '閉じる');
  },
  async showRuleViolations() {
    const v = await api('/api/vrules');
    const shown = v.slice(0, 2000);
    modal(`<h2>⛔ 5Sルールに合わない項目(整理後のフォルダ構成)</h2>
      <p class="hint">ルールを後から設定・変更した場合などに、すでに作った構成の違反をここで確認できます。名前の変更・移動で直してください(ルールは ⚙ オプション → 5Sルール で変更できます)。<br>
      赤 = 禁止のルール、黄 = 警告のみのルール</p>
      <table class="t" style="table-layout:fixed">${shown.map(x => `<tr><td class="wrap" style="width:40%">${esc(vpathLabel(x.path))}</td><td class="wrap"><span class="b ${x.block ? 'mig' : 'org'}">${x.block ? '禁止' : '警告'}</span>${esc(x.msg)}</td></tr>`).join('')}</table>
      ${v.length > shown.length ? `<p class="hint">ほか ${fmtNum(v.length - shown.length)} 件</p>` : ''}`, null, '', '閉じる').querySelector('.box').classList.add('wide');
  },
  async updateBar() {
    const [p, w, h] = await Promise.all([api('/api/progress'), api('/api/vwarnings'), api('/api/rulehits')]);
    $('#org-progress').innerHTML = progressHTML(p);
    const lh = $('#org-progress .legend-hold'); if (lh) lh.onclick = guard(() => this.setFilter('hold'));
    $('#vt-warn').hidden = !w.length;
    $('#vt-warn').textContent = `⚠ 似た名前 ${w.length}組`;
    $('#tr-rule').hidden = !h.count;
    $('#tr-rule').textContent = `⛔ 5S外れ ${fmtNum(h.count)}件`;
    $('#tr-rule').classList.toggle('primary', $('#tr-filter').value === 'rule');
    $('#tr-filter option[value="rule"]').disabled = !S.rules?.applyCurrent;
    // 整理後の構成のルール違反は大規模な構成では計算に時間がかかるため、待たずに後から表示する
    const ver = this.barv = (this.barv || 0) + 1;
    api('/api/vrules').then(v => {
      if (ver !== this.barv) return;
      $('#vt-rule').hidden = !v.length;
      $('#vt-rule').textContent = `⛔ ルール違反 ${fmtNum(v.length)}件`;
    }, () => { });
  },
  async afterEdit() {
    await Promise.all([this.model.reloadKeep(), this.vmodel.reloadKeep()]);
    this.src.setSource(this.model, true);
    this.vt.setSource(this.vmodel, true);
    await this.updateBar();
  },
  async planChanged() { if (this.loaded) await this.afterEdit(); },
  async enter(arg) {
    if (arg.filter !== undefined || arg.owner !== undefined) {
      if (arg.owner !== undefined) { this.ownerOptions(); $('#tr-owner').value = arg.owner; }
      if (arg.filter !== undefined) $('#tr-filter').value = arg.filter;
      if (this.loaded) await this.applyFilter();
    }
    if (!this.loaded) { await Promise.all([this.loadSrc(), this.loadV()]); this.loaded = true; await this.updateBar(); if (this.model.o.owner || this.model.o.tf) await this.applyFilter(); }
    else { this.src.render(); this.vt.render(); }
    if (arg.reveal) await this.reveal(arg.reveal);
    this.src.host.focus();
  },
};

// 検索・リスト / アクション一覧の右クリック(整理アクションは整理画面で行う)
function listMenu(rows, e, r) {
  if (!r) return;
  Menu.show(e.clientX, e.clientY, [
    ...(rows.length === 1 ? browseItems(r, true) : [{ label: 'ツリーへ移動(先頭の項目)', icon: '🌳', onClick: () => Nav.go('tree', r) }, { label: 'プロパティ(先頭の項目)', icon: 'ℹ', onClick: () => Props.show(r) }]),
    '-', ...ownerItems(rows), { label: `タグ / メモの編集…${rows.length > 1 ? `(${rows.length}件)` : ''}`, icon: '🏷', onClick: () => EditNotes.show(rows) },
  ]);
}

// ===== 検索・リスト =====
const STATES = [['', '処理状況: すべて'], ['unhandled', '未処理'], ['handled', '処理済み(親フォルダの設定を含む)'], ['own', '個別に設定あり'], ['delete', '削除(個別)'], ['move', '移動(個別)'], ['hold', '保留(個別)'], ['ihold', '保留(親フォルダの設定を含む)']];
Views.list = {
  reset() {
    if (!this.grid) {
      this.grid = new Grid($('#ls-grid'), {
        columns: cols({ key: 'name', label: '名前', w: 240, sort: true, render: r => nameCell(r, false) }, 'depth', 'kind', 'ext', 'mtime', 'size', 'action', 'owner', 'tags', 'due', 'warn', 'path'),
        rowClass,
        onSelect: rows => Sel.set(rows, this.grid),
        onOpen: guard(r => r.dir ? Nav.go('tree', r) : openFile(r)),
        onSort: () => this.search(),
        onContext: listMenu,
      });
      this.grid.setEmpty('該当する項目はありません');
      $('#ls-go').onclick = guard(() => this.search());
      $$('#view-list .toolbar input').forEach(i => i.addEventListener('keydown', e => { if (e.key === 'Enter') this.search(); }));
      $$('#view-list .toolbar select').forEach(s => s.addEventListener('change', () => this.search()));
      $('#ls-clear').onclick = () => { this.setForm({}); this.search(); };
      $('#ls-csv').onclick = () => download('/api/export/list.csv', this.params());
      $('#ls-bulk').onclick = () => this.bulk();
    }
    $('#ls-check').innerHTML = '<option value="">警告・ヒント: 指定なし</option>' + S.checks.map(c => `<option value="${c.key}">[${esc(c.group)}] ${esc(c.label)}</option>`).join('');
    $('#ls-state').innerHTML = STATES.map(([k, l]) => `<option value="${k}">${l}</option>`).join('');
    this.scope = null;
    this.setForm({});
    this.ran = false;
  },
  ownerOptions() { const cur = $('#ls-owner').value; $('#ls-owner').innerHTML = ownerOptionsHTML(cur, '担当: 指定なし'); $('#ls-owner').value = cur; },
  tagOptions() { const cur = $('#ls-tag').value; $('#ls-tag').innerHTML = '<option value="">タグ: 指定なし</option>' + S.tags.map(t => `<option>${esc(t)}</option>`).join(''); $('#ls-tag').value = cur; },
  setForm(a) {
    $('#ls-q').value = a.q || ''; $('#ls-kind').value = a.kind || ''; $('#ls-ext').value = a.ext || '';
    $('#ls-check').value = a.check || ''; $('#ls-state').value = a.state || '';
    this.tagOptions(); $('#ls-tag').value = a.tag || '';
    this.ownerOptions(); $('#ls-owner').value = a.owner || '';
    $('#ls-min').value = ''; $('#ls-before').value = '';
    this.scope = a.under ? { k: 'under', v: a.under, label: '配下: ' + a.underPath.split(/[\\/]/).pop(), title: a.underPath }
      : a.vparent ? { k: 'vparent', v: a.vparent, label: '移動先: ' + a.vparentPath, title: a.vparentPath } : null;
    this.renderScope();
  },
  renderScope() {
    const el = $('#ls-under');
    el.hidden = !this.scope;
    if (this.scope) {
      el.innerHTML = `<span class="b org" title="${esc(this.scope.title)}">${esc(this.scope.label)} <a id="ls-under-x">×</a></span>`;
      $('#ls-under-x').onclick = () => { this.scope = null; this.renderScope(); this.search(); };
    }
  },
  params() {
    const p = { q: $('#ls-q').value, kind: $('#ls-kind').value, ext: $('#ls-ext').value, check: $('#ls-check').value, state: $('#ls-state').value, tag: $('#ls-tag').value, owner: $('#ls-owner').value };
    const mb = +$('#ls-min').value; if (mb > 0) p.minSize = Math.round(mb * 1048576);
    const d = $('#ls-before').value; if (d) p.before = Math.floor(new Date(d + 'T00:00:00').getTime() / 1000);
    if (this.scope) p[this.scope.k] = this.scope.v;
    const s = this.grid.o.sort; if (s) { p.sort = s.key; if (s.desc) p.desc = 1; }
    for (const k in p) if (p[k] === '' || p[k] == null) delete p[k];
    return p;
  },
  search: guard(async function (keep) {
    const self = Views.list;
    const p = self.params();
    $('#ls-count').textContent = '検索中…';
    const src = new PagedSource((off, lim) => '/api/search?' + new URLSearchParams({ ...p, offset: off, limit: lim }),
      n => $('#ls-count').textContent = `${fmtNum(n)} 件`);
    await src.init();
    self.grid.setSource(src, keep === true);
    self.ran = true;
    if (keep !== true) Sel.set([], self.grid);
  }),
  bulk() {
    const p = this.params();
    const n = $('#ls-count').textContent;
    modal(`<h2>検索結果すべてに一括操作</h2><p class="hint">現在の検索結果(${esc(n)})のすべての項目に適用します。</p>
      <div class="form" style="grid-template-columns:150px 300px">
        <span>操作</span><select id="bk-op"><option value="delete">削除を設定</option><option value="move">移動を設定(移動先を選択)</option><option value="hold">保留を設定</option><option value="clear">アクションを解除</option><option value="tag">タグを追加</option><option value="owner">担当を設定</option></select>
        <span>タグ / 担当</span><input type="text" id="bk-tag" list="taglist" placeholder="タグ、または担当「部/課/担当/担当者」(空欄可。例: 整備業務部/業務推進課//山田)"></div>`,
      async m => {
        const op = $('#bk-op', m).value, tag = $('#bk-tag', m).value.trim();
        if ((op === 'tag' || op === 'owner') && !tag) { toast(op === 'tag' ? 'タグを入力してください' : '担当を入力してください', true); return false; }
        let target = '';
        if (op === 'move') { m.close(); target = await VPicker.pick('検索結果すべての移動先を選択'); if (!target) return; }
        const r = await api('/api/plan/filter', { query: new URLSearchParams(p).toString(), op, target, tag });
        if (op === 'move') showReport(r, '移動'); else toast(`${fmtNum(r.applied)}件に適用しました`);
        if (op === 'tag') { S.tags.includes(tag) || S.tags.push(tag); updateTagList(); }
        if (op === 'owner') await loadOwners();
        await afterEdit();
      }, '実行');
  },
  async afterEdit() { if (this.ran) await this.search(true); },
  async planChanged() { this.tagOptions(); this.ownerOptions(); if (this.ran) await this.search(true); },
  async enter(arg) {
    if (Object.keys(arg).length) { this.setForm(arg); return this.search(); }
    this.tagOptions();
    if (!this.ran) return this.search();
    this.grid.render();
  },
};

// ===== 重複候補 =====
Views.dups = {
  reset() { this.off = 0; this.under = null; this.data = null; this.hash = {}; },
  async enter(arg) {
    if (arg.under) { this.under = { id: arg.under, path: arg.underPath }; this.off = 0; this.data = null; }
    if (!this.data) await this.load();
    else this.render();
  },
  async load() {
    const q = new URLSearchParams({ offset: this.off, limit: 50 });
    if (this.under) q.set('under', this.under.id);
    this.data = await api('/api/dups?' + q);
    this.render();
  },
  async afterEdit() {
    const keep = new Set(S.sel.map(r => r.id));
    await this.load();
    S.sel = this.data.groups.flatMap(g => g.members).filter(r => keep.has(r.id));
    this.render();
  },
  planChanged() { this.data = null; },
  render() {
    const el = $('#view-dups'), d = this.data;
    if (!d) return;
    const pages = Math.ceil(d.total / 50), page = this.off / 50 + 1;
    el.innerHTML = `<div class="card"><h2>重複候補(同名・同サイズ)${this.under ? ` — 配下: ${esc(this.under.path)} <a id="dp-all">全体に戻す</a>` : ''}</h2>
      <p class="hint">${fmtNum(d.total)} グループ ・ 重複を1つに減らすと最大 <b>${fmtSize(d.wasted)}</b> 削減できます(削減量の多い順)。<br>
      ファイル名とサイズだけで判定しています。「内容を比較」でSHA-256を計算し、本当に同一か確認できます(このPCからアクセスできる場合)。<br>
      項目をクリック → 右パネルでアクションを設定。「最新以外を削除」で、更新日時が最新の1件を残し他に「削除」を設定します。</p>
      <div style="display:flex;gap:6px;align-items:center"><button id="dp-prev" ${page <= 1 ? 'disabled' : ''}>← 前</button><span>${page} / ${Math.max(1, pages)} ページ</span><button id="dp-next" ${page >= pages ? 'disabled' : ''}>次 →</button></div></div>
      ${d.groups.map((g, gi) => `<div class="dupg"><div class="h"><b>${esc(g.name)}</b><span>${fmtSize(g.size)} × ${fmtNum(g.count)}件</span>
        <span class="muted">削減可能 ${fmtSize(g.size * (g.count - 1))}</span><span class="grow" style="flex:1"></span>
        <button data-hash="${gi}">内容を比較</button><button data-keep="${gi}">最新以外を削除</button></div>
        ${g.members.map(r => { const h = this.hash[r.id]; return `<div class="m ${S.sel.some(s => s.id === r.id) ? 'sel' : ''}" data-g="${gi}" data-id="${r.id}">
          <span>${fmtDate(r.m)}</span><span class="p" style="${eff(r) === 'delete' ? 'text-decoration:line-through;color:var(--gone)' : ''}">${esc(r.path)}</span>${actHTML(r)}${h ? `<span class="hash" title="${esc(h)}">${esc(h.startsWith('ERROR') ? h : h.slice(0, 10))}</span>` : ''}</div>`; }).join('')}
        ${g.count > g.members.length ? `<div class="m muted">… 他 ${fmtNum(g.count - g.members.length)} 件(検索・リストで「${esc(g.name)}」を検索してください)</div>` : ''}</div>`).join('')}`;
    $('#dp-prev').onclick = guard(() => { this.off -= 50; return this.load(); });
    $('#dp-next').onclick = guard(() => { this.off += 50; return this.load(); });
    if (this.under) $('#dp-all').onclick = guard(() => { this.under = null; this.off = 0; return this.load(); });
    $$('.dupg .m[data-id]', el).forEach(m => m.ondblclick = guard(() => openFile(d.groups[+m.dataset.g].members.find(x => x.id === +m.dataset.id))));
    $$('.dupg .m[data-id]', el).forEach(m => m.oncontextmenu = e => {
      e.preventDefault();
      const r = d.groups[+m.dataset.g].members.find(x => x.id === +m.dataset.id);
      if (!S.sel.some(x => x.id === r.id)) { Sel.set([r], null); this.render(); }
      const rows = S.sel;
      Menu.show(e.clientX, e.clientY, [
        { label: `削除${rows.length > 1 ? `(${rows.length}件)` : ''}`, icon: '🗑', onClick: () => Plan.delete(rows) },
        { label: '解除', icon: '↺', disabled: !rows.some(x => x.act), onClick: () => Plan.clear(rows.filter(x => x.act)) },
        '-', ...browseItems(r, true),
      ]);
    });
    $$('.dupg .m[data-id]', el).forEach(m => m.onclick = e => {
      const g = d.groups[+m.dataset.g], r = g.members.find(x => x.id === +m.dataset.id);
      const sel = (e.ctrlKey || e.metaKey) ? (S.sel.some(s => s.id === r.id) ? S.sel.filter(s => s.id !== r.id) : [...S.sel, r]) : [r];
      Sel.set(sel, null); this.render();
    });
    $$('[data-hash]', el).forEach(b => b.onclick = guard(async () => {
      b.disabled = true; b.textContent = '計算中…';
      const g = d.groups[+b.dataset.hash];
      Object.assign(this.hash, await api('/api/hash', { ids: g.members.map(m => m.id) }));
      const hs = new Set(g.members.map(m => this.hash[m.id]));
      toast(hs.size === 1 && ![...hs][0].startsWith('ERROR') ? 'すべて同一内容です' : '内容が異なる(または読めない)ファイルがあります');
      this.render();
    }));
    $$('[data-keep]', el).forEach(b => b.onclick = guard(async () => {
      const g = d.groups[+b.dataset.keep];
      const [, ...rest] = [...g.members].sort((a, b) => b.m - a.m);
      await api('/api/plan/delete', { ids: rest.map(r => r.id) });
      await api('/api/plan/fields', { ids: rest.map(r => r.id), memo: '重複(最新を残す)' });
      toast(`${rest.length}件に「削除」を設定しました`);
      await afterEdit();
    }));
  },
};

// ===== 拡張子別 =====
Views.exts = {
  reset() { this.rows = null; },
  async enter() {
    if (!this.rows) this.rows = await api('/api/exts?limit=1000');
    const tot = this.rows.reduce((a, x) => a + x.size, 0) || 1;
    const cnt = this.rows.reduce((a, x) => a + x.count, 0);
    $('#view-exts').innerHTML = `<div class="card"><h2>拡張子別の集計(容量順)</h2><p class="hint">${fmtNum(this.rows.length)} 種類 ・ ${fmtNum(cnt)} ファイル。行をクリックすると、その拡張子のファイル一覧を表示します。</p>
      <table class="t"><tr><th>拡張子</th><th class="num">件数</th><th class="num">合計サイズ</th><th class="num">割合</th><th></th><th class="num">平均サイズ</th></tr>
      ${this.rows.map(x => `<tr class="click" data-ext="${esc(x.key)}"><td>${fileIcon(x.key)} ${esc(x.label)}</td><td class="num">${fmtNum(x.count)}</td><td class="num">${fmtSize(x.size)}</td>
        <td class="num">${(x.size / tot * 100).toFixed(1)}%</td><td><div class="meter"><i style="width:${Math.round(x.size / tot * 160)}px"></i></div></td><td class="num">${fmtSize(Math.round(x.size / x.count))}</td></tr>`).join('')}</table></div>`;
    $$('#view-exts tr[data-ext]').forEach(tr => tr.onclick = () => show('list', { ext: tr.dataset.ext, kind: 'file' }));
  },
};

// ===== アクション一覧 =====
Views.plan = {
  reset() {
    if (!this.grid) {
      this.grid = new Grid($('#pl-grid'), {
        columns: cols('action', { key: 'name', label: '名前', w: 220, sort: true, render: r => nameCell(r, false) }, 'owner', 'tags', 'due', 'memo', 'editor', 'size', 'mtime', 'path'),
        rowClass,
        onSelect: rows => Sel.set(rows, this.grid),
        onOpen: guard(r => r.dir ? Nav.go('tree', r) : openFile(r)),
        onSort: () => this.load(true),
        onContext: listMenu,
      });
      this.grid.setEmpty('アクションはまだありません');
      $('#pl-state').onchange = $('#pl-editor').onchange = $('#pl-tag').onchange = $('#pl-owner').onchange = guard(() => this.load());
      $('#pl-csv').onclick = () => download('/api/export/list.csv', this.params());
      $('#pl-ps1').onclick = () => modal(`<h2>PowerShell実行スクリプトの出力</h2><p class="hint">整理後のフォルダ構成を作成し、「削除」「移動(名前変更を含む)」を実行するスクリプト(.ps1)を出力します。<br>
        ・<b>既定はドライラン</b>です。そのまま実行しても何も変更せず、実行予定をログCSVに書き出します。<br>
        ・内容を確認後、<code>-Execute</code> を付けて実行すると実際に変更します。<br>
        ・整理後のルートの場所は「⚙ オプション」で設定、または <code>-TargetRoot "\\\\srv\\share\\整理後"</code> で指定できます。<br>
        ・子→親の順に実行するため、フォルダを移動する前に、その配下の個別の削除・移動が済みます。<br>
        ・「削除」は完全削除です(ごみ箱に入りません)。</p>`, () => download('/api/export/plan.ps1'), 'ダウンロード');
    }
  },
  params() {
    const p = { state: $('#pl-state').value || 'own' };
    const e = $('#pl-editor').value; if (e) p.editor = e;
    const t = $('#pl-tag').value; if (t) p.tag = t;
    const o = $('#pl-owner').value; if (o) p.owner = o;
    const s = this.grid.o.sort; if (s) { p.sort = s.key; if (s.desc) p.desc = 1; }
    return p;
  },
  async load(keep) {
    const p = this.params();
    const src = new PagedSource((off, lim) => '/api/search?' + new URLSearchParams({ ...p, offset: off, limit: lim }), n => $('#pl-count').textContent = `${fmtNum(n)} 件`);
    await src.init();
    this.grid.setSource(src, keep === true);
  },
  ownerOptions() { const cur = $('#pl-owner').value; $('#pl-owner').innerHTML = ownerOptionsHTML(cur, '担当: すべて'); $('#pl-owner').value = cur; },
  async afterEdit() { await this.load(true); await this.top(); },
  async top() {
    const sm = await api('/api/summary');
    const cur = $('#pl-editor').value, curT = $('#pl-tag').value;
    $('#pl-editor').innerHTML = '<option value="">作業者: すべて</option>' + sm.editors.map(o => `<option value="${esc(o.key)}">${esc(o.key)}</option>`).join('');
    $('#pl-editor').value = cur;
    $('#pl-tag').innerHTML = '<option value="">タグ: すべて</option>' + sm.tags.map(o => `<option value="${esc(o.key)}">${esc(o.key)}</option>`).join('');
    $('#pl-tag').value = curT;
    $('#plan-top').innerHTML = `<div class="card"><h2>整理の進み具合</h2>${progressHTML(sm.progress)}</div>`;
    const lh = $('#plan-top .legend-hold'); if (lh) lh.onclick = () => { $('#pl-state').value = 'hold'; guard(() => this.load())(); };
    this.ownerOptions();
  },
  async enter() { await this.top(); await this.load(); },
};

// ===== アクションの統合(作業用コピー → マスター) =====
Views.actmerge = {
  items: [], analysis: null, target: 'master', master: '',
  async add(paths) {
    paths = paths.map(p => p.trim().replace(/^"|"$/g, '')).filter(p => p && !this.items.some(x => x.path.toLowerCase() === p.toLowerCase()));
    if (!paths.length) return;
    const info = await api('/api/dbinfo', { paths });
    info.forEach(i => {
      if (i.error) toast(`${i.path}: ${i.error}`, true);
      else if (!i.code) toast(`${i.path.split(/[\\/]/).pop()} は作業用コピーではありません(作業者コードなし)`, true);
      else { this.items.push(i); if (!this.master && i.masterPath) this.master = i.masterPath; }
    });
    this.analysis = null;
    this.render();
  },
  isOpenCopy() { const p = S.state?.db?.path?.toLowerCase(); return !!S.code && this.items.some(x => x.path.toLowerCase() === p); },
  render() {
    const a = this.analysis;
    const el = $('#view-actmerge');
    el.innerHTML = `<div class="card"><h2>アクションの統合(作業用コピー → マスター)</h2>
      <p class="hint">作業用コピー(作業者コード付き)の変更を、マスターにまとめます。<b>1人分だけでも統合できます。</b><br>
      ・同じ項目を複数人(またはマスター側)が<b>異なる内容</b>に変えていた場合は「競合」として並べて表示するので、採用する方を選んでください。<br>
      ・タグは全員の追加・削除をすべて反映します。</p>
      <div style="display:flex;gap:6px;margin-bottom:8px"><input type="text" id="am-path" placeholder="作業用コピーのパス" style="flex:1"><button id="am-add">追加</button><button id="am-ref">参照…</button><button id="am-recent">最近開いたファイルから…</button></div>
      <table class="t">${this.items.length ? `<tr><th>作業者コード</th><th>ファイル</th><th>元のマスター</th><th></th></tr>` + this.items.map((x, i) => `<tr><td><b>${esc(x.code)}</b></td><td title="${esc(x.path)}">📄 ${esc(x.path.split(/[\\/]/).pop())}</td><td class="wrap muted">${esc(x.masterPath || '(不明)')}</td><td><button class="mini danger" data-rm="${i}">×</button></td></tr>`).join('')
        : '<tr><td class="muted">作業用コピーを追加してください(1つでも可)。</td></tr>'}</table>
      <h3 style="margin-top:12px">統合先</h3>
      <label class="inl"><input type="radio" name="am-target" value="master" ${this.target === 'master' ? 'checked' : ''}> <b>元のマスターへ統合する</b>(推奨。マスターを更新し、元のマスターは「_backup_日時」として残します)</label>
      <div class="form" style="grid-template-columns:110px 1fr auto;margin:2px 0 6px 24px"><span>マスター</span><input type="text" id="am-master" value="${esc(this.master)}" placeholder="共有フォルダにあるマスターDB"><button id="am-mref">参照…</button></div>
      <label class="inl"><input type="radio" name="am-target" value="new" ${this.target === 'new' ? 'checked' : ''}> 新しいファイルとして保存する</label>
      <div class="form" style="grid-template-columns:110px 1fr auto;margin:2px 0 6px 24px"><span>保存先</span><input type="text" id="am-out" placeholder="空欄なら master_日時.db(DB保存フォルダ)"><button id="am-outref">参照…</button></div>
      ${this.isOpenCopy() ? `<label class="inl"><input type="checkbox" id="am-refresh" checked> 統合後、今開いている作業用コピー(${esc(S.code)})を新しいマスターから作り直して、そのまま作業を続ける</label>` : ''}
      <div style="margin-top:8px"><button class="primary" id="am-analyze" ${this.items.length < 1 ? 'disabled' : ''}>比較する</button></div></div>
      ${a ? `<div class="card"><h2>比較結果</h2>
        ${a.masterPath ? `<p class="hint">統合先のマスター: ${esc(a.masterPath)}</p>` : ''}
        <table class="t"><tr><th>作業者</th><th class="num">変更数</th></tr>${a.sources.map(s => `<tr><td>${esc(s.code)}</td><td class="num">${fmtNum(s.changes)}</td></tr>`).join('')}</table>
        <p>自動で統合できる変更: <b>${fmtNum(a.auto)}</b> 件 ・ 競合: <b>${fmtNum(a.conflicts.length)}</b> 件</p>
        ${(a.warnings || []).map(w => `<div class="issue">${esc(w)}</div>`).join('')}
        ${a.conflicts.length ? `<h3>競合の解決</h3><p class="hint">一括選択: ${a.sources.map(s => `<button class="mini" data-pref="${esc(s.code)}">「${esc(s.code)}」を優先</button>`).join(' ')}</p>
          ${a.conflicts.map((c, ci) => `<div class="conf"><div class="ti">${c.kind === 'vnode' ? '🗂 ' : '📄 '}${esc(c.title)}</div><div class="muted" style="font-size:12px">元の状態: ${esc(c.base)}</div>
            ${c.options.map((o, oi) => `<label><input type="radio" name="cf${ci}" value="${oi}" ${oi === 0 ? 'checked' : ''}> ${esc(o.label)} <span class="tag">${o.editors.map(esc).join(', ')}</span></label>`).join('')}</div>`).join('')}` : ''}
        <div style="margin-top:8px"><button class="primary" id="am-apply">${a.masterPath ? 'マスターへ統合する' : '統合して新しいマスターを作成'}</button></div></div>` : ''}`;
    $('#am-add').onclick = guard(async () => { await this.add([$('#am-path').value]); });
    $('#am-ref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'dbmulti' }); await this.add(r.paths || []); });
    $('#am-recent').onclick = guard(() => pickRecent(p => this.add(p)));
    $$('[data-rm]', el).forEach(b => b.onclick = () => { this.items.splice(+b.dataset.rm, 1); this.analysis = null; this.render(); });
    $$('input[name="am-target"]', el).forEach(r => r.onchange = () => { this.target = r.value; this.analysis = null; this.render(); });
    $('#am-master').onchange = () => { this.master = $('#am-master').value.trim(); this.analysis = null; };
    $('#am-mref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'db', initial: this.master }); if (r.path) { this.master = r.path; this.analysis = null; this.render(); } });
    $('#am-outref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'savedb', initial: `${S.state.projectsDir}\\master.db` }); if (r.path) $('#am-out').value = r.path; });
    $('#am-analyze').onclick = guard(async () => {
      const out = $('#am-out').value;
      if (this.target === 'master' && !this.master.trim()) return toast('統合先のマスターを指定してください', true);
      this.analysis = await api('/api/actmerge/analyze', { dbs: this.items.map(x => x.path), master: this.target === 'master' ? this.master : '' });
      this.render(); $('#am-out').value = out;
    });
    if (!a) return;
    $$('[data-pref]', el).forEach(b => b.onclick = () => a.conflicts.forEach((c, ci) => {
      const oi = c.options.findIndex(o => o.editors.includes(b.dataset.pref));
      if (oi >= 0) $(`input[name="cf${ci}"][value="${oi}"]`).checked = true;
    }));
    $('#am-apply').onclick = guard(async () => {
      const choices = {};
      a.conflicts.forEach((c, ci) => { choices[c.key] = +$(`input[name="cf${ci}"]:checked`).value; });
      await api('/api/actmerge/apply', { out: $('#am-out').value, choices, refresh: !!$('#am-refresh')?.checked });
      this.items = []; this.analysis = null;
      show('import'); Job.watch();
    });
  },
  async enter() {
    // 今開いている作業用コピーを自動で追加(1人で使う場合の手間を減らす)
    if (S.code && S.state?.db && !this.items.length) await this.add([S.state.db.path]).catch(() => { });
    this.render();
  },
};

// ===== 起動〜終了の管理 =====
// コンソールを出さないため、画面が開いている間はハートビートを送り、
// タブを閉じたら通知する(サーバーは一定時間ハートビートが無いと自動終了する)。
const Life = {
  timer: 0, fails: 0,
  beat: async () => {
    try {
      const r = await fetch('/api/heartbeat', { method: 'POST', headers: { 'X-Token': TOKEN } });
      if (!r.ok) throw new Error();
      Life.fails = 0;
    } catch (e) {
      if (++Life.fails >= 2) Life.ended('ツールは終了しています。続けるには FolderManager.exe をもう一度起動してください。');
    }
  },
  start() {
    this.beat();
    this.timer = setInterval(() => this.beat(), 20000);
    addEventListener('pagehide', () => navigator.sendBeacon('/api/bye?token=' + TOKEN));
    addEventListener('pageshow', e => { if (e.persisted) this.beat(); });
  },
  stop() { clearInterval(this.timer); },
  quit() {
    modal('<h2>ツールを終了しますか？</h2><p class="hint">設定したアクションはDBに保存済みです。</p>', async () => {
      this.stop(); await api('/api/shutdown', {});
      this.ended('終了しました。このタブは閉じて構いません。');
    }, '終了');
  },
  ended(msg) {
    this.stop();
    if ($('#ended')) return;
    const d = document.createElement('div');
    d.id = 'ended';
    d.innerHTML = `<div class="card"><h2>FolderManager</h2><p>${esc(msg)}</p></div>`;
    document.body.appendChild(d);
  },
};

// ===== 起動 =====
function initImport() {
  $$('[data-dialog]').forEach(b => b.onclick = guard(async () => {
    const t = $('#' + b.dataset.target);
    const r = await api('/api/dialog', { kind: b.dataset.dialog, initial: t.value });
    if (r.path) t.value = r.path;
  }));
  $('#scan-go').onclick = guard(async () => {
    await api('/api/scan', { root: $('#scan-root').value, db: $('#scan-db').value, workers: +$('#scan-workers').value, alias: $('#scan-alias').value });
    Job.watch();
  });
  $('#job-cancel').onclick = guard(() => api('/api/job/cancel', {}));
  $('#quit').onclick = () => Life.quit();
  $('#optbtn').onclick = () => Options.open();
}

(async () => {
  Theme.init();
  initImport();
  TreeMerge.init();
  FileMenu.init();
  Life.start();
  $$('#nav button').forEach(b => b.onclick = () => show(b.dataset.view));
  await guard(refreshState)();
  if (S.state?.db) { dbOpened(); show('organize'); } else show('import');
  if (S.state?.job && !S.state.job.finished) Job.watch();
})();
