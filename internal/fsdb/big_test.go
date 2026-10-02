package fsdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBigPlan は実運用規模(約77万項目・多数のアクション)での応答速度を確認する。FM_BIG=1 のときだけ実行。
//
//	FM_BIG=1 go test ./internal/fsdb -run TestBigPlan -v
func TestBigPlan(t *testing.T) {
	if os.Getenv("FM_BIG") == "" {
		t.Skip("FM_BIG=1 で実行")
	}
	db := filepath.Join(t.TempDir(), "big.db")
	b := NewBuilder(`\\fileserver\share\全社共有`, `\`)
	var mid []int32
	for a := 0; a < 40; a++ {
		pa := b.Add(Record{Parent: 0, IsDir: true, Name: fmt.Sprintf("部署%02d_営業企画関連", a)})
		for c := 0; c < 50; c++ {
			pb := b.Add(Record{Parent: pa, IsDir: true, Name: fmt.Sprintf("%d年度_プロジェクト資料_%03d", 2000+c%25, c)})
			mid = append(mid, pb)
			for f := 0; f < 385; f++ {
				b.Add(Record{Parent: pb, Name: fmt.Sprintf("議事録_第%03d回.xlsx", f), Size: int64(1000 + f), Mtime: 1600000000})
			}
		}
	}
	t0 := time.Now()
	if err := b.Finalize(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("build %v", time.Since(t0))
	s, _ := Open(db)
	defer s.Close()
	// 1000フォルダを移動、各フォルダ内の一部ファイルを個別に削除
	var ids []int64
	rows, _ := s.DB.Query(`SELECT id FROM nodes WHERE depth=2 ORDER BY id LIMIT 1000`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	v, _, _ := s.VCreate(VRoot, "整理先", "x")
	r := s.Rules()
	r.MaxItems = 100000
	s.SaveRules(r)
	t0 = time.Now()
	rep, err := s.PlanMove(ids, v, "x")
	t.Logf("move %d folders: %v (applied %d, err %v)", len(ids), time.Since(t0), rep.Applied, err)
	var del []int64
	for _, id := range ids[:500] {
		del = append(del, id+1, id+2)
	}
	t0 = time.Now()
	s.PlanDelete(del, "x")
	t.Logf("delete %d files: %v", len(del), time.Since(t0))
	s.PlanOwner(ids[:300], "担当A", "x")
	s.PlanHold(ids[600:700], "x")
	r = s.Rules()
	r.ApplyCurrent = true
	t0 = time.Now()
	if err := s.SaveRules(r); err != nil {
		t.Fatal(err)
	}
	t.Logf("rule hits (apply to current tree): %v (%d hits)", time.Since(t0), s.RuleHitCount())
	for name, fn := range map[string]func() error{
		"RuleViolations": func() error { _, err := s.RuleViolations(); return err },
		"Filter owner": func() error {
			tf, _ := s.TreeFilterFor("担当A", "")
			_, err := s.Children(1, false, false, tf)
			return err
		},
		"Filter rule": func() error {
			tf, _ := s.TreeFilterFor("", "rule")
			_, err := s.Children(1, false, false, tf)
			return err
		},
		"Search owner":     func() error { _, _, err := s.Search(Filter{Owner: "担当A"}, 0, 500); return err },
		"Search rule5s":    func() error { _, _, err := s.Search(Filter{Check: "rule5s"}, 0, 500); return err },
		"Node(root)":       func() error { _, err := s.Node(1); return err },
		"Children(root)":   func() error { _, err := s.Children(1, false, false, nil); return err },
		"Children(dept)":   func() error { _, err := s.Children(2, false, false, nil); return err },
		"Search unhandled": func() error { _, _, err := s.Search(Filter{State: "unhandled"}, 0, 500); return err },
		"Search handled":   func() error { _, _, err := s.Search(Filter{State: "handled"}, 0, 500); return err },
		"Progress":         func() error { _, err := s.Progress(); return err },
		"VChildren(整理先)":   func() error { _, err := s.VChildren(v); return err },
		"Summary":          func() error { _, err := s.Summary(); return err },
		"Summary(2nd)":     func() error { _, err := s.Summary(); return err },
	} {
		t0 := time.Now()
		if err := fn(); err != nil {
			t.Fatal(name, err)
		}
		t.Logf("%-18s %v", name, time.Since(t0))
	}
}
