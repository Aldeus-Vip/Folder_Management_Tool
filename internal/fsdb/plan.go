package fsdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// アクション(整理計画)。フォルダに設定すると配下すべてに及ぶ(配下に個別の設定があればそちらを優先)。
const (
	ActDelete = "delete" // 削除
	ActMove   = "move"   // 仮想フォルダへ移動(+名前変更・期限)
)

// VRoot は仮想フォルダ構成(整理後のフォルダ構成)のルートのID。
const VRoot = "root"

const planSchema = `
CREATE TABLE IF NOT EXISTS vnodes(
  uuid TEXT PRIMARY KEY, parent TEXT NOT NULL DEFAULT '', name TEXT NOT NULL,
  memo TEXT NOT NULL DEFAULT '', editor TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '',
  deleted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS plan(
  node_id INTEGER PRIMARY KEY, action TEXT NOT NULL DEFAULT '', vparent TEXT NOT NULL DEFAULT '',
  new_name TEXT NOT NULL DEFAULT '', due TEXT NOT NULL DEFAULT '', memo TEXT NOT NULL DEFAULT '',
  editor TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS plan_vparent ON plan(vparent);
CREATE TABLE IF NOT EXISTS tags(node_id INTEGER NOT NULL, tag TEXT NOT NULL, PRIMARY KEY(node_id, tag));
CREATE INDEX IF NOT EXISTS tags_tag ON tags(tag);
-- アクションが及ぶ範囲(互いに素な区間)。検索の「未処理/処理済み」絞り込み用
CREATE TABLE IF NOT EXISTS plan_cover(s INTEGER PRIMARY KEY, e INTEGER NOT NULL);`

func ensurePlanSchema(db *sql.DB) error {
	if _, err := db.Exec(planSchema); err != nil {
		return err
	}
	// 旧バージョンの注記(notes)で「削除」とされたものを引き継ぐ
	var n int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='notes'`).Scan(&n)
	if n > 0 {
		db.Exec(`INSERT OR IGNORE INTO plan(node_id, action, memo, updated_at)
			SELECT node_id, CASE WHEN action='削除' THEN 'delete' ELSE '' END, memo, COALESCE(updated_at,'') FROM notes
			WHERE action='削除' OR memo!=''`)
		db.Exec(`DROP TABLE notes`)
		return rebuildCover(db)
	}
	return nil
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func nowStr() string { return time.Now().Format("2006/01/02 15:04:05") }

// ---- 区間インデックス ----
// ノードIDは行きがけ順なので、ノード X の配下は (X.id, X.end] の連続区間になる。
// アクションを設定したノードの区間は「入れ子」か「互いに素」のどちらかなので、
// 入れ子の木(encl=直近の外側の区間)を作れば、継承されたアクションや未処理件数を高速に求められる。

type prefix struct {
	files []int32 // files[i] = ID が i 以下のファイル数
	size  []int64
}

func (p *prefix) count(a, b int64) (int64, int64) { // 区間 [a, b] のファイル数・サイズ
	if a < 1 {
		a = 1
	}
	if b >= int64(len(p.files)) {
		b = int64(len(p.files)) - 1
	}
	if a > b {
		return 0, 0
	}
	return int64(p.files[b] - p.files[a-1]), p.size[b] - p.size[a-1]
}

type planIndex struct {
	ids, ends []int64
	encl      []int32 // 直近の外側の区間(なければ -1)
	acts      []string
	vps       []string
	// 子区間のグループ: key = 外側の区間のインデックス(-1 = 最上位)
	kids map[int32]*kidGroup
}

type kidGroup struct {
	ids        []int64
	files, siz []int64 // 累積(長さ len(ids)+1)
}

// nearest は x を含む最も内側の区間(x 自身の区間を含む)。無ければ -1。
func (pi *planIndex) nearest(x int64) int32 {
	i := int32(sort.Search(len(pi.ids), func(k int) bool { return pi.ids[k] > x })) - 1
	for i >= 0 && pi.ends[i] < x {
		i = pi.encl[i]
	}
	return i
}

func (s *Store) loadPrefix() error {
	if s.pre != nil {
		return nil
	}
	var n int64
	if err := s.DB.QueryRow(`SELECT max(id) FROM nodes`).Scan(&n); err != nil {
		return err
	}
	p := &prefix{files: make([]int32, n+1), size: make([]int64, n+1)}
	rows, err := s.DB.Query(`SELECT id, is_dir, size FROM nodes ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var prev int64
	for rows.Next() {
		var id, size int64
		var dir bool
		if err := rows.Scan(&id, &dir, &size); err != nil {
			return err
		}
		for k := prev + 1; k < id; k++ {
			p.files[k], p.size[k] = p.files[k-1], p.size[k-1]
		}
		p.files[id], p.size[id] = p.files[id-1], p.size[id-1]
		if !dir {
			p.files[id]++
			p.size[id] += size
		}
		prev = id
	}
	s.pre = p
	return rows.Err()
}

