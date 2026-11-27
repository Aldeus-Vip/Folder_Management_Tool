package fsdb

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- 増加量レポート(最終更新年ごとのファイル数・容量) ----
// 長期保管の予算化のため、ファイルが年ごとにどれだけ増えているかを、切り口(部門・指定フォルダ・種類)別に集計する。
// 最終更新日時で数えるため、作成日とは異なる(コピー時に日付が変わる場合がある)。

const GrowthNote = "最終更新日で集計しています。作成日とは異なり、コピー時に日付が変わる場合があります。"

const DefaultGrowthYears = 10

// GrowthCell は1つの年の集計。
type GrowthCell struct {
	Files int64 `json:"files"`
	Size  int64 `json:"size"`
}

type GrowthRow struct {
	Label string       `json:"label"`
	Note  string       `json:"note,omitempty"` // 補足(指定フォルダの該当数など)
	Cells []GrowthCell `json:"cells"`          // Cols と同じ順
	Total GrowthCell   `json:"total"`
}

type GrowthReport struct {
	Axis    string      `json:"axis"`
	Years   int         `json:"years"`
	Base    int         `json:"base"` // 基準の年(データ取得日の年)
	Cols    []string    `json:"cols"` // 「それ以前」・各年・「不明」
	Rows    []GrowthRow `json:"rows"`
	Total   GrowthRow   `json:"total"`
	Folders []string    `json:"folders"` // 指定フォルダ(設定)
	Missing []string    `json:"missing"` // 見つからなかった指定フォルダ
	Note    string      `json:"note"`
}

// GrowthAxes は集計の切り口。
var GrowthAxes = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}{
	{"depth1", "第1階層のフォルダ"},
	{"depth2", "第2階層のフォルダ"},
	{"depth3", "第3階層のフォルダ"},
	{"folders", "指定したフォルダ / それ以外"},
	{"kind", "ファイルの種類"},
}

// FileKinds はファイルの種類(拡張子のまとまり)。
var FileKinds = []struct {
	Label string
	Exts  string
}{
	{"PDF", "pdf"},
	{"Office", "doc docx docm dot dotx xls xlsx xlsm xlsb xlt xltx xltm csv ppt pptx pptm pps ppsx pot potx msg vsd vsdx one mpp rtf"},
	{"画像", "jpg jpeg png gif bmp tif tiff heic heif webp svg emf wmf"},
	{"動画・音声", "mp4 mov avi wmv mpg mpeg mkv m4v 3gp flv webm mp3 wav wma m4a aac flac ogg"},
	{"圧縮", "zip lzh 7z rar cab gz tar tgz bz2 xz"},
}

var kindOf = func() map[string]int {
	m := map[string]int{}
	for i, k := range FileKinds {
		for _, x := range strings.Fields(k.Exts) {
			m[x] = i
		}
	}
	return m
}()

// FileKind は拡張子の種類の名前(該当なしは「その他」)。
func FileKind(ext string) string {
	if i, ok := kindOf[strings.ToLower(ext)]; ok {
		return FileKinds[i].Label
	}
	return "その他"
}

// GrowthSetting は増加量レポートの設定(期間の年数・指定フォルダ)。
func (s *Store) GrowthSetting() (int, []string) {
	m, _ := s.Meta()
	years, err := strconv.Atoi(m["growth_years"])
	if err != nil || years <= 0 {
		years = DefaultGrowthYears
	}
	var fs []string
	for _, l := range strings.Split(m["growth_folders"], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			fs = append(fs, l)
		}
	}
	return years, fs
}

func (s *Store) SetGrowthSetting(years int, folders []string) error {
	if years <= 0 || years > 100 {
		return fmt.Errorf("期間は1〜100年で指定してください")
	}
	var fs []string
	for _, f := range folders {
		for _, l := range strings.Split(f, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				fs = append(fs, l)
			}
		}
	}
	return s.SetMeta(map[string]string{"growth_years": strconv.Itoa(years), "growth_folders": strings.Join(fs, "\n")})
}

// baseYear はデータ取得日の年(無ければ今年)。
func (s *Store) baseYear() int {
	m, _ := s.Meta()
	for _, k := range []string{"scanned_at", "merged_at", "imported_at", "built_at"} {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", m[k], time.Local); err == nil {
			return t.Year()
		}
	}
	return time.Now().Year()
}

