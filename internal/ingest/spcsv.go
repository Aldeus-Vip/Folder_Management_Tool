package ingest

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// ---- SharePoint の一覧CSVの取り込み ----
// Power Automate などで出力した SharePoint のフォルダ・ファイルの一覧(CSV)から DB を作る。
// 移行先(SharePoint)の構成も、ファイルサーバーと同じ画面・同じ5Sルールで確認できる。
//
// CSV の列(ヘッダ名で判定。大文字小文字・列の順番は問わない):
//   FullPath(必須): パス(区切りは /)。FileRef でも可
//   IsFolder(必須): True / False。FSObjType(1 = フォルダ)でも可
//   Modified(任意): 更新日時(ISO8601)
//   Size    (任意): バイト数(ファイルのみ)。File_x0020_Size / Length でも可
//   UniquePerm(任意): True = 固有の権限(HasUniqueRoleAssignments)。あれば固有の権限の数を数えられる
//
// ルートは全行に共通する最も深いフォルダ。CSV に行が無い途中のフォルダはパスから補完する。
// sitePrefix(SharePoint の URL)を指定すると、ルートの前に付けて URL として記録する。
// ブラウザのアドレス欄の URL(…/Forms/AllItems.aspx?id=… を含む)をそのまま貼り付けてよい(spPrefix で必要な部分だけ使う)。

type spRow struct {
	segs  []string
	dir   bool
	mtime int64
	size  int64
	perm  int8 // 固有の権限: -1 = 列なし, 0 = なし(親から継承), 1 = あり
}

// acl は UniquePerm 列から作る権限(付与先の内訳は CSV に無いので、固有かどうかだけ)。
func (r *spRow) acl() *fsdb.ACL {
	if r.perm < 0 {
		return nil
	}
	return &fsdb.ACL{Protected: r.perm == 1, ACEs: []fsdb.ACE{}}
}

// spFileTime はファイル名の SPTree_YYYYMMDD_HHMM(UTC)をデータ取得日時として読む。
var spFileTime = regexp.MustCompile(`(\d{8})_(\d{4})`)

// ImportSPCSV は SharePoint の一覧CSVから DB を作る。
func ImportSPCSV(ctx context.Context, csvPath, dbPath, sitePrefix string, prog fsdb.Progress) (*Result, error) {
	if prog == nil {
		prog = func(string, int64, int64) {}
	}
	prog("CSVを読み込み中", 0, 0)
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("CSVを開けません: %w", err)
	}
	defer f.Close()
	rows, lead, bad, err := readSPCSV(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSVに取り込める行がありません")
	}

	// ルート = 全行に共通する最も深いフォルダ
	common := rows[0].segs
	for _, r := range rows[1:] {
		n := 0
		for n < len(common) && n < len(r.segs) && strings.EqualFold(common[n], r.segs[n]) {
			n++
		}
		common = common[:n]
	}
	for _, r := range rows {
		if !r.dir && len(r.segs) == len(common) { // 共通部分がファイルそのもの → その親
			common = common[:len(common)-1]
			break
		}
	}
	root := strings.Join(common, "/")
	if lead {
		root = "/" + root
	}
	if p := spPrefix(sitePrefix, lead, common); p != "" {
		root = p + "/" + strings.TrimLeft(root, "/")
	}
	if root == "" {
		root = "/"
	}

	b := fsdb.NewBuilder(root, "/")
	key := func(segs []string) string { return strings.ToLower(strings.Join(segs, "/")) }
	idx := map[string]int32{key(common): 0}
	explicit := map[int32]bool{0: true}
	var ensureDir func(segs []string) int32
	ensureDir = func(segs []string) int32 {
		k := key(segs)
		if i, ok := idx[k]; ok {
			return i
		}
		parent := ensureDir(segs[:len(segs)-1])
		i := b.Add(fsdb.Record{Parent: parent, IsDir: true, Name: segs[len(segs)-1]})
		idx[k] = i
		return i
	}
	var dupRows, conflict int64
	for n, r := range rows {
		if n%20000 == 0 {
			prog("ツリーを作成中", int64(n), int64(len(rows)))
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if len(r.segs) == len(common) { // ルート自身の行
			if r.dir {
				b.Recs[0].Mtime, b.Recs[0].ACL = r.mtime, r.acl()
			}
			continue
		}
		k := key(r.segs)
		if i, ok := idx[k]; ok {
			rec := &b.Recs[i]
			switch {
			case rec.IsDir != r.dir:
				conflict++
			case r.dir && !explicit[i]: // 補完済みのフォルダに行が来た
				explicit[i] = true
				rec.Mtime, rec.ACL = r.mtime, r.acl()
			default:
				dupRows++
			}
			continue
		}
		parent := ensureDir(r.segs[:len(r.segs)-1])
		rec := fsdb.Record{Parent: parent, IsDir: r.dir, Name: r.segs[len(r.segs)-1], Mtime: r.mtime, ACL: r.acl()}
		if !r.dir {
			rec.Size = r.size
		}
		i := b.Add(rec)
		idx[k] = i
		if r.dir {
			explicit[i] = true
		}
	}
	var filled int64
	for i := range b.Recs {
		if b.Recs[i].IsDir && !explicit[int32(i)] {
			filled++
		}
	}
	idx = nil

	b.Meta["source"] = "spcsv"
	b.Meta["source_path"] = csvPath
	b.Meta["scanned_at"] = spDataTime(csvPath).Format("2006/01/02 15:04:05")
	if strings.Contains(root, "://") {
		b.Meta["web_root"] = root // パスが URL のとき「開く」はブラウザで開く
	}
	if rows[0].perm >= 0 {
		b.Meta["acl_mode"] = "spcsv" // 固有の権限かどうかだけ(付与先の内訳なし)
	}
	if err := b.Finalize(ctx, dbPath, prog); err != nil {
		return nil, err
	}
	res := &Result{Items: int64(len(b.Recs))}
	res.Warnings = append(res.Warnings, "ルート: "+root)
	if filled > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("CSVに行が無い途中のフォルダ %d 件を補完しました(更新日時は不明。一覧の取りこぼしの可能性があります)", filled))
	}
	if bad > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("パスが空・読めない行 %d 件を飛ばしました", bad))
	}
	if dupRows > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("同じパスの行 %d 件は最初の行を採用しました", dupRows))
	}
	if conflict > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("フォルダ/ファイルの種別が食い違う行 %d 件は最初の行を採用しました", conflict))
	}
	return res, nil
}