// ensureIndex はアクションの区間インデックスと仮想フォルダのキャッシュを用意する。
func (s *Store) ensureIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadPrefix(); err != nil {
		return err
	}
	if s.pidx == nil {
		rows, err := s.DB.Query(`SELECT p.node_id, n.end_id, p.action, p.vparent FROM plan p JOIN nodes n ON n.id=p.node_id WHERE p.action!='' ORDER BY p.node_id`)
		if err != nil {
			return err
		}
		pi := &planIndex{kids: map[int32]*kidGroup{}}
		var stack []int32
		for rows.Next() {
			var id, end int64
			var act, vp string
			if err := rows.Scan(&id, &end, &act, &vp); err != nil {
				rows.Close()
				return err
			}
			k := int32(len(pi.ids))
			for len(stack) > 0 && pi.ends[stack[len(stack)-1]] < id {
				stack = stack[:len(stack)-1]
			}
			enc := int32(-1)
			if len(stack) > 0 {
				enc = stack[len(stack)-1]
			}
			pi.ids, pi.ends, pi.acts, pi.vps = append(pi.ids, id), append(pi.ends, end), append(pi.acts, act), append(pi.vps, vp)
			pi.encl = append(pi.encl, enc)
			stack = append(stack, k)
			g := pi.kids[enc]
			if g == nil {
				g = &kidGroup{files: []int64{0}, siz: []int64{0}}
				pi.kids[enc] = g
			}
			f, sz := s.pre.count(id, end)
			g.ids = append(g.ids, id)
			g.files = append(g.files, g.files[len(g.files)-1]+f)
			g.siz = append(g.siz, g.siz[len(g.siz)-1]+sz)
		}
		rows.Close()
		s.pidx = pi
	}
	if s.vt == nil {
		vt, err := s.loadVTree()
		if err != nil {
			return err
		}
		s.vt = vt
	}
	return nil
}

func (s *Store) invalidate() {
	s.mu.Lock()
	s.pidx, s.vt = nil, nil
	s.mu.Unlock()
}

// inner は ノードの区間から「配下で個別にアクションが設定された部分」を除いたファイル数・サイズ。
func (s *Store) inner(id, end int64, isDir bool, size int64) (int64, int64) {
	pi := s.pidx
	if !isDir {
		return 1, size
	}
	f, sz := s.pre.count(id+1, end)
	g := pi.kids[pi.nearest(id)]
	if g != nil {
		lo := sort.Search(len(g.ids), func(k int) bool { return g.ids[k] > id })
		hi := sort.Search(len(g.ids), func(k int) bool { return g.ids[k] > end })
		f -= g.files[hi] - g.files[lo]
		sz -= g.siz[hi] - g.siz[lo]
	}
	return f, sz
}