// span は指定フォルダ1つ分の範囲(配下を含む ID の区間)。
type span struct {
	a, b int64
	row  int
}

// folderSpans は指定フォルダを ID の区間にする。
// 指定の仕方: フルパス / ルートからの相対パス / フォルダ名(区切り文字を含まない。同じ名前のフォルダをすべて含める)。
func (s *Store) folderSpans(entries []string) (spans []span, notes []string, missing []string, err error) {
	m, _ := s.Meta()
	root, sep := m["root"], m["sep"]
	notes = make([]string, len(entries))
	for i, e := range entries {
		var rows [][2]int64
		if strings.ContainsAny(e, `\/`) {
			p := strings.ReplaceAll(strings.ReplaceAll(strings.Trim(e, `\/`), `\`, sep), "/", sep)
			id, ferr := s.FindPath(e)
			if ferr != nil {
				id, ferr = s.FindPath(joinRoot(root, sep, p))
			}
			if ferr == nil {
				var end int64
				s.DB.QueryRow(`SELECT end_id FROM nodes WHERE id=? AND is_dir=1`, id).Scan(&end)
				if end > 0 {
					rows = append(rows, [2]int64{id, end})
				}
			}
		} else {
			r, qerr := s.DB.Query(`SELECT id, end_id FROM nodes WHERE is_dir=1 AND name_lc=? ORDER BY id`, strings.ToLower(e))
			if qerr != nil {
				return nil, nil, nil, qerr
			}
			for r.Next() {
				var a, b int64
				r.Scan(&a, &b)
				rows = append(rows, [2]int64{a, b})
			}
			r.Close()
			if len(rows) > 1 {
				notes[i] = fmt.Sprintf("同じ名前のフォルダ %d か所の合計", len(rows))
			}
		}
		if len(rows) == 0 {
			missing = append(missing, e)
			notes[i] = "見つかりません"
		}
		for _, x := range rows {
			spans = append(spans, span{x[0], x[1], i})
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].a != spans[j].a {
			return spans[i].a < spans[j].a
		}
		return spans[i].b > spans[j].b // 外側を先に
	})
	return spans, notes, missing, nil
}

// GrowthReport は最終更新年ごとのファイル数・容量を、切り口ごとに集計する(1回の走査)。
func (s *Store) GrowthReport(axis string, years int) (*GrowthReport, error) {
	setYears, folders := s.GrowthSetting()
	if years <= 0 {
		years = setYears
	}
	base := s.baseYear()
	first := base - years + 1
	rep := &GrowthReport{Axis: axis, Years: years, Base: base, Folders: folders, Missing: []string{}, Note: GrowthNote}
	if rep.Folders == nil {
		rep.Folders = []string{}
	}
	rep.Cols = append(rep.Cols, fmt.Sprintf("%d年より前", first))
	for y := first; y <= base; y++ {
		rep.Cols = append(rep.Cols, strconv.Itoa(y))
	}
	rep.Cols = append(rep.Cols, "不明")
	nc := len(rep.Cols)
	col := func(mtime int64) int {
		if mtime <= 0 {
			return nc - 1
		}
		y := time.Unix(mtime, 0).Year()
		switch {
		case y < first:
			return 0
		case y > base: // 未来の日付(時計の誤りなど)
			return nc - 1
		}
		return y - first + 1
	}

	// 行の決め方
	var rows []GrowthRow
	rowOf := map[int64]int{} // 階層: 祖先フォルダの ID → 行
	newRow := func(label string) int {
		rows = append(rows, GrowthRow{Label: label, Cells: make([]GrowthCell, nc)})
		return len(rows) - 1
	}
	depthK := 0
	var spans []span
	other := -1
	switch axis {
	case "depth1", "depth2", "depth3":
		depthK = int(axis[5] - '0')
	case "folders":
		sp, notes, missing, err := s.folderSpans(folders)
		if err != nil {
			return nil, err
		}
		spans, rep.Missing = sp, append(rep.Missing, missing...)
		for i, f := range folders {
			newRow(f)
			rows[i].Note = notes[i]
		}
		other = newRow("それ以外")
	case "kind":
		for _, k := range FileKinds {
			newRow(k.Label)
		}
		newRow("その他")
	default:
		return nil, fmt.Errorf("不明な切り口: %s", axis)
	}
	upper := -1 // 階層: 指定の階層より上に直接あるファイル

	q, err := s.DB.Query(`SELECT id, depth, is_dir, name, ext, size, mtime FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	sep := `\`
	if m, _ := s.Meta(); m["sep"] != "" {
		sep = m["sep"]
	}
	names := make([]string, 0, 32)
	anc := make([]int64, 0, 32) // 各階層の祖先フォルダの ID
	var stack []span            // 指定フォルダ: いま中にいる区間
	si := 0
	rep.Total = GrowthRow{Label: "合計", Cells: make([]GrowthCell, nc)}
	for q.Next() {
		var id int64
		var depth int
		var dir bool
		var name, ext string
		var size, mtime int64
		if err := q.Scan(&id, &depth, &dir, &name, &ext, &size, &mtime); err != nil {
			return nil, err
		}
		if depthK > 0 {
			if depth < len(names) {
				names, anc = names[:depth], anc[:depth]
			}
			for len(names) < depth {
				names, anc = append(names, ""), append(anc, 0)
			}
			names, anc = append(names, name), append(anc, id)
		}
		if spans != nil {
			for len(stack) > 0 && stack[len(stack)-1].b < id {
				stack = stack[:len(stack)-1]
			}
			for si < len(spans) && spans[si].a <= id {
				if spans[si].b >= id {
					stack = append(stack, spans[si])
				}
				si++
			}
		}
		if dir {
			continue
		}
		var r int
		switch {
		case depthK > 0:
			if depth > depthK {
				a := anc[depthK]
				k, ok := rowOf[a]
				if !ok {
					k = newRow(strings.Join(names[1:depthK+1], sep))
					rowOf[a] = k
				}
				r = k
			} else {
				if upper < 0 {
					upper = newRow(fmt.Sprintf("(第%d階層より上に直接あるファイル)", depthK))
				}
				r = upper
			}
		case spans != nil:
			r = other
			if len(stack) > 0 {
				r = stack[len(stack)-1].row // いちばん内側の指定フォルダ
			}
		default:
			r = len(FileKinds)
			if i, ok := kindOf[strings.ToLower(ext)]; ok {
				r = i
			}
		}
		c := col(mtime)
		for _, row := range []*GrowthRow{&rows[r], &rep.Total} {
			row.Cells[c].Files++
			row.Cells[c].Size += size
			row.Total.Files++
			row.Total.Size += size
		}
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	if upper >= 0 { // 「より上のファイル」は最後に置く
		r := rows[upper]
		rows = append(append(rows[:upper:upper], rows[upper+1:]...), r)
	}
	if rows == nil {
		rows = []GrowthRow{}
	}
	rep.Rows = rows
	return rep, nil
}

// WriteGrowthCSV は増加量レポートをCSVに書き出す(1行目に注意書き。容量はGB)。
func (s *Store) WriteGrowthCSV(w io.Writer, axis string, years int) error {
	rep, err := s.GrowthReport(axis, years)
	if err != nil {
		return err
	}
	io.WriteString(w, "\uFEFF")
	cw := csv.NewWriter(w)
	label := axis
	for _, a := range GrowthAxes {
		if a.Key == axis {
			label = a.Label
		}
	}
	cw.Write([]string{"※" + GrowthNote})
	cw.Write([]string{"切り口: " + label, fmt.Sprintf("基準年: %d(データ取得日)", rep.Base), "容量の単位: GB(1GB = 1024^3 バイト)"})
	head := append([]string{"切り口", "指標"}, rep.Cols...)
	cw.Write(append(head, "合計", "補足"))
	gb := func(n int64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 2, 64) }
	for _, r := range append(rep.Rows, rep.Total) {
		for _, metric := range []string{"容量(GB)", "ファイル数"} {
			rec := []string{r.Label, metric}
			for _, c := range r.Cells {
				if metric == "ファイル数" {
					rec = append(rec, strconv.FormatInt(c.Files, 10))
				} else {
					rec = append(rec, gb(c.Size))
				}
			}
			if metric == "ファイル数" {
				rec = append(rec, strconv.FormatInt(r.Total.Files, 10))
			} else {
				rec = append(rec, gb(r.Total.Size))
			}
			cw.Write(append(rec, r.Note))
		}
	}
	cw.Flush()
	return cw.Error()
}
