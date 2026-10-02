package fsdb

import (
	"context"
	"strings"
	"time"
)

var ctxBG = context.Background()

type Count struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
	Count int64  `json:"count"`
	Size  int64  `json:"size"`
}

type Summary struct {
	Meta       map[string]string `json:"meta"`
	Settings   Settings          `json:"settings"`
	Root       *Node             `json:"root"`
	Folders    int64             `json:"folders"`
	Files      int64             `json:"files"`
	TotalSize  int64             `json:"totalSize"`
	MaxDepth   int64             `json:"maxDepth"`
	Checks     []Count           `json:"checks"`
	Age        []Count           `json:"age"`
	Depth      []Count           `json:"depth"`
	TopFolders []Node            `json:"topFolders"`
	TopExts    []Count           `json:"topExts"`
	Actions    []Count           `json:"actions"`
	Editors    []Count           `json:"editors"`
	Tags       []Count           `json:"tags"`
	Progress   *PlanProgress     `json:"progress"`
}

// Summary は全体の集計を返す。重い静的部分はキャッシュし、注記(アクション/担当)の集計だけ毎回取り直す。
func (s *Store) Summary() (*Summary, error) {
	st := s.Settings()
	s.mu.Lock()
	cached := s.summary
	s.mu.Unlock()
	if cached == nil || cached.Settings != st {
		var err error
		if cached, err = s.staticSummary(st); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.summary = cached
		s.mu.Unlock()
	}
	sm := *cached
	var err error
	if sm.Actions, err = s.counts(`SELECT p.action, p.action, count(*), 0 FROM plan p WHERE p.action!='' GROUP BY p.action ORDER BY count(*) DESC`); err != nil {
		return nil, err
	}
	if sm.Editors, err = s.counts(`SELECT p.editor, p.editor, count(*), 0 FROM plan p WHERE p.editor!='' GROUP BY p.editor ORDER BY count(*) DESC`); err != nil {
		return nil, err
	}
	if sm.Tags, err = s.AllTags(); err != nil {
		return nil, err
	}
	if sm.Progress, err = s.Progress(); err != nil {
		return nil, err
	}
	return &sm, nil
}

func (s *Store) staticSummary(st Settings) (*Summary, error) {
	sm := &Summary{Settings: st}
	var err error
	if sm.Meta, err = s.Meta(); err != nil {
		return nil, err
	}
	if sm.Root, err = s.Node(1); err != nil {
		return nil, err
	}
	sm.Folders, sm.Files, sm.TotalSize = sm.Root.Dirs, sm.Root.Files, sm.Root.Size
	if err := s.DB.QueryRow(`SELECT max(depth) FROM depth_stats`).Scan(&sm.MaxDepth); err != nil {
		return nil, err
	}
	// 全警告種別を1回の走査で数える
	sel := []string{}
	args := []any{}
	dst := []any{}
	sm.Checks = make([]Count, len(CheckDefs))
	for i, c := range CheckDefs {
		cond, a := checkCond(c.Key, sm.Settings)
		sm.Checks[i] = Count{Key: c.Key, Label: c.Label, Group: c.Group}
		sel = append(sel, "COALESCE(sum(CASE WHEN "+cond+" THEN 1 ELSE 0 END),0)",
			"COALESCE(sum(CASE WHEN n.is_dir=0 AND "+cond+" THEN n.size ELSE 0 END),0)")
		args = append(append(args, a...), a...)
		dst = append(dst, &sm.Checks[i].Count, &sm.Checks[i].Size)
	}
	if err := s.DB.QueryRow(`SELECT `+strings.Join(sel, ",")+` FROM nodes n WHERE n.id>1`, args...).Scan(dst...); err != nil {
		return nil, err
	}
	// 更新日時の分布(ファイルのみ)
	now := time.Now()
	y := func(n int) int64 { return now.AddDate(-n, 0, 0).Unix() }
	rows, err := s.DB.Query(`SELECT CASE WHEN mtime<=0 THEN 5 WHEN mtime>=? THEN 0 WHEN mtime>=? THEN 1 WHEN mtime>=? THEN 2 WHEN mtime>=? THEN 3 ELSE 4 END b,
		count(*), sum(size) FROM nodes WHERE is_dir=0 GROUP BY b ORDER BY b`, y(1), y(3), y(5), y(10))
	if err != nil {
		return nil, err
	}
	labels := []string{"1年以内", "1〜3年", "3〜5年", "5〜10年", "10年超", "不明"}
	for rows.Next() {
		var b int
		var c Count
		if err := rows.Scan(&b, &c.Count, &c.Size); err != nil {
			rows.Close()
			return nil, err
		}
		c.Label = labels[b]
		sm.Age = append(sm.Age, c)
	}
	rows.Close()
	if sm.Depth, err = s.counts(`SELECT CAST(depth AS TEXT), CAST(depth AS TEXT), cnt, size FROM depth_stats ORDER BY depth`); err != nil {
		return nil, err
	}
	if sm.TopFolders, err = s.query(`SELECT ` + nodeCols + nodeFrom + ` WHERE n.is_dir=1 AND n.depth BETWEEN 1 AND 2 ORDER BY n.size DESC LIMIT 15`); err != nil {
		return nil, err
	}
	if sm.TopExts, err = s.Exts(0, 10); err != nil {
		return nil, err
	}
	return sm, nil
}

