package fsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Rules は整理後のフォルダ構成(仮想ツリー)に課す5Sのルール。
// Mode: "off"(無効) | "warn"(警告のみ) | "block"(その操作を禁止)
type Rules struct {
	RootName  string `json:"rootName"`  // 仮想ツリーのルートの表示名
	BasePath  string `json:"basePath"`  // 整理後のルートの実パス(例: \\srv\share\新)
	MaxDepth  int    `json:"maxDepth"`  // 最大階層(仮想ルート=0)
	DepthMode string `json:"depthMode"` //
	PathLimit int    `json:"pathLimit"` // 整理後のフルパス文字数の上限
	PathMode  string `json:"pathMode"`
	MaxItems  int    `json:"maxItems"` // 1フォルダ直下の項目数の上限
	ItemsMode string `json:"itemsMode"`
	BadName   string `json:"badName"`     // 禁止文字・末尾の.や空白・予約語
	CopyName  string `json:"copyName"`    // コピー的/版管理的な名前(「- コピー」「(1)」「旧」「v2」等)
	TagNeed   string `json:"tagRequired"` // タグが1つも無い項目の移動
}

var DefaultRules = Rules{RootName: "整理後", MaxDepth: 6, DepthMode: "block", PathLimit: 250, PathMode: "block",
	MaxItems: 100, ItemsMode: "warn", BadName: "block", CopyName: "warn", TagNeed: "off"}

func (s *Store) Rules() Rules {
	r := DefaultRules
	var js string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='vrules'`).Scan(&js)
	if js != "" {
		json.Unmarshal([]byte(js), &r)
	}
	if r.RootName == "" {
		r.RootName = DefaultRules.RootName
	}
	return r
}

func (s *Store) SaveRules(r Rules) error {
	if r.MaxDepth < 1 || r.PathLimit < 10 || r.MaxItems < 1 {
		return fmt.Errorf("ルールの数値が不正です")
	}
	for _, m := range []string{r.DepthMode, r.PathMode, r.ItemsMode, r.BadName, r.CopyName, r.TagNeed} {
		if m != "off" && m != "warn" && m != "block" {
			return fmt.Errorf("ルールの種別が不正です: %s", m)
		}
	}
	b, _ := json.Marshal(r)
	if _, err := s.DB.Exec(`INSERT OR REPLACE INTO meta VALUES('vrules',?)`, string(b)); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// Issue は5Sルールのチェック結果(1項目分)。
type Issue struct {
	ID     int64    `json:"id,omitempty"`
	Name   string   `json:"name"`
	Blocks []string `json:"blocks,omitempty"` // 禁止(操作は行われない)
	Warns  []string `json:"warns,omitempty"`  // 警告(操作は行われる)
}

// Report は操作の結果。
type Report struct {
	Applied int     `json:"applied"`
	Issues  []Issue `json:"issues"`
}

func (r *Report) add(is Issue) {
	if len(is.Blocks) > 0 || len(is.Warns) > 0 {
		r.Issues = append(r.Issues, is)
	}
}

func apply(is *Issue, mode, msg string) {
	switch mode {
	case "block":
		is.Blocks = append(is.Blocks, msg)
	case "warn":
		is.Warns = append(is.Warns, msg)
	}
}

func nameRuleIssues(is *Issue, r Rules, name string, isDir bool) {
	if strings.TrimSpace(name) == "" {
		is.Blocks = append(is.Blocks, "名前が空です")
		return
	}
	f := NameFlags(name, isDir)
	if f&(FlagBadChar|FlagTrailing|FlagReserved) != 0 {
		apply(is, r.BadName, "名前に使えない文字・末尾の「.」や空白・予約語が含まれています(名前変更が必要)")
	}
	if f&(FlagCopyName|FlagVersionName) != 0 {
		apply(is, r.CopyName, "「コピー」「(1)」「旧」「v2」など、コピーや版管理を表す名前です")
	}
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// checkPlace は項目 x を仮想フォルダ target に名前 name で置けるか判定する。ロック中に呼ぶ。
func (s *Store) checkPlace(r Rules, x *Node, target *vnode, name string, adding int) Issue {
	is := Issue{ID: x.ID, Name: x.Name}
	nameRuleIssues(&is, r, name, x.IsDir)
	relDepth, tail := 0, 0
	if x.IsDir && x.End > x.ID {
		var md, mp sql.NullInt64
		s.DB.QueryRow(`SELECT max(depth), max(path_len) FROM nodes WHERE id>? AND id<=?`, x.ID, x.End).Scan(&md, &mp)
		relDepth, tail = int(md.Int64)-x.Depth, int(mp.Int64)-x.PathLen
		if relDepth < 0 {
			relDepth = 0
		}
		if tail < 0 {
			tail = 0
		}
	}
	if d := target.Depth + 1 + relDepth; d > r.MaxDepth {
		apply(&is, r.DepthMode, fmt.Sprintf("整理後の階層が %d になり、上限(%d)を超えます", d, r.MaxDepth))
	}
	if l := runeLen(s.vt.fullPath(target.UUID)) + 1 + runeLen(name) + tail; l > r.PathLimit {
		apply(&is, r.PathMode, fmt.Sprintf("整理後のパスが最長 %d 文字になり、上限(%d)を超えます", l, r.PathLimit))
	}
	if len(x.Tags) == 0 {
		apply(&is, r.TagNeed, "タグが設定されていません(用途・種類を明確にしてください)")
	}
	if adding > 0 {
		if n := s.vStats()[target.UUID]; n != nil && n.direct+adding > r.MaxItems {
			apply(&is, r.ItemsMode, fmt.Sprintf("移動先の直下が %d 項目になり、上限(%d)を超えます", n.direct+adding, r.MaxItems))
		}
	}
	return is
}

// ---- アクションの設定 ----

func (s *Store) writePlans(fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plan WHERE action='' AND memo='' AND due='' AND new_name=''`); err != nil {
		return err
	}
	if err := rebuildCover(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

const upsertPlan = `INSERT INTO plan(node_id, action, vparent, new_name, editor, updated_at) VALUES(?,?,?,?,?,?)
	ON CONFLICT(node_id) DO UPDATE SET action=excluded.action, vparent=excluded.vparent, new_name=excluded.new_name,
	editor=excluded.editor, updated_at=excluded.updated_at`

// PlanDelete は削除を設定する(フォルダは配下ごと)。
func (s *Store) PlanDelete(ids []int64, editor string) (*Report, error) {
	rep := &Report{}
	err := s.writePlans(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(upsertPlan, id, ActDelete, "", "", editor, nowStr()); err != nil {
				return err
			}
			rep.Applied++
		}
		return nil
	})
	return rep, err
}

