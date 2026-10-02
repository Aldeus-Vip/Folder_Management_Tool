package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// buildDB はテスト用DBを作る。entries は root からの相対パス(\ 区切り、末尾 \ はフォルダ)。ファイルは1KB。
func buildDB(t testing.TB, db, root string, entries ...string) {
	t.Helper()
	b := fsdb.NewBuilder(root, `\`)
	dirs := map[string]int32{"": 0}
	var ensure func(rel string) int32
	ensure = func(rel string) int32 {
		if id, ok := dirs[rel]; ok {
			return id
		}
		parent, name := "", rel
		if i := strings.LastIndex(rel, `\`); i >= 0 {
			parent, name = rel[:i], rel[i+1:]
		}
		id := b.Add(fsdb.Record{Parent: ensure(parent), IsDir: true, Name: name, Mtime: 1700000000})
		dirs[rel] = id
		return id
	}
	for _, e := range entries {
		if strings.HasSuffix(e, `\`) {
			ensure(strings.TrimSuffix(e, `\`))
			continue
		}
		parent, name := "", e
		if i := strings.LastIndex(e, `\`); i >= 0 {
			parent, name = e[:i], e[i+1:]
		}
		b.Add(fsdb.Record{Parent: ensure(parent), Name: name, Size: 1024, Mtime: 1700000000})
	}
	if err := b.Finalize(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	mk := func(p, body string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	mk("a/b/c.txt", "hello")
	mk("a/b/Thumbs.db", "x")
	mk("a/d (1).docx", "12345")
	mk("z/c.txt", "hello")
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	db := filepath.Join(t.TempDir(), "s.db")
	res, err := Scan(context.Background(), root, db, 4, func(string, int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 9 {
		t.Fatalf("items=%d", res.Items)
	}
	s, _ := fsdb.Open(db)
	defer s.Close()
	r, _ := s.Node(1)
	if r.Files != 4 || r.Dirs != 4 || r.Size != 16 {
		t.Fatalf("root=%+v", r)
	}
	for k, want := range map[string]int64{"dup": 2, "temp": 1, "copyname": 1, "empty": 1, "singlechild": 0} {
		if _, n, _ := s.Search(fsdb.Filter{Check: k}, 0, 1); n != want {
			t.Errorf("%s=%d want %d", k, n, want)
		}
	}
}
