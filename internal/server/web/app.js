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

async function api(path, body) {
  const opt = { method: body ? 'POST' : 'GET', headers: { 'X-Token': TOKEN } };
  if (body) { opt.body = JSON.stringify(body); opt.headers['Content-Type'] = 'application/json'; }
  const r = await fetch(path, opt);
  const j = await r.json().catch(() => ({ error: r.statusText }));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
function toast(msg, err) {
  const d = document.createElement('div');
  d.textContent = msg;
  if (err) d.className = 'err';
  $('#toast').appendChild(d);
  setTimeout(() => d.remove(), err ? 7000 : 3500);
}
const guard = fn => async (...a) => { try { return await fn(...a); } catch (e) { toast(e.message, true); } };
function download(path, params) {
  const q = new URLSearchParams(params || {});
  q.set('token', TOKEN);
  location.href = path + '?' + q;
}
function modal(html, onOk, okLabel = 'OK') {
  const m = $('#modal');
  m.innerHTML = `<div class="box">${html}<div class="foot"><button data-x>キャンセル</button>${onOk ? `<button class="primary" data-ok>${okLabel}</button>` : ''}</div></div>`;
  m.hidden = false;
  const close = () => { m.hidden = true; m.innerHTML = ''; };
  $('[data-x]', m).onclick = close;
  if (onOk) $('[data-ok]', m).onclick = guard(async () => { if (await onOk(m) !== false) close(); });
  return m;
}

// ===== 状態 =====
const S = {
  state: null, settings: null, actions: [], checks: [],
  dbv: 0,          // DBを開き直すたびに増える(各画面の再初期化判定)
  notesv: 0,       // 一括設定などで注記が大きく変わったら増える
  noteCache: new Map(), // 今回の操作で変更した注記(画面間で反映)
  owners: new Set(),
  sel: [], selGrid: null, view: 'home',
};

// フラグ(Go側 fsdb/flags.go と同じ値)
const FL = { badchar: 1, trailing: 2, reserved: 4, access: 8, empty: 16, temp: 32, copy: 64, version: 128, single: 256, dup: 512 };
const MIG = FL.badchar | FL.trailing | FL.reserved | FL.access;

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
  if (r.d > st.deepDepth) add('org', '深い', `階層${r.d}(閾値${st.deepDepth})`);
  if (r.dir && r.cc > st.manyFiles) add('org', '項目過多', `直下に${r.cc}項目`);
  if (!r.dir && r.m > 0 && r.m < Date.now() / 1000 - st.oldYears * 365.25 * 86400) add('org', `${st.oldYears}年超`, `更新から${st.oldYears}年以上`);
  return w;
}
const warnHTML = r => warnings(r).map(x => `<span class="b ${x.c}" title="${esc(x.t)}">${esc(x.l)}</span>`).join('');
const actHTML = a => a ? `<span class="act act-${esc(a)}">${esc(a)}</span>` : '';
function applyCache(r) { const c = S.noteCache.get(r.id); if (c) Object.assign(r, c); return r; }

// ===== 仮想スクロールのグリッド =====
class Grid {
  constructor(host, o) {
    this.o = Object.assign({ rh: 24 }, o);
    this.host = host;
    host.tabIndex = 0;
    host.innerHTML = '<div class="g-scroll"><div class="g-head"></div><div class="g-spacer"></div><div class="g-rows"></div></div><div class="g-empty" hidden></div>';
    this.sc = $('.g-scroll', host); this.head = $('.g-head', host);
    this.spacer = $('.g-spacer', host); this.rowsEl = $('.g-rows', host); this.empty = $('.g-empty', host);
    this.sel = new Map(); this.anchor = -1; this.cursor = -1;
    this.src = { count: 0, get: () => null };
    this.cols = this.o.columns;
    this.renderHead();
    this.sc.addEventListener('scroll', () => this.schedule());
    new ResizeObserver(() => this.schedule()).observe(this.sc);
    this.rowsEl.addEventListener('mousedown', e => this.onDown(e));
    host.addEventListener('keydown', e => this.onKey(e));
  }
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
    this.refresh();
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
      applyCache(r);
      const cls = (this.o.rowClass?.(r) || '') + (this.sel.has(r.id) ? ' sel' : '') + (i === this.cursor ? ' cur' : '');
      html += `<div class="g-row ${cls}" data-i="${i}" style="top:${i * rh}px">`;
      for (const c of this.cols) html += `<div class="g-cell ${c.cls || ''}" style="width:${c.w}px">${c.render(r)}</div>`;
      html += '</div>';
    }
    this.rowsEl.innerHTML = html;
  }
  idx(e) { const el = e.target.closest('.g-row'); return el && el.dataset.i ? +el.dataset.i : -1; }
  selected() { return [...this.sel.values()]; }
  emitSel() { this.render(); this.o.onSelect?.(this.selected()); }
  onDown(e) {
    const i = this.idx(e); if (i < 0) return;
    const r = this.src.get(i); if (!r) return;
    e.preventDefault(); // 行の再描画でフォーカス・ダブルクリックが失われないよう自前で処理する
    this.host.focus({ preventScroll: true });
    if (e.target.closest('.tw')) { this.o.onToggle?.(r, i); return; }
    if (e.detail === 2 && !e.shiftKey && !e.ctrlKey) { this.o.onOpen?.(r, i); return; }
    if (e.button === 2 && this.sel.has(r.id)) return;
    if (e.shiftKey && this.anchor >= 0) {
      this.sel.clear();
      const [a, b] = [Math.min(this.anchor, i), Math.max(this.anchor, i)];
      for (let j = a; j <= b; j++) { const x = this.src.get(j); if (x) this.sel.set(x.id, x); }
    } else if (e.ctrlKey || e.metaKey) {
      this.sel.has(r.id) ? this.sel.delete(r.id) : this.sel.set(r.id, r);
      this.anchor = i;
    } else {
      this.sel.clear(); this.sel.set(r.id, r); this.anchor = i;
    }
    this.cursor = i;
    this.emitSel();
  }
  moveTo(i, extend) {
    const n = this.src.count; if (!n) return;
    i = Math.max(0, Math.min(n - 1, i));
    const r = this.src.get(i); if (!r) return;
    if (!extend) { this.sel.clear(); this.anchor = i; }
    this.sel.set(r.id, r);
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
      this.sel.clear(); this.src.rows.forEach(x => this.sel.set(x.id, x)); this.emitSel();
    } else if (/^[0-6]$/.test(k) && !e.ctrlKey && this.sel.size) {
      Panel.quickAction(k === '0' ? '' : S.actions[+k - 1]);
    } else return;
    e.preventDefault();
  }
  // 選択中の行インデックス(カーソル)を保ったままデータを差し替える
  focusId(id) {
    for (let i = 0; i < this.src.count; i++) {
      const r = this.src.get(i);
      if (r && r.id === id) { this.moveTo(i); this.sc.scrollTop = Math.max(0, i * this.o.rh - this.sc.clientHeight / 3); this.host.focus(); return true; }
    }
    return false;
  }
}

