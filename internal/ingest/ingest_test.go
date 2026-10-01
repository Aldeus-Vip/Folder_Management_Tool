package ingest

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// writeXlsx はテスト用xlsxを作る。shared=true なら共有文字列+ふりがな(rPh)付き(Excelで保存し直した形式)。
func writeXlsx(t testing.TB, path string, rows [][]string, shared bool) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	put := func(name, body string) {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	put("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="サマリー" sheetId="1" r:id="rId1"/><sheet name="FolderTree" sheetId="2" r:id="rId2"/></sheets></workbook>`)
	put("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="x" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="x" Target="/xl/worksheets/sheet2.xml"/></Relationships>`)
	put("xl/worksheets/sheet1.xml", `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>対象</t></is></c></row></sheetData></worksheet>`)
	var sb, ss strings.Builder
	sst := map[string]int{}
	var sstList []string
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for ri, r := range rows {
		fmt.Fprintf(&sb, `<row r="%d" outlineLevel="1">`, ri+1)
		for ci, v := range r {
			if v == "" {
				continue
			}
			ref := fmt.Sprintf("%s%d", colName(ci), ri+1)
			if _, err := fmt.Sscanf(v, "%f", new(float64)); err == nil && !strings.Contains(v, "/") && !strings.ContainsAny(v, `\ `) {
				fmt.Fprintf(&sb, `<c r="%s" s="4"><v>%s</v></c>`, ref, v)
			} else if shared {
				i, ok := sst[v]
				if !ok {
					i = len(sstList)
					sst[v] = i
					sstList = append(sstList, v)
				}
				fmt.Fprintf(&sb, `<c r="%s" t="s"><v>%d</v></c>`, ref, i)
			} else {
				fmt.Fprintf(&sb, `<c r="%s" t="inlineStr" s="1"><is><t xml:space="preserve">%s</t></is></c>`, ref, xmlEsc(v))
			}
		}
		sb.WriteString("</row>\n")
	}
	sb.WriteString(`</sheetData></worksheet>`)
	put("xl/worksheets/sheet2.xml", sb.String())
	if shared {
		ss.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
		for _, s := range sstList {
			fmt.Fprintf(&ss, `<si><r><t>%s</t></r><rPh sb="0" eb="1"><t>フリガナ</t></rPh></si>`, xmlEsc(s))
		}
		ss.WriteString(`</sst>`)
		put("xl/sharedStrings.xml", ss.String())
	}
	zw.Close()
}

var header = []string{"階層0", "階層1", "階層2", "種別", "更新日時", "サイズ(KB)", "パス文字数", "警告", "所属パス(検索・フィルタ用)", "フルパス"}

func sampleRows() [][]string {
	R := `\\srv\share\部署A`
	return [][]string{
		header,
		{"部署A", "", "", "フォルダ", "2024/05/01 10:00", "6.0", "0", "", "", R},
		{"", "┳━ 2024年度", "", "フォルダ", "2024/05/01 10:00", "5.0", "6", "", "部署A", R + `\2024年度`},
		{"", "┃", "┣━ 見積 - コピー.xlsx", "ファイル", "2024/05/01 10:00", "2.0", "20", "", "部署A > 2024年度", R + `\2024年度\見積 - コピー.xlsx`},
		{"", "┃", "┣━ a&b #1.txt", "ファイル", "2019/01/02 03:04", "1.0", "16", "禁止文字", "部署A > 2024年度", R + `\2024年度\a&b #1.txt`},
		{"", "┃", "┗━ ~$見積.xlsx", "ファイル", "2024/05/01 10:00", "2.0", "14", "", "部署A > 2024年度", R + `\2024年度\~$見積.xlsx`},
		{"", "┣━ 空", "", "フォルダ", "2024/05/01 10:00", "0.0", "1", "空フォルダ", "部署A", R + `\空`},
		{"", "┣━ 鍵", "", "フォルダ", "2024/05/01 10:00", "0.0", "1", "アクセス不可", "部署A", R + `\鍵`},
		// 親フォルダ行がない(フィルタ後に保存された等)ファイル → 中間フォルダを補完
		{"", "┗━ x", "", "ファイル", "2024/05/01 10:00", "2.0", "5", "", "部署A > 他", R + `\他\x\見積 - コピー.xlsx`},
	}
}

func TestImportExcel(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint("shared=", shared), func(t *testing.T) {
			dir := t.TempDir()
			x := filepath.Join(dir, "in.xlsx")
			db := filepath.Join(dir, "out.db")
			writeXlsx(t, x, sampleRows(), shared)
			res, err := ImportExcel(context.Background(), x, db, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Items != 10 { // 8行 + 補完フォルダ2つ(他, x)
				t.Fatalf("items=%d", res.Items)
			}
			s, err := fsdb.Open(db)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			root, _ := s.Node(1)
			if root.Path != `\\srv\share\部署A` || root.Files != 4 || root.Dirs != 5 || root.Size != 7*1024 {
				t.Fatalf("root=%+v", root)
			}
			kids, _ := s.Children(1, false)
			var names []string
			for _, k := range kids {
				names = append(names, k.Name)
			}
			if strings.Join(names, ",") != "2024年度,他,空,鍵" {
				t.Fatalf("children=%v", names)
			}
			chk := func(check string, want int64) {
				t.Helper()
				_, n, err := s.Search(fsdb.Filter{Check: check}, 0, 10)
				if err != nil || n != want {
					t.Errorf("check %s: got %d (%v), want %d", check, n, err, want)
				}
			}
			chk("badchar", 1)
			chk("temp", 1)
			chk("copyname", 2)
			chk("dup", 2)
			chk("empty", 1)
			chk("access", 1)
			chk("old", 1)
			chk("singlechild", 1) // 「他」の中身はフォルダ x だけ
			ns, _, _ := s.Search(fsdb.Filter{Q: "a&b"}, 0, 10)
			if len(ns) != 1 || ns[0].Path != `\\srv\share\部署A\2024年度\a&b #1.txt` || ns[0].Size != 1024 {
				t.Fatalf("search=%+v", ns)
			}
			// サブツリー範囲・祖先
			anc, _ := s.Ancestors(ns[0].ID)
			if len(anc) != 2 || anc[1].Name != "2024年度" {
				t.Fatalf("ancestors=%+v", anc)
			}
			if _, err := s.Summary(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNotesAndPlan(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "in.xlsx")
	db := filepath.Join(dir, "out.db")
	writeXlsx(t, x, sampleRows(), false)
	if _, err := ImportExcel(context.Background(), x, db, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := fsdb.Open(db)
	defer s.Close()
	del, arc, memo := "削除", "アーカイブ", "確認済み"
	// 一時ファイルを検索条件で一括「削除」
	n, err := s.SetNotesByFilter(fsdb.Filter{Check: "temp"}, fsdb.NoteFields{Action: &del})
	if err != nil || n != 1 {
		t.Fatalf("bulk: %d %v", n, err)
	}
	// フォルダ「2024年度」をアーカイブ → 配下の削除指定はスクリプトでスキップされる
	id, _ := s.FindPath(`\\srv\share\部署A\2024年度`)
	if _, err := s.SetNotes([]int64{id}, fsdb.NoteFields{Action: &arc, Memo: &memo}); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := s.WritePlanScript(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "親フォルダの操作に含まれるためスキップ") || !strings.Contains(out, "Invoke-Step 'アーカイブ'") {
		t.Fatalf("plan:\n%s", out)
	}
	var csv strings.Builder
	if err := s.WriteCSV(&csv, fsdb.Filter{Action: "any"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(csv.String(), "\r\n") != 3 {
		t.Fatalf("csv:\n%s", csv.String())
	}
	// 空にすると注記は消える
	empty := ""
	s.SetNotes([]int64{id}, fsdb.NoteFields{Action: &empty, Memo: &empty})
	_, cnt, _ := s.Search(fsdb.Filter{Action: "any"}, 0, 10)
	if cnt != 1 {
		t.Fatalf("notes left=%d", cnt)
	}
	// 別DBへの引継ぎ
	db2 := filepath.Join(dir, "out2.db")
	ImportExcel(context.Background(), x, db2, nil)
	s2, _ := fsdb.Open(db2)
	defer s2.Close()
	if n, err := s2.ImportNotes(db); err != nil || n != 1 {
		t.Fatalf("import notes: %d %v", n, err)
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
