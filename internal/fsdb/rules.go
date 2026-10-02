package fsdb

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ---- カスタムの5Sルール ----
// 「対象(種類・階層・名前の条件)」に当てはまる項目が「要件」を満たさなければ違反。例:
//   第1〜3階層のフォルダ             → 名前が「数字3桁_名前」の形式であること
//   第3階層までのファイル            → 置けない
//   すべてのフォルダ                 → 名前に OLD・ゴミ箱 などを含まない
//   第2階層の「課」を含むフォルダ    → 直下に「規定／SOP」「契約」フォルダがあること

// Cond は名前の条件。Val は「,」「、」・改行区切りで複数指定(どれか1つ)。
//
//	contains / notcontains : 次の語を含む / どれも含まない
//	chars / nochars        : 次の文字のどれかを含む / どれも含まない
//	prefix / suffix        : 次の語で始まる / 終わる
//	regex / notregex       : 正規表現に一致する / しない
//	format                 : 決まった形式(num3 / alnum3 / any3)
//	maxlen                 : 名前の文字数が Val 以下
//	haschild               : 直下に次の名前を含むフォルダがある(「,」区切りのすべて。「|」はどれか)
//	forbid                 : (要件のみ)置けない
type Cond struct {
	Op  string `json:"op"`
	Val string `json:"val"`
}

type CustomRule struct {
	ID      string `json:"id"`
	Cat     string `json:"cat"` // 整理 / 整頓 / 清掃 / 清潔 / 躾
	Label   string `json:"label"`
	Mode    string `json:"mode"`    // off / warn / block
	Kind    string `json:"kind"`    // dir / file / any
	DepthOp string `json:"depthOp"` // "" (すべて) / eq / ge / le / range
	Depth   int    `json:"depth"`
	Depth2  int    `json:"depth2"`
	When    Cond   `json:"when"` // 追加の対象条件(Op "" = 名前は問わない)
	Must    Cond   `json:"must"` // 要件
}

var Categories = []string{"整理", "整頓", "清掃", "清潔", "躾"}

var nameFormats = map[string]struct{ re, label string }{
	"num3":   {`^[0-9]{3}_.+$`, "数字3桁_名前(例: 010_経理)"},
	"alnum3": {`^[A-Za-z0-9]{3}_.+$`, "英数字3文字_名前(例: A01_経理)"},
	"any3":   {`^.{3}_.+$`, "任意の3文字_名前"},
}

// DefaultCustomRules は「ファイル整理方針」に沿った初期ルール。
var DefaultCustomRules = []CustomRule{
	{ID: "top-name", Cat: "整頓", Label: "上位階層のフォルダ名は「3桁の番号_名前」", Mode: "warn", Kind: "dir", DepthOp: "le", Depth: 3, Must: Cond{"format", "num3"}},
	{ID: "top-file", Cat: "整頓", Label: "上位階層(第3階層まで)にはファイルを置かない", Mode: "warn", Kind: "file", DepthOp: "le", Depth: 3, Must: Cond{"forbid", ""}},
	{ID: "required", Cat: "整頓", Label: "各課に「規定／SOP」「契約」フォルダを設置する", Mode: "off", Kind: "dir", DepthOp: "eq", Depth: 2, When: Cond{"contains", "課"}, Must: Cond{"haschild", "規定|SOP, 契約"}},
	{ID: "old", Cat: "清掃", Label: "旧版・ゴミ箱のようなフォルダを残さない(バージョン履歴で管理)", Mode: "warn", Kind: "dir", Must: Cond{"notregex", `(^|[^a-z])(old|bk|backup)([^a-z]|$)|旧版|ゴミ箱|ごみ箱|まもなく消去|削除予定`}},
	{ID: "temp", Cat: "整理", Label: "一時・システムファイルは移動しない", Mode: "warn", Kind: "file", Must: Cond{"notregex", `^~\$|\.tmp$|^thumbs\.db$|^desktop\.ini$|^\.ds_store$`}},
}

// ---- 評価 ----