// enrich は継承されたアクション・移動先パス・未処理件数を埋める。
func (s *Store) enrich(ns []Node) error {
	if len(ns) == 0 {
		return nil
	}
	if err := s.ensureIndex(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pi, vt := s.pidx, s.vt
	for i := range ns {
		x := &ns[i]
		if x.VParent != "" {
			x.VPath = vt.path(x.VParent)
		}
		k := pi.nearest(x.ID)
		if k >= 0 && pi.ids[k] == x.ID {
			k = pi.encl[k] // 自身の設定ではなく、親から継承されるもの
		}
		if k >= 0 {
			x.IAction, x.IFrom = pi.acts[k], pi.ids[k]
			if pi.vps[k] != "" {
				x.IVPath = vt.path(pi.vps[k])
			}
		}
		x.Inner, x.InnerS = s.inner(x.ID, x.End, x.IsDir, x.Size)
		if x.Action == "" && x.IAction == "" {
			x.Rem, x.RemS = x.Inner, x.InnerS
		}
	}
	return nil
}

// rebuildCover は plan_cover(アクションが及ぶ区間の和集合)を作り直す。
func rebuildCover(db interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
}) error {
	rows, err := db.Query(`SELECT p.node_id, n.end_id FROM plan p JOIN nodes n ON n.id=p.node_id WHERE p.action!='' ORDER BY p.node_id`)
	if err != nil {
		return err
	}
	var ss, es []int64
	for rows.Next() {
		var a, b int64
		rows.Scan(&a, &b)
		if len(es) > 0 && a <= es[len(es)-1] {
			if b > es[len(es)-1] {
				es[len(es)-1] = b
			}
			continue
		}
		ss, es = append(ss, a), append(es, b)
	}
	rows.Close()
	if _, err := db.Exec(`DELETE FROM plan_cover`); err != nil {
		return err
	}
	for i := range ss {
		if _, err := db.Exec(`INSERT INTO plan_cover VALUES(?,?)`, ss[i], es[i]); err != nil {
			return err
		}
	}
	return nil
}

// Progress は全体の処理状況(ルート配下の未処理ファイル数など)。
type PlanProgress struct {
	Files     int64 `json:"files"`
	Size      int64 `json:"size"`
	RemFiles  int64 `json:"remFiles"`
	RemSize   int64 `json:"remSize"`
	DelFiles  int64 `json:"delFiles"`
	DelSize   int64 `json:"delSize"`
	MoveFiles int64 `json:"moveFiles"`
	MoveSize  int64 `json:"moveSize"`
}

func (s *Store) Progress() (*PlanProgress, error) {
	root, err := s.Node(1)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pp := &PlanProgress{Files: root.Files, Size: root.Size, RemFiles: root.Rem, RemSize: root.RemS}
	pi := s.pidx
	for k := range pi.ids {
		dir := pi.ends[k] > pi.ids[k]
		f, sz := s.pre.count(pi.ids[k], pi.ends[k])
		if dir {
			f, sz = s.inner(pi.ids[k], pi.ends[k], true, 0)
		}
		if pi.acts[k] == ActDelete {
			pp.DelFiles += f
			pp.DelSize += sz
		} else {
			pp.MoveFiles += f
			pp.MoveSize += sz
		}
	}
	return pp, nil
}

// ---- 仮想フォルダ構成 ----

type vnode struct {
	UUID, Parent, Name, Memo, Editor string
	Depth                            int
	kids                             []*vnode
}

type vtree struct {
	nodes    map[string]*vnode
	rootName string
	base     string // 整理後のルートの実パス(パス長の計算・スクリプト出力用)
	sep      string
}