// PlanClear はアクションを解除する(メモ・期限は残す)。
func (s *Store) PlanClear(ids []int64, editor string) (*Report, error) {
	rep := &Report{}
	err := s.writePlans(func(tx *sql.Tx) error {
		for _, id := range ids {
			r, err := tx.Exec(`UPDATE plan SET action='', vparent='', new_name='', editor=?, updated_at=? WHERE node_id=?`, editor, nowStr(), id)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			rep.Applied += int(n)
		}
		return nil
	})
	return rep, err
}

// PlanMove は仮想フォルダ target へ移動を設定する。5Sルールで禁止される項目は設定しない。
func (s *Store) PlanMove(ids []int64, target, editor string) (*Report, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	nodes, err := s.nodesByID(ids)
	if err != nil {
		return nil, err
	}
	r := s.Rules()
	rep := &Report{}
	var ok []*Node
	s.mu.Lock()
	tv := s.vt.nodes[target]
	if tv == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("移動先の仮想フォルダが見つかりません")
	}
	adding := 0
	for i := range nodes {
		if !(nodes[i].Action == ActMove && nodes[i].VParent == target) {
			adding++
		}
	}
	for i := range nodes {
		x := &nodes[i]
		name := x.Name
		if x.NewName != "" {
			name = x.NewName
		}
		is := s.checkPlace(r, x, tv, name, adding)
		rep.add(is)
		if len(is.Blocks) == 0 {
			ok = append(ok, x)
		}
	}
	s.mu.Unlock()
	err = s.writePlans(func(tx *sql.Tx) error {
		for _, x := range ok {
			nn := ""
			if x.Action == ActMove {
				nn = x.NewName // 移動先を変えるだけなら名前変更は維持
			}
			if _, err := tx.Exec(upsertPlan, x.ID, ActMove, target, nn, editor, nowStr()); err != nil {
				return err
			}
			rep.Applied++
		}
		return nil
	})
	return rep, err
}

// PlanFields は期限・メモ・名前変更の更新(nil は変更しない)。名前変更は移動を設定した1項目のみ。
type PlanFields struct {
	Due     *string `json:"due"`
	Memo    *string `json:"memo"`
	NewName *string `json:"newName"`
}