type crule struct {
	CustomRule
	when, must func(string) bool // 名前(正規化済み)の判定
	groups     [][]string        // haschild の語のグループ
	desc       string
}

func normN(s string) string { return strings.ToLower(halfWidth(s)) }

func splitVals(v string) []string {
	f := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '、' || r == '\n' || r == '，' })
	var out []string
	for _, x := range f {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, normN(x))
		}
	}
	return out
}

// condFunc は名前の条件を関数にする。
func condFunc(c Cond) (func(string) bool, error) {
	vals := splitVals(c.Val)
	any := func(f func(n, v string) bool) func(string) bool {
		return func(n string) bool {
			for _, v := range vals {
				if f(n, v) {
					return true
				}
			}
			return false
		}
	}
	not := func(f func(string) bool) func(string) bool { return func(n string) bool { return !f(n) } }
	needVals := func() error {
		if len(vals) == 0 {
			return fmt.Errorf("条件の値を入力してください")
		}
		return nil
	}
	switch c.Op {
	case "contains", "notcontains":
		f := any(strings.Contains)
		if c.Op == "notcontains" {
			f = not(f)
		}
		return f, needVals()
	case "chars", "nochars":
		cs := strings.Join(strings.Fields(normN(c.Val)), "")
		if cs == "" {
			return nil, fmt.Errorf("文字を入力してください")
		}
		f := func(n string) bool { return strings.ContainsAny(n, cs) }
		if c.Op == "nochars" {
			f = not(f)
		}
		return f, nil
	case "prefix":
		return any(strings.HasPrefix), needVals()
	case "suffix":
		return any(strings.HasSuffix), needVals()
	case "regex", "notregex", "format":
		pat := c.Val
		if c.Op == "format" {
			f, ok := nameFormats[c.Val]
			if !ok {
				return nil, fmt.Errorf("不明な形式です: %s", c.Val)
			}
			pat = f.re
		}
		if strings.TrimSpace(pat) == "" {
			return nil, fmt.Errorf("正規表現を入力してください")
		}
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return nil, fmt.Errorf("正規表現が正しくありません: %v", err)
		}
		f := func(n string) bool { return re.MatchString(n) }
		if c.Op == "notregex" {
			f = not(f)
		}
		return f, nil
	case "maxlen":
		m, err := strconv.Atoi(strings.TrimSpace(c.Val))
		if err != nil || m < 1 {
			return nil, fmt.Errorf("文字数は1以上の数値で指定してください")
		}
		return func(n string) bool { return runeLen(n) <= m }, nil
	}
	return nil, fmt.Errorf("不明な条件です: %s", c.Op)
}

func condText(c Cond) string {
	q := func() string {
		var vs []string
		for _, v := range strings.FieldsFunc(c.Val, func(r rune) bool { return r == ',' || r == '、' || r == '\n' || r == '，' }) {
			if v = strings.TrimSpace(v); v != "" {
				vs = append(vs, v)
			}
		}
		return "「" + strings.Join(vs, "」「") + "」"
	}
	switch c.Op {
	case "contains":
		return "名前に" + q() + "のどれかを含む"
	case "notcontains":
		return "名前に" + q() + "を含まない"
	case "chars":
		return "名前に文字「" + c.Val + "」のどれかを含む"
	case "nochars":
		return "名前に文字「" + c.Val + "」を含まない"
	case "prefix":
		return "名前が" + q() + "で始まる"
	case "suffix":
		return "名前が" + q() + "で終わる"
	case "regex":
		return "名前が正規表現 " + c.Val + " に一致する"
	case "notregex":
		return "名前が正規表現 " + c.Val + " に一致しない"
	case "format":
		return "名前が「" + nameFormats[c.Val].label + "」の形式"
	case "maxlen":
		return "名前が" + c.Val + "文字以内"
	case "haschild":
		return "直下に" + q() + "のフォルダがある"
	case "forbid":
		return "置けない"
	}
	return ""
}

