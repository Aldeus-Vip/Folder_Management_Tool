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

	mu      sync.Mutex
	summary *Summary // 静的部分のキャッシュ(ノードは構築後に変わらないため)
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
	Action   string `json:"act"`
	Owner    string `json:"own"`
	NewName  string `json:"nn"`
	Dest     string `json:"dst"`
	Memo     string `json:"memo"`
}

const nodeCols = `n.id, COALESCE(n.parent_id,0), n.end_id, n.depth, n.is_dir, n.name, n.ext, n.size, n.mtime,
 n.child_count, n.file_count, n.dir_count, n.path_len, n.flags, n.path,
 COALESCE(t.action,''), COALESCE(t.owner,''), COALESCE(t.new_name,''), COALESCE(t.dest,''), COALESCE(t.memo,'')`
const nodeFrom = ` FROM nodes n LEFT JOIN notes t ON t.node_id = n.id `

func scanNodes(rows *sql.Rows) ([]Node, error) {
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var x Node
		if err := rows.Scan(&x.ID, &x.Parent, &x.End, &x.Depth, &x.IsDir, &x.Name, &x.Ext, &x.Size, &x.Mtime,
			&x.Children, &x.Files, &x.Dirs, &x.PathLen, &x.Flags, &x.Path,
			&x.Action, &x.Owner, &x.NewName, &x.Dest, &x.Memo); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) query(q string, args ...any) ([]Node, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
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

func (s *Store) Children(id int64, dirsOnly bool) ([]Node, error) {
	q := `SELECT ` + nodeCols + nodeFrom + ` WHERE n.parent_id=?`
	if dirsOnly {
		q += ` AND n.is_dir=1`
	}
	return s.query(q+` ORDER BY n.id`, id)
}

// Subtree は id 配下を maxDepth 階層分まとめて返す(limit 超過時はエラー)。
func (s *Store) Subtree(id int64, maxDepth int, dirsOnly bool, limit int) ([]Node, error) {
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
	Action  string // アクション(any=何か設定済, none=未設定)
	Owner   string
	Sort    string
	Desc    bool
}

func FilterFromQuery(v url.Values) Filter {
	i64 := func(k string) int64 { n, _ := strconv.ParseInt(v.Get(k), 10, 64); return n }
	return Filter{
		Q: strings.TrimSpace(v.Get("q")), Kind: v.Get("kind"), Ext: strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v.Get("ext")), ".")),
		Under: i64("under"), MinSize: i64("minSize"), MaxSize: i64("maxSize"), Before: i64("before"),
		Check: v.Get("check"), Action: v.Get("action"), Owner: v.Get("owner"),
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
	switch f.Action {
	case "":
	case "any":
		add("t.action IS NOT NULL AND (t.action!='' OR t.owner!='' OR t.memo!='')")
	case "none":
		add("(t.action IS NULL OR t.action='')")
	default:
		add("t.action=?", f.Action)
	}
	if f.Owner != "" {
		add("t.owner=?", f.Owner)
	}
	return strings.Join(conds, " AND "), args, nil
}

