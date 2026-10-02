package fsdb

import (
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store は構築済みDBを開いて閲覧・編集するためのラッパー。
type Store struct {
	DB   *sql.DB
	Path string

	mu         sync.Mutex
	summary    *Summary   // 静的部分のキャッシュ(ノードは構築後に変わらないため)
	pre        *prefix    // ID順のファイル数・サイズの累積(未処理件数の計算用)
	pidx       *planIndex // アクションの区間インデックス(nil=再構築が必要)
	vt         *vtree     // 仮想フォルダ構成のキャッシュ(nil=再構築が必要)
	rulesCache Rules      // 仮想ツリーを読み込んだときのルール
}

// Settings は後から変更できる判定閾値(meta テーブルに保存)。
type Settings struct {
	OldYears  int `json:"oldYears"`  // 更新から何年以上で「古い」とするか
	PathLimit int `json:"pathLimit"` // パス文字数の警告閾値
	DeepDepth int `json:"deepDepth"` // この階層以上を「深い」とする(ルート=階層0)
	ManyFiles int `json:"manyFiles"` // 1フォルダ直下の項目数の閾値
}

var DefaultSettings = Settings{OldYears: 3, PathLimit: 250, DeepDepth: 8, ManyFiles: 500}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='nodes'`).Scan(&n); err != nil || n == 0 {
		db.Close()
		if err == nil {
			err = fmt.Errorf("このファイルは本ツールのDBではありません")
		}
		return nil, err
	}
	if err := ensurePlanSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Path: path}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Meta() (map[string]string, error) {
	rows, err := s.DB.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (s *Store) SetMeta(kv map[string]string) error {
	for k, v := range kv {
		if _, err := s.DB.Exec(`INSERT OR REPLACE INTO meta VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Settings() Settings {
	st := DefaultSettings
	m, err := s.Meta()
	if err != nil {
		return st
	}
	get := func(k string, dst *int) {
		if v, err := strconv.Atoi(m[k]); err == nil && v > 0 {
			*dst = v
		}
	}
	get("old_years", &st.OldYears)
	get("path_limit", &st.PathLimit)
	get("deep_depth", &st.DeepDepth)
	get("many_files", &st.ManyFiles)
	return st
}

func (s *Store) SaveSettings(st Settings) error {
	for k, v := range map[string]int{"old_years": st.OldYears, "path_limit": st.PathLimit, "deep_depth": st.DeepDepth, "many_files": st.ManyFiles} {
		if v <= 0 {
			return fmt.Errorf("%s は1以上を指定してください", k)
		}
		if _, err := s.DB.Exec(`INSERT OR REPLACE INTO meta VALUES(?,?)`, k, strconv.Itoa(v)); err != nil {
			return err
		}
	}
	return nil
}

// Node はAPIで返す1行分。
type Node struct {
	ID       int64  `json:"id"`
	Parent   int64  `json:"p"`
	End      int64  `json:"e"`
	Depth    int    `json:"d"`
	IsDir    bool   `json:"dir"`
	Name     string `json:"n"`
	Ext      string `json:"x"`
	Size     int64  `json:"s"`
	Mtime    int64  `json:"m"`
	Children int64  `json:"cc"`
	Files    int64  `json:"fc"`
	Dirs     int64  `json:"dc"`
	PathLen  int    `json:"pl"`
	Flags    int    `json:"f"`
	Path     string `json:"path"`
	Err      string `json:"err,omitempty"` // 読み込みエラー(アクセス拒否など)。フォルダならサイズ・件数は不明

	// この項目自身に設定されたアクション
	Action  string   `json:"act"`   // delete | move | ""
	VParent string   `json:"vp"`    // 移動先の仮想フォルダ
	VPath   string   `json:"vpath"` // 移動先の仮想フォルダのパス(表示用)
	NewName string   `json:"nn"`    // 移動後の名前(Rename)
	Due     string   `json:"due"`   // 期限(YYYY-MM-DD)
	Memo    string   `json:"memo"`
	Editor  string   `json:"ed"` // 設定した作業者コード
	Tags    []string `json:"tags"`

	// 親フォルダから引き継いだアクション(自身に設定が無い場合)
	IAction string `json:"ia"`
	IVPath  string `json:"ivpath"`
	IFrom   int64  `json:"if"`

	// 未処理(アクション未設定)のファイル数・サイズ。フォルダは配下の合計、親で処理済みなら0
	Rem  int64 `json:"rem"`
	RemS int64 `json:"remS"`
	// 配下のうち、個別に別のアクションが設定された部分を除いたファイル数・サイズ(仮想ツリーでの件数)
	Inner  int64 `json:"inner"`
	InnerS int64 `json:"innerS"`
}

const nodeCols = `n.id, COALESCE(n.parent_id,0), n.end_id, n.depth, n.is_dir, n.name, n.ext, n.size, n.mtime,
 n.child_count, n.file_count, n.dir_count, n.path_len, n.flags, n.path,
 COALESCE(p.action,''), COALESCE(p.vparent,''), COALESCE(p.new_name,''), COALESCE(p.due,''), COALESCE(p.memo,''), COALESCE(p.editor,''),
 COALESCE((SELECT group_concat(g.tag, char(31)) FROM tags g WHERE g.node_id = n.id),''),
 COALESCE((SELECT message FROM node_errors e WHERE e.node_id = n.id),'')`
const nodeFrom = ` FROM nodes n LEFT JOIN plan p ON p.node_id = n.id `

func scanNode(rows *sql.Rows, x *Node) error {
	var tags string
	err := rows.Scan(&x.ID, &x.Parent, &x.End, &x.Depth, &x.IsDir, &x.Name, &x.Ext, &x.Size, &x.Mtime,
		&x.Children, &x.Files, &x.Dirs, &x.PathLen, &x.Flags, &x.Path,
		&x.Action, &x.VParent, &x.NewName, &x.Due, &x.Memo, &x.Editor, &tags, &x.Err)
	x.Tags = []string{}
	if tags != "" {
		x.Tags = strings.Split(tags, "\x1f")
	}
	return err
}

func (s *Store) query(q string, args ...any) ([]Node, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var x Node
		if err := scanNode(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.enrich(out)
}

func (s *Store) Node(id int64) (*Node, error) {
	ns, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE n.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(ns) == 0 {
		return nil, fmt.Errorf("id %d が見つかりません", id)
	}
	return &ns[0], nil
}

// Ancestors はルートから id の親までを返す。
func (s *Store) Ancestors(id int64) ([]Node, error) {
	return s.query(`WITH RECURSIVE a(id) AS (SELECT parent_id FROM nodes WHERE id=? UNION ALL
		SELECT nodes.parent_id FROM nodes JOIN a ON nodes.id=a.id WHERE nodes.parent_id IS NOT NULL)
		SELECT `+nodeCols+nodeFrom+` WHERE n.id IN (SELECT id FROM a) ORDER BY n.id`, id)
}

// Children は直下の項目を返す。hidePlanned=true ならアクション設定済みの項目を除く。
func (s *Store) Children(id int64, dirsOnly, hidePlanned bool) ([]Node, error) {
	q := `SELECT ` + nodeCols + nodeFrom + ` WHERE n.parent_id=?`
	if dirsOnly {
		q += ` AND n.is_dir=1`
	}
	if hidePlanned {
		q += ` AND COALESCE(p.action,'')=''`
	}
	return s.query(q+` ORDER BY n.id`, id)
}

// Subtree は id 配下を maxDepth 階層分まとめて返す(limit 超過時はエラー)。
// hidePlanned=true ならアクション設定済みの項目とその配下を除く。
func (s *Store) Subtree(id int64, maxDepth int, dirsOnly, hidePlanned bool, limit int) ([]Node, error) {
	nd, err := s.Node(id)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + nodeCols + nodeFrom + ` WHERE n.id>? AND n.id<=? AND n.depth<=?`
	if dirsOnly {
		q += ` AND n.is_dir=1`
	}
	ns, err := s.query(q+` ORDER BY n.id LIMIT ?`, id, nd.End, nd.Depth+maxDepth, limit+1)
	if err != nil {
		return nil, err
	}
	if len(ns) > limit {
		return nil, fmt.Errorf("展開する行数が多すぎます(%d行超)。展開する階層数を減らしてください", limit)
	}
	if hidePlanned {
		out := ns[:0]
		var skipEnd int64
		for _, x := range ns {
			if x.ID <= skipEnd {
				continue
			}
			if x.Action != "" {
				skipEnd = x.End
				continue
			}
			out = append(out, x)
		}
		ns = out
	}
	return ns, nil
}

func (s *Store) FindPath(p string) (int64, error) {
	p = strings.TrimRight(strings.TrimSpace(p), `\/`)
	var id int64
	err := s.DB.QueryRow(`SELECT id FROM nodes WHERE path=? OR path=? LIMIT 1`, p, p+`\`).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("パスが見つかりません: %s", p)
	}
	return id, err
}

// ---- 検索(リスト表示・CSV出力・一括編集で共通のフィルタ) ----

// Filter は検索条件。URLクエリから組み立てる。
type Filter struct {
	Q       string
	Kind    string // file | dir
	Ext     string
	Under   int64
	MinSize int64
	MaxSize int64
	Before  int64  // mtime がこれより前(UNIX秒)
	Check   string // 警告種別
	State   string // 処理状況: unhandled | handled | delete | move | own
	Editor  string
	Tag     string
	VParent string // 移動先の仮想フォルダ(その配下を含む)
	Sort    string
	Desc    bool
}

func FilterFromQuery(v url.Values) Filter {
	i64 := func(k string) int64 { n, _ := strconv.ParseInt(v.Get(k), 10, 64); return n }
	return Filter{
		Q: strings.TrimSpace(v.Get("q")), Kind: v.Get("kind"), Ext: strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v.Get("ext")), ".")),
		Under: i64("under"), MinSize: i64("minSize"), MaxSize: i64("maxSize"), Before: i64("before"),
		Check: v.Get("check"), State: v.Get("state"), Editor: v.Get("editor"), Tag: v.Get("tag"), VParent: v.Get("vparent"),
		Sort: v.Get("sort"), Desc: v.Get("desc") == "1",
	}
}

// CheckDefs は警告種別の一覧(UI表示順)。
var CheckDefs = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
}{
	{"old", "更新から一定年数以上", "整理"},
	{"dup", "重複候補(同名・同サイズ)", "整理"},
	{"temp", "一時・システムファイル", "整理"},
	{"empty", "空フォルダ", "整理"},
	{"copyname", "コピー的な名称", "整頓"},
	{"version", "版管理的な名称(旧/old/v2等)", "整頓"},
	{"singlechild", "中身がフォルダ1つだけ", "整頓"},
	{"deep", "階層が深い", "整頓"},
	{"many", "直下の項目が多すぎる", "整頓"},
	{"badchar", "禁止文字", "移行(SharePoint)"},
	{"trailing", "末尾が「.」かスペース", "移行(SharePoint)"},
	{"reserved", "予約語", "移行(SharePoint)"},
	{"longpath", "パスが長い", "移行(SharePoint)"},
	{"access", "アクセス不可", "移行(SharePoint)"},
}

func checkCond(key string, st Settings) (string, []any) {
	flag := map[string]int{"dup": FlagDup, "temp": FlagTempFile, "empty": FlagEmptyDir, "copyname": FlagCopyName,
		"version": FlagVersionName, "singlechild": FlagSingleChild, "badchar": FlagBadChar, "trailing": FlagTrailing,
		"reserved": FlagReserved, "access": FlagAccess}
	if f, ok := flag[key]; ok {
		return "n.flags & ? != 0", []any{f}
	}
	switch key {
	case "old":
		return "n.is_dir=0 AND n.mtime>0 AND n.mtime<?", []any{time.Now().AddDate(-st.OldYears, 0, 0).Unix()}
	case "deep":
		return "n.depth>=?", []any{st.DeepDepth}
	case "many":
		return "n.is_dir=1 AND n.child_count>?", []any{st.ManyFiles}
	case "longpath":
		return "n.path_len>?", []any{st.PathLimit}
	case "migration":
		return "(n.flags & ? != 0 OR n.path_len>?)", []any{FlagBadChar | FlagTrailing | FlagReserved | FlagAccess, st.PathLimit}
	}
	return "", nil
}

// coveredExpr は「自身または祖先にアクションが設定されている」ことを表すSQL式(plan_cover は互いに素な区間)。
const coveredExpr = `COALESCE((SELECT c.e FROM plan_cover c WHERE c.s <= n.id ORDER BY c.s DESC LIMIT 1), 0) >= n.id`

func (s *Store) where(f Filter) (string, []any, error) {
	st := s.Settings()
	conds := []string{"1=1"}
	args := []any{}
	add := func(c string, a ...any) { conds = append(conds, c); args = append(args, a...) }
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if strings.ContainsAny(q, `\/`) {
			add("instr(lower(n.path), ?) > 0", q)
		} else {
			add("instr(n.name_lc, ?) > 0", q)
		}
	}
	switch f.Kind {
	case "file":
		add("n.is_dir=0")
	case "dir":
		add("n.is_dir=1")
	}
	if f.Ext != "" {
		if f.Ext == "(なし)" {
			add("n.is_dir=0 AND n.ext=''")
		} else {
			add("n.ext=?", f.Ext)
		}
	}
	if f.Under > 0 {
		var end int64
		if err := s.DB.QueryRow(`SELECT end_id FROM nodes WHERE id=?`, f.Under).Scan(&end); err != nil {
			return "", nil, fmt.Errorf("配下指定のフォルダが見つかりません")
		}
		add("n.id>? AND n.id<=?", f.Under, end)
	}
	if f.MinSize > 0 {
		add("n.size>=?", f.MinSize)
	}
	if f.MaxSize > 0 {
		add("n.size<=?", f.MaxSize)
	}
	if f.Before > 0 {
		add("n.mtime>0 AND n.mtime<?", f.Before)
	}
	if f.Check != "" {
		c, a := checkCond(f.Check, st)
		if c == "" {
			return "", nil, fmt.Errorf("不明な警告種別: %s", f.Check)
		}
		add(c, a...)
	}
	switch f.State {
	case "":
	case "unhandled":
		add("NOT (" + coveredExpr + ")")
	case "handled":
		add(coveredExpr)
	case "own":
		add("(p.action IS NOT NULL AND (p.action!='' OR p.memo!='' OR p.due!=''))")
	case ActDelete, ActMove:
		add("p.action=?", f.State)
	default:
		return "", nil, fmt.Errorf("不明な処理状況: %s", f.State)
	}
	if f.Editor != "" {
		add("p.editor=?", f.Editor)
	}
	if f.Tag != "" {
		add("EXISTS (SELECT 1 FROM tags g WHERE g.node_id=n.id AND g.tag=?)", f.Tag)
	}
	if f.VParent != "" {
		ids := s.vSubtreeIDs(f.VParent)
		if len(ids) == 0 {
			return "", nil, fmt.Errorf("仮想フォルダが見つかりません")
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		a := make([]any, len(ids))
		for i, x := range ids {
			a[i] = x
		}
		add("p.action='move' AND p.vparent IN ("+ph+")", a...)
	}
	return strings.Join(conds, " AND "), args, nil
}

func orderBy(f Filter) string {
	col := map[string]string{"name": "n.name_lc", "size": "n.size", "mtime": "n.mtime", "path": "n.path", "depth": "n.depth",
		"pathlen": "n.path_len", "ext": "n.ext", "kind": "n.is_dir", "files": "n.file_count", "children": "n.child_count",
		"action": "p.action", "editor": "p.editor", "due": "p.due"}[f.Sort]
	if col == "" {
		col = "n.id"
	}
	dir := " ASC"
	if f.Desc {
		dir = " DESC"
	}
	return " ORDER BY " + col + dir + ", n.id"
}

func (s *Store) Search(f Filter, offset, limit int) ([]Node, int64, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, 0, err
	}
	w, args, err := s.where(f)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := s.DB.QueryRow(`SELECT count(*)`+nodeFrom+` WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	ns, err := s.query(`SELECT `+nodeCols+nodeFrom+` WHERE `+w+orderBy(f)+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	return ns, total, err
}

// SearchIDs は検索結果のIDだけを返す(一括設定用。max件を超えたらエラー)。
func (s *Store) SearchIDs(f Filter, max int) ([]int64, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	w, args, err := s.where(f)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(`SELECT n.id`+nodeFrom+` WHERE `+w+` ORDER BY n.id LIMIT ?`, append(args, max+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) > max {
		return nil, fmt.Errorf("対象が多すぎます(%d件超)。条件を絞り込んでください", max)
	}
	return ids, rows.Err()
}

// SearchEach は検索結果を1行ずつ fn に渡す(CSV出力用)。
func (s *Store) SearchEach(f Filter, fn func(*Node) error) error {
	if err := s.ensureIndex(); err != nil {
		return err
	}
	w, args, err := s.where(f)
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT `+nodeCols+nodeFrom+` WHERE `+w+orderBy(f), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	batch := make([]Node, 0, 1000)
	flush := func() error {
		if err := s.enrich(batch); err != nil {
			return err
		}
		for i := range batch {
			if err := fn(&batch[i]); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var x Node
		if err := scanNode(rows, &x); err != nil {
			return err
		}
		if batch = append(batch, x); len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}
