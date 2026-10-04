package fsdb

import (
	"fmt"
	"sort"
)

// ---- ツリーの絞り込み(担当 / 保留 / 5Sルール外れ) ----
// 対象の範囲を「互いに素なID区間の列」で表す。ノードIDは行きがけ順なので、
// ノード X を表示するかどうかは「X の区間 [id, end] が対象の区間と重なるか」で判定できる
// (重なる = X 自身が対象か、配下に対象がある)。

// TreeFilter は絞り込みの結果。nil は絞り込みなし。
type TreeFilter struct {
	segs [][2]int64
}

// Empty は該当する項目が無いかどうか。
func (tf *TreeFilter) Empty() bool { return tf != nil && len(tf.segs) == 0 }

func (tf *TreeFilter) find(id int64) int {
	return sort.Search(len(tf.segs), func(i int) bool { return tf.segs[i][1] >= id })
}

// state は 1 = 対象、2 = 対象への経路(配下に対象がある)、0 = 対象外。
func (tf *TreeFilter) state(id, end int64) int {
	i := tf.find(id)
	if i >= len(tf.segs) || tf.segs[i][0] > end {
		return 0
	}
	if tf.segs[i][0] <= id {
		return 1
	}
	return 2
}

func (tf *TreeFilter) apply(ns []Node) []Node {
	if tf == nil {
		return ns
	}
	out := ns[:0]
	for _, x := range ns {
		if x.TF = tf.state(x.ID, x.End); x.TF != 0 {
			out = append(out, x)
		}
	}
	return out
}

func intersectSegs(a, b [][2]int64) [][2]int64 {
	out := [][2]int64{}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		lo, hi := max(a[i][0], b[j][0]), min(a[i][1], b[j][1])
		if lo <= hi {
			out = append(out, [2]int64{lo, hi})
		}
		if a[i][1] < b[j][1] {
			i++
		} else {
			j++
		}
	}
	return out
}

// segsCond は区間の列をSQLの条件にする(OR を平衡な木にして式の深さの上限を避ける)。
func segsCond(segs [][2]int64) (string, []any) {
	if len(segs) == 0 {
		return "0", nil
	}
	var args []any
	var build func(ss [][2]int64) string
	build = func(ss [][2]int64) string {
		if len(ss) == 1 {
			args = append(args, ss[0][0], ss[0][1])
			return "n.id BETWEEN ? AND ?"
		}
		m := len(ss) / 2
		l := build(ss[:m])
		return "(" + l + " OR " + build(ss[m:]) + ")"
	}
	return build(segs), args
}

// TreeFilterFor は絞り込みを作る(結果はアクション・担当・ルールを変えるまでキャッシュ)。
//
//	owner: 担当(親フォルダからの引き継ぎを含む)。"段:値"(例 "1:業務推進課")。"-" = 担当が決まっていない項目。"" = 指定なし
//	mode : "hold"(保留。親フォルダの設定を含む)/ "rule"(5Sルールに合わない項目)/ "unhandled"(未処理)/ ""
func (s *Store) TreeFilterFor(owner, mode string) (*TreeFilter, error) {
	if owner == "" && mode == "" {
		return nil, nil
	}
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	key := owner + "\x00" + mode
	s.mu.Lock()
	if tf := s.tf[key]; tf != nil {
		s.mu.Unlock()
		return tf, nil
	}
	maxID := int64(len(s.pre.files) - 1)
	var segs [][2]int64
	all := [][2]int64{{1, maxID}}
	if owner != "" {
		match, err := s.oidx.matcher(owner)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		segs = s.oidx.segments(match, maxID)
	} else {
		segs = all
	}
	pi := s.pidx
	switch mode {
	case "":
	case "hold":
		segs = intersectSegs(segs, pi.segments(func(k int32) bool { return k >= 0 && pi.acts[k] == ActHold }, maxID))
	case "unhandled":
		segs = intersectSegs(segs, pi.segments(func(k int32) bool { return k < 0 }, maxID))
	case "rule":
		s.mu.Unlock()
		pts, err := s.ruleHitSegs()
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		segs = intersectSegs(segs, pts)
	default:
		s.mu.Unlock()
		return nil, fmt.Errorf("不明な絞り込み: %s", mode)
	}
	tf := &TreeFilter{segs: segs}
	if s.tf == nil {
		s.tf = map[string]*TreeFilter{}
	}
	s.tf[key] = tf
	s.mu.Unlock()
	return tf, nil
}

func (s *Store) ruleHitSegs() ([][2]int64, error) {
	rows, err := s.DB.Query(`SELECT node_id FROM rule_hits ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][2]int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		if n := len(out); n > 0 && out[n-1][1]+1 == id {
			out[n-1][1] = id
		} else {
			out = append(out, [2]int64{id, id})
		}
	}
	return out, rows.Err()
}