// 配列データ
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

// 遅延読み込みのツリー(ツリー画面・エクスプローラー左ペインで共用)
class TreeModel {
  constructor(dirsOnly) { this.dirsOnly = dirsOnly; this.rows = []; }
  get count() { return this.rows.length; }
  get(i) { return this.rows[i]; }
  forEach(fn) { this.rows.forEach(fn); }
  q() { return this.dirsOnly ? '&dirs=1' : ''; }
  prep(r, parent) {
    r._anc = parent ? parent._anc + (parent.d >= 1 ? (parent._last ? '　' : '┃') : '') : '';
    r._has = r.dir && (this.dirsOnly ? r.dc > 0 : r.cc > 0);
    r._open = false;
  }
  markLast(list) {
    const last = new Map();
    for (const r of list) last.set(r.p, r.id);
    for (const r of list) r._last = last.get(r.p) === r.id;
  }
  async load() {
    const { node } = await api('/api/node?id=1');
    node._last = true; this.prep(node, null);
    this.rows = [node];
    await this.expand(0);
  }
  indexOf(id) { return this.rows.findIndex(r => r.id === id); }
  async expand(i) {
    const r = this.rows[i];
    if (!r || !r._has || r._open) return;
    const kids = await api(`/api/children?id=${r.id}${this.q()}`);
    this.markLast(kids);
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
    // 読み込んだ範囲の最深部のフォルダは「未展開」扱い
    r._open = true;
    this.rows = this.rows.slice(0, i + 1).concat(sub, this.rows.slice(i + 1));
  }
  // id の祖先をすべて展開して、その行のインデックスを返す
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

function nameCell(r, tree) {
  const icon = r.dir ? (r._open ? '📂' : '📁') : fileIcon(r.x);
  if (!tree) return `<span class="ico">${icon}</span>${esc(r.n)}`;
  const conn = r.d === 0 ? '' : (r._last ? '┗' : '┣');
  const tw = r._has ? `<span class="tw">${r._open ? '▼' : '▶'}</span>` : '<span class="tw"></span>';
  return `<span class="tl">${r._anc}${conn}</span>${tw}<span class="ico">${icon}</span>${esc(r.n)}`;
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
const rowClass = r => (r.f & MIG) || (r.pl > (S.settings?.pathLimit ?? 250)) ? 'mig' : (r.dir ? 'dir' : '');

// 列定義(画面ごとに組み合わせて使う)
const COL = {
  kind: { key: 'kind', label: '種別', w: 72, render: r => r.dir ? 'フォルダ' : 'ファイル', sort: true },
  ext: { key: 'ext', label: '拡張子', w: 60, render: r => esc(r.x), sort: true },
  mtime: { key: 'mtime', label: '更新日時', w: 128, render: r => fmtDate(r.m), sort: true, descFirst: true },
  size: { key: 'size', label: 'サイズ', w: 85, cls: 'num', render: r => fmtSize(r.s), sort: true, descFirst: true },
  files: { key: 'files', label: '配下ファイル数', w: 95, cls: 'num', render: r => r.dir ? fmtNum(r.fc) : '', sort: true, descFirst: true },
  pathlen: { key: 'pathlen', label: 'パス長', w: 62, cls: 'num', render: r => r.pl, sort: true, descFirst: true, tip: 'ルートからの相対パスの文字数' },
  warn: { key: 'warn', label: '警告・整理のヒント', w: 220, render: warnHTML },
  action: { key: 'action', label: 'アクション', w: 80, render: r => actHTML(r.act), sort: true },
  owner: { key: 'owner', label: '担当', w: 80, render: r => esc(r.own), sort: true },
  memo: { key: 'memo', label: 'メモ', w: 200, render: r => esc(r.memo) },
  path: { key: 'path', label: 'フルパス', w: 480, render: r => `<span title="${esc(r.path)}">${esc(r.path)}</span>`, sort: true },
};
const cols = (...ks) => ks.map(k => typeof k === 'string' ? { ...COL[k] } : k);

// ===== 右パネル(詳細・編集) =====
const Panel = {
  el: () => $('#panel'),
  show(on) { this.el().hidden = !on; },
  set(rows, grid) { S.sel = rows; S.selGrid = grid; this.render(); },
  render() {
    const el = this.el(), rows = S.sel;
    if (!rows.length) {
      el.innerHTML = `<h3>詳細・編集</h3><p class="hint">行をクリックすると詳細が表示され、アクション(残す・削除・アーカイブ等)や担当・メモを記録できます。<br><br>
        ・Ctrl/Shift+クリックで複数選択<br>・キー 1〜6 で選択行にアクションを設定、0 で解除<br>・記録した内容は「アクション計画」でCSV/実行スクリプトに出力できます</p>`;
      return;
    }
    const one = rows.length === 1 ? rows[0] : null;
    let h = '';
    if (one) {
      const r = applyCache(one);
      h += `<h3>${r.dir ? '📁 フォルダ' : '📄 ファイル'}</h3><div style="font-weight:600;word-break:break-all">${esc(r.n)}</div>
        <div class="path">${esc(r.path)}</div>
        <dl><dt>サイズ</dt><dd>${fmtSize(r.s)} <span class="muted">(${fmtNum(r.s)} B)</span></dd>
        <dt>更新日時</dt><dd>${fmtDate(r.m) || '-'}</dd>
        ${r.dir ? `<dt>配下</dt><dd>ファイル ${fmtNum(r.fc)} / フォルダ ${fmtNum(r.dc)}</dd><dt>直下の項目</dt><dd>${fmtNum(r.cc)}</dd>` : ''}
        <dt>階層 / パス長</dt><dd>${r.d} / ${r.pl}文字</dd>
        <dt>警告・ヒント</dt><dd>${warnHTML(r) || '<span class="muted">なし</span>'}</dd></dl>
        <div class="btns">
          <button data-go="tree">ツリーで表示</button>
          <button data-go="explorer">エクスプローラー表示</button>
          ${r.dir ? '<button data-go="under">この配下を検索</button><button data-go="dups">配下の重複</button>' : ''}
          <button data-go="copy">パスをコピー</button>
          <button data-go="reveal" title="このPCからアクセスできる場合、Windowsのエクスプローラーで開きます">Windowsで開く</button>
        </div>`;
    } else {
      const tot = rows.reduce((a, r) => a + r.s, 0);
      h += `<h3>${rows.length}件を選択中</h3><p class="hint">合計 ${fmtSize(tot)}。入力した項目だけがまとめて更新されます(空欄の項目は変更しません)。</p>`;
    }
    const r = one ? applyCache(one) : {};
    h += `<h3 style="margin-top:6px">整理アクション</h3>
      <div class="actbtns">${['', ...S.actions].map((a, i) => `<button data-act="${esc(a)}" class="${one && (r.act || '') === a ? 'on' : ''}" title="キー ${i}">${a ? actHTML(a) : 'なし'}</button>`).join('')}</div>
      <label>担当</label><input id="pe-own" list="owners" value="${esc(r.own || '')}" placeholder="${one ? '' : '(変更しない)'}">
      <div id="pe-nn-w"><label>新しい名前(名前変更)</label><input id="pe-nn" value="${esc(r.nn || '')}" placeholder="${one ? esc(r.n) : '(変更しない)'}"></div>
      <div id="pe-dst-w"><label>移動先フォルダ(移動)</label><input id="pe-dst" value="${esc(r.dst || '')}" placeholder="${one ? '例: \\\\srv\\share\\整理後\\部署A' : '(変更しない)'}"></div>
      <label>メモ</label><textarea id="pe-memo" placeholder="${one ? '判断理由・確認事項など' : '(変更しない)'}">${esc(r.memo || '')}</textarea>
      <div style="margin-top:8px;display:flex;gap:6px"><button class="primary" id="pe-save">保存 <span class="kbd">Ctrl+Enter</span></button></div>`;
    el.innerHTML = h;
    const dirty = new Set();
    ['own', 'nn', 'dst', 'memo'].forEach(k => {
      const inp = $('#pe-' + k, el);
      inp.addEventListener('input', () => dirty.add(k));
      inp.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) save(); });
    });
    const save = guard(async () => {
      const set = {};
      const map = { own: 'owner', nn: 'newName', dst: 'dest', memo: 'memo' };
      for (const k of dirty) set[map[k]] = $('#pe-' + k, el).value;
      if (!Object.keys(set).length) return toast('変更はありません');
      await this.save(set);
      dirty.clear();
    });
    $('#pe-save', el).onclick = save;
    $$('[data-act]', el).forEach(b => b.onclick = () => this.quickAction(b.dataset.act));
    $$('[data-go]', el).forEach(b => b.onclick = guard(() => this.go(b.dataset.go, one)));
  },
  async save(set) {
    const rows = S.sel;
    if (!rows.length) return;
    await api('/api/notes', { ids: rows.map(r => r.id), set });
    const m = { action: 'act', owner: 'own', newName: 'nn', dest: 'dst', memo: 'memo' };
    for (const r of rows) {
      const c = S.noteCache.get(r.id) || {};
      for (const k in set) { c[m[k]] = set[k].trim(); r[m[k]] = set[k].trim(); }
      S.noteCache.set(r.id, c);
    }
    if (set.owner) { S.owners.add(set.owner.trim()); updateOwners(); }
    S.selGrid?.render();
    if (S.view === 'dups') Dups.render();
    this.render();
    toast(`${rows.length}件を保存しました`);
  },
  quickAction: guard(async function (a) {
    if (!S.sel.length) return;
    await Panel.save({ action: a });
  }),
  async go(where, r) {
    if (where === 'tree') return show('tree', { reveal: r.id });
    if (where === 'explorer') return show('explorer', r.dir ? { folder: r.id } : { folder: r.p, focus: r.id });
    if (where === 'under') return show('list', { under: r.id, underPath: r.path });
    if (where === 'dups') return show('dups', { under: r.id, underPath: r.path });
    if (where === 'copy') { await navigator.clipboard.writeText(r.path); return toast('パスをコピーしました'); }
    if (where === 'reveal') { await api('/api/reveal', { id: r.id }); }
  },
};
function updateOwners() { $('#owners').innerHTML = [...S.owners].sort().map(o => `<option value="${esc(o)}">`).join(''); }

