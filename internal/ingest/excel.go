// Package ingest は Excel取込・フォルダスキャンの結果を fsdb.Builder に流し込む。
package ingest

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
	"github.com/aldeus-vip/folder_management_tool/internal/xlsx"
)

// ImportExcel は FolderTreeExporter が出力した Excel(FolderTreeシート)を読み込み、DBを作る。
// 行の並び順に依存せず「フルパス」列から親子関係を復元するため、Excel上で並べ替え・
// 保存し直したファイルでも取り込める。
func ImportExcel(ctx context.Context, xlsxPath, dbPath string, prog fsdb.Progress) (*Result, error) {
	if prog == nil {
		prog = func(string, int64, int64) {}
	}
	f, err := xlsx.Open(xlsxPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sheet := f.Sheets[0]
	found := false
	for _, s := range f.Sheets {
		if s.Name == "FolderTree" {
			sheet, found = s, true
		}
	}
	if !found && len(f.Sheets) > 1 {
		sheet = f.Sheets[1] // 既定のシート順: サマリー, FolderTree, ...
	}

	type row struct {
		path  string
		isDir bool
		size  int64
		mtime int64
		flags int
	}
	var rows []row
	col := map[string]int{}
	header := false
	total := sheet.Size
	err = f.Rows(sheet, func(n int64) { prog("Excelを読み込み中", n, total) }, func(r []string) bool {
		if ctx.Err() != nil {
			return false
		}
		if !header {
			for i, v := range r {
				v = strings.TrimSpace(v)
				switch {
				case v == "フルパス":
					col["path"] = i
				case v == "種別":
					col["kind"] = i
				case v == "更新日時":
					col["mtime"] = i
				case strings.HasPrefix(v, "サイズ"):
					col["size"] = i
				case v == "警告":
					col["warn"] = i
				}
			}
			if _, ok := col["path"]; ok {
				header = true
			}
			return true
		}
		get := func(k string) string {
			if i, ok := col[k]; ok && i < len(r) {
				return strings.TrimSpace(r[i])
			}
			return ""
		}
		p := get("path")
		if p == "" {
			return true
		}
		x := row{path: p, isDir: get("kind") == "フォルダ"}
		if s := strings.ReplaceAll(get("size"), ",", ""); s != "" {
			if kb, err := strconv.ParseFloat(s, 64); err == nil && !x.isDir {
				x.size = int64(math.Round(kb * 1024))
			}
		}
		x.mtime = parseTime(get("mtime"))
		if strings.Contains(get("warn"), "アクセス") {
			x.flags |= fsdb.FlagAccess
		}
		rows = append(rows, x)
		return true
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !header {
		return nil, fmt.Errorf("シート「%s」に「フルパス」列が見つかりません。FolderTreeExporter の出力ファイルを指定してください", sheet.Name)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("データ行がありません")
	}

	// ルート = 最も短いフォルダパス(通常は先頭行)
	rootIdx := 0
	for i, r := range rows {
		if r.isDir && len(r.path) < len(rows[rootIdx].path) {
			rootIdx = i
		}
	}
	root := strings.TrimRight(rows[rootIdx].path, `\/`)
	if strings.HasSuffix(rows[rootIdx].path, `:\`) {
		root = rows[rootIdx].path
	}
	sep := `\`
	if !strings.Contains(root, `\`) && strings.Contains(root, "/") {
		sep = "/"
	}

	prog("ツリーを復元中", 0, int64(len(rows)))
	b := fsdb.NewBuilder(root, sep)
	b.Recs[0].Mtime = rows[rootIdx].mtime
	b.Recs[0].Flags = rows[rootIdx].flags
	b.Recs = append(make([]fsdb.Record, 0, len(rows)+16), b.Recs...)
	idx := map[string]int32{strings.ToLower(root): 0}
	// 1回目: 全フォルダ・ファイルを登録(親は後で解決)
	recIdx := make([]int32, len(rows))
	for i, r := range rows {
		if i == rootIdx {
			recIdx[i] = 0
			continue
		}
		p := strings.TrimRight(r.path, sep)
		name := p
		if j := strings.LastIndex(p, sep); j >= 0 {
			name = p[j+1:]
		}
		key := strings.ToLower(p)
		if r.isDir {
			if j, dup := idx[key]; dup {
				recIdx[i] = j
				continue
			}
		}
		recIdx[i] = b.Add(fsdb.Record{Parent: -1, IsDir: r.isDir, Size: r.size, Mtime: r.mtime, Name: name, Flags: r.flags})
		if r.isDir {
			idx[key] = recIdx[i]
		}
	}
	// 2回目: 親を解決(存在しない中間フォルダは補完)
	var parentOf func(p string) int32
	parentOf = func(p string) int32 {
		j := strings.LastIndex(p, sep)
		if j <= 0 {
			return 0
		}
		pp := p[:j]
		if id, ok := idx[strings.ToLower(pp)]; ok {
			return id
		}
		if len(pp) <= len(root) {
			return 0
		}
		id := b.Add(fsdb.Record{IsDir: true, Name: pp[strings.LastIndex(pp, sep)+1:]})
		idx[strings.ToLower(pp)] = id
		b.Recs[id].Parent = parentOf(pp)
		return id
	}
	outside := 0
	for i, r := range rows {
		ri := recIdx[i]
		if ri == 0 || b.Recs[ri].Parent >= 0 {
			continue
		}
		p := strings.TrimRight(r.path, sep)
		if !strings.HasPrefix(strings.ToLower(p), strings.ToLower(root)+sep) && !strings.HasSuffix(root, sep) {
			outside++
		}
		b.Recs[ri].Parent = parentOf(p)
	}
	rows = nil
	idx = nil

	b.Meta["source"] = "excel"
	b.Meta["source_path"] = xlsxPath
	b.Meta["imported_at"] = time.Now().Format("2006/01/02 15:04:05")
	b.Meta["size_note"] = "サイズはExcelのKB値(小数1桁)からの換算値です"
	if err := b.Finalize(ctx, dbPath, prog); err != nil {
		return nil, err
	}
	res := &Result{Items: int64(len(b.Recs))}
	if outside > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("ルート(%s)の外にあるパスが %d 件あり、ルート直下に配置しました", root, outside))
	}
	return res, nil
}

// Result は取込/スキャンの結果概要。
type Result struct {
	Items    int64    `json:"items"`
	Errors   int64    `json:"errors"`
	Warnings []string `json:"warnings"`
}

var timeLayouts = []string{"2006/01/02 15:04:05", "2006/01/02 15:04", "2006/1/2 15:04:05", "2006/1/2 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006/01/02", "2006-01-02"}

// parseTime は文字列の日時、またはExcelのシリアル値を UNIX秒 に変換する。
func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil && v > 1 && v < 100000 {
		base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.Local)
		return base.Add(time.Duration(v * 24 * float64(time.Hour))).Unix()
	}
	return 0
}
