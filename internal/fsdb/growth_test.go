package fsdb

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func growthDB(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "g.db")
	b := NewBuilder(`\\srv\share`, `\`)
	b.Meta["scanned_at"] = "2025/06/01 10:00:00"
	y := func(year int) int64 { return time.Date(year, 6, 1, 0, 0, 0, 0, time.Local).Unix() }
	dir := func(p int32, n string) int32 { return b.Add(Record{Parent: p, IsDir: true, Name: n}) }
	file := func(p int32, n string, size int64, mt int64) { b.Add(Record{Parent: p, Name: n, Size: size, Mtime: mt}) }
	a := dir(0, "営業部")
	k1 := dir(a, "第一課")
	file(k1, "a.pdf", 100, y(2025))
	file(k1, "b.xlsx", 50, y(2024))
	lb := dir(k1, "返却")
	file(lb, "c.zip", 1000, y(2023))
	file(dir(lb, "2023"), "d.mp4", 2000, y(2023))
	g := dir(0, "総務部")
	file(g, "old.doc", 10, y(2010)) // 期間より前
	file(dir(g, "返却"), "e.jpg", 300, y(2025))
	file(g, "nodate.txt", 7, 0) // 不明
	file(0, "readme.txt", 1, y(2025))
	if err := b.Finalize(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rowBy(t *testing.T, r *GrowthReport, label string) GrowthRow {
	t.Helper()
	for _, x := range r.Rows {
		if x.Label == label {
			return x
		}
	}
	t.Fatalf("行がありません: %s (%v)", label, r.Rows)
	return GrowthRow{}
}

func TestGrowthReport(t *testing.T) {
	s := growthDB(t)
	r, err := s.GrowthReport("depth1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Base != 2025 || strings.Join(r.Cols, ",") != "2023年より前,2023,2024,2025,不明" {
		t.Fatalf("cols=%v base=%d", r.Cols, r.Base)
	}
	ei := rowBy(t, r, "営業部")
	if ei.Cells[1].Size != 3000 || ei.Cells[2].Size != 50 || ei.Cells[3].Size != 100 || ei.Total.Files != 4 {
		t.Errorf("営業部=%+v", ei)
	}
	so := rowBy(t, r, "総務部")
	if so.Cells[0].Files != 1 || so.Cells[4].Files != 1 || so.Cells[3].Size != 300 {
		t.Errorf("総務部=%+v", so)
	}
	if last := r.Rows[len(r.Rows)-1]; !strings.Contains(last.Label, "より上") || last.Total.Files != 1 {
		t.Errorf("直下のファイル=%+v", last)
	}
	if r.Total.Total.Files != 8 || r.Total.Total.Size != 3468 {
		t.Errorf("total=%+v", r.Total.Total)
	}
	r2, _ := s.GrowthReport("depth2", 3)
	if x := rowBy(t, r2, `営業部\第一課`); x.Total.Files != 4 {
		t.Errorf("depth2=%+v", x)
	}
}

func TestGrowthFoldersAndKind(t *testing.T) {
	s := growthDB(t)
	// フォルダ名だけ(2か所の合計)・相対パス・見つからないもの
	if err := s.SetGrowthSetting(5, []string{"返却\n営業部\\第一課", "無い"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GrowthReport("folders", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Years != 5 || len(r.Missing) != 1 {
		t.Errorf("years=%d missing=%v", r.Years, r.Missing)
	}
	ret := rowBy(t, r, "返却")
	if ret.Total.Files != 3 || ret.Total.Size != 3300 || !strings.Contains(ret.Note, "2 か所") {
		t.Errorf("返却=%+v", ret)
	}
	// 返却の外側の 第一課 には、返却を除いた分だけ入る(いちばん内側の指定フォルダ)
	if k := rowBy(t, r, `営業部\第一課`); k.Total.Files != 2 {
		t.Errorf("第一課=%+v", k)
	}
	if o := rowBy(t, r, "それ以外"); o.Total.Files != 3 {
		t.Errorf("それ以外=%+v", o)
	}
	rk, _ := s.GrowthReport("kind", 3)
	if rowBy(t, rk, "圧縮").Total.Size != 1000 || rowBy(t, rk, "動画・音声").Total.Size != 2000 || rowBy(t, rk, "Office").Total.Files != 2 || rowBy(t, rk, "その他").Total.Files != 2 {
		t.Errorf("kind=%+v", rk.Rows)
	}
	var buf bytes.Buffer
	if err := s.WriteGrowthCSV(&buf, "kind", 3); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\uFEFF※最終更新日") || !strings.Contains(out, "合計,ファイル数,") {
		t.Errorf("csv=%s", out)
	}
	if _, err := s.GrowthReport("bad", 3); err == nil {
		t.Error("不明な切り口")
	}
}