// ===== 画面切り替え =====
const Views = {};
function show(name, arg) {
  if (name !== 'home' && !S.state?.db) name = 'home';
  S.view = name;
  $$('#nav button').forEach(b => b.classList.toggle('active', b.dataset.view === name));
  $$('.view').forEach(v => v.classList.toggle('active', v.id === 'view-' + name));
  Panel.show(['tree', 'explorer', 'list', 'dups', 'plan'].includes(name));
  Panel.set([], null);
  const v = Views[name];
  if (v) {
    if (v.dbv !== S.dbv) { v.dbv = S.dbv; v.notesv = S.notesv; v.reset?.(); }
    else if (v.notesv !== S.notesv) { v.notesv = S.notesv; v.notesChanged?.(); }
    guard(() => v.enter(arg || {}))();
  }
}

async function refreshState() {
  S.state = await api('/api/state');
  S.actions = S.state.actions; S.checks = S.state.checks;
  const db = S.state.db;
  S.settings = db?.settings || null;
  $('#dbname').textContent = db ? '📄 ' + db.path : 'DB未選択';
  $('#dbname').title = db ? db.path : '';
  $$('#nav .needdb').forEach(b => b.disabled = !db);
  return S.state;
}
function dbOpened() {
  S.dbv++; S.noteCache.clear(); S.owners.clear();
  api('/api/summary').then(sm => { sm.owners.forEach(o => S.owners.add(o.key)); updateOwners(); }).catch(() => { });
}