// readSPCSV は CSV を読む。lead はパスが「/」で始まるか(サーバー相対パス)。
func readSPCSV(ctx context.Context, rd io.Reader) (rows []spRow, lead bool, bad int64, err error) {
	br := bufio.NewReaderSize(rd, 1<<20)
	if bom, _ := br.Peek(3); string(bom) == "\xEF\xBB\xBF" {
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		return nil, false, 0, fmt.Errorf("CSVのヘッダを読めません: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	pick := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}
		return -1
	}
	cPath, cDir := pick("fullpath", "fileref", "path"), pick("isfolder", "fsobjtype")
	cMod, cSize := pick("modified"), pick("size", "file_x0020_size", "length", "file/length")
	cPerm := pick("uniqueperm", "hasuniqueroleassignments", "uniquepermissions")
	if cPath < 0 || cDir < 0 {
		return nil, false, 0, fmt.Errorf("CSVに必須の列(FullPath・IsFolder)がありません。ヘッダ: %s", strings.Join(head, ", "))
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	first := true
	for n := 0; ; n++ {
		rec, e := cr.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, false, 0, fmt.Errorf("CSVの %d 行目を読めません: %w", n+2, e)
		}
		if n%50000 == 0 && ctx.Err() != nil {
			return nil, false, 0, ctx.Err()
		}
		p := strings.ReplaceAll(get(rec, cPath), `\`, "/")
		var segs []string
		for _, s := range strings.Split(p, "/") {
			if s != "" {
				segs = append(segs, s)
			}
		}
		if len(segs) == 0 {
			bad++
			continue
		}
		if first {
			lead, first = strings.HasPrefix(p, "/"), false
		}
		d := strings.ToLower(get(rec, cDir))
		r := spRow{segs: segs, dir: d == "true" || d == "1" || d == "yes" || d == "folder"}
		if t, ok := parseSPTime(get(rec, cMod)); ok {
			r.mtime = t
		}
		r.size, _ = strconv.ParseInt(get(rec, cSize), 10, 64)
		r.perm = -1
		if cPerm >= 0 {
			switch strings.ToLower(get(rec, cPerm)) {
			case "true", "1", "yes":
				r.perm = 1
			default:
				r.perm = 0
			}
		}
		rows = append(rows, r)
	}
	return rows, lead, bad, nil
}

func parseSPTime(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006/01/02 15:04:05", "2006/01/02 15:04", "2006/1/2 15:04"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// spDataTime はデータ取得日時(ファイル名の YYYYMMDD_HHMM を UTC として読む。無ければCSVの更新日時)。
func spDataTime(csvPath string) time.Time {
	if m := spFileTime.FindStringSubmatch(filepath.Base(csvPath)); m != nil {
		if t, err := time.Parse("20060102 1504", m[1]+" "+m[2]); err == nil {
			return t.Local()
		}
	}
	if st, err := os.Stat(csvPath); err == nil {
		return st.ModTime()
	}
	return time.Now()
}

// spPrefix は貼り付けた URL から、CSV のパスの前に付ける部分を取り出す。
//   - CSV のパスが「/」で始まる(/sites/… のサーバー相対パス)→ https://テナント だけ
//   - ライブラリからのパス(Shared Documents/…)→ その最初のフォルダ名の手前まで(サイトの URL)
//
// アドレス欄の URL(…/Shared Documents/Forms/AllItems.aspx?id=/sites/…)でも、id のパスを手がかりに切り出す。
func spPrefix(raw string, lead bool, common []string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	base := u.Scheme + "://" + u.Host
	if lead {
		return base
	}
	path := u.Path
	if id := u.Query().Get("id"); strings.HasPrefix(id, "/") {
		path = id // フォルダを開いた画面の URL: 実際のフォルダのパスは id にある
	}
	var segs []string
	for _, x := range strings.Split(path, "/") {
		if x != "" {
			segs = append(segs, x)
		}
	}
	cut := -1
	if len(common) > 0 {
		for i, x := range segs {
			if strings.EqualFold(x, common[0]) {
				cut = i
				break
			}
		}
	}
	if cut < 0 { // CSV のパスと重なる部分が無い: 画面のページ部分(/Forms/…, /SitePages/…, *.aspx)を除く
		cut = len(segs)
		for i, x := range segs {
			if l := strings.ToLower(x); l == "forms" || l == "sitepages" || l == "_layouts" || strings.HasSuffix(l, ".aspx") {
				cut = i
				break
			}
		}
	}
	if cut == 0 {
		return base
	}
	return base + "/" + strings.Join(segs[:cut], "/")
}