func orderBy(f Filter) string {
	col := map[string]string{"name": "n.name_lc", "size": "n.size", "mtime": "n.mtime", "path": "n.path", "depth": "n.depth",
		"pathlen": "n.path_len", "ext": "n.ext", "kind": "n.is_dir", "files": "n.file_count", "children": "n.child_count",
		"action": "t.action", "owner": "t.owner"}[f.Sort]
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

// SearchRange は検索結果を1行ずつ fn に渡す(CSV出力用)。
func (s *Store) SearchEach(f Filter, fn func(*Node) error) error {
	w, args, err := s.where(f)
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT `+nodeCols+nodeFrom+` WHERE `+w+orderBy(f), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x Node
		if err := rows.Scan(&x.ID, &x.Parent, &x.End, &x.Depth, &x.IsDir, &x.Name, &x.Ext, &x.Size, &x.Mtime,
			&x.Children, &x.Files, &x.Dirs, &x.PathLen, &x.Flags, &x.Path,
			&x.Action, &x.Owner, &x.NewName, &x.Dest, &x.Memo); err != nil {
			return err
		}
		if err := fn(&x); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---- 注記(アクション・担当・メモ等の編集) ----

// NoteFields は更新する項目。nil の項目は変更しない。
type NoteFields struct {
	Action  *string `json:"action"`
	Owner   *string `json:"owner"`
	NewName *string `json:"newName"`
	Dest    *string `json:"dest"`
	Memo    *string `json:"memo"`
}

func (nf NoteFields) sets() (string, []any) {
	cols := []string{}
	args := []any{}
	for _, c := range []struct {
		col string
		v   *string
	}{{"action", nf.Action}, {"owner", nf.Owner}, {"new_name", nf.NewName}, {"dest", nf.Dest}, {"memo", nf.Memo}} {
		if c.v != nil {
			cols = append(cols, c.col)
			args = append(args, strings.TrimSpace(*c.v))
		}
	}
	return strings.Join(cols, ","), args
}

func upsertSQL(cols string, selectFrom string) string {
	cs := strings.Split(cols, ",")
	ph := make([]string, len(cs))
	upd := make([]string, len(cs))
	for i, c := range cs {
		ph[i] = "?"
		upd[i] = c + "=excluded." + c
	}
	return `INSERT INTO notes(node_id, path, updated_at, ` + cols + `) SELECT n.id, n.path, ?, ` + strings.Join(ph, ",") +
		` ` + selectFrom + ` ON CONFLICT(node_id) DO UPDATE SET updated_at=excluded.updated_at, ` + strings.Join(upd, ",")
}

const cleanupNotes = `DELETE FROM notes WHERE action='' AND owner='' AND new_name='' AND dest='' AND memo=''`

// SetNotes は指定IDの注記を更新する。
func (s *Store) SetNotes(ids []int64, nf NoteFields) (int64, error) {
	cols, vals := nf.sets()
	if cols == "" || len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(upsertSQL(cols, `FROM nodes n WHERE n.id=?`))
	if err != nil {
		return 0, err
	}
	now := time.Now().Format("2006/01/02 15:04:05")
	var cnt int64
	for _, id := range ids {
		r, err := stmt.Exec(append(append([]any{now}, vals...), id)...)
		if err != nil {
			return 0, err
		}
		c, _ := r.RowsAffected()
		cnt += c
	}
	stmt.Close()
	if _, err := tx.Exec(cleanupNotes); err != nil {
		return 0, err
	}
	return cnt, tx.Commit()
}

// SetNotesByFilter は検索条件に一致する全項目の注記を一括更新する。
func (s *Store) SetNotesByFilter(f Filter, nf NoteFields) (int64, error) {
	cols, vals := nf.sets()
	if cols == "" {
		return 0, nil
	}
	w, args, err := s.where(f)
	if err != nil {
		return 0, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Format("2006/01/02 15:04:05")
	all := append(append([]any{now}, vals...), args...)
	r, err := tx.Exec(upsertSQL(cols, nodeFrom+` WHERE `+w), all...)
	if err != nil {
		return 0, err
	}
	cnt, _ := r.RowsAffected()
	if _, err := tx.Exec(cleanupNotes); err != nil {
		return 0, err
	}
	return cnt, tx.Commit()
}

// ImportNotes は別のDB(前回スキャン等)の注記を、フルパスが一致する項目へ引き継ぐ。
func (s *Store) ImportNotes(otherDB string) (int64, error) {
	if _, err := s.DB.Exec(`CREATE INDEX IF NOT EXISTS nodes_path ON nodes(path)`); err != nil {
		return 0, err
	}
	conn, err := s.DB.Conn(ctxBG)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctxBG, `ATTACH DATABASE ? AS other`, otherDB); err != nil {
		return 0, err
	}
	defer conn.ExecContext(ctxBG, `DETACH DATABASE other`)
	r, err := conn.ExecContext(ctxBG, `INSERT INTO main.notes(node_id, path, action, owner, new_name, dest, memo, updated_at)
		SELECT n.id, n.path, o.action, o.owner, o.new_name, o.dest, o.memo, o.updated_at
		FROM other.notes o JOIN main.nodes n ON n.path = o.path WHERE true
		ON CONFLICT(node_id) DO UPDATE SET action=excluded.action, owner=excluded.owner, new_name=excluded.new_name,
		  dest=excluded.dest, memo=excluded.memo, updated_at=excluded.updated_at`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