func (s *Store) counts(q string, args ...any) ([]Count, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Count{}
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Key, &c.Label, &c.Count, &c.Size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Exts は拡張子別の件数・合計サイズ(サイズ降順)。under>0 ならその配下のみ。
func (s *Store) Exts(under int64, limit int) ([]Count, error) {
	if under <= 1 {
		return s.counts(`SELECT CASE WHEN ext='' THEN '(なし)' ELSE ext END, CASE WHEN ext='' THEN '(なし)' ELSE ext END, cnt, size FROM ext_stats ORDER BY size DESC LIMIT ?`, limit)
	}
	q := `SELECT CASE WHEN ext='' THEN '(なし)' ELSE ext END, CASE WHEN ext='' THEN '(なし)' ELSE ext END, count(*), sum(size) FROM nodes WHERE is_dir=0`
	args := []any{}
	{
		var end int64
		if err := s.DB.QueryRow(`SELECT end_id FROM nodes WHERE id=?`, under).Scan(&end); err != nil {
			return nil, err
		}
		q += ` AND id>? AND id<=?`
		args = append(args, under, end)
	}
	q += ` GROUP BY ext ORDER BY sum(size) DESC LIMIT ?`
	return s.counts(q, append(args, limit)...)
}

type DupGroup struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Count   int64  `json:"count"`
	Members []Node `json:"members"`
}

// Dups は同名・同サイズのファイルのグループを「削減できる容量」の大きい順に返す。
func (s *Store) Dups(under int64, offset, limit int) ([]DupGroup, int64, int64, error) {
	cond := "is_dir=0 AND size>0 AND flags & ? != 0"
	args := []any{FlagDup}
	if under > 1 {
		var end int64
		if err := s.DB.QueryRow(`SELECT end_id FROM nodes WHERE id=?`, under).Scan(&end); err != nil {
			return nil, 0, 0, err
		}
		cond += " AND id>? AND id<=?"
		args = append(args, under, end)
	}
	grp := `SELECT name_lc, size, count(*) cnt, (count(*)-1)*size wasted FROM nodes WHERE ` + cond + ` GROUP BY name_lc, size HAVING cnt>1`
	if under <= 1 {
		grp = `SELECT name_lc, size, cnt, wasted FROM dup_groups` // 構築時に集計済み
		args = nil
	}
	var total, wasted int64
	if err := s.DB.QueryRow(`SELECT count(*), COALESCE(sum(wasted),0) FROM (`+grp+`)`, args...).Scan(&total, &wasted); err != nil {
		return nil, 0, 0, err
	}
	rows, err := s.DB.Query(`SELECT name_lc, size, cnt FROM (`+grp+`) ORDER BY wasted DESC, name_lc LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, 0, err
	}
	groups := []DupGroup{}
	for rows.Next() {
		var g DupGroup
		if err := rows.Scan(&g.Name, &g.Size, &g.Count); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	for i := range groups {
		q := `SELECT ` + nodeCols + nodeFrom + ` WHERE n.name_lc=? AND n.size=? AND n.is_dir=0`
		a := []any{groups[i].Name, groups[i].Size}
		if under > 1 {
			q += ` AND n.id>? AND n.id<=?`
			a = append(a, args[1], args[2])
		}
		// 1グループの表示は最大200件(件数自体は Count で分かる)
		if groups[i].Members, err = s.query(q+` ORDER BY n.mtime DESC LIMIT 200`, a...); err != nil {
			return nil, 0, 0, err
		}
		if len(groups[i].Members) > 0 {
			groups[i].Name = groups[i].Members[0].Name
		}
	}
	return groups, total, wasted, nil
}
