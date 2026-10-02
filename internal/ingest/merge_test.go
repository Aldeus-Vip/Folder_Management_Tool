package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

func TestMergeScans(t *testing.T) {
	base := t.TempDir()
	mk := func(p, body string) {
		full := filepath.Join(base, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	mk("share/A/a1.txt", "aaaa")
	mk("share/A/sub/a2.txt", "aa")
	mk("share/B/b1.txt", "bbbbbb")
	dir := t.TempDir()
	dbA, dbB, dbAll, out := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db"), filepath.Join(dir, "all.db"), filepath.Join(dir, "m.db")
	if _, err := Scan(context.Background(), filepath.Join(base, "share/A"), dbA, 2, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(context.Background(), filepath.Join(base, "share/B"), dbB, 2, "", nil); err != nil {
		t.Fatal(err)
	}
	// 後から A を再スキャンした想定: a1.txt のサイズが変わり、新しいファイルが増えた
	time.Sleep(1100 * time.Millisecond)
	mk("share/A/a1.txt", "aaaaaaaaaa")
	mk("share/A/new.txt", "n")
	if _, err := Scan(context.Background(), filepath.Join(base, "share"), dbAll, 2, "", nil); err != nil {
		t.Fatal(err)
	}
	// 旧A のDBに注記を付けておく → 統合後に引き継がれる
	sa, _ := fsdb.Open(dbA)
	id, _ := sa.FindPath(filepath.Join(base, "share/A/sub"))
	sa.PlanDelete([]int64{id}, "A")
	sa.Close()

	res, err := Merge(context.Background(), []string{dbA, dbB, dbAll}, out, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(res.Warnings)
	s, _ := fsdb.Open(out)
	defer s.Close()
	root, _ := s.Node(1)
	if root.Path != filepath.Join(base, "share") {
		t.Fatalf("root=%s", root.Path)
	}
	// share, A, sub, B / a1, a2, b1, new
	if root.Files != 4 || root.Dirs != 3 {
		t.Fatalf("files=%d dirs=%d", root.Files, root.Dirs)
	}
	a1, _ := s.FindPath(filepath.Join(base, "share/A/a1.txt"))
	n, _ := s.Node(a1)
	if n.Size != 10 { // 新しいDB(all.db)の値が優先される
		t.Fatalf("a1 size=%d", n.Size)
	}
	sub, _ := s.FindPath(filepath.Join(base, "share/A/sub"))
	if n, _ := s.Node(sub); n.Action != fsdb.ActDelete {
		t.Fatalf("note not carried: %+v", n)
	}
	// 指定順優先なら古い a.db の値になる
	out2 := filepath.Join(dir, "m2.db")
	if _, err := Merge(context.Background(), []string{dbA, dbAll}, out2, false, nil); err != nil {
		t.Fatal(err)
	}
	s2, _ := fsdb.Open(out2)
	defer s2.Close()
	id2, _ := s2.FindPath(filepath.Join(base, "share/A/a1.txt"))
	if n, _ := s2.Node(id2); n.Size != 4 {
		t.Fatalf("order priority: size=%d", n.Size)
	}
}

func TestMergeVirtualRoot(t *testing.T) {
	dir := t.TempDir()
	mkx := func(name, root string) string {
		db := filepath.Join(dir, name+".db")
		buildDB(t, db, root, `資料\a.txt`)
		return db
	}
	d1 := mkx("s1", `\\srv1\share\部署A`)
	d2 := mkx("s2", `\\srv2\data`)
	d3 := mkx("s3", `\\srv1\share\部署A\資料\深い`) // d1 の配下 → d1 のツリーに入る
	out := filepath.Join(dir, "m.db")
	if _, err := Merge(context.Background(), []string{d1, d2, d3}, out, false, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := fsdb.Open(out)
	defer s.Close()
	kids, _ := s.Children(1, false, false, nil)
	var names []string
	for _, k := range kids {
		names = append(names, k.Path)
	}
	if strings.Join(names, ",") != `\\srv1\share\部署A,\\srv2\data` {
		t.Fatalf("tops=%v", names)
	}
	if kids[0].Flags&fsdb.FlagBadChar != 0 || kids[0].PathLen != 0 { // フルパス名でも禁止文字扱いしない
		t.Fatalf("top flags=%d pl=%d", kids[0].Flags, kids[0].PathLen)
	}
	id, err := s.FindPath(`\\srv1\share\部署A\資料\深い\資料\a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := s.Node(id)
	if n.Depth != 5 || n.PathLen != len([]rune(`資料\深い\資料\a.txt`)) {
		t.Fatalf("node=%+v", n)
	}
	if r, _ := s.Node(1); r.Files != 3 {
		t.Fatalf("files=%d", r.Files)
	}
}

func TestCommonRoot(t *testing.T) {
	for _, c := range []struct {
		roots []string
		sep   string
		want  string
		virt  bool
	}{
		{[]string{`\\srv\share\A`, `\\srv\share\B`}, `\`, `\\srv\share`, false},
		{[]string{`\\srv\share\A`, `\\srv\other`}, `\`, virtualRootName, true},
		{[]string{`C:\x\a`, `C:\y`}, `\`, `C:\`, false},
		{[]string{`C:\x`, `D:\x`}, `\`, virtualRootName, true},
		{[]string{`/usr/a`, `/usr/b`}, `/`, `/usr`, false},
		{[]string{`/usr/a`, `/opt`}, `/`, `/`, false},
	} {
		got, v := commonRoot(c.roots, c.sep)
		if got != c.want || v != c.virt {
			t.Errorf("%v: got %q %v", c.roots, got, v)
		}
	}
}