// Describe はルールの説明文(例:「第1〜3階層のフォルダ: 名前が「数字3桁_名前」の形式であること」)。
func (c CustomRule) Describe() string {
	kind := map[string]string{"dir": "フォルダ", "file": "ファイル"}[c.Kind]
	if kind == "" {
		kind = "フォルダ・ファイル"
	}
	scope := ""
	switch c.DepthOp {
	case "eq":
		scope = fmt.Sprintf("第%d階層の", c.Depth)
	case "ge":
		scope = fmt.Sprintf("第%d階層以下(深い側)の", c.Depth)
	case "le":
		scope = fmt.Sprintf("第%d階層までの", c.Depth)
	case "range":
		scope = fmt.Sprintf("第%d〜%d階層の", c.Depth, c.Depth2)
	default:
		scope = "すべての"
	}
	if c.When.Op != "" {
		scope += "(" + condText(c.When) + ")"
	}
	must := condText(c.Must)
	if c.Must.Op != "forbid" {
		must += "こと"
	}
	return scope + kind + ": " + must
}

func (c CustomRule) compile() (*crule, error) {
	cr := &crule{CustomRule: c, desc: c.Describe()}
	var err error
	if c.When.Op != "" {
		if c.When.Op == "forbid" || c.When.Op == "haschild" {
			return nil, fmt.Errorf("対象の条件に「%s」は使えません", condText(c.When))
		}
		if cr.when, err = condFunc(c.When); err != nil {
			return nil, err
		}
	}
	switch c.Must.Op {
	case "forbid":
	case "haschild":
		if c.Kind == "file" {
			return nil, fmt.Errorf("「直下にフォルダがある」はフォルダのルールにだけ使えます")
		}
		for _, g := range splitVals(c.Must.Val) {
			var alts []string
			for _, a := range strings.Split(g, "|") {
				if a = strings.TrimSpace(a); a != "" {
					alts = append(alts, a)
				}
			}
			if len(alts) > 0 {
				cr.groups = append(cr.groups, alts)
			}
		}
		if len(cr.groups) == 0 {
			return nil, fmt.Errorf("必要なフォルダ名を入力してください")
		}
	default:
		if cr.must, err = condFunc(c.Must); err != nil {
			return nil, err
		}
	}
	switch c.DepthOp {
	case "", "eq", "ge", "le", "range":
	default:
		return nil, fmt.Errorf("階層の条件が不正です")
	}
	if c.DepthOp == "range" && c.Depth2 < c.Depth {
		return nil, fmt.Errorf("階層の範囲が不正です(%d〜%d)", c.Depth, c.Depth2)
	}
	return cr, nil
}

func (c *crule) label() string {
	if strings.TrimSpace(c.Label) != "" {
		return "[" + c.Cat + "] " + c.Label
	}
	return "[" + c.Cat + "] " + c.desc
}

func (c *crule) depthOK(d int) bool {
	switch c.DepthOp {
	case "eq":
		return d == c.Depth
	case "ge":
		return d >= c.Depth
	case "le":
		return d <= c.Depth
	case "range":
		return d >= c.Depth && d <= c.Depth2
	}
	return true
}

// maxDepth はルールが対象にする最も深い階層(-1 = 上限なし)。
func (c *crule) maxDepth() int {
	switch c.DepthOp {
	case "eq", "le":
		return c.Depth
	case "range":
		return c.Depth2
	}
	return -1
}

// applies は項目がルールの対象か。nn は正規化した名前。
func (c *crule) applies(isDir bool, depth int, nn string) bool {
	if (c.Kind == "dir" && !isDir) || (c.Kind == "file" && isDir) || !c.depthOK(depth) {
		return false
	}
	return c.when == nil || c.when(nn)
}

// violates は名前の要件の違反(haschild は別に判定)。
func (c *crule) violates(nn string) bool {
	switch c.Must.Op {
	case "forbid":
		return true
	case "haschild":
		return false
	}
	return !c.must(nn)
}