func (s *Store) PlanSetFields(ids []int64, pf PlanFields, editor string) (*Report, error) {
	rep := &Report{}
	if pf.NewName != nil {
		if len(ids) != 1 {
			return nil, fmt.Errorf("名前変更は1項目ずつ設定してください")
		}
		if err := s.ensureIndex(); err != nil {
			return nil, err
		}
		ns, err := s.nodesByID(ids)
		if err != nil || len(ns) == 0 {
			return nil, fmt.Errorf("項目が見つかりません")
		}
		x := &ns[0]
		nn := strings.TrimSpace(*pf.NewName)
		if nn != "" {
			if x.Action != ActMove {
				return nil, fmt.Errorf("名前変更は「移動」を設定した項目にだけ設定できます")
			}
			s.mu.Lock()
			is := s.checkPlace(s.Rules(), x, s.vt.nodes[x.VParent], nn, 0)
			s.mu.Unlock()
			rep.add(is)
			if len(is.Blocks) > 0 {
				return rep, nil
			}
		}
		pf.NewName = &nn
	}
	err := s.writePlans(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO plan(node_id, editor, updated_at) VALUES(?,?,?)`, id, editor, nowStr()); err != nil {
				return err
			}
			for col, v := range map[string]*string{"due": pf.Due, "memo": pf.Memo, "new_name": pf.NewName} {
				if v == nil {
					continue
				}
				if _, err := tx.Exec(`UPDATE plan SET `+col+`=?, editor=?, updated_at=? WHERE node_id=?`, strings.TrimSpace(*v), editor, nowStr(), id); err != nil {
					return err
				}
			}
			rep.Applied++
		}
		return nil
	})
	return rep, err
}

func (s *Store) nodesByID(ids []int64) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []Node
	for i := 0; i < len(ids); i += 500 {
		j := min(i+500, len(ids))
		ph := strings.TrimSuffix(strings.Repeat("?,", j-i), ",")
		args := make([]any, j-i)
		for k, id := range ids[i:j] {
			args[k] = id
		}
		ns, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE n.id IN (`+ph+`) ORDER BY n.id`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return out, nil
}

// ---- タグ ----

func (s *Store) SetTags(ids []int64, add, remove []string) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		for _, t := range add {
			if t = strings.TrimSpace(t); t != "" {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO tags VALUES(?,?)`, id, t); err != nil {
					return 0, err
				}
			}
		}
		for _, t := range remove {
			if _, err := tx.Exec(`DELETE FROM tags WHERE node_id=? AND tag=?`, id, t); err != nil {
				return 0, err
			}
		}
	}
	return len(ids), tx.Commit()
}

// AllTags はタグの一覧(件数の多い順)。
func (s *Store) AllTags() ([]Count, error) {
	return s.counts(`SELECT tag, tag, count(*), 0 FROM tags GROUP BY tag ORDER BY count(*) DESC, tag`)
}

// ---- 仮想フォルダの編集 ----

func (s *Store) vfolderIssue(r Rules, parent *vnode, name, except string) Issue {
	is := Issue{Name: name}
	nameRuleIssues(&is, r, name, true)
	if parent.Depth+1 > r.MaxDepth {
		apply(&is, r.DepthMode, fmt.Sprintf("階層 %d になり、上限(%d)を超えます", parent.Depth+1, r.MaxDepth))
	}
	if l := runeLen(s.vt.fullPath(parent.UUID)) + 1 + runeLen(name); l > r.PathLimit {
		apply(&is, r.PathMode, fmt.Sprintf("パスが %d 文字になり、上限(%d)を超えます", l, r.PathLimit))
	}
	for _, k := range parent.kids {
		if k.UUID != except && (strings.EqualFold(k.Name, name) || normName(k.Name) == normName(name)) {
			is.Blocks = append(is.Blocks, "同じ名前(全角/半角などの表記ゆれを含む)のフォルダ「"+k.Name+"」がすでにあります")
			return is
		}
	}
	if sim := similarAmong(name, siblingNames(s, parent, except)); len(sim) > 0 {
		is.Warns = append(is.Warns, "同じ階層に似た名前のフォルダがあります: "+strings.Join(sim, ", "))
	}
	return is
}

func (s *Store) VCreate(parent, name, editor string) (string, *Issue, error) {
	if err := s.ensureIndex(); err != nil {
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	s.mu.Lock()
	p := s.vt.nodes[parent]
	if p == nil {
		s.mu.Unlock()
		return "", nil, fmt.Errorf("親の仮想フォルダが見つかりません")
	}
	is := s.vfolderIssue(s.Rules(), p, name, "")
	if st := s.vStats()[p.UUID]; st != nil && st.direct+1 > s.Rules().MaxItems {
		apply(&is, s.Rules().ItemsMode, fmt.Sprintf("直下が %d 項目になり、上限(%d)を超えます", st.direct+1, s.Rules().MaxItems))
	}
	s.mu.Unlock()
	if len(is.Blocks) > 0 {
		return "", &is, nil
	}
	id := newUUID()
	if _, err := s.DB.Exec(`INSERT INTO vnodes(uuid, parent, name, editor, updated_at) VALUES(?,?,?,?,?)`, id, parent, name, editor, nowStr()); err != nil {
		return "", nil, err
	}
	s.invalidate()
	return id, &is, nil
}

func (s *Store) VRename(uuid, name, editor string) (*Issue, error) {
	if uuid == VRoot {
		r := s.Rules()
		r.RootName = strings.TrimSpace(name)
		return &Issue{}, s.SaveRules(r)
	}
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	s.mu.Lock()
	n := s.vt.nodes[uuid]
	if n == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("仮想フォルダが見つかりません")
	}
	is := s.vfolderIssue(s.Rules(), s.vt.nodes[n.Parent], name, uuid)
	s.mu.Unlock()
	if len(is.Blocks) > 0 {
		return &is, nil
	}
	if _, err := s.DB.Exec(`UPDATE vnodes SET name=?, editor=?, updated_at=? WHERE uuid=?`, name, editor, nowStr(), uuid); err != nil {
		return nil, err
	}
	s.invalidate()
	return &is, nil
}

func (s *Store) VSetMemo(uuid, memo, editor string) error {
	_, err := s.DB.Exec(`UPDATE vnodes SET memo=?, editor=?, updated_at=? WHERE uuid=?`, strings.TrimSpace(memo), editor, nowStr(), uuid)
	s.invalidate()
	return err
}

// VMove は仮想フォルダを別の仮想フォルダの下へ移す。
func (s *Store) VMove(uuid, parent, editor string) (*Issue, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	n, p := s.vt.nodes[uuid], s.vt.nodes[parent]
	if n == nil || p == nil || uuid == VRoot {
		s.mu.Unlock()
		return nil, fmt.Errorf("仮想フォルダが見つかりません")
	}
	for a := p; a != nil; a = s.vt.nodes[a.Parent] {
		if a.UUID == uuid {
			s.mu.Unlock()
			return nil, fmt.Errorf("自分自身の配下へは移動できません")
		}
		if a.UUID == VRoot {
			break
		}
	}
	r := s.Rules()
	is := s.vfolderIssue(r, p, n.Name, uuid)
	// 配下の仮想フォルダの最大の深さ
	var maxRel func(*vnode) int
	maxRel = func(v *vnode) int {
		m := 0
		for _, k := range v.kids {
			if d := 1 + maxRel(k); d > m {
				m = d
			}
		}
		return m
	}
	if d := p.Depth + 1 + maxRel(n); d > r.MaxDepth {
		apply(&is, r.DepthMode, fmt.Sprintf("配下の仮想フォルダが階層 %d になり、上限(%d)を超えます", d, r.MaxDepth))
	}
	s.mu.Unlock()
	if len(is.Blocks) > 0 {
		return &is, nil
	}
	if _, err := s.DB.Exec(`UPDATE vnodes SET parent=?, editor=?, updated_at=? WHERE uuid=?`, parent, editor, nowStr(), uuid); err != nil {
		return nil, err
	}
	s.invalidate()
	return &is, nil
}

// VDelete は仮想フォルダ(と配下)を削除する。そこへ移動を設定していた項目は「未処理」に戻る。
func (s *Store) VDelete(uuid, editor string) (int, error) {
	if uuid == VRoot {
		return 0, fmt.Errorf("ルートは削除できません")
	}
	ids := s.vSubtreeIDs(uuid)
	if len(ids) == 0 {
		return 0, fmt.Errorf("仮想フォルダが見つかりません")
	}
	cleared := 0
	err := s.writePlans(func(tx *sql.Tx) error {
		for _, id := range ids {
			r, err := tx.Exec(`UPDATE plan SET action='', vparent='', new_name='', editor=?, updated_at=? WHERE action='move' AND vparent=?`, editor, nowStr(), id)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			cleared += int(n)
			if _, err := tx.Exec(`UPDATE vnodes SET deleted=1, editor=?, updated_at=? WHERE uuid=?`, editor, nowStr(), id); err != nil {
				return err
			}
		}
		return nil
	})
	return cleared, err
}

// VPlacedCount は仮想フォルダ(配下含む)へ移動を設定した項目数。
func (s *Store) VPlacedCount(uuid string) int {
	ids := s.vSubtreeIDs(uuid)
	n := 0
	for _, id := range ids {
		var c int
		s.DB.QueryRow(`SELECT count(*) FROM plan WHERE action='move' AND vparent=?`, id).Scan(&c)
		n += c
	}
	return n
}
