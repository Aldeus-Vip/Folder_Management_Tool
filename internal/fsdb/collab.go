package fsdb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- 複数人での編集 ----
// 運用: マスター(作業者コードなし)のDBから、各人が「作業用コピー(作業者コード付き)」を作って編集する。
// コピー作成時の状態を *_base テーブルに保存しておき、統合時は各コピーの「base からの変更」を集めて
// マスターに適用する(3方向マージ)。同じ項目を複数人が異なる内容に変えていれば「競合」として選んでもらう。

const baseTables = `
DROP TABLE IF EXISTS plan_base; DROP TABLE IF EXISTS vnodes_base; DROP TABLE IF EXISTS tags_base;
CREATE TABLE plan_base AS SELECT * FROM plan;
CREATE TABLE vnodes_base AS SELECT * FROM vnodes;
CREATE TABLE tags_base AS SELECT * FROM tags;`

// EditorCode は作業者コード(空=マスター)。
func (s *Store) EditorCode() string {
	var c string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='editor_code'`).Scan(&c)
	return c
}

func (s *Store) ensureMasterID() error {
	m, err := s.Meta()
	if err != nil {
		return err
	}
	kv := map[string]string{}
	if m["master_id"] == "" {
		kv["master_id"] = newUUID()
	}
	if m["master_rev"] == "" {
		kv["master_rev"] = newUUID()
	}
	return s.SetMeta(kv)
}

func validCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("作業者コードを入力してください")
	}
	if strings.ContainsAny(code, `\/:*?"<>|`) || len([]rune(code)) > 40 {
		return "", fmt.Errorf("作業者コードに使えない文字が含まれるか、長すぎます")
	}
	return code, nil
}

// WorkCopy はマスターから作業用コピー(作業者コード付き)を作る。
func (s *Store) WorkCopy(dst, code string) error {
	code, err := validCode(code)
	if err != nil {
		return err
	}
	if s.EditorCode() != "" {
		return fmt.Errorf("作業用コピーはマスター(作業者コードなし)のDBから作成してください。現在のDBは「%s」さんの作業用コピーです", s.EditorCode())
	}
	if err := s.ensureMasterID(); err != nil {
		return err
	}
	if abs, _ := filepath.Abs(dst); strings.EqualFold(abs, mustAbs(s.Path)) {
		return fmt.Errorf("同じファイルには保存できません")
	}
	os.Remove(dst)
	if _, err := s.DB.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	c, err := Open(dst)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.becomeCopy(code); err != nil {
		return err
	}
	// 統合時に「元のマスターへ統合」できるよう、マスターの場所を覚えておく
	return c.SetMeta(map[string]string{"master_path": mustAbs(s.Path)})
}

func (s *Store) becomeCopy(code string) error {
	if _, err := s.DB.Exec(baseTables); err != nil {
		return err
	}
	return s.SetMeta(map[string]string{"editor_code": code, "copied_at": nowStr()})
}

// SetCode は今開いているDBに作業者コードを設定する(1人で使う場合など)。
func (s *Store) SetCode(code string) error {
	code, err := validCode(code)
	if err != nil {
		return err
	}
	if err := s.ensureMasterID(); err != nil {
		return err
	}
	if s.EditorCode() != "" {
		return s.SetMeta(map[string]string{"editor_code": code})
	}
	return s.becomeCopy(code)
}

func mustAbs(p string) string { a, _ := filepath.Abs(p); return a }

// ---- アクションの統合(マージ) ----

type planVal struct{ Action, VParent, NewName, Due, Memo string }
type vnodeVal struct {
	Parent, Name, Memo string
	Deleted            bool
}

type mergeSrc struct {
	Path, Code, MasterID, MasterRev, MasterPath string
	master                                      bool // 統合先のマスター(作業用コピーではない)
	plan, planBase                              map[int64]planVal
	editor                                      map[int64]string
	vn, vnBase                                  map[string]vnodeVal
	vnEditor                                    map[string]string
	tags, tagsBase                              map[int64]map[string]bool
	db                                          *sql.DB
}

