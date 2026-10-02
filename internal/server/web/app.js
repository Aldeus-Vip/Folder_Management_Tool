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
  tags: [],
  sel: [], selGrid: null, view: 'import',
  vTarget: 'root', vTargetPath: '',
};

// フラグ(Go側 fsdb/flags.go と同じ値)
const FL = { badchar: 1, trailing: 2, reserved: 4, access: 8, empty: 16, temp: 32, copy: 64, version: 128, single: 256, dup: 512 };
const MIG = FL.badchar | FL.trailing | FL.reserved | FL.access;
const ACT = { delete: '削除', move: '移動' };

function warnings(r) {
  const st = S.settings || { oldYears: 3, pathLimit: 250, deepDepth: 8, manyFiles: 500 };
  const w = [], f = r.f;
  const add = (c, l, t) => w.push({ c, l, t });
  if (f & FL.badchar) add('mig', '禁止文字', 'SharePointで使えない文字(\\ / : * ? " < > | # %)を含む');
  if (f & FL.trailing) add('mig', '末尾.空白', '名前の末尾が「.」または半角スペース');
  if (f & FL.reserved) add('mig', '予約語', 'CON, PRN, AUX, NUL, COM1-9, LPT1-9');
  if (f & FL.access) add('mig', 'アクセス不可', 'スキャン時にアクセスできなかった');
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
  return w;
}
const warnHTML = r => warnings(r).map(x => `<span class="b ${x.c}" title="${esc(x.t)}">${esc(x.l)}</span>`).join('');
const isDeep = r => r.d >= (S.settings?.deepDepth ?? 8);
const eff = r => r.act || r.ia || '';
const rowClass = r => {
  const e = eff(r);
  const base = (r.f & MIG) || (r.pl > (S.settings?.pathLimit ?? 250)) ? 'mig' : isDeep(r) ? 'deep' : (r.dir ? 'dir' : '');
  return base + (e === 'delete' ? ' del' : e === 'move' ? ' mov' : '');
};
const vroot = () => S.rules?.rootName || '整理後';
const vpathLabel = p => p ? vroot() + '\\' + p : vroot();

function actHTML(r) {
  if (r.act === 'delete') return `<span class="actb delete">削除</span>${memoMark(r)}`;
  if (r.act === 'move') return `<span class="actb move">移動</span> <span title="${esc(vpathLabel(r.vpath))}">→ ${esc(vpathLabel(r.vpath))}</span>${r.nn ? ` <span class="muted">名前: ${esc(r.nn)}</span>` : ''}${memoMark(r)}`;
  if (r.ia) return `<span class="actb inh" title="親フォルダに設定されたアクションが及んでいます">親で${ACT[r.ia]}</span>${r.ivpath ? ` <span class="muted">→ ${esc(vpathLabel(r.ivpath))}</span>` : ''}${memoMark(r)}`;
  return memoMark(r);
}
const memoMark = r => r.memo ? ` <span title="${esc(r.memo)}">📝</span>` : '';
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
    if (this.o.draggable && this.sel.size) this.armDrag(e, plain && already ? r : null);
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
  q() { return (this.o.dirs ? '&dirs=1' : '') + (this.o.hide ? '&hide=1' : ''); }
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
    for (const u of chain.slice(0, -1)) { const i = this.indexOf('v:' + u); if (i >= 0) await this.expand(i); }
    const root = this.indexOf('v:root');
    if (root >= 0 && chain.length) await this.expand(root);
    return this.indexOf('v:' + uuid);
  }
}
function vrow(v) {
  const r = Object.assign({}, v);
  if (r.node) { // 実フォルダ/ファイルはノードの情報も行に持たせる(パネル・警告表示用)
    for (const k of ['id', 'dir', 'x', 'm', 'f', 'pl', 'act', 'ia', 'nn', 'due', 'memo', 'tags', 'path', 'ivpath', 'ed', 'p', 'e', 'cc', 'dc', 'rem']) r[k] = r.node[k];
    r.nvpath = r.node.vpath;
    r.realD = r.node.d;
  }
  return r;
}