// missing は haschild の要件のうち、kids(直下のフォルダ名)に無いもの。
func (c *crule) missing(kids []string) []string {
	var out []string
	for _, g := range c.groups {
		ok := false
		for _, k := range kids {
			nk := normN(k)
			for _, a := range g {
				if strings.Contains(nk, a) {
					ok = true
				}
			}
		}
		if !ok {
			out = append(out, strings.Join(g, "/"))
		}
	}
	return out
}

func (c *crule) msg(name string, depth int) string {
	if c.Must.Op == "forbid" {
		return fmt.Sprintf("%s: 「%s」は第%d階層には置けません", c.label(), name, depth)
	}
	return fmt.Sprintf("%s: 「%s」(第%d階層)が「%sこと」を満たしていません", c.label(), name, depth, condText(c.Must))
}

func (c *crule) missMsg(name string, miss []string) string {
	return fmt.Sprintf("%s: 「%s」の直下に「%s」のフォルダがありません", c.label(), name, strings.Join(miss, "」「"))
}

// ruleSet は有効なカスタムルールをコンパイルしたもの。
type ruleSet struct {
	r     Rules
	rules []*crule
	kids  bool // haschild のルールがある
	maxD  int  // 対象の最も深い階層(-1 = 上限なし)
}

func (r Rules) compiled() *ruleSet { return r.compile(false) }

// memoized は同じ名前の判定結果を覚えておく版(1つの処理の中だけで使う。大量の項目を調べるとき用)。
func (r Rules) memoized() *ruleSet { return r.compile(true) }

func memo(f func(string) bool) func(string) bool {
	if f == nil {
		return nil
	}
	m := map[string]bool{}
	return func(n string) bool {
		if v, ok := m[n]; ok {
			return v
		}
		v := f(n)
		if len(m) < 1<<20 {
			m[n] = v
		}
		return v
	}
}

func (r Rules) compile(useMemo bool) *ruleSet {
	rs := &ruleSet{r: r, maxD: 0}
	for _, c := range r.Custom {
		if c.Mode == "off" || c.Mode == "" {
			continue
		}
		cr, err := c.compile()
		if err != nil {
			continue // 保存時に検証済み
		}
		if useMemo {
			cr.when, cr.must = memo(cr.when), memo(cr.must)
		}
		rs.rules = append(rs.rules, cr)
		if cr.Must.Op == "haschild" {
			rs.kids = true
		}
		if md := cr.maxDepth(); md < 0 || rs.maxD < 0 {
			rs.maxD = -1
		} else if md > rs.maxD {
			rs.maxD = md
		}
	}
	return rs
}

// item は1項目のチェック(名前の要件のみ)。fn(mode, msg) に違反を渡す。
func (rs *ruleSet) item(isDir bool, depth int, name string, fn func(mode, msg string)) {
	if len(rs.rules) == 0 {
		return
	}
	nn := normN(name)
	for _, c := range rs.rules {
		if c.applies(isDir, depth, nn) && c.violates(nn) {
			fn(c.Mode, c.msg(name, depth))
		}
	}
}

// dirKids は haschild のチェック。
func (rs *ruleSet) dirKids(depth int, name string, kids []string, fn func(mode, msg string)) {
	if !rs.kids {
		return
	}
	nn := normN(name)
	for _, c := range rs.rules {
		if c.Must.Op == "haschild" && c.applies(true, depth, nn) {
			if m := c.missing(kids); len(m) > 0 {
				fn(c.Mode, c.missMsg(name, m))
			}
		}
	}
}

// needKids は depth 階層目の名前 name のフォルダに haschild のチェックが必要か。
func (rs *ruleSet) needKids(depth int, name string) bool {
	if !rs.kids {
		return false
	}
	nn := normN(name)
	for _, c := range rs.rules {
		if c.Must.Op == "haschild" && c.applies(true, depth, nn) {
			return true
		}
	}
	return false
}

// ---- 実フォルダの配下をまとめて調べる ----

