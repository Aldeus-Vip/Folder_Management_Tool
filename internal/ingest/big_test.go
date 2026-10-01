package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// TestBigImport は実運用規模(約80万行)の取込性能を確認する。FM_BIG=1 のときだけ実行。
//
//	FM_BIG=1 go test ./internal/ingest -run TestBigImport -v
func TestBigImport(t *testing.T) {
	if os.Getenv("FM_BIG") == "" {
		t.Skip("FM_BIG=1 で実行")
	}
	dir := t.TempDir()
	x := filepath.Join(dir, "big.xlsx")
	if out := os.Getenv("FM_BIG_OUT"); out != "" { // 生成したxlsxを残す(手動確認用)
		x = out
	}
	R := `\\fileserver\share\全社共有`
	rows := [][]string{header, {"全社共有", "", "", "フォルダ", "2024/05/01 10:00", "0", "0", "", "", R}}
	for a := 0; a < 40; a++ {
		pa := fmt.Sprintf(`%s\部署%02d_営業企画関連`, R, a)
		rows = append(rows, []string{"", "x", "", "フォルダ", "2023/01/01 00:00", "0", "10", "", "", pa})
		for b := 0; b < 50; b++ {
			pb := fmt.Sprintf(`%s\%d年度_プロジェクト資料_%03d`, pa, 2000+b%25, b)
			rows = append(rows, []string{"", "", "x", "フォルダ", "2023/01/01 00:00", "0", "20", "", "", pb})
			for c := 0; c < 385; c++ {
				rows = append(rows, []string{"", "", "x", "ファイル", fmt.Sprintf("20%02d/%02d/01 12:00", 10+c%15, 1+c%12),
					fmt.Sprintf("%d.5", c*13%9000), "60", "", "", fmt.Sprintf(`%s\議事録_打合せ資料_第%03d回 - コピー.xlsx`, pb, c%300)})
			}
		}
	}
	writeXlsx(t, x, rows, false)
	st, _ := os.Stat(x)
	t.Logf("rows=%d xlsx=%.0fMB", len(rows), float64(st.Size())/1e6)
	rows = nil
	runtime.GC()
	start := time.Now()
	res, err := ImportExcel(context.Background(), x, filepath.Join(dir, "big.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("import: items=%d %.1fs (sys mem %.0fMB)", res.Items, time.Since(start).Seconds(), float64(m.Sys)/1e6)
	s, _ := fsdb.Open(filepath.Join(dir, "big.db"))
	defer s.Close()
	for _, f := range []fsdb.Filter{{Q: "第123回"}, {Check: "old"}, {Check: "dup"}, {Kind: "file", Sort: "size", Desc: true}} {
		t0 := time.Now()
		_, n, err := s.Search(f, 0, 200)
		t.Logf("search %+v: %d rows %v %v", f, n, time.Since(t0), err)
	}
	for _, q := range []string{`SELECT max(depth) FROM nodes`,
		`SELECT count(*) FROM (SELECT ` + "1" + ` FROM nodes n WHERE n.is_dir=1 AND n.depth BETWEEN 1 AND 2 ORDER BY n.size DESC LIMIT 15)`,
		`SELECT count(*) FROM (SELECT ext, count(*), sum(size) FROM nodes WHERE is_dir=0 GROUP BY ext)`,
		`SELECT count(*) FROM (SELECT depth, count(*) FROM nodes GROUP BY depth)`,
		`SELECT count(*) FROM (SELECT name_lc, size, count(*) c FROM nodes WHERE is_dir=0 AND size>0 AND flags & 512 != 0 GROUP BY name_lc, size HAVING c>1)`} {
		t0 := time.Now()
		var x any
		s.DB.QueryRow(q).Scan(&x)
		t.Logf("%v  %.70s", time.Since(t0), q)
	}
	t0 := time.Now()
	s.Summary()
	t.Logf("summary %v", time.Since(t0))
	t0 = time.Now()
	_, n, _, _ := s.Dups(0, 0, 50)
	t.Logf("dups %d groups %v", n, time.Since(t0))
}