// loadMergeSrc は作業用コピーを読む。base を渡すとマスターとして読み、比較の基準に base の控えを使う。
func loadMergeSrc(path string, base *mergeSrc) (*mergeSrc, error) {
	st, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	ms := &mergeSrc{Path: path, db: st.DB, editor: map[int64]string{}, vnEditor: map[string]string{}}
	m, _ := st.Meta()
	ms.Code, ms.MasterID, ms.MasterRev, ms.MasterPath = m["editor_code"], m["master_id"], m["master_rev"], m["master_path"]
	var nb int
	st.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='plan_base'`).Scan(&nb)
	if base == nil && (ms.Code == "" || nb == 0) {
		st.Close()
		return nil, fmt.Errorf("%s は作業用コピー(作業者コード付き)ではありません", filepath.Base(path))
	}
	if base != nil { // マスター: 現在の内容を、作業用コピーの base と比べる
		if ms.Code != "" {
			st.Close()
			return nil, fmt.Errorf("%s はマスターではありません(作業者コード「%s」の作業用コピーです)", filepath.Base(path), ms.Code)
		}
		ms.master, ms.Code = true, "マスター(現在)"
	}
	loadPlan := func(table string, withEditor bool) (map[int64]planVal, error) {
		rows, err := st.DB.Query(`SELECT node_id, action, vparent, new_name, due, memo, editor FROM ` + table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[int64]planVal{}
		for rows.Next() {
			var id int64
			var v planVal
			var ed string
			rows.Scan(&id, &v.Action, &v.VParent, &v.NewName, &v.Due, &v.Memo, &ed)
			if v != (planVal{}) {
				out[id] = v
				if withEditor {
					ms.editor[id] = ed
				}
			}
		}
		return out, rows.Err()
	}
	loadVn := func(table string, withEditor bool) (map[string]vnodeVal, error) {
		rows, err := st.DB.Query(`SELECT uuid, parent, name, memo, deleted, editor FROM ` + table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]vnodeVal{}
		for rows.Next() {
			var id, ed string
			var v vnodeVal
			rows.Scan(&id, &v.Parent, &v.Name, &v.Memo, &v.Deleted, &ed)
			out[id] = v
			if withEditor {
				ms.vnEditor[id] = ed
			}
		}
		return out, rows.Err()
	}
	loadTags := func(table string) (map[int64]map[string]bool, error) {
		rows, err := st.DB.Query(`SELECT node_id, tag FROM ` + table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[int64]map[string]bool{}
		for rows.Next() {
			var id int64
			var t string
			rows.Scan(&id, &t)
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			out[id][t] = true
		}
		return out, rows.Err()
	}
	if base != nil {
		ms.planBase, ms.vnBase, ms.tagsBase = base.planBase, base.vnBase, base.tagsBase
		if ms.plan, err = loadPlan("plan", true); err == nil {
			if ms.vn, err = loadVn("vnodes", true); err == nil {
				ms.tags, err = loadTags("tags")
			}
		}
	} else if ms.plan, err = loadPlan("plan", true); err == nil {
		if ms.planBase, err = loadPlan("plan_base", false); err == nil {
			if ms.vn, err = loadVn("vnodes", true); err == nil {
				if ms.vnBase, err = loadVn("vnodes_base", false); err == nil {
					if ms.tags, err = loadTags("tags"); err == nil {
						ms.tagsBase, err = loadTags("tags_base")
					}
				}
			}
		}
	}
	if err != nil {
		st.Close()
		return nil, err
	}
	return ms, nil
}

// MergeOption は競合の選択肢の1つ。
type MergeOption struct {
	Label   string   `json:"label"`
	Editors []string `json:"editors"`
}

// Conflict は複数人が同じ項目を異なる内容に変更したもの。
type Conflict struct {
	Key     string        `json:"key"`
	Kind    string        `json:"kind"` // plan | vnode
	Title   string        `json:"title"`
	Base    string        `json:"base"` // 元(マスター)の状態
	Options []MergeOption `json:"options"`
	values  []any
}

// MergeSourceInfo は統合元の概要。
type MergeSourceInfo struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Changes int    `json:"changes"`
}

type MergeAnalysis struct {
	MasterPath string            `json:"masterPath"` // 統合先のマスター(空 = 新しいファイルとして保存)
	Sources    []MergeSourceInfo `json:"sources"`
	Conflicts  []Conflict        `json:"conflicts"`
	Auto       int               `json:"auto"` // 自動で統合できた変更の数
	Warnings   []string          `json:"warnings"`

	srcs     []*mergeSrc
	plan     map[int64]planVal
	planEd   map[int64]string
	vn       map[string]vnodeVal
	vnEd     map[string]string
	tags     map[int64]map[string]bool
	conflict map[string]*Conflict
}

func (ma *MergeAnalysis) Close() {
	for _, s := range ma.srcs {
		s.db.Close()
	}
}

func (ms *mergeSrc) vpath(uuid string) string {
	var parts []string
	for i := 0; uuid != "" && uuid != VRoot && i < 64; i++ {
		v, ok := ms.vn[uuid]
		if !ok {
			v = ms.vnBase[uuid]
		}
		parts = append([]string{v.Name}, parts...)
		uuid = v.Parent
	}
	return strings.Join(parts, `\`)
}

func (ms *mergeSrc) planLabel(v planVal) string {
	var parts []string
	switch v.Action {
	case ActDelete:
		parts = append(parts, "削除")
	case ActMove:
		parts = append(parts, "移動 → "+joinV("(整理後)", ms.vpath(v.VParent)))
	default:
		parts = append(parts, "アクションなし")
	}
	if v.NewName != "" {
		parts = append(parts, "名前: "+v.NewName)
	}
	if v.Due != "" {
		parts = append(parts, "期限: "+v.Due)
	}
	if v.Memo != "" {
		parts = append(parts, "メモ: "+v.Memo)
	}
	return strings.Join(parts, " / ")
}

func (ms *mergeSrc) vnodeLabel(v vnodeVal) string {
	if v.Deleted || v.Name == "" {
		return "削除"
	}
	l := "名前: " + v.Name + " / 場所: " + joinV("(整理後)", ms.vpath(v.Parent))
	if v.Memo != "" {
		l += " / メモ: " + v.Memo
	}
	return l
}

// AnalyzeActionMerge は作業用コピーの変更を比較し、自動統合できるものと競合を洗い出す。
// master を指定すると、そのマスターへの統合として扱い、マスターの現在の内容も比較に加える
// (コピー作成後に他の人の統合でマスターが更新されていても、その内容と突き合わせて統合できる)。
func AnalyzeActionMerge(paths []string, master string) (*MergeAnalysis, error) {
	if len(paths) < 1 {
		return nil, fmt.Errorf("統合する作業用コピーを指定してください")
	}
	ma := &MergeAnalysis{conflict: map[string]*Conflict{}}
	for _, p := range paths {
		if master != "" && strings.EqualFold(mustAbs(p), mustAbs(master)) {
			ma.Close()
			return nil, fmt.Errorf("統合先のマスターは作業用コピーの一覧に含めないでください")
		}
		ms, err := loadMergeSrc(p, nil)
		if err != nil {
			ma.Close()
			return nil, err
		}
		ma.srcs = append(ma.srcs, ms)
	}
	s0 := ma.srcs[0]
	if master != "" {
		ms, err := loadMergeSrc(master, s0)
		if err != nil {
			ma.Close()
			return nil, err
		}
		if ms.MasterID != s0.MasterID {
			ms.db.Close()
			ma.Close()
			return nil, fmt.Errorf("統合先(%s)は、作業用コピーの元になったマスターではありません", filepath.Base(master))
		}
		if ms.MasterRev != s0.MasterRev {
			ma.Warnings = append(ma.Warnings, "マスターは作業用コピーの作成後に更新されています(他の人の統合が先に行われました)。マスターの現在の内容とも比較して統合します")
		}
		ma.MasterPath = mustAbs(master)
		ma.srcs = append(ma.srcs, ms)
	}
	codes := map[string]bool{}
	for _, s := range ma.srcs {
		if s.master {
			continue
		}
		if s.MasterID != s0.MasterID {
			ma.Close()
			return nil, fmt.Errorf("%s は別のマスターから作られたコピーです", filepath.Base(s.Path))
		}
		if s.MasterRev != s0.MasterRev {
			ma.Close()
			return nil, fmt.Errorf("%s は別の版のマスターから作られたコピーです(統合後のマスターから作り直したコピー同士で統合してください)", filepath.Base(s.Path))
		}
		if codes[s.Code] {
			ma.Warnings = append(ma.Warnings, "作業者コード「"+s.Code+"」のコピーが複数あります")
		}
		codes[s.Code] = true
	}

	// --- アクション ---
	ma.plan, ma.planEd = map[int64]planVal{}, map[int64]string{}
	for id, v := range s0.planBase {
		ma.plan[id] = v
	}
	keys := map[int64]bool{}
	for _, s := range ma.srcs {
		for id := range s.plan {
			keys[id] = true
		}
		for id := range s.planBase {
			keys[id] = true
		}
	}
	var pathsNeeded []int64
	for id := range keys {
		base := s0.planBase[id]
		type cand struct {
			v   planVal
			eds []string
			src *mergeSrc
		}
		var cands []*cand
		for i, s := range ma.srcs {
			v := s.plan[id]
			if v == s.planBase[id] {
				continue
			}
			ma.Sources = growSources(ma.Sources, ma.srcs, i)
			ma.Sources[i].Changes++
			found := false
			for _, c := range cands {
				if c.v == v {
					c.eds = append(c.eds, s.Code)
					found = true
				}
			}
			if !found {
				cands = append(cands, &cand{v: v, eds: []string{s.Code}, src: s})
			}
		}
		switch len(cands) {
		case 0:
		case 1:
			ma.plan[id] = cands[0].v
			ma.planEd[id] = cands[0].src.editor[id]
			ma.Auto++
		default:
			c := &Conflict{Key: fmt.Sprintf("p:%d", id), Kind: "plan", Base: s0.planLabel(base)}
			for _, x := range cands {
				c.Options = append(c.Options, MergeOption{Label: x.src.planLabel(x.v), Editors: x.eds})
				c.values = append(c.values, x.v)
			}
			ma.conflict[c.Key] = c
			pathsNeeded = append(pathsNeeded, id)
		}
	}
	// --- 仮想フォルダ ---
	ma.vn, ma.vnEd = map[string]vnodeVal{}, map[string]string{}
	for id, v := range s0.vnBase {
		ma.vn[id] = v
	}
	vkeys := map[string]bool{}
	for _, s := range ma.srcs {
		for id := range s.vn {
			vkeys[id] = true
		}
	}
	for id := range vkeys {
		var vals []vnodeVal
		var eds [][]string
		var from []*mergeSrc
		for i, s := range ma.srcs {
			v, ok := s.vn[id]
			b, inBase := s.vnBase[id]
			if ok == inBase && v == b {
				continue
			}
			ma.Sources = growSources(ma.Sources, ma.srcs, i)
			ma.Sources[i].Changes++
			k := -1
			for j := range vals {
				if vals[j] == v {
					k = j
				}
			}
			if k < 0 {
				vals, eds, from = append(vals, v), append(eds, nil), append(from, s)
				k = len(vals) - 1
			}
			eds[k] = append(eds[k], s.Code)
		}
		switch len(vals) {
		case 0:
		case 1:
			ma.vn[id] = vals[0]
			ma.vnEd[id] = from[0].vnEditor[id]
			ma.Auto++
		default:
			title := vals[0].Name
			if b, ok := s0.vnBase[id]; ok {
				title = joinV("(整理後)", s0.vpath(id)) + "(" + b.Name + ")"
			}
			c := &Conflict{Key: "v:" + id, Kind: "vnode", Title: "仮想フォルダ " + title, Base: s0.vnodeLabel(s0.vnBase[id])}
			for j, v := range vals {
				c.Options = append(c.Options, MergeOption{Label: from[j].vnodeLabel(v), Editors: eds[j]})
				c.values = append(c.values, v)
			}
			ma.conflict[c.Key] = c
		}
	}
	// --- タグ(追加・削除をすべて反映。競合なし) ---
	ma.tags = map[int64]map[string]bool{}
	for id, ts := range s0.tagsBase {
		ma.tags[id] = map[string]bool{}
		for t := range ts {
			ma.tags[id][t] = true
		}
	}
	for i, s := range ma.srcs {
		changed := false
		for id, ts := range s.tags {
			for t := range ts {
				if !s.tagsBase[id][t] {
					if ma.tags[id] == nil {
						ma.tags[id] = map[string]bool{}
					}
					ma.tags[id][t] = true
					changed = true
				}
			}
		}
		for id, ts := range s.tagsBase {
			for t := range ts {
				if !s.tags[id][t] {
					delete(ma.tags[id], t)
					changed = true
				}
			}
		}
		if changed {
			ma.Sources = growSources(ma.Sources, ma.srcs, i)
			ma.Sources[i].Changes++
		}
	}
	ma.Sources = growSources(ma.Sources, ma.srcs, len(ma.srcs)-1)
	// 競合の見出し(フルパス)
	for _, id := range pathsNeeded {
		var p string
		s0.db.QueryRow(`SELECT path FROM nodes WHERE id=?`, id).Scan(&p)
		ma.conflict[fmt.Sprintf("p:%d", id)].Title = p
	}
	ma.Conflicts = []Conflict{} // 競合なしでも空の一覧で返す(JSONで null にしない)
	for _, c := range ma.conflict {
		ma.Conflicts = append(ma.Conflicts, *c)
	}
	sort.Slice(ma.Conflicts, func(i, j int) bool {
		if ma.Conflicts[i].Kind != ma.Conflicts[j].Kind {
			return ma.Conflicts[i].Kind > ma.Conflicts[j].Kind // 仮想フォルダを先に
		}
		return ma.Conflicts[i].Title < ma.Conflicts[j].Title
	})
	return ma, nil
}

func growSources(out []MergeSourceInfo, srcs []*mergeSrc, i int) []MergeSourceInfo {
	for len(out) <= i {
		s := srcs[len(out)]
		out = append(out, MergeSourceInfo{Path: s.Path, Code: s.Code})
	}
	return out
}

// ApplyActionMerge は競合の選択(choices: 競合キー → 選択肢の番号)を反映して、
// 作業者コードなしの新しいマスターDBを out に作る。
// MasterPath が設定されていれば、マスターを直接更新する(元のマスターは _backup_日時.db として残す)。
// そうでなければ out に新しいマスターを作る。
func (ma *MergeAnalysis) Apply(out string, choices map[string]int) ([]string, error) {
	for _, c := range ma.conflict {
		k, ok := choices[c.Key]
		if !ok || k < 0 || k >= len(c.values) {
			return nil, fmt.Errorf("未選択の競合があります: %s", c.Title)
		}
		switch v := c.values[k].(type) {
		case planVal:
			var id int64
			fmt.Sscanf(strings.TrimPrefix(c.Key, "p:"), "%d", &id)
			ma.plan[id] = v
			ma.planEd[id] = strings.Join(c.Options[k].Editors, ",")
		case vnodeVal:
			id := strings.TrimPrefix(c.Key, "v:")
			ma.vn[id] = v
			ma.vnEd[id] = strings.Join(c.Options[k].Editors, ",")
		}
	}
	var warns []string
	// 整合性: 移動先・親が削除された仮想フォルダは復活させる
	revive := func(id string) {
		for i := 0; id != "" && id != VRoot && i < 64; i++ {
			v, ok := ma.vn[id]
			if !ok {
				return
			}
			if v.Deleted {
				v.Deleted = false
				ma.vn[id] = v
				warns = append(warns, "移動先として使われているため、削除された仮想フォルダ「"+v.Name+"」を残しました")
			}
			id = v.Parent
		}
	}
	for _, v := range ma.plan {
		if v.Action == ActMove {
			if _, ok := ma.vn[v.VParent]; !ok && v.VParent != VRoot {
				continue
			}
			revive(v.VParent)
		}
	}
	for id, v := range ma.vn {
		if !v.Deleted {
			revive(v.Parent)
		}
		_ = id
	}

	final := out
	tmpl := ma.srcs[0]
	if ma.MasterPath != "" {
		final = ma.MasterPath
		out = ma.MasterPath + ".merging"
		tmpl = ma.srcs[len(ma.srcs)-1] // マスター
	} else if abs, _ := filepath.Abs(out); func() bool {
		for _, s := range ma.srcs {
			if strings.EqualFold(mustAbs(s.Path), abs) {
				return true
			}
		}
		return false
	}() {
		return nil, fmt.Errorf("保存先に統合元のファイルは指定できません")
	}
	os.Remove(out)
	if _, err := tmpl.db.Exec(`VACUUM INTO ?`, out); err != nil {
		return nil, err
	}
	st, err := Open(out)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	tx, err := st.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM plan`, `DELETE FROM vnodes`, `DELETE FROM tags`,
		`DROP TABLE IF EXISTS plan_base`, `DROP TABLE IF EXISTS vnodes_base`, `DROP TABLE IF EXISTS tags_base`} {
		if _, err := tx.Exec(q); err != nil {
			return nil, err
		}
	}
	now := nowStr()
	for id, v := range ma.plan {
		if v == (planVal{}) {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO plan VALUES(?,?,?,?,?,?,?,?)`, id, v.Action, v.VParent, v.NewName, v.Due, v.Memo, ma.planEd[id], now); err != nil {
			return nil, err
		}
	}
	for id, v := range ma.vn {
		if _, err := tx.Exec(`INSERT INTO vnodes VALUES(?,?,?,?,?,?,?)`, id, v.Parent, v.Name, v.Memo, ma.vnEd[id], now, v.Deleted); err != nil {
			return nil, err
		}
	}
	for id, ts := range ma.tags {
		for t := range ts {
			if _, err := tx.Exec(`INSERT INTO tags VALUES(?,?)`, id, t); err != nil {
				return nil, err
			}
		}
	}
	if err := rebuildCover(tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var codes []string
	for _, s := range ma.srcs {
		if !s.master {
			codes = append(codes, s.Code)
		}
	}
	st.DB.Exec(`DELETE FROM meta WHERE key IN ('editor_code','copied_at','master_path')`)
	if err := st.SetMeta(map[string]string{"master_rev": newUUID(), "merged_from": strings.Join(codes, ", "), "merged_at": now}); err != nil {
		return nil, err
	}
	st.invalidate()
	if sim, err := st.SimilarWarnings(); err == nil && len(sim) > 0 {
		warns = append(warns, fmt.Sprintf("同じ階層に似た名前の仮想フォルダが %d 組あります(整理画面で ⚠ を確認してください)", len(sim)))
	}
	if final == out {
		return warns, nil
	}
	// マスターを置き換える(元のマスターはバックアップとして残す)
	st.Close()
	ma.Close()
	ma.srcs = nil
	ext := filepath.Ext(final)
	backup := strings.TrimSuffix(final, ext) + "_backup_" + time.Now().Format("20060102_150405") + ext
	if err := os.Rename(final, backup); err != nil {
		os.Remove(out)
		return nil, fmt.Errorf("マスターを更新できません(誰かが開いている可能性があります): %v", err)
	}
	if err := os.Rename(out, final); err != nil {
		os.Rename(backup, final)
		return nil, fmt.Errorf("マスターを更新できません: %v", err)
	}
	return append(warns, "マスターを更新しました。元のマスターは "+filepath.Base(backup)+" として残しています"), nil
}

// ImportPlans は別のDBの仮想フォルダ・アクション・タグを、フルパスが一致する項目へ引き継ぐ(ツリー統合用)。
func (s *Store) ImportPlans(otherDB string) (int64, error) {
	if _, err := s.DB.Exec(`CREATE INDEX IF NOT EXISTS nodes_path ON nodes(path)`); err != nil {
		return 0, err
	}
	conn, err := s.DB.Conn(ctxBG)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctxBG, `ATTACH DATABASE ? AS other`, otherDB); err != nil {
		return 0, err
	}
	defer conn.ExecContext(ctxBG, `DETACH DATABASE other`)
	var has int
	conn.QueryRowContext(ctxBG, `SELECT count(*) FROM other.sqlite_master WHERE name='plan'`).Scan(&has)
	if has == 0 {
		return 0, nil
	}
	if _, err := conn.ExecContext(ctxBG, `INSERT OR REPLACE INTO main.vnodes SELECT * FROM other.vnodes`); err != nil {
		return 0, err
	}
	r, err := conn.ExecContext(ctxBG, `INSERT OR REPLACE INTO main.plan
		SELECT n.id, o.action, o.vparent, o.new_name, o.due, o.memo, o.editor, o.updated_at
		FROM other.plan o JOIN other.nodes onn ON onn.id = o.node_id JOIN main.nodes n ON n.path = onn.path`)
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctxBG, `INSERT OR IGNORE INTO main.tags
		SELECT n.id, o.tag FROM other.tags o JOIN other.nodes onn ON onn.id = o.node_id JOIN main.nodes n ON n.path = onn.path`); err != nil {
		return 0, err
	}
	if err := rebuildCover(conn2db{conn}); err != nil {
		return 0, err
	}
	s.invalidate()
	return r.RowsAffected()
}

type conn2db struct{ c *sql.Conn }

func (c conn2db) Exec(q string, a ...any) (sql.Result, error) { return c.c.ExecContext(ctxBG, q, a...) }
func (c conn2db) Query(q string, a ...any) (*sql.Rows, error) {
	return c.c.QueryContext(ctxBG, q, a...)
}
