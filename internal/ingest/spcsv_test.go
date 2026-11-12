package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

func TestImportSPCSV(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "SPTree_20260102_0304.csv")
	// BOM付き・列の順番違い・途中のフォルダ欠け(営業/2024)・同じ行の重複・空パス
	body := "\uFEFFSize,isfolder,FullPath,Modified\n" +
		",True,/sites/T/Shared Documents/部門,2025-01-01T00:00:00Z\n" +
		",True,/sites/T/Shared Documents/部門/営業,2025-02-01T00:00:00Z\n" +
		"100,False,/sites/T/Shared Documents/部門/営業/見積.xlsx,2025-03-01T00:00:00Z\n" +
		"200,False,/sites/T/Shared Documents/部門/営業/2024/請求書.pdf,2025-04-01T00:00:00Z\n" +
		"200,False,/sites/T/Shared Documents/部門/営業/2024/請求書.pdf,2025-04-01T00:00:00Z\n" +
		"5,False,\"/sites/T/Shared Documents/部門/総務/a,b.txt\",\n" +
		",False,,\n"
	os.WriteFile(csvPath, []byte(body), 0o644)
	db := filepath.Join(dir, "sp.db")
	res, err := ImportSPCSV(context.Background(), csvPath, db, "https://example.sharepoint.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 7 || len(res.Warnings) != 4 { // ルート・補完2件(2024, 総務)・空パス・重複
		t.Errorf("res=%+v", res)
	}
	s, err := fsdb.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := s.Meta()
	const root = "https://example.sharepoint.com/sites/T/Shared Documents/部門"
	if m["root"] != root || m["sep"] != "/" || m["source"] != "spcsv" || m["web_root"] != root {
		t.Errorf("meta=%v", m)
	}
	want := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC).Local().Format("2006/01/02 15:04:05")
	if m["scanned_at"] != want {
		t.Errorf("scanned_at=%s want %s", m["scanned_at"], want)
	}
	r, _ := s.Node(1)
	if r.Size != 305 || r.Files != 3 || r.Dirs != 3 || r.Mtime != time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("root=%+v", r)
	}
	id, err := s.FindPath(root + "/営業/2024/請求書.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Node(id); n.Size != 200 || n.Ext != "pdf" || n.Depth != 3 {
		t.Errorf("file=%+v", n)
	}
	if _, err := s.FindPath(root + "/総務/a,b.txt"); err != nil {
		t.Error("引用符で囲んだカンマ入りのパス")
	}
}

func TestImportSPCSVErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.csv")
	os.WriteFile(p, []byte("Name,Kind\na,b\n"), 0o644)
	if _, err := ImportSPCSV(context.Background(), p, filepath.Join(dir, "x.db"), "", nil); err == nil {
		t.Error("必須の列が無い")
	}
	// 1件だけ(ファイル)→ その親がルート。ライブラリ相対パス・FileRef/FSObjType の列名
	os.WriteFile(p, []byte("FileRef,FSObjType,File_x0020_Size\nShared Documents/A/b.txt,0,9\n"), 0o644)
	db := filepath.Join(dir, "y.db")
	res, err := ImportSPCSV(context.Background(), p, db, "", nil)
	if err != nil || res.Items != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	s, _ := fsdb.Open(db)
	defer s.Close()
	if m, _ := s.Meta(); m["root"] != "Shared Documents/A" || m["web_root"] != "" {
		t.Errorf("meta=%v", m)
	}
}

func TestSPPrefix(t *testing.T) {
	addr := "https://example.sharepoint.com/sites/P/Top/Shared%20Documents/Forms/AllItems.aspx?id=%2Fsites%2FP%2FTop%2FShared%20Documents%2F%E9%83%A8%E9%96%80&viewid=x"
	lib := []string{"Shared Documents", "部門"}
	for _, c := range []struct {
		raw    string
		lead   bool
		common []string
		want   string
	}{
		{addr, true, []string{"sites", "P"}, "https://example.sharepoint.com"},                       // /sites/… のパス → テナントだけ
		{"https://example.sharepoint.com/sites/P/Top/", true, nil, "https://example.sharepoint.com"}, // サイトまで入れても重ならない
		{addr, false, lib, "https://example.sharepoint.com/sites/P/Top"},                             // ライブラリからのパス → サイトまで
		{"https://example.sharepoint.com/sites/P/Top", false, lib, "https://example.sharepoint.com/sites/P/Top"},
		{"https://example.sharepoint.com/sites/P/Top/SitePages/Home.aspx", false, lib, "https://example.sharepoint.com/sites/P/Top"},
		{"", true, nil, ""},
	} {
		if got := spPrefix(c.raw, c.lead, c.common); got != c.want {
			t.Errorf("%s (%v): %s want %s", c.raw, c.lead, got, c.want)
		}
	}
}