// ===== ホーム =====
Views.home = {
  async enter() {
    await refreshState();
    const db = S.state.db;
    $('#home-current').innerHTML = db ? `<h2>開いているDB</h2>
      <div class="path" style="font-family:var(--mono)">${esc(db.path)}</div>
      <p class="hint">取込元: ${db.meta.source === 'excel' ? 'Excel' : 'フォルダスキャン'} (${esc(db.meta.source_path || '')})<br>
      作成: ${esc(db.meta.built_at || '')} ・ 項目数: ${fmtNum(+db.meta.node_count)}</p>
      <button class="primary" onclick="show('summary')">サマリーを見る</button> <button onclick="show('tree')">ツリーで見る</button> <button onclick="show('explorer')">エクスプローラーで見る</button>
      <button id="home-close">閉じる</button>`
      : `<h2>はじめに</h2><p class="hint">下の ①Excel取込 ②フォルダスキャン ③保存済みDB のいずれかでデータを開いてください。<br>DBの保存先: ${esc(S.state.projectsDir)}</p>`;
    if (db) $('#home-close').onclick = guard(async () => { await api('/api/close', {}); await refreshState(); S.dbv++; this.enter(); });
    const ps = await api('/api/projects');
    $('#projects').innerHTML = ps.length ? `<table class="t"><tr><th>保存済みDB(${esc(S.state.projectsDir)})</th><th>サイズ</th><th>更新</th></tr>
      ${ps.map(p => `<tr class="click" data-path="${esc(p.path)}"><td>📄 ${esc(p.name)}</td><td>${fmtSize(p.size)}</td><td>${esc(p.modified)}</td></tr>`).join('')}</table>`
      : '<p class="muted">保存済みのDBはまだありません。</p>';
    $$('#projects tr[data-path]').forEach(tr => tr.onclick = () => openDB(tr.dataset.path));
    if (S.state.job && !S.state.job.finished) Job.watch();
  },
};
const openDB = guard(async path => {
  await api('/api/open', { path });
  await refreshState(); dbOpened();
  toast('DBを開きました');
  show('summary');
});

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
    const pct = j.total > 0 ? Math.min(100, j.done / j.total * 100) : 0;
    const txt = `${j.kind === 'excel' ? 'Excel取込' : 'スキャン'}: ${j.phase}` + (j.total > 0 ? ` (${pct.toFixed(0)}%)` : j.done ? ` ${fmtNum(j.done)}件` : '') + ` ・ 経過 ${j.elapsed}`;
    $('#job-text').textContent = txt;
    $('#job-prog').classList.toggle('indet', !(j.total > 0));
    $('#job-prog i').style.width = pct + '%';
    $('#jobmini').hidden = !!j.finished;
    $('#jobmini').textContent = '⏳ ' + txt;
    if (j.finished) {
      clearInterval(Job.timer);
      $('#home-job').hidden = true;
      if (j.error) return toast(j.error, true);
      await refreshState(); dbOpened();
      toast(`完了しました(${fmtNum(j.result?.items)}項目)`);
      (j.result?.warnings || []).forEach(w => toast(w));
      show('summary');
    }
  }),
};

