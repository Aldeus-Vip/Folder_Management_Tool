package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// 権限の読み取りを差し替えて、スキャン(フォルダだけ / ファイルも)と統合での引き継ぎを確かめる。
func TestScanACL(t *testing.T) {
	old := aclReader
	defer func() { aclReader = old }()
	aclReader = func(p string) (*fsdb.ACL, error) {
		inh := !strings.HasSuffix(p, "secret")
		return &fsdb.ACL{Protected: !inh, ACEs: []fsdb.ACE{{SID: "S-1-5-21-1", Principal: `CORP\` + filepath.Base(p), Rights: 0x1200A9, Inherited: inh}}}, nil
	}
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "share/A/secret"), 0o755)
	os.WriteFile(filepath.Join(base, "share/A/secret/x.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(base, "share/B"), 0o755)
	os.WriteFile(filepath.Join(base, "share/B/y.txt"), []byte("y"), 0o644)
	dir := t.TempDir()
	count := func(db string) (all, uniq int) {
		s, err := fsdb.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.DB.QueryRow(`SELECT count(*) FROM acls`).Scan(&all)
		s.DB.QueryRow(`SELECT count(*) FROM acls WHERE protected=1 OR explicit>0`).Scan(&uniq)
		return
	}
	dbA, dbB, out := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db"), filepath.Join(dir, "m.db")
	if _, err := Scan(context.Background(), filepath.Join(base, "share/A"), dbA, 2, "", "dirs", nil); err != nil {
		t.Fatal(err)
	}
	if a, u := count(dbA); a != 2 || u != 1 { // A と secret(ファイルは取得しない)
		t.Errorf("dirs: acls=%d uniq=%d", a, u)
	}
	if _, err := Scan(context.Background(), filepath.Join(base, "share/B"), dbB, 2, "", "all", nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := count(dbB); a != 2 { // B と y.txt
		t.Errorf("all: acls=%d", a)
	}
	if _, err := Merge(context.Background(), []string{dbA, dbB}, out, false, nil); err != nil {
		t.Fatal(err)
	}
	if a, u := count(out); a != 4 || u != 1 {
		t.Errorf("merge: acls=%d uniq=%d", a, u)
	}
	s, _ := fsdb.Open(out)
	defer s.Close()
	id, _ := s.FindPath(filepath.Join(base, "share/A/secret"))
	v, _ := s.NodeACL(id)
	if !v.Unique || len(v.ACEs) != 1 || v.ACEs[0].Principal != `CORP\secret` {
		t.Errorf("view=%+v", v)
	}
	if _, err := Scan(context.Background(), base, filepath.Join(dir, "x.db"), 2, "", "bad", nil); err == nil {
		t.Error("不明な取得方法")
	}
}
