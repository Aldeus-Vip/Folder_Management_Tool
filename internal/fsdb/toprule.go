package fsdb

import (
	"fmt"
	"regexp"
	"strings"
)

// ---- 上位階層の命名・配置ルール ----
// 例: 「第3階層までは "XXX_フォルダ名"(数字3桁_名前)の形式を必須とし、ファイルは置けない」
//   TopDepth   : 対象とする階層(1〜TopDepth。0 = 無効)。仮想ルート = 0
//   TopFormat  : 名前の形式(num3 / alnum3 / any3 / custom)。custom は TopPattern(正規表現)
//   TopNameMode: 形式に合わないフォルダ名の扱い(off / warn / block)
//   TopFileMode: 対象階層にファイルを置く場合の扱い(off / warn / block)

var topFormats = map[string]struct{ re, label string }{
	"num3":   {`^[0-9]{3}_.+$`, "数字3桁_名前(例: 010_経理)"},
	"alnum3": {`^[A-Za-z0-9]{3}_.+$`, "英数字3文字_名前(例: A01_経理)"},
	"any3":   {`^.{3}_.+$`, "任意の3文字_名前(例: 経理部_契約書)"},
}

// TopFormatLabel は名前の形式の説明。
func (r Rules) TopFormatLabel() string {
	if f, ok := topFormats[r.TopFormat]; ok {
		return f.label
	}
	return "指定の形式(" + r.TopPattern + ")"
}

func (r Rules) topRegexp() (*regexp.Regexp, error) {
	if f, ok := topFormats[r.TopFormat]; ok {
		return regexp.MustCompile(f.re), nil
	}
	if strings.TrimSpace(r.TopPattern) == "" {
		return nil, fmt.Errorf("名前の形式(正規表現)を入力してください")
	}
	re, err := regexp.Compile(r.TopPattern)
	if err != nil {
		return nil, fmt.Errorf("名前の形式(正規表現)が正しくありません: %v", err)
	}
	return re, nil
}

// halfWidth は全角英数・記号を半角にする(「０１０＿経理」も「010_経理」と同じに扱う)。
func halfWidth(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0xFF01 && r <= 0xFF5E {
			return r - 0xFEE0
		}
		return r
	}, s)
}

// topNameOK は depth 階層目のフォルダ名が形式に合うか(対象外の階層・ルールが無効なら true)。
func (r Rules) topNameOK(depth int, name string) bool {
	if r.TopDepth <= 0 || depth < 1 || depth > r.TopDepth || r.TopNameMode == "off" {
		return true
	}
	re, err := r.topRegexp()
	if err != nil {
		return true
	}
	return re.MatchString(halfWidth(name))
}

func (r Rules) topNameMsg(depth int, name string) string {
	return fmt.Sprintf("第%d階層までのフォルダ名は「%s」の形式にしてください(「%s」は第%d階層)", r.TopDepth, r.TopFormatLabel(), name, depth)
}

func (r Rules) topFileMsg(depth int) string {
	return fmt.Sprintf("第%d階層まではファイルを置けません(第%d階層になります)", r.TopDepth, depth)
}

// checkTopPlace は項目 x を depth 階層目に置くときの、上位階層ルールの違反を is に追加する。
// フォルダの場合は、その中身のうち対象階層に入る部分(ファイル・サブフォルダ名)も調べる。
func (s *Store) checkTopPlace(is *Issue, r Rules, x *Node, depth int, name string) {
	if r.TopDepth <= 0 || depth > r.TopDepth {
		return
	}
	if !x.IsDir {
		apply(is, r.TopFileMode, r.topFileMsg(depth))
		return
	}
	if !r.topNameOK(depth, name) {
		apply(is, r.TopNameMode, r.topNameMsg(depth, name))
	}
	if depth >= r.TopDepth || x.End <= x.ID {
		return
	}
	// 中身: 実際の階層 = depth + (node.depth - x.Depth)。TopDepth 以内に入るもの
	limit := x.Depth + r.TopDepth - depth
	var files int
	s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id>? AND id<=? AND is_dir=0 AND depth<=?`, x.ID, x.End, limit).Scan(&files)
	if files > 0 && r.TopFileMode != "off" {
		apply(is, r.TopFileMode, fmt.Sprintf("フォルダの中のファイル %d 件が第%d階層以内に入ります(第%d階層まではファイルを置けません)", files, r.TopDepth, r.TopDepth))
	}
	if r.TopNameMode == "off" {
		return
	}
	rows, err := s.DB.Query(`SELECT name, depth FROM nodes WHERE id>? AND id<=? AND is_dir=1 AND depth<=?`, x.ID, x.End, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var n string
		var d int
		rows.Scan(&n, &d)
		if !r.topNameOK(depth+d-x.Depth, n) {
			bad = append(bad, n)
		}
	}
	if len(bad) > 0 {
		ex := strings.Join(bad[:min(3, len(bad))], "、")
		apply(is, r.TopNameMode, fmt.Sprintf("フォルダの中のサブフォルダ %d 件(%s など)が第%d階層以内に入りますが、「%s」の形式ではありません", len(bad), ex, r.TopDepth, r.TopFormatLabel()))
	}
}

// RuleViolation は今の整理後の構成のうち、上位階層ルールに合わないもの。
type RuleViolation struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

// TopRuleViolations はルールを後から有効にした場合などに、既存の構成の違反を洗い出す
// (仮想フォルダの名前と、仮想フォルダの直下に置いた項目が対象)。
func (s *Store) TopRuleViolations() ([]RuleViolation, error) {
	r := s.Rules()
	out := []RuleViolation{}
	if r.TopDepth <= 0 {
		return out, nil
	}
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	type vf struct {
		uuid, path string
		depth      int
	}
	var vfs []vf
	for _, n := range s.vt.nodes {
		if n.UUID == VRoot {
			continue
		}
		if !r.topNameOK(n.Depth, n.Name) {
			out = append(out, RuleViolation{s.vt.path(n.UUID), r.topNameMsg(n.Depth, n.Name)})
		}
		if n.Depth < r.TopDepth {
			vfs = append(vfs, vf{n.UUID, s.vt.path(n.UUID), n.Depth})
		}
	}
	s.mu.Unlock()
	// ルート直下(第1階層)に置いた項目も対象
	vfs = append(vfs, vf{VRoot, "", 0})
	for _, v := range vfs {
		ns, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE p.action='move' AND p.vparent=?`, v.uuid)
		if err != nil {
			return nil, err
		}
		for i := range ns {
			x := &ns[i]
			name := x.Name
			if x.NewName != "" {
				name = x.NewName
			}
			var is Issue
			s.checkTopPlace(&is, r, x, v.depth+1, name)
			for _, m := range append(is.Blocks, is.Warns...) {
				out = append(out, RuleViolation{joinV(v.path, name), m})
			}
		}
	}
	return out, nil
}

// topRowRule は仮想ツリーの1行(depth 階層目)についての上位階層ルールの違反(表示用・簡易)。
func (r Rules) topRowRule(kind string, depth int, name string) []string {
	if r.TopDepth <= 0 || depth > r.TopDepth {
		return nil
	}
	if kind == "file" {
		if r.TopFileMode != "off" {
			return []string{r.topFileMsg(depth)}
		}
		return nil
	}
	if !r.topNameOK(depth, name) {
		return []string{r.topNameMsg(depth, name)}
	}
	return nil
}