function nameCell(r, tree, label) {
  const icon = r.kind === 'vdir' ? '🗂' : r.dir ? (r._open ? '📂' : '📁') : fileIcon(r.x);
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
  size: { key: 'size', label: 'サイズ', w: 80, cls: 'num', render: r => fmtSize(r.s), sort: true, descFirst: true },
  warn: { key: 'warn', label: '警告・整理のヒント', w: 200, render: warnHTML },
  action: { key: 'action', label: 'アクション(移動先)', w: 250, render: actHTML, sort: true },
  tags: { key: 'tags', label: 'タグ', w: 140, render: r => tagsHTML(r.tags) },
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
  Panel.render();
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
      let cur = all.some(v => v.uuid === S.vTarget) ? S.vTarget : 'root';
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

// ===== 右パネル(詳細・編集) =====
const Panel = {
  el: () => $('#panel'),
  show(on) { this.el().hidden = !on; },
  set(rows, grid) { S.sel = rows; S.selGrid = grid; this.render(); },
  render() {
    const el = this.el(), rows = S.sel;
    if (!rows.length) {
      el.innerHTML = `<h3>詳細・編集</h3><p class="hint">行をクリックすると詳細が表示され、整理アクションを設定できます。<br><br>
        <b>🗑 削除</b>: 整理後には残さない(元のツリーで取り消し線)<br>
        <b>📦 移動</b>: 整理後のフォルダ構成(右側)のどこへ置くかを決める。名前変更・期限も設定可<br><br>
        ・フォルダに設定すると配下すべてに及びます(配下に個別の設定があればそちらが優先)<br>
        ・Ctrl/Shift+クリックで複数選択 → まとめて設定<br>
        ・左の項目を右の仮想フォルダへドラッグしても移動できます<br>
        ・キー: Del=削除 / M=移動 / Backspace=解除<br>
        ・ファイルをダブルクリック(またはEnter)で、既定のアプリで開いて中身を確認できます</p>`;
      return;
    }
    if (rows.length === 1 && rows[0].kind === 'vdir') return this.renderV(rows[0]);
    const nodes = rows.filter(r => r.kind !== 'vdir');
    if (!nodes.length) { el.innerHTML = `<h3>${rows.length}件の仮想フォルダを選択中</h3><p class="hint">ドラッグで別の仮想フォルダの下へ移せます。</p>`; return; }
    const one = nodes.length === 1 ? nodes[0] : null;
    let h = '';
    if (one) {
      const r = one, vp = r.kind ? r.nvpath : r.vpath;
      const st = r.act ? (r.act === 'delete' ? '<span class="actb delete">削除</span>' : `<span class="actb move">移動</span> → ${esc(vpathLabel(vp))}`)
        : r.ia ? `<span class="actb inh">親フォルダで${ACT[r.ia]}</span>${r.ivpath ? ' → ' + esc(vpathLabel(r.ivpath)) : ''}` : '<span class="muted">未設定(未処理)</span>';
      h += `<h3>${r.dir ? '📁 フォルダ' : '📄 ファイル'}</h3><div style="font-weight:600;word-break:break-all">${esc(r.node?.n ?? r.n)}</div>
        <div class="path">${esc(r.path)}</div>
        <dl><dt>サイズ</dt><dd>${fmtSize(r.s)}</dd>
        <dt>更新日時</dt><dd>${fmtDate(r.m) || '-'}</dd>
        ${r.dir ? `<dt>配下</dt><dd>ファイル ${fmtNum(r.node?.fc ?? r.fc)} / フォルダ ${fmtNum(r.dc)}</dd><dt>未処理</dt><dd>${fmtNum(r.rem)} ファイル</dd>` : ''}
        <dt>階層 / パス長</dt><dd>${r.realD ?? r.d} / ${r.pl}文字</dd>
        <dt>警告・ヒント</dt><dd>${warnHTML(r.node || r) || '<span class="muted">なし</span>'}</dd>
        <dt>現在の設定</dt><dd>${st}${r.ed ? ` <span class="muted">(${esc(r.ed)})</span>` : ''}</dd></dl>
        <div class="btns">
          ${r.dir ? '' : '<button data-go="open" title="既定のアプリで開く(ダブルクリックでも開けます)">📂 ファイルを開く</button>'}
          <button data-go="tree">現在のツリーで表示</button>
          ${r.dir ? '<button data-go="under">この配下を検索</button><button data-go="dups">配下の重複</button>' : ''}
          <button data-go="copy">パスをコピー</button>
          <button data-go="reveal" title="このPCからアクセスできる場合、Windowsのエクスプローラーで開きます">Windowsで開く</button>
        </div>`;
    } else {
      const tot = nodes.reduce((a, r) => a + r.s, 0);
      h += `<h3>${nodes.length}件を選択中</h3><p class="hint">合計 ${fmtSize(tot)}。下の操作はすべての項目にまとめて適用されます(名前変更は1件ずつ)。</p>`;
    }
    const tagCount = new Map();
    nodes.forEach(r => (r.tags || []).forEach(t => tagCount.set(t, (tagCount.get(t) || 0) + 1)));
    h += `<h3 style="margin-top:6px">整理アクション</h3>
      <div class="actbtns">
        <button data-a="delete" class="${one?.act === 'delete' ? 'on' : ''}" title="キー Del">🗑 削除</button>
        <button data-a="move" class="${one?.act === 'move' ? 'on' : ''}" title="移動先を整理後のツリーから選ぶ(キー M)">📦 移動…</button>
        <button data-a="clear" title="キー Backspace">↺ 解除</button>
      </div>
      ${one?.act === 'move' ? `<label>移動後の名前(名前変更・空欄で元の名前)</label><div style="display:flex;gap:4px"><input id="pe-nn" value="${esc(one.nn || '')}" placeholder="${esc(one.node?.n ?? one.n)}"><button id="pe-nn-save">保存</button></div>` : ''}
      <label>期限(保存期限・見直し日など)</label><div style="display:flex;gap:4px"><input type="date" id="pe-due" value="${esc(one?.due || '')}" style="flex:1"><button id="pe-due-save">保存</button></div>
      <label>タグ(目的・種類)</label>
      <div>${[...tagCount].map(([t, c]) => `<span class="tag">${esc(t)}${nodes.length > 1 ? ` ${c}/${nodes.length}` : ''}<a data-rmtag="${esc(t)}" title="外す">×</a></span>`).join('') || '<span class="muted">なし</span>'}</div>
      <div style="display:flex;gap:4px;margin-top:4px"><input id="pe-tag" list="taglist" placeholder="タグを追加(Enter)"><button id="pe-tag-add">追加</button></div>
      <label>メモ</label><textarea id="pe-memo" placeholder="${one ? '判断理由・確認事項など' : '(入力すると全件に設定)'}">${esc(one?.memo || '')}</textarea>
      <div style="margin-top:6px"><button class="primary" id="pe-memo-save">メモを保存 <span class="kbd">Ctrl+Enter</span></button></div>`;
    el.innerHTML = h;
    $$('[data-a]', el).forEach(b => b.onclick = guard(() => this.action(b.dataset.a)));
    $$('[data-go]', el).forEach(b => b.onclick = guard(() => this.go(b.dataset.go, one)));
    $$('[data-rmtag]', el).forEach(a => a.onclick = guard(() => Plan.tags(nodes, [], [a.dataset.rmtag])));
    const addTag = guard(async () => { const t = $('#pe-tag', el).value.trim(); if (t) await Plan.tags(nodes, [t], []); });
    $('#pe-tag-add', el).onclick = addTag;
    $('#pe-tag', el).onkeydown = e => { if (e.key === 'Enter') addTag(); };
    $('#pe-due-save', el).onclick = guard(() => Plan.fields(nodes, { due: $('#pe-due', el).value }));
    const saveMemo = guard(() => Plan.fields(nodes, { memo: $('#pe-memo', el).value }));
    $('#pe-memo-save', el).onclick = saveMemo;
    $('#pe-memo', el).onkeydown = e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) saveMemo(); };
    if ($('#pe-nn', el)) {
      const saveNN = guard(() => Plan.fields([one], { newName: $('#pe-nn', el).value }));
      $('#pe-nn-save', el).onclick = saveNN;
      $('#pe-nn', el).onkeydown = e => { if (e.key === 'Enter') saveNN(); };
    }
  },
  renderV(v) {
    const el = this.el();
    const isRoot = v.uuid === 'root';
    el.innerHTML = `<h3>🗂 整理後のフォルダ(仮想)</h3><div style="font-weight:600">${esc(isRoot ? vroot() : v.n)}</div>
      <div class="path">${esc(vpathLabel(v.vpath))}</div>
      ${S.rules?.basePath ? `<p class="hint">実際の場所: ${esc(S.rules.basePath + (v.vpath ? '\\' + v.vpath : ''))}</p>` : ''}
      <dl><dt>配下のファイル</dt><dd>${fmtNum(v.fc)} (${fmtSize(v.s)})</dd><dt>直下の項目</dt><dd>${fmtNum(v.cc)}</dd><dt>階層</dt><dd>${v.d}</dd>
      ${v.ed ? `<dt>作成・変更</dt><dd>${esc(v.ed)}</dd>` : ''}</dl>
      ${v.sim?.length ? `<div class="issue"><b>⚠ 同じ階層に似た名前のフォルダがあります</b><ul>${v.sim.map(s => `<li>${esc(s)}</li>`).join('')}</ul>目的が同じなら1つにまとめることを検討してください。</div>` : ''}
      <div class="btns">
        <button data-v="new">＋ この下にフォルダ作成</button>
        <button data-v="rename">名前変更</button>
        ${isRoot ? '' : '<button data-v="del" class="danger">削除</button>'}
        <button data-v="list">ここへ移動した項目を一覧</button>
      </div>
      ${isRoot ? '' : `<label>メモ(このフォルダの用途・保存ルールなど)</label><textarea id="pv-memo">${esc(v.memo || '')}</textarea>
      <div style="margin-top:6px"><button class="primary" id="pv-memo-save">メモを保存</button></div>`}`;
    $$('[data-v]', el).forEach(b => b.onclick = guard(() => VOps[b.dataset.v](v)));
    if (!isRoot) $('#pv-memo-save', el).onclick = guard(async () => { await api('/api/vmemo', { uuid: v.uuid, memo: $('#pv-memo', el).value }); toast('保存しました'); await afterEdit(); });
  },
  async action(a) {
    const rows = S.sel.filter(r => r.kind !== 'vdir');
    if (!rows.length) return;
    if (a === 'delete') return Plan.delete(rows);
    if (a === 'clear') return Plan.clear(rows);
    if (a === 'move') return Plan.move(rows);
  },
  async go(where, r) {
    if (where === 'open') return openFile(r);
    if (where === 'tree') return show('organize', { reveal: r.id });
    if (where === 'under') return show('list', { under: r.id, underPath: r.path });
    if (where === 'dups') return show('dups', { under: r.id, underPath: r.path });
    if (where === 'copy') { await navigator.clipboard.writeText(r.path); return toast('パスをコピーしました'); }
    if (where === 'reveal') { await api('/api/reveal', { id: r.id }); }
  },
};
function updateTagList() { $('#taglist').innerHTML = S.tags.map(t => `<option value="${esc(t)}">`).join(''); }
async function loadTags() { try { S.tags = (await api('/api/tags')).map(t => t.key); updateTagList(); } catch (e) { } }