// subtreeIssues は実フォルダ x を depth 階層目に置いた場合の、配下の項目の違反(ルールごとに件数と例)。
// 配下で個別にアクションを設定した項目(とその配下)は、x と一緒には移らないので除く。
func (s *Store) subtreeIssues(rs *ruleSet, x *Node, depth int, fn func(mode, msg string)) {
	if len(rs.rules) == 0 || !x.IsDir || x.End <= x.ID {
		return
	}
	q := `SELECT n.id, n.end_id, n.parent_id, n.is_dir, n.depth, n.name, COALESCE(p.action,'') FROM nodes n LEFT JOIN plan p ON p.node_id=n.id WHERE n.id>? AND n.id<=?`
	args := []any{x.ID, x.End}
	if rs.maxD >= 0 {
		if depth >= rs.maxD {
			return
		}
		q += ` AND n.depth<=?`
		args = append(args, x.Depth+rs.maxD-depth)
	}
	rows, err := s.DB.Query(q+` ORDER BY n.id`, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	type agg struct {
		mode string
		n    int
		ex   []string
	}
	byRule := map[string]*agg{}
	var order []string
	add := func(key, mode, name string) {
		a := byRule[key]
		if a == nil {
			a = &agg{mode: mode}
			byRule[key] = a
			order = append(order, key)
		}
		a.n++
		if len(a.ex) < 3 {
			a.ex = append(a.ex, name)
		}
	}
	kids := map[int64][]string{} // haschild の対象フォルダ → 直下のフォルダ名
	kidName := map[int64]string{}
	kidDepth := map[int64]int{}
	if rs.needKids(depth, x.Name) {
		kids[x.ID] = []string{}
		kidName[x.ID], kidDepth[x.ID] = x.Name, depth
	}
	var skip int64
	for rows.Next() {
		var id, end, parent int64
		var dir bool
		var d int
		var name, act string
		if rows.Scan(&id, &end, &parent, &dir, &d, &name, &act) != nil {
			return
		}
		if id <= skip {
			continue
		}
		if act != "" {
			skip = end
			continue
		}
		nd := depth + d - x.Depth
		if _, ok := kids[parent]; ok && dir {
			kids[parent] = append(kids[parent], name)
		}
		nn := normN(name)
		for _, c := range rs.rules {
			if c.applies(dir, nd, nn) && c.violates(nn) {
				add(c.label(), c.Mode, name)
			}
		}
		if dir && rs.needKids(nd, name) {
			kids[id] = []string{}
			kidName[id], kidDepth[id] = name, nd
		}
	}
	for _, key := range order {
		a := byRule[key]
		fn(a.mode, fmt.Sprintf("%s: フォルダの中の %d 件(%s など)が該当します", key, a.n, strings.Join(a.ex, "、")))
	}
	for id, ks := range kids {
		if id == x.ID {
			continue // x 自身は呼び出し側で判定(仮想フォルダ側の情報が必要なため)
		}
		rs.dirKids(kidDepth[id], kidName[id], ks, fn)
	}
}

// realKidDirs は実フォルダの直下のフォルダ名(個別にアクションを設定したものを除く)。
func (s *Store) realKidDirs(id int64) []string {
	rows, err := s.DB.Query(`SELECT n.name FROM nodes n LEFT JOIN plan p ON p.node_id=n.id WHERE n.parent_id=? AND n.is_dir=1 AND COALESCE(p.action,'')=''`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

// ---- 整理後の構成全体のルール違反 ----

// RuleViolation は整理後の構成のうち、5Sルールに合わないもの。
type RuleViolation struct {
	Path  string `json:"path"`
	Msg   string `json:"msg"`
	Block bool   `json:"block"`
}

// RuleViolations はルールを後から有効にした場合などに、整理後の構成全体の違反を洗い出す(キャッシュ)。
func (s *Store) RuleViolations() ([]RuleViolation, error) {
	// 大規模な構成では時間がかかるため、同時に1つだけ計算し、結果は次の編集までキャッシュする
	s.vviolMu.Lock()
	defer s.vviolMu.Unlock()
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.vviol != nil {
		v := s.vviol
		s.mu.Unlock()
		return v, nil
	}
	gen := s.gen
	r := s.Rules()
	rs := r.memoized()
	type vf struct {
		uuid, path, name string
		depth            int
		kids             []string
	}
	var vfs []vf
	var walk func(n *vnode)
	walk = func(n *vnode) {
		var ks []string
		for _, k := range n.kids {
			ks = append(ks, k.Name)
		}
		vfs = append(vfs, vf{n.UUID, s.vt.path(n.UUID), n.Name, n.Depth, ks})
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(s.vt.nodes[VRoot])
	s.mu.Unlock()
	out := []RuleViolation{}
	for _, v := range vfs {
		emit := func(path string) func(mode, msg string) {
			return func(mode, msg string) {
				if mode != "off" && mode != "" {
					out = append(out, RuleViolation{path, msg, mode == "block"})
				}
			}
		}
		if v.uuid != VRoot {
			var is Issue
			nameRuleIssues(&is, r, v.name, true)
			for _, m := range is.Blocks {
				out = append(out, RuleViolation{v.path, m, true})
			}
			for _, m := range is.Warns {
				out = append(out, RuleViolation{v.path, m, false})
			}
			if v.depth > r.MaxDepth {
				emit(v.path)(r.DepthMode, fmt.Sprintf("階層 %d が上限(%d)を超えています", v.depth, r.MaxDepth))
			}
			rs.item(true, v.depth, v.name, emit(v.path))
		}
		placed, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE p.action='move' AND p.vparent=?`, v.uuid)
		if err != nil {
			return nil, err
		}
		kids := v.kids
		for i := range placed {
			x := &placed[i]
			name := x.Name
			if x.NewName != "" {
				name = x.NewName
			}
			if x.IsDir {
				kids = append(kids, name)
			}
			path := joinV(v.path, name)
			var is Issue
			s.checkPlaceRules(&is, r, rs, x, v.depth+1, name)
			for _, m := range is.Blocks {
				out = append(out, RuleViolation{path, m, true})
			}
			for _, m := range is.Warns {
				out = append(out, RuleViolation{path, m, false})
			}
		}
		if v.uuid != VRoot {
			rs.dirKids(v.depth, v.name, kids, emit(v.path))
		}
	}
	s.mu.Lock()
	if s.gen == gen {
		s.vviol = out
	}
	s.mu.Unlock()
	return out, nil
}

// checkPlaceRules は項目 x を depth 階層目に名前 name で置く場合の、カスタムルールの違反を is に追加する。
func (s *Store) checkPlaceRules(is *Issue, r Rules, rs *ruleSet, x *Node, depth int, name string) {
	ap := func(mode, msg string) { apply(is, mode, msg) }
	rs.item(x.IsDir, depth, name, ap)
	if x.IsDir {
		if rs.needKids(depth, name) {
			rs.dirKids(depth, name, s.realKidDirs(x.ID), ap)
		}
		s.subtreeIssues(rs, x, depth, ap)
	}
}

// rowRule は仮想ツリーの1行の違反(表示用)。kids は haschild 用の直下のフォルダ名(nil なら調べない)。
func (rs *ruleSet) rowRule(isDir bool, depth int, name string, kids func() []string) []string {
	var out []string
	fn := func(mode, msg string) { out = append(out, msg) }
	rs.item(isDir, depth, name, fn)
	if isDir && kids != nil && rs.needKids(depth, name) {
		rs.dirKids(depth, name, kids(), fn)
	}
	return out
}

// ---- 現在のフォルダ構成への当てはめ ----

func (r Rules) hitsVersion() string {
	if !r.ApplyCurrent {
		return "off"
	}
	b, _ := json.Marshal(r)
	h := sha1.Sum(append(b, "rule_hits/v1"...))
	return hex.EncodeToString(h[:8])
}

// EnsureRuleHits は5Sルールを現在のフォルダ構成に当てはめた結果(rule_hits)を必要なら作り直す。
// 階層は「現在の階層 + CurrentOffset」で数える(スキャンしたフォルダが整理後のどの階層に当たるか)。
func (s *Store) EnsureRuleHits() error {
	r := s.Rules()
	ver := r.hitsVersion()
	var cur string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='rule_hits_ver'`).Scan(&cur)
	if cur == ver {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM rule_hits`); err != nil {
		return err
	}
	if r.ApplyCurrent {
		if err := s.computeRuleHits(tx, r); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES('rule_hits_ver',?)`, ver); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.tf, s.summary = nil, nil
	s.mu.Unlock()
	return nil
}

func (s *Store) computeRuleHits(tx *sql.Tx, r Rules) error {
	rs := r.memoized()
	rows, err := tx.Query(`SELECT id, COALESCE(parent_id,0), is_dir, depth, name, flags, child_count FROM nodes ORDER BY id`)
	if err != nil {
		return err
	}
	hits := map[int64][]string{}
	var order []int64
	add := func(id int64, msg string) {
		if hits[id] == nil {
			order = append(order, id)
		}
		hits[id] = append(hits[id], msg)
	}
	type kd struct {
		name  string
		depth int
		kids  []string
	}
	kids := map[int64]*kd{}
	for rows.Next() {
		var id, parent, cc int64
		var dir bool
		var d, flags int
		var name string
		if err := rows.Scan(&id, &parent, &dir, &d, &name, &flags, &cc); err != nil {
			rows.Close()
			return err
		}
		nd := d + r.CurrentOffset
		if k := kids[parent]; k != nil && dir {
			k.kids = append(k.kids, name)
		}
		if id == 1 {
			if nd <= 0 {
				continue // ルート(整理後のルートに当たる)は名前・配置を問わない
			}
			name = name[strings.LastIndexAny(strings.TrimRight(name, `\/`), `\/`)+1:] // ルートの名前はフルパスで記録されている
			name = strings.TrimRight(name, `\/`)
		}
		if r.DepthMode != "off" && nd > r.MaxDepth {
			add(id, fmt.Sprintf("階層 %d が上限(%d)を超えています", nd, r.MaxDepth))
		}
		if dir && r.ItemsMode != "off" && cc > int64(r.MaxItems) {
			add(id, fmt.Sprintf("直下の項目が %d 件あり、上限(%d)を超えています", cc, r.MaxItems))
		}
		if r.BadName != "off" && flags&(FlagBadChar|FlagTrailing|FlagReserved) != 0 {
			add(id, "名前に使えない文字・末尾の「.」や空白・予約語が含まれています")
		}
		if r.CopyName != "off" && flags&(FlagCopyName|FlagVersionName) != 0 {
			add(id, "「コピー」「(1)」「旧」「v2」など、コピーや版管理を表す名前です")
		}
		rs.item(dir, nd, name, func(_, msg string) { add(id, msg) })
		if dir && rs.needKids(nd, name) {
			kids[id] = &kd{name: name, depth: nd, kids: []string{}}
		}
	}
	rows.Close()
	for id, k := range kids {
		rs.dirKids(k.depth, k.name, k.kids, func(_, msg string) { add(id, msg) })
	}
	// まとめて書き込む(大規模なDBで違反が多い場合に速くするため)
	const batch = 400
	for i := 0; i < len(order); i += batch {
		j := min(i+batch, len(order))
		args := make([]any, 0, 2*(j-i))
		for _, id := range order[i:j] {
			args = append(args, id, strings.Join(hits[id], "\n"))
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO rule_hits VALUES`+strings.TrimSuffix(strings.Repeat("(?,?),", j-i), ","), args...); err != nil {
			return err
		}
	}
	return nil
}

// RuleHitCount は現在の構成で5Sルールに合わない項目の数。
func (s *Store) RuleHitCount() int64 {
	var n int64
	s.DB.QueryRow(`SELECT count(*) FROM rule_hits`).Scan(&n)
	return n
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
