package fsdb

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---- 担当(部 / 課 / 担当 / 担当者 の4段) ----
// フォルダに設定すると配下に引き継がれる。段ごとに引き継ぐので、空欄の段は親フォルダの値になる
// (例: 上位フォルダに「部」、その中のフォルダに「課」だけを設定 → 部・課の両方が決まる)。
// DBには4段を \x1f で区切った1つの文字列で保存する(統合・競合の単位は4段まとめて)。

var OwnerLevels = []string{"部", "課", "担当", "担当者"}

const ownerSep = "\x1f"

type OwnerFields [4]string

// splitOwner は保存形式を4段に分ける(区切りの無い旧形式は「担当」の段とみなす)。
func splitOwner(s string) OwnerFields {
	var f OwnerFields
	if s == "" {
		return f
	}
	parts := strings.Split(s, ownerSep)
	if len(parts) == 1 {
		f[2] = s
		return f
	}
	for i := 0; i < len(f) && i < len(parts); i++ {
		f[i] = strings.TrimSpace(parts[i])
	}
	return f
}

func (f OwnerFields) empty() bool { return f == OwnerFields{} }

func (f OwnerFields) join() string {
	if f.empty() {
		return ""
	}
	return strings.Join(f[:], ownerSep)
}

// inherit は空欄の段を親の値で補う。
func (f OwnerFields) inherit(parent OwnerFields) OwnerFields {
	for i := range f {
		if f[i] == "" {
			f[i] = parent[i]
		}
	}
	return f
}

// Label は表示用(例:「部: 整備業務部 / 課: 業務推進課」)。
func (f OwnerFields) Label() string {
	var out []string
	for i, v := range f {
		if v != "" {
			out = append(out, OwnerLevels[i]+": "+v)
		}
	}
	return strings.Join(out, " / ")
}

// matcher は絞り込みの指定("段:値" / "-")を、区間の判定にする。
func (oi *ownerIndex) matcher(spec string) (func(k int32) bool, error) {
	if spec == "-" {
		return func(k int32) bool { return k < 0 || oi.eff[k].empty() }, nil
	}
	lv, val, ok := strings.Cut(spec, ":")
	l, err := strconv.Atoi(lv)
	if !ok || err != nil || l < 0 || l >= len(OwnerLevels) {
		return nil, fmt.Errorf("担当の指定が不正です: %s", spec)
	}
	return func(k int32) bool { return k >= 0 && oi.eff[k][l] == val }, nil
}

// PlanOwner は担当(4段)を設定する。すべて空欄なら解除(親フォルダの担当に戻る)。
func (s *Store) PlanOwner(ids []int64, fields []string, editor string) (*Report, error) {
	var f OwnerFields
	for i := 0; i < len(f) && i < len(fields); i++ {
		f[i] = strings.TrimSpace(strings.ReplaceAll(fields[i], ownerSep, ""))
	}
	owner := f.join()
	rep := &Report{}
	err := s.writePlans(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`INSERT INTO plan(node_id, owner, editor, updated_at) VALUES(?,?,?,?)
				ON CONFLICT(node_id) DO UPDATE SET owner=excluded.owner, editor=excluded.editor, updated_at=excluded.updated_at`, id, owner, editor, nowStr()); err != nil {
				return err
			}
			rep.Applied++
		}
		return nil
	})
	return rep, err
}

// OwnerCount は担当の段ごとの値と、その担当範囲のファイル数。
type OwnerCount struct {
	Level int    `json:"level"`
	Value string `json:"value"`
	Files int64  `json:"files"`
}

// Owners は担当の一覧(段ごと、ファイル数の多い順)。
func (s *Store) Owners() ([]OwnerCount, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oi := s.oidx
	maxID := int64(len(s.pre.files) - 1)
	seen := map[OwnerCount]bool{}
	out := []OwnerCount{}
	for _, f := range oi.eff {
		for l, v := range f {
			if v == "" || seen[OwnerCount{Level: l, Value: v}] {
				continue
			}
			seen[OwnerCount{Level: l, Value: v}] = true
			c := OwnerCount{Level: l, Value: v}
			match, _ := oi.matcher(strconv.Itoa(l) + ":" + v)
			for _, sg := range oi.segments(match, maxID) {
				n, _ := s.pre.count(sg[0], sg[1])
				c.Files += n
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level < out[j].Level
		}
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}