// 仮想フォルダの操作
const VOps = {
  async new(v) {
    const name = prompt(`「${v.uuid === 'root' ? vroot() : v.n}」の下に作るフォルダ名`);
    if (!name?.trim()) return;
    const r = await api('/api/vcreate', { parent: v.uuid, name: name.trim() });
    if (showIssue(r.issue, 'フォルダを作成')) return;
    toast('フォルダを作成しました');
    await afterEdit();
    if (S.view === 'organize') await Views.organize.focusV(r.uuid);
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
    modal(`<h2>仮想フォルダ「${esc(v.n)}」を削除しますか？</h2><p class="hint">配下の仮想フォルダも削除されます。ここへ「移動」を設定していた項目(${fmtNum(v.fc)}ファイル)は、移動の設定が解除され「未処理」に戻ります。</p>`,
      async () => { const r = await api('/api/vdelete', { uuid: v.uuid }); toast(`削除しました(${r.cleared}件の移動を解除)`); if (S.vTarget === v.uuid) setVTarget(null); await afterEdit(); }, '削除');
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
  Panel.show(['organize', 'list', 'dups', 'plan'].includes(name));
  Panel.set([], null);
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
  S.vTarget = 'root';
  loadTags();
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

// ===== オプション =====
const Options = {
  open() {
    const db = S.state?.db, m = db?.meta || {}, st = S.settings || {}, ru = S.rules || {};
    const t = store.get('fm-theme', 'auto');
    const mode = (id, v) => `<select id="${id}">${[['block', '禁止'], ['warn', '警告のみ'], ['off', '無効']].map(([k, l]) => `<option value="${k}" ${v === k ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
    const md = modal(`<h2>⚙ オプション</h2>
      <div class="optsec"><h3>表示</h3>テーマ: <span class="seg" id="op-theme">${[['auto', '🌓 自動'], ['light', '☀ ライト'], ['dark', '🌙 ダーク']].map(([k, l]) => `<button data-t="${k}" class="${t === k ? 'on' : ''}">${l}</button>`).join('')}</span>
        <span class="hint">「自動」はWindowsの設定に従います</span></div>
      ${db ? `<div class="optsec"><h3>開いているDB</h3><table class="t">
        <tr><td>ファイル</td><td class="wrap">${esc(db.path)}</td></tr>
        <tr><td>作業者コード</td><td>${S.code ? esc(S.code) : '<span class="muted">なし(マスター)</span>'}</td></tr>
        <tr><td>対象ルート</td><td class="wrap">${esc(m.root)}</td></tr>
        <tr><td>取得元</td><td>${esc(srcLabel(m))}${m.merged_from ? ' / アクション統合: ' + esc(m.merged_from) : ''}</td></tr>
        <tr><td>データ取得</td><td>${esc(m.scanned_at || m.merged_at || m.built_at || '')}</td></tr>
        <tr><td>項目数</td><td>${fmtNum(+m.node_count)}</td></tr>
        <tr><td>マスターID / 版</td><td class="muted">${esc((m.master_id || '-').slice(0, 8))} / ${esc((m.master_rev || '-').slice(0, 8))}</td></tr></table></div>
      <div class="optsec"><h3>判定の閾値(現在のフォルダ構成の警告)</h3><div class="form" style="grid-template-columns:240px 110px">
        <span>古いファイル(更新から○年以上)</span><input type="number" id="st-old" value="${st.oldYears}" min="1">
        <span>パス文字数の警告(○文字超)</span><input type="number" id="st-path" value="${st.pathLimit}" min="1">
        <span>深い階層(○階層以上で警告)</span><input type="number" id="st-deep" value="${st.deepDepth}" min="1">
        <span>項目過多(直下○項目超)</span><input type="number" id="st-many" value="${st.manyFiles}" min="1"></div></div>
      <div class="optsec"><h3>整理後の構成ルール(5S)</h3>
        <p class="hint">整理後のフォルダ構成(仮想)へ移動・フォルダ作成するときのルールです。「禁止」にすると、ルールに合わない操作はできません。</p>
        <div class="form" style="grid-template-columns:240px 240px 110px">
        <span>整理後のルートの表示名</span><input type="text" id="ru-root" value="${esc(ru.rootName)}"><span></span>
        <span>整理後のルートの実際の場所</span><input type="text" id="ru-base" value="${esc(ru.basePath)}" placeholder="例: \\\\srv\\share\\整理後"><span class="hint">パス長・実行スクリプト用</span>
        <span>最大階層(ルート=0)</span><input type="number" id="ru-depth" value="${ru.maxDepth}" min="1">${mode('ru-depthm', ru.depthMode)}
        <span>整理後のフルパス文字数の上限</span><input type="number" id="ru-path" value="${ru.pathLimit}" min="10">${mode('ru-pathm', ru.pathMode)}
        <span>1フォルダ直下の項目数の上限</span><input type="number" id="ru-items" value="${ru.maxItems}" min="1">${mode('ru-itemsm', ru.itemsMode)}
        <span>禁止文字・末尾の.や空白・予約語</span><span class="hint">名前変更で直せます</span>${mode('ru-bad', ru.badName)}
        <span>コピー・版管理的な名前(「- コピー」「旧」「v2」等)</span><span></span>${mode('ru-copy', ru.copyName)}
        <span>タグが無い項目の移動</span><span class="hint">目的・種類の明確化</span>${mode('ru-tag', ru.tagRequired)}
        </div></div>` : '<p class="hint">DBを開くと、DBの情報と判定ルールを設定できます。</p>'}`,
      db ? async mm => {
        S.settings = await api('/api/settings', { oldYears: +$('#st-old', mm).value, pathLimit: +$('#st-path', mm).value, deepDepth: +$('#st-deep', mm).value, manyFiles: +$('#st-many', mm).value });
        S.rules = await api('/api/rules', {
          rootName: $('#ru-root', mm).value, basePath: $('#ru-base', mm).value, maxDepth: +$('#ru-depth', mm).value, depthMode: $('#ru-depthm', mm).value,
          pathLimit: +$('#ru-path', mm).value, pathMode: $('#ru-pathm', mm).value, maxItems: +$('#ru-items', mm).value, itemsMode: $('#ru-itemsm', mm).value,
          badName: $('#ru-bad', mm).value, copyName: $('#ru-copy', mm).value, tagRequired: $('#ru-tag', mm).value,
        });
        await refreshState();
        toast('保存しました');
        S.planv++; show(S.view);
      } : null, '保存', '閉じる');
    $$('#op-theme [data-t]', md).forEach(b => b.onclick = () => {
      store.set('fm-theme', b.dataset.t); Theme.apply();
      $$('#op-theme [data-t]', md).forEach(x => x.classList.toggle('on', x === b));
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
      toast(j.kind === 'actmerge' ? '統合した新しいマスターDBを開きました' : `完了しました(${fmtNum(j.result?.items)}項目)`);
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
  const done = p.files - p.remFiles;
  return `<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
    <div class="tr" style="width:240px;height:12px;background:var(--track);border-radius:6px;overflow:hidden;display:flex" title="移動 ${fmtNum(p.moveFiles)} / 削除 ${fmtNum(p.delFiles)} / 未処理 ${fmtNum(p.remFiles)}">
      <i class="seg-move" style="display:block;width:${pct(p.moveFiles, p.files)}%"></i><i class="seg-del" style="display:block;width:${pct(p.delFiles, p.files)}%"></i></div>
    <span>処理済み <b>${pct(done, p.files).toFixed(1)}%</b> (${fmtNum(done)} / ${fmtNum(p.files)} ファイル)</span>
    <span><span class="legend-dot seg-move"></span>移動 ${fmtNum(p.moveFiles)} (${fmtSize(p.moveSize)})</span>
    <span><span class="legend-dot seg-del"></span>削除 ${fmtNum(p.delFiles)} (${fmtSize(p.delSize)})</span>
    <span><span class="legend-dot" style="background:var(--track);border:1px solid var(--line)"></span>未処理 ${fmtNum(p.remFiles)} (${fmtSize(p.remSize)})</span></div>`;
}
Views.summary = {
  async enter() {
    const el = $('#view-summary');
    if (!this.html) el.innerHTML = '<p class="muted">集計中…(初回は大規模データで数秒かかります)</p>';
    const sm = await api('/api/summary');
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
          <div class="card"><h2>作業者別・タグ別</h2>
            ${sm.editors.length ? `<table class="t"><tr><th>作業者</th><th class="num">設定数</th></tr>${sm.editors.map(a => `<tr><td>${esc(a.key)}</td><td class="num">${fmtNum(a.count)}</td></tr>`).join('')}</table>` : '<p class="muted">まだアクションはありません。</p>'}
            ${sm.tags.length ? `<p>${sm.tags.slice(0, 30).map(t => `<a data-tag="${esc(t.key)}"><span class="tag">${esc(t.key)} ${fmtNum(t.count)}</span></a>`).join('')}</p>` : ''}
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
  },
};

// ===== 整理(現在のツリー ⇔ 整理後のフォルダ構成) =====
function setVTarget(v) {
  S.vTarget = v ? v.uuid : 'root';
  S.vTargetPath = v ? vpathLabel(v.vpath) : vroot();
  const el = $('#vt-target');
  if (el) el.innerHTML = `移動先: <b>🗂 ${esc(S.vTargetPath)}</b> <span class="muted">(右のツリーでフォルダを選ぶと変わります)</span>`;
}
Views.organize = {
  reset() {
    if (!this.src) this.build();
    this.model = new TreeModel({ dirs: $('#tr-dirs').checked, hide: $('#tr-hide').checked });
    this.vmodel = new VTreeModel();
    this.loaded = false;
  },
  build() {
    const keyAct = (k) => {
      if (k === 'Delete') { guard(() => Plan.delete(S.sel))(); return true; }
      if (k === 'm' || k === 'M') { guard(() => this.moveSel())(); return true; }
      if (k === 'Backspace') { guard(() => Plan.clear(S.sel))(); return true; }
      return false;
    };
    this.src = new Grid($('#tr-grid'), {
      columns: this.columns(), rowClass, draggable: true,
      onSelect: rows => Panel.set(rows, this.src),
      onToggle: guard((r, i) => this.toggle(this.model, this.src, i)),
      onOpen: guard((r, i) => r.dir ? this.toggle(this.model, this.src, i) : openFile(r)),
      onArrow: guard(async (r, i, right) => {
        if (right && r._has && !r._open) await this.toggle(this.model, this.src, i);
        else if (!right && r._open) await this.toggle(this.model, this.src, i);
        else if (!right && r.d > 0) { const pi = this.model.indexOf(r.p); if (pi >= 0) this.src.moveTo(pi); }
      }),
      onKeyAction: keyAct,
    });
    this.vt = new Grid($('#vt-grid'), {
      keyOf: r => r.key, draggable: true,
      columns: [
        { key: 'name', label: '名前(整理後)', w: 300, render: r => nameCell(r, true, r.uuid === 'root' ? vroot() : r.n) + (r.nn && r.kind !== 'vdir' ? ` <span class="muted" title="元の名前">(元: ${esc(r.node.n)})</span>` : '') },
        { key: 'fc', label: 'ファイル', w: 70, cls: 'num', render: r => r.kind === 'file' ? '' : fmtNum(r.fc) },
        { key: 's', label: 'サイズ', w: 80, cls: 'num', render: r => fmtSize(r.s) },
        { key: 'tags', label: 'タグ', w: 120, render: r => tagsHTML(r.tags) },
        { key: 'due', label: '期限', w: 96, render: r => dueHTML(r.due) },
        { key: 'warn', label: '警告', w: 200, render: r => (r.sim?.length ? `<span class="b org" title="同じ階層に似た名前: ${esc(r.sim.join(', '))}">⚠ 似た名前 ${r.sim.length}</span>` : '') + (r.kind !== 'vdir' && r.node ? warnHTML(r.node) : '') },
      ],
      rowClass: r => (r.kind === 'vdir' ? 'vdir' : r.kind === 'dir' ? 'dir' : '') + (r.sim?.length ? ' sim' : '') + (r.kind === 'vdir' && r.uuid === S.vTarget ? ' vtarget' : ''),
      onSelect: rows => {
        Panel.set(rows, this.vt);
        if (rows.length === 1 && rows[0].kind === 'vdir') { setVTarget(rows[0]); this.vt.render(); }
      },
      onToggle: guard((r, i) => this.toggle(this.vmodel, this.vt, i)),
      onOpen: guard((r, i) => r.kind === 'file' ? openFile(r) : r._has ? this.toggle(this.vmodel, this.vt, i) : null),
      onArrow: guard(async (r, i, right) => { if (r._has && right !== !!r._open) await this.toggle(this.vmodel, this.vt, i); }),
      onKeyAction: (k) => {
        const v = S.sel[0];
        if (k === 'F2' && v?.kind === 'vdir') { guard(() => VOps.rename(v))(); return true; }
        if (k === 'Delete' && S.sel.length === 1 && v?.kind === 'vdir') { if (v.uuid !== 'root') guard(() => VOps.del(v))(); return true; }
        if (S.sel.every(r => r.kind === 'vdir')) return false;
        return keyAct(k);
      },
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
    $('#tr-dirs').onchange = $('#tr-hide').onchange = guard(async () => {
      this.model.o = { dirs: $('#tr-dirs').checked, hide: $('#tr-hide').checked };
      await this.model.reloadKeep(); this.src.setSource(this.model, true);
    });
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
    $('#vt-new').onclick = guard(() => VOps.new(this.targetRow()));
    $('#vt-rename').onclick = guard(() => VOps.rename(this.targetRow()));
    $('#vt-del').onclick = guard(() => { const v = this.targetRow(); if (v.uuid === 'root') return toast('ルートは削除できません', true); return VOps.del(v); });
    $('#vt-moveto').onclick = guard(() => this.moveSel(true));
    $('#vt-warn').onclick = guard(() => this.showWarnings());
  },
  targetRow() { return this.vmodel.rows.find(r => r.uuid === S.vTarget) || this.vmodel.rows[0]; },
  gaugeMode() { return store.get('fm-gauge2', 'remain'); },
  columns() {
    const c = cols({ key: 'name', label: '名前(現在)', w: 400, render: r => nameCell(r, true) }, 'depth', 'size');
    const mode = this.gaugeMode();
    if (mode !== 'off') c.push({
      key: 'gauge', w: 140, label: mode === 'remain' ? '未処理' : mode === 'root' ? 'サイズ比(全体)' : 'サイズ比(親内)',
      tip: mode === 'remain' ? 'フォルダ内でアクションが未設定のファイルの割合' : mode === 'root' ? 'ルート全体のサイズに占める割合' : '1つ上のフォルダのサイズに占める割合',
      render: r => this.gauge(r, mode),
    });
    return c.concat(cols('action', 'tags', 'due', 'mtime', 'warn'));
  },
  gauge(r, mode) {
    if (mode === 'remain') {
      if (!r.dir) return eff(r) ? '<span class="muted">処理済み</span>' : '<span>未処理</span>';
      return r.fc ? gaugeHTML(pct(r.rem, r.fc), `${fmtNum(r.rem)}`, `未処理 ${fmtNum(r.rem)} / ${fmtNum(r.fc)} ファイル (${pct(r.rem, r.fc).toFixed(1)}%)`) : '';
    }
    const base = mode === 'root' ? (this.model.rows[0]?.s || 0) : r._ps;
    const p = pct(r.s, base);
    return gaugeHTML(p, (r.s > 0 && p < 0.1 ? '<0.1' : p.toFixed(1)) + '%', `${fmtSize(r.s)} / ${fmtSize(base)}`);
  },
  async loadSrc() { await this.model.load(); this.src.setSource(this.model); },
  async loadV() { await this.vmodel.load(); this.vt.setSource(this.vmodel); setVTarget(this.vmodel.rows.find(r => r.uuid === S.vTarget) || null); },
  async toggle(model, grid, i) {
    const r = model.rows[i];
    if (r._open) model.collapse(i); else await model.expand(i);
    grid.refresh();
  },
  async reveal(id) {
    const i = await this.model.reveal(id);
    this.src.refresh();
    if (i >= 0) this.src.focusKey(id);
    else toast('この項目は「アクション設定済みを非表示」で隠れています', true);
  },
  async focusV(uuid) {
    const i = await this.vmodel.revealV(uuid);
    this.vt.refresh();
    if (i >= 0) this.vt.focusKey('v:' + uuid);
  },
  // 左で選んだ項目を、右で選択中の仮想フォルダへ移動
  async moveSel(fromButton) {
    const rows = (fromButton ? this.src.selected() : S.sel).filter(r => r.kind !== 'vdir');
    if (!rows.length) return toast('左のツリーで移動する項目を選択してください', true);
    await Plan.move(rows, S.vTarget || 'root');
  },
  async drop(t, rows) {
    const vdirs = rows.filter(r => r.kind === 'vdir'), nodes = rows.filter(r => r.kind !== 'vdir');
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
  async updateBar() {
    const [p, w] = await Promise.all([api('/api/progress'), api('/api/vwarnings')]);
    $('#org-progress').innerHTML = progressHTML(p);
    $('#vt-warn').hidden = !w.length;
    $('#vt-warn').textContent = `⚠ 似た名前 ${w.length}組`;
  },
  async afterEdit() {
    await Promise.all([this.model.reloadKeep(), this.vmodel.reloadKeep()]);
    this.src.setSource(this.model, true);
    this.vt.setSource(this.vmodel, true);
    setVTarget(this.vmodel.rows.find(r => r.uuid === S.vTarget) || null);
    await this.updateBar();
  },
  async planChanged() { if (this.loaded) await this.afterEdit(); },
  async enter(arg) {
    if (!this.loaded) { await Promise.all([this.loadSrc(), this.loadV()]); this.loaded = true; await this.updateBar(); }
    else { this.src.render(); this.vt.render(); }
    if (arg.reveal) await this.reveal(arg.reveal);
    this.src.host.focus();
  },
};

// ===== 検索・リスト =====
const STATES = [['', '処理状況: すべて'], ['unhandled', '未処理'], ['handled', '処理済み(親フォルダの設定を含む)'], ['own', '個別に設定あり'], ['delete', '削除(個別)'], ['move', '移動(個別)']];
Views.list = {
  reset() {
    if (!this.grid) {
      this.grid = new Grid($('#ls-grid'), {
        columns: cols({ key: 'name', label: '名前', w: 240, sort: true, render: r => nameCell(r, false) }, 'depth', 'kind', 'ext', 'mtime', 'size', 'action', 'tags', 'due', 'warn', 'path'),
        rowClass,
        onSelect: rows => Panel.set(rows, this.grid),
        onOpen: guard(r => r.dir ? Panel.go('tree', r) : openFile(r)),
        onSort: () => this.search(),
        onKeyAction: k => {
          if (k === 'Delete') { guard(() => Plan.delete(S.sel))(); return true; }
          if (k === 'm' || k === 'M') { guard(() => Plan.move(S.sel))(); return true; }
          if (k === 'Backspace') { guard(() => Plan.clear(S.sel))(); return true; }
        },
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
  tagOptions() { const cur = $('#ls-tag').value; $('#ls-tag').innerHTML = '<option value="">タグ: 指定なし</option>' + S.tags.map(t => `<option>${esc(t)}</option>`).join(''); $('#ls-tag').value = cur; },
  setForm(a) {
    $('#ls-q').value = a.q || ''; $('#ls-kind').value = a.kind || ''; $('#ls-ext').value = a.ext || '';
    $('#ls-check').value = a.check || ''; $('#ls-state').value = a.state || '';
    this.tagOptions(); $('#ls-tag').value = a.tag || '';
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
    const p = { q: $('#ls-q').value, kind: $('#ls-kind').value, ext: $('#ls-ext').value, check: $('#ls-check').value, state: $('#ls-state').value, tag: $('#ls-tag').value };
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
    if (keep !== true) Panel.set([], self.grid);
  }),
  bulk() {
    const p = this.params();
    const n = $('#ls-count').textContent;
    modal(`<h2>検索結果すべてに一括操作</h2><p class="hint">現在の検索結果(${esc(n)})のすべての項目に適用します。</p>
      <div class="form" style="grid-template-columns:150px 300px">
        <span>操作</span><select id="bk-op"><option value="delete">削除を設定</option><option value="move">移動を設定(移動先を選択)</option><option value="clear">アクションを解除</option><option value="tag">タグを追加</option></select>
        <span>タグ</span><input type="text" id="bk-tag" list="taglist" placeholder="タグを追加する場合"></div>`,
      async m => {
        const op = $('#bk-op', m).value, tag = $('#bk-tag', m).value.trim();
        if (op === 'tag' && !tag) { toast('タグを入力してください', true); return false; }
        let target = '';
        if (op === 'move') { m.close(); target = await VPicker.pick('検索結果すべての移動先を選択'); if (!target) return; }
        const r = await api('/api/plan/filter', { query: new URLSearchParams(p).toString(), op, target, tag });
        if (op === 'move') showReport(r, '移動'); else toast(`${fmtNum(r.applied)}件に適用しました`);
        if (tag) { S.tags.includes(tag) || S.tags.push(tag); updateTagList(); }
        await afterEdit();
      }, '実行');
  },
  async afterEdit() { if (this.ran) await this.search(true); },
  async planChanged() { this.tagOptions(); if (this.ran) await this.search(true); },
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
    $$('.dupg .m[data-id]', el).forEach(m => m.onclick = e => {
      const g = d.groups[+m.dataset.g], r = g.members.find(x => x.id === +m.dataset.id);
      const sel = (e.ctrlKey || e.metaKey) ? (S.sel.some(s => s.id === r.id) ? S.sel.filter(s => s.id !== r.id) : [...S.sel, r]) : [r];
      Panel.set(sel, null); this.render();
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
        columns: cols('action', { key: 'name', label: '名前', w: 220, sort: true, render: r => nameCell(r, false) }, 'tags', 'due', 'memo', 'editor', 'size', 'mtime', 'path'),
        rowClass,
        onSelect: rows => Panel.set(rows, this.grid),
        onOpen: guard(r => r.dir ? Panel.go('tree', r) : openFile(r)),
        onSort: () => this.load(true),
      });
      this.grid.setEmpty('アクションはまだありません');
      $('#pl-state').onchange = $('#pl-editor').onchange = $('#pl-tag').onchange = guard(() => this.load());
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
    const s = this.grid.o.sort; if (s) { p.sort = s.key; if (s.desc) p.desc = 1; }
    return p;
  },
  async load(keep) {
    const p = this.params();
    const src = new PagedSource((off, lim) => '/api/search?' + new URLSearchParams({ ...p, offset: off, limit: lim }), n => $('#pl-count').textContent = `${fmtNum(n)} 件`);
    await src.init();
    this.grid.setSource(src, keep === true);
  },
  async afterEdit() { await this.load(true); await this.top(); },
  async top() {
    const sm = await api('/api/summary');
    const cur = $('#pl-editor').value, curT = $('#pl-tag').value;
    $('#pl-editor').innerHTML = '<option value="">作業者: すべて</option>' + sm.editors.map(o => `<option value="${esc(o.key)}">${esc(o.key)}</option>`).join('');
    $('#pl-editor').value = cur;
    $('#pl-tag').innerHTML = '<option value="">タグ: すべて</option>' + sm.tags.map(o => `<option value="${esc(o.key)}">${esc(o.key)}</option>`).join('');
    $('#pl-tag').value = curT;
    $('#plan-top').innerHTML = `<div class="card"><h2>整理の進み具合</h2>${progressHTML(sm.progress)}</div>`;
  },
  async enter() { await this.top(); await this.load(); },
};

// ===== アクションの統合(複数人の作業用コピー → 新しいマスター) =====
Views.actmerge = {
  items: [], analysis: null,
  async add(paths) {
    paths = paths.map(p => p.trim().replace(/^"|"$/g, '')).filter(p => p && !this.items.some(x => x.path.toLowerCase() === p.toLowerCase()));
    if (!paths.length) return;
    const info = await api('/api/dbinfo', { paths });
    info.forEach(i => {
      if (i.error) toast(`${i.path}: ${i.error}`, true);
      else if (!i.code) toast(`${i.path.split(/[\\/]/).pop()} は作業用コピーではありません(作業者コードなし)`, true);
      else this.items.push(i);
    });
    this.analysis = null;
    this.render();
  },
  render() {
    const a = this.analysis;
    const el = $('#view-actmerge');
    el.innerHTML = `<div class="card"><h2>アクションの統合(複数人の作業をまとめる)</h2>
      <p class="hint">各自が編集した<b>作業用コピー(作業者コード付き)</b>を集めて比較し、<b>作業者コードなしの新しいマスターDB</b>を作ります(元のファイルは変更しません)。<br>
      ・同じ項目を複数人が<b>異なる内容</b>に変えていた場合は「競合」として表示されるので、比較して採用する方を選んでください。<br>
      ・タグは全員の追加・削除をすべて反映します。<br>
      ・統合後は、新しいマスターから作業用コピーを作り直して作業を再開してください(古い版のコピーとは統合できません)。</p>
      <div style="display:flex;gap:6px;margin-bottom:8px"><input type="text" id="am-path" placeholder="作業用コピーのパス" style="flex:1"><button id="am-add">追加</button><button id="am-ref">参照…</button><button id="am-recent">最近開いたファイルから…</button></div>
      <table class="t">${this.items.length ? `<tr><th>作業者コード</th><th>ファイル</th><th>マスターの版</th><th></th></tr>` + this.items.map((x, i) => `<tr><td><b>${esc(x.code)}</b></td><td title="${esc(x.path)}">📄 ${esc(x.path.split(/[\\/]/).pop())}</td><td class="muted">${esc((x.masterRev || '').slice(0, 8))}</td><td><button class="mini danger" data-rm="${i}">×</button></td></tr>`).join('')
        : '<tr><td class="muted">作業用コピーを2つ以上追加してください。</td></tr>'}</table>
      <div style="margin-top:8px"><button class="primary" id="am-analyze" ${this.items.length < 2 ? 'disabled' : ''}>比較する</button></div></div>
      ${a ? `<div class="card"><h2>比較結果</h2>
        <table class="t"><tr><th>作業者</th><th class="num">変更数</th></tr>${a.sources.map(s => `<tr><td>${esc(s.code)}</td><td class="num">${fmtNum(s.changes)}</td></tr>`).join('')}</table>
        <p>自動で統合できる変更: <b>${fmtNum(a.auto)}</b> 件 ・ 競合: <b>${fmtNum(a.conflicts.length)}</b> 件</p>
        ${(a.warnings || []).map(w => `<div class="issue">${esc(w)}</div>`).join('')}
        ${a.conflicts.length ? `<h3>競合の解決</h3><p class="hint">一括選択: ${a.sources.map(s => `<button class="mini" data-pref="${esc(s.code)}">「${esc(s.code)}」を優先</button>`).join(' ')}</p>
          ${a.conflicts.map((c, ci) => `<div class="conf"><div class="ti">${c.kind === 'vnode' ? '🗂 ' : '📄 '}${esc(c.title)}</div><div class="muted" style="font-size:12px">元の状態: ${esc(c.base)}</div>
            ${c.options.map((o, oi) => `<label><input type="radio" name="cf${ci}" value="${oi}" ${oi === 0 ? 'checked' : ''}> ${esc(o.label)} <span class="tag">${o.editors.map(esc).join(', ')}</span></label>`).join('')}</div>`).join('')}` : ''}
        <div class="form" style="grid-template-columns:140px 1fr auto;margin-top:10px"><span>新しいマスターの保存先</span><input type="text" id="am-out" placeholder="空欄なら master_日時.db(DB保存フォルダ)"><button id="am-outref">参照…</button></div>
        <div style="margin-top:8px"><button class="primary" id="am-apply">統合して新しいマスターを作成</button></div></div>` : ''}`;
    $('#am-add').onclick = guard(async () => { await this.add([$('#am-path').value]); });
    $('#am-ref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'dbmulti' }); await this.add(r.paths || []); });
    $('#am-recent').onclick = guard(() => pickRecent(p => this.add(p)));
    $$('[data-rm]', el).forEach(b => b.onclick = () => { this.items.splice(+b.dataset.rm, 1); this.analysis = null; this.render(); });
    $('#am-analyze').onclick = guard(async () => { this.analysis = await api('/api/actmerge/analyze', { dbs: this.items.map(x => x.path) }); this.render(); });
    if (!a) return;
    $$('[data-pref]', el).forEach(b => b.onclick = () => a.conflicts.forEach((c, ci) => {
      const oi = c.options.findIndex(o => o.editors.includes(b.dataset.pref));
      if (oi >= 0) $(`input[name="cf${ci}"][value="${oi}"]`).checked = true;
    }));
    $('#am-outref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'savedb', initial: `${S.state.projectsDir}\\master.db` }); if (r.path) $('#am-out').value = r.path; });
    $('#am-apply').onclick = guard(async () => {
      const choices = {};
      a.conflicts.forEach((c, ci) => { choices[c.key] = +$(`input[name="cf${ci}"]:checked`).value; });
      await api('/api/actmerge/apply', { out: $('#am-out').value, choices });
      this.items = []; this.analysis = null;
      show('import'); Job.watch();
    });
  },
  enter() { this.render(); },
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
    await api('/api/scan', { root: $('#scan-root').value, db: $('#scan-db').value, workers: +$('#scan-workers').value });
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
