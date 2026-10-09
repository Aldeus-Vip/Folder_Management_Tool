package fsdb

import (
	"regexp"
	"sort"
	"strings"
)

// ---- 初動移行の範囲(IT部門が SharePoint へそのままコピーした範囲) ----
// IT から渡されるリストは「移行した拡張子」の場合と「移行しなかった(除外した)拡張子」の場合があるので、両方に対応する。
// 拡張子とは別に、フォルダ単位で移行対象外にした範囲(除外フォルダ)も指定できる。

// MigScope は初動移行の範囲の設定。
type MigScope struct {
	Mode    string   `json:"mode"`    // include: Exts を移行した / exclude: Exts 以外を移行した
	Exts    []string `json:"exts"`    // 拡張子(小文字・ドットなし)。拡張子なしは "(なし)"
	Paths   []string `json:"paths"`   // 移行対象外のフォルダ(フルパス、またはルートからの相対パス)
	Ignored []string `json:"ignored"` // 入力のうち拡張子として読めなかったもの(見出し・種類名など)
	Missing []string `json:"missing"` // 見つからなかった除外フォルダ
}

var reExt = regexp.MustCompile(`^[a-z0-9_~$\-]{1,16}$`)

// parseExts は「,」・空白・改行・タブ区切りの拡張子を読む。Excel からの貼り付け(種類名の列・見出しを含む)も受け付け、
// 拡張子として読めないものは ignored に返す。
func parseExts(v string) (exts, ignored []string) {
	seen := map[string]bool{}
	for _, x := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';' || r == '、' || r == '　'
	}) {
		x = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(halfWidth(x)), "."))
		if x == "(なし)" || x == "なし" { // 全角の括弧は halfWidth で半角になっている
			x = "(なし)"
		} else if !reExt.MatchString(x) {
			if x != "" && !seen["!"+x] {
				seen["!"+x] = true
				ignored = append(ignored, x)
			}
			continue
		}
		if !seen[x] {
			seen[x] = true
			exts = append(exts, x)
		}
	}
	sort.Strings(exts)
	return exts, ignored
}

func splitLines(v string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(v, "\r", ""), "\n") {
		if l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), `"`)); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// MigrationScope は保存されている初動移行の範囲。
func (s *Store) MigrationScope() MigScope {
	m, _ := s.Meta()
	sc := MigScope{Mode: m["mig_mode"], Paths: splitLines(m["mig_paths"]), Ignored: []string{}}
	if sc.Mode != "exclude" {
		sc.Mode = "include"
	}
	sc.Exts, _ = parseExts(m["migrated_exts"])
	if sc.Exts == nil {
		sc.Exts = []string{}
	}
	if sc.Paths == nil {
		sc.Paths = []string{}
	}
	_, sc.Missing = s.migMatcher(sc)
	return sc
}

// SetMigrationScope は初動移行の範囲を保存する(mode: include / exclude)。
func (s *Store) SetMigrationScope(mode, exts, paths string) (MigScope, error) {
	if mode != "exclude" {
		mode = "include"
	}
	list, ignored := parseExts(exts)
	if err := s.SetMeta(map[string]string{"mig_mode": mode, "migrated_exts": strings.Join(list, ","), "mig_paths": strings.Join(splitLines(paths), "\n")}); err != nil {
		return MigScope{}, err
	}
	sc := s.MigrationScope()
	if ignored != nil {
		sc.Ignored = ignored
	}
	return sc, nil
}

// SetMigratedExts は「移行した拡張子」を指定する(include)。
func (s *Store) SetMigratedExts(list string) error {
	_, err := s.SetMigrationScope("include", list, "")
	return err
}

// migMatch は項目が初動移行で SharePoint へコピーされたかを判定する。
type migMatch struct {
	exclude bool
	exts    map[string]bool
	ranges  [][2]int64 // 除外フォルダの範囲(ID区間)
}

func (s *Store) migMatcher(sc MigScope) (*migMatch, []string) {
	m := &migMatch{exclude: sc.Mode == "exclude", exts: map[string]bool{}}
	for _, e := range sc.Exts {
		m.exts[e] = true
	}
	missing := []string{}
	meta, _ := s.Meta()
	root := strings.TrimRight(meta["root"], `\/`)
	for _, p := range sc.Paths {
		id, err := s.FindPath(p)
		if err != nil && root != "" {
			id, err = s.FindPath(root + `\` + strings.TrimLeft(p, `\/`))
			if err != nil {
				id, err = s.FindPath(root + `/` + strings.TrimLeft(p, `\/`))
			}
		}
		var end int64
		if err == nil {
			err = s.DB.QueryRow(`SELECT end_id FROM nodes WHERE id=?`, id).Scan(&end)
		}
		if err != nil {
			missing = append(missing, p)
			continue
		}
		m.ranges = append(m.ranges, [2]int64{id, end})
	}
	sort.Slice(m.ranges, func(i, j int) bool { return m.ranges[i][0] < m.ranges[j][0] })
	return m, missing
}

func (m *migMatch) inExcludedFolder(id int64) bool {
	for _, r := range m.ranges {
		if id >= r[0] && id <= r[1] {
			return true
		}
	}
	return false
}

// file はファイルが初動移行で SharePoint へコピーされたか。
func (m *migMatch) file(id int64, ext string) bool {
	if m.inExcludedFolder(id) {
		return false
	}
	e := strings.ToLower(ext)
	if e == "" {
		e = "(なし)"
	}
	return m.exts[e] != m.exclude
}

// folderLocation はフォルダの所在(除外フォルダの中なら FileServer、それ以外は中身が分かれるので Both)。
func (m *migMatch) folderLocation(id int64) string {
	if m.inExcludedFolder(id) {
		return "FileServer"
	}
	return "Both"
}