// ===== サマリー(5S) =====
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
        <div class="stat"><div class="k">データ取得</div><div class="v" style="font-size:13px">${m.source === 'excel' ? 'Excel取込' : 'スキャン'}<br>${esc(m.scanned_at || m.imported_at || m.built_at)}</div></div>
      </div>
      ${m.source === 'excel' ? `<p class="hint">※ ${esc(m.size_note || '')}</p>` : ''}
      <div class="cols2">
        <div class="card"><h2>5S チェック</h2>
          <p class="hint">件数をクリックすると該当項目の一覧を表示します。判定の閾値は <a id="sm-settings">設定</a> で変更できます(古い: ${st.oldYears}年 / パス長: ${st.pathLimit}文字 / 深い階層: ${st.deepDepth} / 項目過多: ${st.manyFiles})。</p>
          ${Object.entries(groups).map(([g, cs]) => `<div class="check-grp"><h3>${esc(gdesc[g] || g)}</h3><table class="t">
            ${cs.map(c => `<tr class="click" data-check="${c.key}"><td>${esc(c.label)}</td><td class="num">${fmtNum(c.count)} 件</td><td class="num muted">${c.size ? fmtSize(c.size) : ''}</td></tr>`).join('')}</table></div>`).join('')}
        </div>
        <div>
          <div class="card"><h2>容量の大きいフォルダ(第1〜2階層)</h2><table class="t">
            ${sm.topFolders.map(f => `<tr class="click" data-id="${f.id}"><td class="wrap">📁 ${esc(f.path.slice(m.root.length) || f.n)}</td><td class="num">${fmtSize(f.s)}</td><td>${bar(f.s, sm.totalSize || 1)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>最終更新からの経過(ファイル)</h2><table class="t"><tr><th>経過</th><th class="num">件数</th><th class="num">サイズ</th><th></th></tr>
            ${sm.age.map(a => `<tr><td>${a.label}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td><td>${bar(a.count, maxAge)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>階層の深さ別の項目数</h2><table class="t"><tr><th>階層</th><th class="num">項目数</th><th class="num">ファイル容量</th><th></th></tr>
            ${sm.depth.map(a => `<tr class="${+a.key > st.deepDepth ? 'click' : ''}" data-deep="${a.key}"><td>${a.label}${+a.key > st.deepDepth ? ' <span class="b org">深い</span>' : ''}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td><td>${bar(a.count, maxDepth)}</td></tr>`).join('')}</table></div>
          <div class="card"><h2>拡張子 上位10(容量順)</h2><table class="t">
            ${sm.topExts.map(x => `<tr class="click" data-ext="${esc(x.key)}"><td>${esc(x.label)}</td><td class="num">${fmtNum(x.count)} 件</td><td class="num">${fmtSize(x.size)}</td></tr>`).join('')}</table>
            <p><a onclick="show('exts')">すべての拡張子を見る →</a></p></div>
          <div class="card"><h2>アクションの記録状況</h2>
            ${sm.actions.length ? `<table class="t">${sm.actions.map(a => `<tr><td>${actHTML(a.key)}</td><td class="num">${fmtNum(a.count)} 件</td><td class="num">${fmtSize(a.size)}</td></tr>`).join('')}</table>` : '<p class="muted">まだ記録はありません。ツリー等で行を選び、右パネルでアクションを設定してください。</p>'}
            <p><a onclick="show('plan')">アクション計画を開く →</a></p></div>
        </div>
      </div>`;
    el.innerHTML = this.html;
    $$('tr[data-check]', el).forEach(tr => tr.onclick = () => show('list', { check: tr.dataset.check }));
    $$('tr[data-ext]', el).forEach(tr => tr.onclick = () => show('list', { ext: tr.dataset.ext, kind: 'file' }));
    $$('tr[data-id]', el).forEach(tr => tr.onclick = () => show('explorer', { folder: +tr.dataset.id }));
    $$('tr.click[data-deep]', el).forEach(tr => tr.onclick = () => show('list', { check: 'deep' }));
    $('#sm-settings').onclick = () => this.settings(st);
  },
  settings(st) {
    modal(`<h2>判定の閾値</h2><div class="form" style="grid-template-columns:220px 120px">
      <span>古いファイル(更新から○年以上)</span><input type="number" id="st-old" value="${st.oldYears}" min="1">
      <span>パス文字数の警告(○文字超)</span><input type="number" id="st-path" value="${st.pathLimit}" min="1">
      <span>深い階層(○階層超)</span><input type="number" id="st-deep" value="${st.deepDepth}" min="1">
      <span>項目過多(直下○項目超)</span><input type="number" id="st-many" value="${st.manyFiles}" min="1"></div>
      <p class="hint">パス文字数は選択フォルダからの相対パスです。SharePointの上限(400文字)は移行先のサイトURL等を含むため、余裕を持った値にしてください。</p>`,
      async m => {
        S.settings = await api('/api/settings', { oldYears: +$('#st-old', m).value, pathLimit: +$('#st-path', m).value, deepDepth: +$('#st-deep', m).value, manyFiles: +$('#st-many', m).value });
        await refreshState(); S.notesv++;
        this.enter();
      }, '保存');
  },
};

// ===== ツリー(Excel風) =====
Views.tree = {
  reset() {
    this.model = new TreeModel($('#tr-dirs').checked);
    if (!this.grid) {
      this.grid = new Grid($('#tr-grid'), {
        columns: cols({ key: 'name', label: '名前(ツリー)', w: 460, render: r => nameCell(r, true) }, 'kind', 'mtime', 'size', 'files', 'pathlen', 'warn', 'action', 'owner', 'memo'),
        rowClass,
        onSelect: rows => Panel.set(rows, this.grid),
        onToggle: guard((r, i) => this.toggle(i)),
        onOpen: guard((r, i) => r.dir ? this.toggle(i) : Panel.go('explorer', r)),
        onArrow: guard(async (r, i, right) => {
          if (right && r._has && !r._open) await this.toggle(i);
          else if (!right && r._open) await this.toggle(i);
          else if (!right && r.d > 0) { const pi = this.model.indexOf(r.p); if (pi >= 0) this.grid.moveTo(pi); }
        }),
      });
      $('#tr-collapse').onclick = guard(() => this.load());
      $('#tr-expand').onclick = guard(async () => {
        const r = this.grid.selected()[0] || this.model.rows[0];
        const i = this.model.indexOf(r.id);
        await this.model.expandDepth(i, +$('#tr-depth').value);
        this.grid.refresh();
      });
      $('#tr-dirs').onchange = guard(() => { this.reset(); return this.load(); });
      const go = guard(async () => {
        const p = $('#tr-path').value.trim(); if (!p) return;
        const { id } = await api('/api/find?path=' + encodeURIComponent(p));
        await this.reveal(id);
      });
      $('#tr-go').onclick = go;
      $('#tr-path').onkeydown = e => { if (e.key === 'Enter') go(); };
    }
    this.loaded = false;
  },
  async load() {
    await this.model.load();
    this.grid.setSource(this.model);
    this.loaded = true;
  },
  async toggle(i) {
    const r = this.model.rows[i];
    if (r._open) this.model.collapse(i); else await this.model.expand(i);
    this.grid.refresh();
  },
  async reveal(id) {
    const i = await this.model.reveal(id);
    this.grid.refresh();
    if (i >= 0) this.grid.focusId(id);
  },
  async enter(arg) {
    if (!this.loaded) await this.load();
    else this.grid.render();
    if (arg.reveal) await this.reveal(arg.reveal);
    this.grid.host.focus();
  },
};

// ===== エクスプローラー =====
Views.explorer = {
  reset() {
    this.model = new TreeModel(true);
    this.hist = [];
    if (!this.tree) {
      this.tree = new Grid($('#ex-tree'), {
        columns: [{ key: 'name', label: 'フォルダ', w: 600, render: r => nameCell(r, true) }],
        rowClass: r => '',
        onSelect: rows => rows[0] && this.open(rows[0].id, { fromTree: true }),
        onToggle: guard(async (r, i) => { r._open ? this.model.collapse(i) : await this.model.expand(i); this.tree.refresh(); }),
        onArrow: guard(async (r, i, right) => { if (right !== !!r._open && r._has) { right ? await this.model.expand(i) : this.model.collapse(i); this.tree.refresh(); } }),
      });
      this.list = new Grid($('#ex-list'), {
        columns: cols({ key: 'name', label: '名前', w: 320, sort: true, render: r => nameCell(r, false) }, 'mtime', 'ext', { ...COL.size, w: 130, render: r => this.sizeCell(r) }, 'files', 'warn', 'action', 'owner', 'memo'),
        rowClass, sort: null,
        onSelect: rows => Panel.set(rows, this.list),
        onOpen: guard(r => r.dir ? this.open(r.id) : null),
        onSort: s => this.sortRows(s),
      });
      this.list.setEmpty('このフォルダは空です');
      $('#ex-up').onclick = guard(() => this.cur && this.cur.d > 0 && this.open(this.cur.p, { focus: this.cur.id }));
      $('#ex-back').onclick = guard(() => { this.hist.pop(); const h = this.hist.pop(); if (h) this.open(h); });
      $('#ex-q').onkeydown = e => { if (e.key === 'Enter' && this.cur) show('list', { under: this.cur.id, underPath: this.cur.path, q: e.target.value }); };
    }
    this.cur = null;
  },
  sizeCell(r) {
    const tot = this.cur?.s || 1;
    return `<div class="szbar"><i style="width:${Math.round(r.s / tot * 50)}px"></i>${fmtSize(r.s)}</div>`;
  },
  sortRows(s) {
    const key = { name: r => r.n.toLowerCase(), mtime: r => r.m, ext: r => r.x, size: r => r.s, files: r => r.fc, action: r => r.act, owner: r => r.own }[s.key];
    if (!key || !this.rows) return;
    const d = s.desc ? -1 : 1;
    this.rows.sort((a, b) => (b.dir - a.dir) || (key(a) < key(b) ? -d : key(a) > key(b) ? d : 0));
    this.list.setSource(new ArraySource(this.rows));
  },
  async open(id, o = {}) {
    const { node, ancestors } = await api('/api/node?id=' + id);
    if (!node.dir) return;
    this.cur = node;
    if (this.hist[this.hist.length - 1] !== id) this.hist.push(id);
    this.rows = await api('/api/children?id=' + id);
    if (this.list.o.sort) this.sortRows(this.list.o.sort); else this.list.setSource(new ArraySource(this.rows));
    $('#ex-crumbs').innerHTML = [...ancestors, node].map(a => `<a data-id="${a.id}">${esc(a.d === 0 ? a.path : a.n)}</a>`).join('<span class="muted">›</span>') +
      `<span class="muted" style="margin-left:8px">${fmtNum(node.cc)}項目 ・ ${fmtSize(node.s)}</span>`;
    $$('#ex-crumbs a').forEach(a => a.onclick = guard(() => this.open(+a.dataset.id)));
    Panel.set([], this.list);
    if (!o.fromTree) {
      const i = await this.model.reveal(id);
      this.tree.refresh();
      if (i >= 0) { this.tree.sel.clear(); this.tree.sel.set(id, this.model.rows[i]); this.tree.cursor = i; this.tree.render();
        const sc = this.tree.sc; const top = i * 24; if (top < sc.scrollTop || top > sc.scrollTop + sc.clientHeight - 50) sc.scrollTop = top - sc.clientHeight / 3; }
    }
    if (o.focus) this.list.focusId(o.focus);
  },
  async enter(arg) {
    if (!this.model.rows.length) { await this.model.load(); this.tree.setSource(this.model); }
    else { this.tree.render(); this.list.render(); }
    if (arg.folder) await this.open(arg.folder, { focus: arg.focus });
    else if (!this.cur) await this.open(1);
  },
  notesChanged() { if (this.cur) this.open(this.cur.id); },
};

// ===== 検索・リスト =====
Views.list = {
  reset() {
    if (!this.grid) {
      $('#ls-check').innerHTML = '<option value="">警告・ヒント: 指定なし</option>' + S.checks.map(c => `<option value="${c.key}">[${esc(c.group)}] ${esc(c.label)}</option>`).join('');
      $('#ls-action').innerHTML = actionOptions('アクション');
      this.grid = new Grid($('#ls-grid'), {
        columns: cols({ key: 'name', label: '名前', w: 260, sort: true, render: r => nameCell(r, false) }, 'kind', 'ext', 'mtime', 'size', 'files', 'warn', 'action', 'owner', 'memo', 'path'),
        rowClass,
        onSelect: rows => Panel.set(rows, this.grid),
        onOpen: r => Panel.go('explorer', r),
        onSort: () => this.search(),
      });
      this.grid.setEmpty('該当する項目はありません');
      $('#ls-go').onclick = guard(() => this.search());
      $$('#view-list .toolbar input').forEach(i => i.addEventListener('keydown', e => { if (e.key === 'Enter') this.search(); }));
      $$('#view-list .toolbar select').forEach(s => s.addEventListener('change', () => this.search()));
      $('#ls-clear').onclick = () => { this.setForm({}); this.search(); };
      $('#ls-csv').onclick = () => download('/api/export/list.csv', this.params());
      $('#ls-bulk').onclick = () => this.bulk();
    }
    this.under = null;
    this.setForm({});
    this.ran = false;
  },
  setForm(a) {
    $('#ls-q').value = a.q || ''; $('#ls-kind').value = a.kind || ''; $('#ls-ext').value = a.ext || '';
    $('#ls-check').value = a.check || ''; $('#ls-action').value = a.action || '';
    $('#ls-min').value = ''; $('#ls-before').value = '';
    this.under = a.under ? { id: a.under, path: a.underPath } : null;
    this.renderUnder();
  },
  renderUnder() {
    const el = $('#ls-under');
    el.hidden = !this.under;
    if (this.under) {
      el.innerHTML = `<span class="b org" title="${esc(this.under.path)}">配下: ${esc(this.under.path.split(/[\\/]/).pop())} <a id="ls-under-x">×</a></span>`;
      $('#ls-under-x').onclick = () => { this.under = null; this.renderUnder(); this.search(); };
    }
  },
  params() {
    const p = { q: $('#ls-q').value, kind: $('#ls-kind').value, ext: $('#ls-ext').value, check: $('#ls-check').value, action: $('#ls-action').value };
    const mb = +$('#ls-min').value; if (mb > 0) p.minSize = Math.round(mb * 1048576);
    const d = $('#ls-before').value; if (d) p.before = Math.floor(new Date(d + 'T00:00:00').getTime() / 1000);
    if (this.under) p.under = this.under.id;
    const s = this.grid.o.sort; if (s) { p.sort = s.key; if (s.desc) p.desc = 1; }
    for (const k in p) if (p[k] === '' || p[k] == null) delete p[k];
    return p;
  },
  search: guard(async function () {
    const self = Views.list;
    const p = self.params();
    $('#ls-count').textContent = '検索中…';
    const src = new PagedSource((off, lim) => '/api/search?' + new URLSearchParams({ ...p, offset: off, limit: lim }),
      n => $('#ls-count').textContent = `${fmtNum(n)} 件`);
    await src.init();
    self.grid.setSource(src);
    self.ran = true;
    Panel.set([], self.grid);
  }),
  bulk() {
    const p = this.params();
    const n = $('#ls-count').textContent;
    modal(`<h2>検索結果すべてに一括設定</h2><p class="hint">現在の検索結果(${esc(n)})のすべての項目に設定します。入力した項目だけが更新されます。</p>
      <div class="form" style="grid-template-columns:120px 300px">
        <span>アクション</span><select id="bk-act"><option value="__keep">(変更しない)</option><option value="">なし(解除)</option>${S.actions.map(a => `<option>${esc(a)}</option>`).join('')}</select>
        <span>担当</span><input type="text" id="bk-own" list="owners" placeholder="(変更しない)">
        <span>メモ</span><input type="text" id="bk-memo" placeholder="(変更しない)"></div>`,
      async m => {
        const set = {};
        const a = $('#bk-act', m).value; if (a !== '__keep') set.action = a;
        const o = $('#bk-own', m).value; if (o) { set.owner = o; S.owners.add(o); updateOwners(); }
        const me = $('#bk-memo', m).value; if (me) set.memo = me;
        if (!Object.keys(set).length) { toast('変更する項目を入力してください', true); return false; }
        const r = await api('/api/notes/filter', { query: new URLSearchParams(p).toString(), set });
        S.notesv++; this.notesv = S.notesv; S.noteCache.clear();
        toast(`${fmtNum(r.updated)}件を更新しました`);
        this.search();
      }, '一括設定');
  },
  async enter(arg) {
    if (Object.keys(arg).length) { this.setForm(arg); return this.search(); }
    if (!this.ran) return this.search();
    this.grid.render();
  },
  notesChanged() { if (this.ran) this.search(); },
};
function actionOptions(label) {
  return `<option value="">${label}: 指定なし</option><option value="any">記録あり(すべて)</option><option value="none">アクション未設定</option>` +
    S.actions.map(a => `<option value="${esc(a)}">${esc(a)}</option>`).join('');
}

// ===== 重複候補 =====
const Dups = Views.dups = {
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
  render() {
    const el = $('#view-dups'), d = this.data;
    if (!d) return;
    const pages = Math.ceil(d.total / 50), page = this.off / 50 + 1;
    el.innerHTML = `<div class="card"><h2>重複候補(同名・同サイズ)${this.under ? ` — 配下: ${esc(this.under.path)} <a id="dp-all">全体に戻す</a>` : ''}</h2>
      <p class="hint">${fmtNum(d.total)} グループ ・ 重複を1つに減らすと最大 <b>${fmtSize(d.wasted)}</b> 削減できます(削減量の多い順)。<br>
      ファイル名とサイズだけで判定しています。「内容を比較」でSHA-256を計算し、本当に同一か確認できます(このPCからアクセスできる場合)。<br>
      項目をクリック → 右パネルでアクションを設定。「最新以外を削除候補に」で、更新日時が最新の1件を残し他を「削除」にします。</p>
      <div style="display:flex;gap:6px;align-items:center"><button id="dp-prev" ${page <= 1 ? 'disabled' : ''}>← 前</button><span>${page} / ${Math.max(1, pages)} ページ</span><button id="dp-next" ${page >= pages ? 'disabled' : ''}>次 →</button></div></div>
      ${d.groups.map((g, gi) => `<div class="dupg"><div class="h"><b>${esc(g.name)}</b><span>${fmtSize(g.size)} × ${fmtNum(g.count)}件</span>
        <span class="muted">削減可能 ${fmtSize(g.size * (g.count - 1))}</span><span class="grow" style="flex:1"></span>
        <button data-hash="${gi}">内容を比較</button><button data-keep="${gi}">最新以外を削除候補に</button></div>
        ${g.members.map(r => { applyCache(r); const h = this.hash[r.id]; return `<div class="m ${S.sel.some(s => s.id === r.id) ? 'sel' : ''}" data-g="${gi}" data-id="${r.id}">
          <span>${fmtDate(r.m)}</span><span class="p">${esc(r.path)}</span>${actHTML(r.act)}${h ? `<span class="hash" title="${esc(h)}">${esc(h.startsWith('ERROR') ? h : h.slice(0, 10))}</span>` : ''}</div>`; }).join('')}
        ${g.count > g.members.length ? `<div class="m muted">… 他 ${fmtNum(g.count - g.members.length)} 件(検索・リストで「${esc(g.name)}」を検索してください)</div>` : ''}</div>`).join('')}`;
    $('#dp-prev').onclick = guard(() => { this.off -= 50; return this.load(); });
    $('#dp-next').onclick = guard(() => { this.off += 50; return this.load(); });
    if (this.under) $('#dp-all').onclick = guard(() => { this.under = null; this.off = 0; return this.load(); });
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
      S.sel = rest;
      await Panel.save({ action: '削除', memo: '重複(最新を残す)' });
      this.render();
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
    $$('#view-exts tr[data-ext]').forEach(tr => tr.onclick = () => show('list', { ext: tr.dataset.ext, kind: 'file', sort: 'size' }));
  },
};

// ===== アクション計画 =====
Views.plan = {
  reset() {
    if (!this.grid) {
      $('#pl-action').innerHTML = `<option value="any">記録あり(すべて)</option>` + S.actions.map(a => `<option value="${esc(a)}">${esc(a)}</option>`).join('');
      this.grid = new Grid($('#pl-grid'), {
        columns: cols('action', 'owner', { key: 'name', label: '名前', w: 220, sort: true, render: r => nameCell(r, false) }, { key: 'nn', label: '新しい名前', w: 140, render: r => esc(r.nn) },
          { key: 'dst', label: '移動先', w: 200, render: r => esc(r.dst) }, 'memo', 'size', 'mtime', 'path'),
        rowClass,
        onSelect: rows => Panel.set(rows, this.grid),
        onOpen: r => Panel.go('explorer', r),
        onSort: () => this.load(),
      });
      this.grid.setEmpty('記録はまだありません');
      $('#pl-action').onchange = $('#pl-owner').onchange = guard(() => this.load());
      $('#pl-csv').onclick = () => download('/api/export/list.csv', this.params());
      $('#pl-ps1').onclick = () => modal(`<h2>PowerShell実行スクリプトの出力</h2><p class="hint">「削除」「アーカイブ」「移動」「名前変更」を実行するスクリプト(.ps1)を出力します。<br>
        ・<b>既定はドライラン</b>です。そのまま実行しても何も変更せず、実行予定をログCSVに書き出します。<br>
        ・内容を確認後、<code>-Execute</code> を付けて実行すると実際に変更します。<br>
        ・アーカイブ先は <code>-ArchiveRoot "D:\\退避先"</code> で指定できます(既定: ルート名_Archive)。<br>
        ・「削除」は完全削除です。不安なものは「アーカイブ」にして退避する運用をおすすめします。<br>
        ・親フォルダを削除/移動する場合、その配下の個別アクションは自動的にスキップされます。</p>`, () => download('/api/export/plan.ps1'), 'ダウンロード');
      $('#pl-import').onclick = () => modal(`<h2>別DBから注記を引継ぐ</h2><p class="hint">再スキャン・再取込したDBに、以前のDBで記録したアクション・担当・メモを<b>フルパスが一致する項目</b>へコピーします。</p>
        <div class="form"><span>以前のDB</span><input type="text" id="ni-path"><button id="ni-ref">参照…</button></div>`,
        async m => { const r = await api('/api/notes/import', { path: $('#ni-path', m).value }); S.notesv++; this.notesv = S.notesv; S.noteCache.clear(); toast(`${fmtNum(r.imported)}件を引継ぎました`); this.enter(); }, '引継ぐ') &&
        ($('#ni-ref').onclick = guard(async () => { const r = await api('/api/dialog', { kind: 'db' }); if (r.path) $('#ni-path').value = r.path; }));
    }
  },
  params() {
    const p = { action: $('#pl-action').value || 'any' };
    const o = $('#pl-owner').value; if (o) p.owner = o;
    const s = this.grid.o.sort; if (s) { p.sort = s.key; if (s.desc) p.desc = 1; }
    return p;
  },
  async load() {
    const p = this.params();
    const src = new PagedSource((off, lim) => '/api/search?' + new URLSearchParams({ ...p, offset: off, limit: lim }), n => $('#pl-count').textContent = `${fmtNum(n)} 件`);
    await src.init();
    this.grid.setSource(src);
  },
  async enter() {
    const sm = await api('/api/summary');
    sm.owners.forEach(o => S.owners.add(o.key)); updateOwners();
    const cur = $('#pl-owner').value;
    $('#pl-owner').innerHTML = '<option value="">担当: すべて</option>' + sm.owners.map(o => `<option value="${esc(o.key)}">${esc(o.key)}</option>`).join('');
    $('#pl-owner').value = cur;
    $('#plan-top').innerHTML = `<div class="cols2"><div class="card"><h2>アクション別</h2>${sm.actions.length ? `<table class="t"><tr><th>アクション</th><th class="num">件数</th><th class="num">サイズ</th></tr>
      ${sm.actions.map(a => `<tr><td>${actHTML(a.key)}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td></tr>`).join('')}</table>` : '<p class="muted">記録なし</p>'}</div>
      <div class="card"><h2>担当別</h2>${sm.owners.length ? `<table class="t"><tr><th>担当</th><th class="num">件数</th><th class="num">サイズ</th></tr>
      ${sm.owners.map(a => `<tr><td>${esc(a.key)}</td><td class="num">${fmtNum(a.count)}</td><td class="num">${fmtSize(a.size)}</td></tr>`).join('')}</table>` : '<p class="muted">記録なし</p>'}</div></div>`;
    await this.load();
  },
};

// ===== 起動 =====
function initHome() {
  $$('[data-dialog]').forEach(b => b.onclick = guard(async () => {
    const t = $('#' + b.dataset.target);
    const r = await api('/api/dialog', { kind: b.dataset.dialog, initial: t.value });
    if (r.path) t.value = r.path;
  }));
  $('#imp-go').onclick = guard(async () => {
    await api('/api/import-excel', { xlsx: $('#imp-xlsx').value, db: $('#imp-db').value });
    Job.watch();
  });
  $('#scan-go').onclick = guard(async () => {
    await api('/api/scan', { root: $('#scan-root').value, db: $('#scan-db').value, workers: +$('#scan-workers').value });
    Job.watch();
  });
  $('#job-cancel').onclick = guard(() => api('/api/job/cancel', {}));
  $('#open-go').onclick = () => openDB($('#open-path').value);
  $('#quit').onclick = () => modal('<h2>ツールを終了しますか？</h2>', async () => { await api('/api/shutdown', {}); document.body.innerHTML = '<p style="padding:40px">終了しました。このタブは閉じて構いません。</p>'; }, '終了');
}

(async () => {
  initHome();
  $$('#nav button').forEach(b => b.onclick = () => show(b.dataset.view));
  await guard(refreshState)();
  if (S.state?.db) { dbOpened(); show('summary'); } else show('home');
  if (S.state?.job && !S.state.job.finished) Job.watch();
})();