func (vt *vtree) path(uuid string) string {
	var parts []string
	for n := vt.nodes[uuid]; n != nil && n.UUID != VRoot; n = vt.nodes[n.Parent] {
		parts = append(parts, n.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, vt.sep)
}

// fullPath は整理後の実パス(ルートの実パス + 仮想パス)。
func (vt *vtree) fullPath(uuid string) string {
	p := vt.path(uuid)
	if p == "" {
		return vt.base
	}
	if vt.base == "" {
		return p
	}
	return strings.TrimRight(vt.base, `\/`) + vt.sep + p
}

func (s *Store) loadVTree() (*vtree, error) {
	r := s.Rules()
	meta, _ := s.Meta()
	sep := meta["sep"]
	if sep == "" {
		sep = `\`
	}
	vt := &vtree{nodes: map[string]*vnode{VRoot: {UUID: VRoot, Name: r.RootName}}, rootName: r.RootName, base: r.BasePath, sep: sep}
	rows, err := s.DB.Query(`SELECT uuid, parent, name, memo, editor FROM vnodes WHERE deleted=0`)
	if err != nil {
		return nil, err
	}
	var all []*vnode
	for rows.Next() {
		n := &vnode{}
		if err := rows.Scan(&n.UUID, &n.Parent, &n.Name, &n.Memo, &n.Editor); err != nil {
			rows.Close()
			return nil, err
		}
		vt.nodes[n.UUID] = n
		all = append(all, n)
	}
	rows.Close()
	for _, n := range all {
		p := vt.nodes[n.Parent]
		if p == nil {
			p = vt.nodes[VRoot] // 親が無い(統合時の不整合など)→ルート直下に表示
			n.Parent = VRoot
		}
		p.kids = append(p.kids, n)
	}
	var setDepth func(n *vnode, d int)
	setDepth = func(n *vnode, d int) {
		n.Depth = d
		sort.Slice(n.kids, func(i, j int) bool { return strings.ToLower(n.kids[i].Name) < strings.ToLower(n.kids[j].Name) })
		for _, k := range n.kids {
			setDepth(k, d+1)
		}
	}
	setDepth(vt.nodes[VRoot], 0)
	return vt, nil
}

// vSubtreeIDs は仮想フォルダとその配下の仮想フォルダのID。
func (s *Store) vSubtreeIDs(uuid string) []string {
	if s.ensureIndex() != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.vt.nodes[uuid]
	if n == nil {
		return nil
	}
	var out []string
	var walk func(*vnode)
	walk = func(v *vnode) {
		out = append(out, v.UUID)
		for _, k := range v.kids {
			walk(k)
		}
	}
	walk(n)
	return out
}

// VRow は仮想ツリーの1行(仮想フォルダ、または移動して配置された実フォルダ/ファイル)。
type VRow struct {
	Key     string   `json:"key"` // v:<uuid> または n:<id>
	Kind    string   `json:"kind"`
	UUID    string   `json:"uuid,omitempty"`
	Parent  string   `json:"parent,omitempty"` // 仮想フォルダの親
	Name    string   `json:"n"`
	Path    string   `json:"vpath"` // 仮想パス
	Depth   int      `json:"d"`
	Files   int64    `json:"fc"`
	Size    int64    `json:"s"`
	Kids    int      `json:"cc"` // 直下の項目数
	Memo    string   `json:"memo,omitempty"`
	Editor  string   `json:"ed,omitempty"`
	Similar []string `json:"sim,omitempty"` // 同じ階層にある似た名前
	Node    *Node    `json:"node,omitempty"`
}

type vstat struct {
	files, size int64
	direct      int
}

// vStats は各仮想フォルダの合計(配下の仮想フォルダ・配置された項目を含む)を計算する。ロック中に呼ぶ。
func (s *Store) vStats() map[string]*vstat {
	pi, vt := s.pidx, s.vt
	st := map[string]*vstat{}
	get := func(u string) *vstat {
		if st[u] == nil {
			st[u] = &vstat{}
		}
		return st[u]
	}
	for k := range pi.ids {
		if pi.acts[k] != ActMove || vt.nodes[pi.vps[k]] == nil {
			continue
		}
		var f, sz int64
		if pi.ends[k] > pi.ids[k] {
			f, sz = s.inner(pi.ids[k], pi.ends[k], true, 0)
		} else {
			f, sz = s.pre.count(pi.ids[k], pi.ids[k])
		}
		get(pi.vps[k]).direct++
		for n := vt.nodes[pi.vps[k]]; n != nil; n = vt.nodes[n.Parent] {
			x := get(n.UUID)
			x.files += f
			x.size += sz
			if n.UUID == VRoot {
				break
			}
		}
	}
	for _, n := range vt.nodes {
		get(n.UUID).direct += len(n.kids)
	}
	return st
}

func (s *Store) vfolderRow(n *vnode, st map[string]*vstat) VRow {
	x := st[n.UUID]
	if x == nil {
		x = &vstat{}
	}
	r := VRow{Key: "v:" + n.UUID, Kind: "vdir", UUID: n.UUID, Parent: n.Parent, Name: n.Name, Path: s.vt.path(n.UUID), Depth: n.Depth,
		Files: x.files, Size: x.size, Kids: x.direct, Memo: n.Memo, Editor: n.Editor}
	if n.UUID == VRoot {
		r.Path = ""
	}
	return r
}

// VNode は仮想フォルダ1件。
func (s *Store) VNode(uuid string) (*VRow, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.vt.nodes[uuid]
	if n == nil {
		return nil, fmt.Errorf("仮想フォルダが見つかりません")
	}
	r := s.vfolderRow(n, s.vStats())
	if p := s.vt.nodes[n.Parent]; p != nil && n.UUID != VRoot {
		r.Similar = similarAmong(n.Name, siblingNames(s, p, n.UUID))
	}
	return &r, nil
}

func siblingNames(s *Store, parent *vnode, except string) []string {
	var names []string
	for _, k := range parent.kids {
		if k.UUID != except {
			names = append(names, k.Name)
		}
	}
	return names
}

// VChildren は仮想フォルダの直下(仮想フォルダ + 移動して配置された項目)。
func (s *Store) VChildren(uuid string) ([]VRow, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	n := s.vt.nodes[uuid]
	if n == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("仮想フォルダが見つかりません")
	}
	st := s.vStats()
	out := []VRow{}
	var names []string
	for _, k := range n.kids {
		out = append(out, s.vfolderRow(k, st))
		names = append(names, k.Name)
	}
	s.mu.Unlock()
	placed, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE p.action='move' AND p.vparent=? ORDER BY n.is_dir DESC, lower(CASE WHEN p.new_name!='' THEN p.new_name ELSE n.name END)`, uuid)
	if err != nil {
		return nil, err
	}
	for i := range placed {
		x := &placed[i]
		name := x.Name
		if x.NewName != "" {
			name = x.NewName
		}
		if x.IsDir {
			names = append(names, name)
		}
		kind := "file"
		if x.IsDir {
			kind = "dir"
		}
		out = append(out, VRow{Key: fmt.Sprintf("n:%d", x.ID), Kind: kind, Name: name, Depth: n.Depth + 1,
			Files: x.Inner, Size: x.InnerS, Kids: int(x.Children), Node: x})
	}
	// 同じ階層の似た名前(フォルダ同士)。項目が多いときは、仮想フォルダが絡む組だけを調べる
	var idx []int
	for i := range out {
		if out[i].Kind != "file" {
			idx = append(idx, i)
		}
	}
	keys := make([]simKey, len(out))
	for _, i := range idx {
		keys[i] = mkSim(out[i].Name)
	}
	many := len(idx) > 300
	for x, i := range idx {
		for _, j := range idx[x+1:] {
			if many && out[i].Kind != "vdir" && out[j].Kind != "vdir" {
				continue
			}
			if ok, _ := simPair(keys[i], keys[j]); ok {
				out[i].Similar = append(out[i].Similar, out[j].Name)
				out[j].Similar = append(out[j].Similar, out[i].Name)
			}
		}
	}
	return out, nil
}

// VReal は仮想ツリー上に配置された実フォルダの中身(個別に別のアクションが設定された項目は除く)。
func (s *Store) VReal(id int64, depth int) ([]VRow, error) {
	ns, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE n.parent_id=? AND COALESCE(p.action,'')='' ORDER BY n.id`, id)
	if err != nil {
		return nil, err
	}
	out := make([]VRow, len(ns))
	for i := range ns {
		x := &ns[i]
		kind := "file"
		if x.IsDir {
			kind = "dir"
		}
		out[i] = VRow{Key: fmt.Sprintf("n:%d", x.ID), Kind: kind, Name: x.Name, Depth: depth + 1, Files: x.Inner, Size: x.InnerS, Kids: int(x.Children), Node: x}
	}
	return out, nil
}

// VTreeAll は仮想フォルダだけのツリー(移動先の選択用)。
func (s *Store) VTreeAll() ([]VRow, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.vStats()
	var out []VRow
	var walk func(*vnode)
	walk = func(n *vnode) {
		out = append(out, s.vfolderRow(n, st))
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(s.vt.nodes[VRoot])
	return out, nil
}

// SimilarWarnings は仮想ツリー全体で、同じ階層に似た名前のフォルダがある組を返す。
func (s *Store) SimilarWarnings() ([][2]string, error) {
	rows, err := s.VTreeAll()
	if err != nil {
		return nil, err
	}
	var out [][2]string
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		n := s.vt.nodes[r.UUID]
		for i := 0; i < len(n.kids); i++ {
			for j := i + 1; j < len(n.kids); j++ {
				if ok, _ := Similar(n.kids[i].Name, n.kids[j].Name); ok {
					out = append(out, [2]string{s.vt.path(n.kids[i].UUID), n.kids[j].Name})
				}
			}
		}
	}
	return out, nil
}
