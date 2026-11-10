package fsdb

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// aclDB は権限を取得したDB。経理は継承を無効化、経理\2023 は直接の付与あり、総務\古い は不明なアカウントへの付与。
func aclDB(t *testing.T) (*Store, map[string]int64) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "acl.db")
	b := NewBuilder(`\\srv\share`, `\`)
	inh := func() *ACL {
		return &ACL{ACEs: []ACE{{SID: "S-1-5-32-544", Principal: `BUILTIN\Administrators`, Rights: 0x1F01FF, Inherited: true, Flags: 0x3}}}
	}
	b.Recs[0].ACL = &ACL{ACEs: []ACE{{SID: "S-1-5-32-544", Principal: `BUILTIN\Administrators`, Rights: 0x1F01FF, Flags: 0x3}}}
	dir := func(p int32, n string, a *ACL) int32 { return b.Add(Record{Parent: p, IsDir: true, Name: n, ACL: a}) }
	file := func(p int32, n string) { b.Add(Record{Parent: p, Name: n, Size: 10}) }
	k := dir(0, "経理", &ACL{Protected: true, ACEs: []ACE{
		{SID: "S-1-5-21-1-1001", Principal: `CORP\経理G`, Rights: 0x1301BF, Flags: 0x3},
		{Deny: true, SID: "S-1-5-21-1-1002", Principal: `CORP\派遣`, Rights: 0x1F01FF, Flags: 0x3},
	}})
	y := dir(k, "2023", &ACL{ACEs: append(inh().ACEs, ACE{SID: "S-1-5-21-1-1003", Principal: `CORP\監査`, Rights: 0x1200A9, Flags: 0x3})})
	file(y, "a.pdf")
	dir(k, "2024", inh())
	g := dir(0, "総務", inh())
	file(dir(g, "古い", &ACL{ACEs: []ACE{{SID: "S-1-5-21-1-9999", Rights: 0x120089}}}), "b.txt")
	dir(g, "新しい", inh())
	if err := b.Finalize(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ids := map[string]int64{}
	rows, _ := s.DB.Query(`SELECT id, path FROM nodes`)
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		ids[strings.TrimPrefix(p, `\\srv\share\`)] = id
	}
	rows.Close()
	return s, ids
}

func TestACLNodeAndView(t *testing.T) {
	s, ids := aclDB(t)
	if !s.ACLScanned() {
		t.Fatal("ACLScanned")
	}
	// 同じ内容の ACL は1行にまとめる(継承のみの3フォルダ → 1行)
	var defs int
	s.DB.QueryRow(`SELECT count(*) FROM acl_defs`).Scan(&defs)
	if defs != 5 {
		t.Errorf("acl_defs=%d want 5", defs)
	}
	k := node(t, s, ids["経理"])
	if !k.Unique() || k.Perm&PermProtected == 0 || k.PermACEs != 2 || k.PermSub != 2 {
		t.Errorf("経理: perm=%d n=%d sub=%d", k.Perm, k.PermACEs, k.PermSub)
	}
	if n := node(t, s, ids[`経理\2024`]); n.Unique() || n.Perm != PermScanned {
		t.Errorf("2024: perm=%d", n.Perm)
	}
	if n := node(t, s, ids[`総務\古い`]); n.Perm&PermOrphan == 0 || !n.Unique() {
		t.Errorf("古い: perm=%d", n.Perm)
	}
	if n := node(t, s, ids[`総務\古い\b.txt`]); n.Perm != 0 {
		t.Errorf("ファイルは未取得: perm=%d", n.Perm)
	}
	// ルートは数えない
	if r := node(t, s, 1); r.PermSub != 3 {
		t.Errorf("root sub=%d want 3", r.PermSub)
	}
	v, _ := s.NodeACL(ids["経理"])
	if !v.Scanned || !v.Protected || len(v.ACEs) != 2 || !v.ACEs[1].Deny || v.ACEs[0].RightsLabel != "変更" || v.ACEs[0].Principal != `CORP\経理G` {
		t.Errorf("view=%+v", v)
	}
	if v.ACEs[0].ScopeLabel != "このフォルダー、サブフォルダー、ファイル" {
		t.Errorf("scope=%s", v.ACEs[0].ScopeLabel)
	}
	if v, _ := s.NodeACL(ids[`総務\古い\b.txt`]); v.Scanned {
		t.Error("未取得のファイル")
	}
}

func TestACLChecksAndEstimate(t *testing.T) {
	s, ids := aclDB(t)
	sm, err := s.Summary()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, c := range sm.Checks {
		got[c.Key] = c.Count
	}
	if got["aclunique"] != 3 || got["aclorphan"] != 1 {
		t.Errorf("checks=%v", got)
	}
	ns, _, err := s.Search(Filter{Check: "aclunique"}, 0, 100)
	if err != nil || len(ns) != 3 {
		t.Fatalf("search=%d %v", len(ns), err)
	}
	e, _ := s.EstimateItems()
	if !e.Perm.Scanned || e.Perm.Current != 3 || e.Perm.Total != 3 || e.Perm.Limit != DefaultPermLimit {
		t.Errorf("perm=%+v", e.Perm)
	}
	// 削除した分は整理後から除く
	s.PlanDelete([]int64{ids[`総務\古い`]}, "x")
	e, _ = s.EstimateItems()
	if e.Perm.Current != 3 || e.Perm.Total != 2 {
		t.Errorf("after delete perm=%+v", e.Perm)
	}
	s.SetPermLimit(1)
	e, _ = s.EstimateItems()
	if e.Perm.Over != 1 {
		t.Errorf("over=%d", e.Perm.Over)
	}
}

func TestACLNotScanned(t *testing.T) {
	s, _ := testDB(t)
	if s.ACLScanned() {
		t.Error("ACLScanned")
	}
	e, _ := s.EstimateItems()
	if e.Perm.Scanned || e.Perm.Current != 0 {
		t.Errorf("perm=%+v", e.Perm)
	}
	if n := node(t, s, 1); n.Perm != 0 || n.PermSub != 0 {
		t.Errorf("node perm=%d", n.Perm)
	}
}

func TestRightsLabel(t *testing.T) {
	for m, want := range map[uint32]string{0x1F01FF: "フルコントロール", 0x10000000: "フルコントロール", 0x1301BF: "変更",
		0x1200A9: "読み取りと実行", 0x120089: "読み取り", 0xA0000000: "読み取りと実行", 0x100116: "書き込み", 0x40: "特殊なアクセス許可(0x40)"} {
		if got := RightsLabel(m); got != want {
			t.Errorf("%X: %s want %s", m, got, want)
		}
	}
}
