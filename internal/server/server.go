// Package server はブラウザUIとJSON APIを提供するローカルHTTPサーバー。
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
	"github.com/aldeus-vip/folder_management_tool/internal/ingest"
	"github.com/aldeus-vip/folder_management_tool/internal/osutil"
)

//go:embed web
var webFS embed.FS

type App struct {
	ProjectsDir string
	Version     string
	Shutdown    func()

	token    string
	lastBeat atomic.Int64 // 画面(ブラウザ)からの最終ハートビート(UnixNano)
	byeAt    atomic.Int64 // タブを閉じた通知の時刻(その後ハートビートが無ければ猶予後に終了)
	mu       sync.RWMutex
	store    *fsdb.Store
	job      *Job
	analysis *fsdb.MergeAnalysis // アクション統合の比較結果(適用待ち)
	recentMu sync.Mutex
}

// Job はバックグラウンドで実行中のスキャン・統合。
type Job struct {
	mu       sync.Mutex
	Kind     string
	Phase    string
	Done     int64
	Total    int64
	Started  time.Time
	Finished bool
	Err      string
	Result   *ingest.Result
	DBPath   string
	cancel   context.CancelFunc
}

func New(projectsDir, version string) *App {
	b := make([]byte, 16)
	rand.Read(b)
	return &App{ProjectsDir: projectsDir, Version: version, token: hex.EncodeToString(b)}
}

type apiFunc = func(*http.Request) (any, error)

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			b, _ := fs.ReadFile(sub, "index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(strings.Replace(string(b), "__TOKEN__", a.token, 1)))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	})
	api := map[string]apiFunc{
		// アプリ・ファイル
		"GET /api/state":             a.apiState,
		"GET /api/recent":            a.apiRecent,
		"POST /api/open":             a.apiOpen,
		"POST /api/close":            a.apiClose,
		"POST /api/dialog":           a.apiDialog,
		"POST /api/shutdown":         a.apiShutdown,
		"POST /api/heartbeat":        a.apiHeartbeat,
		"POST /api/bye":              a.apiBye,
		"POST /api/scan":             a.apiScan,
		"POST /api/merge":            a.apiMerge,
		"POST /api/dbinfo":           a.apiDBInfo,
		"GET /api/job":               a.apiJob,
		"POST /api/job/cancel":       a.apiJobCancel,
		"POST /api/workcopy":         a.withStore(a.apiWorkCopy),
		"POST /api/setcode":          a.withStore(a.apiSetCode),
		"POST /api/actmerge/analyze": a.apiActMergeAnalyze,
		"POST /api/actmerge/apply":   a.apiActMergeApply,
		// 閲覧
		"GET /api/summary":   a.withStore(a.apiSummary),
		"GET /api/progress":  a.withStore(a.apiProgress),
		"POST /api/settings": a.withStore(a.apiSettings),
		"POST /api/rules":    a.withStore(a.apiRules),
		"GET /api/node":      a.withStore(a.apiNode),
		"GET /api/children":  a.withStore(a.apiChildren),
		"GET /api/subtree":   a.withStore(a.apiSubtree),
		"GET /api/find":      a.withStore(a.apiFind),
		"GET /api/search":    a.withStore(a.apiSearch),
		"GET /api/exts":      a.withStore(a.apiExts),
		"GET /api/dups":      a.withStore(a.apiDups),
		"GET /api/tags":      a.withStore(a.apiTags),
		"POST /api/hash":     a.withStore(a.apiHash),
		"POST /api/reveal":   a.withStore(a.apiReveal),
		// 仮想フォルダ構成(整理後)
		"GET /api/vnode":     a.withStore(a.apiVNode),
		"GET /api/vchildren": a.withStore(a.apiVChildren),
		"GET /api/vreal":     a.withStore(a.apiVReal),
		"GET /api/vtree":     a.withStore(a.apiVTree),
		"GET /api/vwarnings": a.withStore(a.apiVWarnings),
		"POST /api/vcreate":  a.withEdit(a.apiVCreate),
		"POST /api/vrename":  a.withEdit(a.apiVRename),
		"POST /api/vmemo":    a.withEdit(a.apiVMemo),
		"POST /api/vmove":    a.withEdit(a.apiVMove),
		"POST /api/vdelete":  a.withEdit(a.apiVDelete),
		// アクション・タグの編集
		"POST /api/plan/delete": a.withEdit(a.apiPlanDelete),
		"POST /api/plan/move":   a.withEdit(a.apiPlanMove),
		"POST /api/plan/clear":  a.withEdit(a.apiPlanClear),
		"POST /api/plan/fields": a.withEdit(a.apiPlanFields),
		"POST /api/plan/filter": a.withEdit(a.apiPlanFilter),
		"POST /api/tags":        a.withEdit(a.apiSetTags),
	}
	for pat, fn := range api {
		mux.HandleFunc(pat, a.jsonHandler(fn))
	}
	// 二重起動の確認用(トークン不要・情報は返さない)
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "FolderManager") })
	mux.HandleFunc("GET /api/export/list.csv", a.download(func(w io.Writer, r *http.Request, s *fsdb.Store) error {
		return s.WriteCSV(w, fsdb.FilterFromQuery(r.URL.Query()))
	}))
	mux.HandleFunc("GET /api/export/plan.ps1", a.download(func(w io.Writer, r *http.Request, s *fsdb.Store) error {
		return s.WritePlanScript(w)
	}))
	return a.guard(mux)
}

// guard はローカルの他サイトからのAPI呼び出し(CSRF・DNSリバインディング)を防ぐ。
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			tok := r.Header.Get("X-Token")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if tok != a.token {
				http.Error(w, "invalid token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func badRequest(f string, a ...any) error { return httpError{400, fmt.Sprintf(f, a...)} }

func (a *App) jsonHandler(fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			code := 500
			var he httpError
			if errors.As(err, &he) {
				code = he.code
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
}

func (a *App) current() *fsdb.Store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store
}

func (a *App) withStore(fn func(*http.Request, *fsdb.Store) (any, error)) apiFunc {
	return func(r *http.Request) (any, error) {
		s := a.current()
		if s == nil {
			return nil, httpError{409, "DBが開かれていません。「ファイル」→「開く」でDBを開くか、データ取り込みを実行してください"}
		}
		return fn(r, s)
	}
}

// errNeedCode は作業者コードが無いDBを編集しようとしたとき(UIはコード設定を促す)。
const errNeedCode = "NEED_CODE"

// withEdit は編集系API。作業者コード(誰の変更かを記録するため)が必要。
func (a *App) withEdit(fn func(*http.Request, *fsdb.Store, string) (any, error)) apiFunc {
	return a.withStore(func(r *http.Request, s *fsdb.Store) (any, error) {
		code := s.EditorCode()
		if code == "" {
			return nil, httpError{428, errNeedCode}
		}
		return fn(r, s, code)
	})
}

func (a *App) download(fn func(io.Writer, *http.Request, *fsdb.Store) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.current()
		if s == nil {
			http.Error(w, "DBが開かれていません", 409)
			return
		}
		name := filepath.Base(r.URL.Path)
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(filepath.Base(s.Path), filepath.Ext(s.Path))
		fname := fmt.Sprintf("%s_%s_%s%s", stem, strings.TrimSuffix(name, ext), time.Now().Format("20060102_150405"), ext)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEscape(fname))
		if err := fn(w, r, s); err != nil {
			fmt.Fprintf(w, "\n\nERROR: %v\n", err)
		}
	}
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest("リクエストが不正です: %v", err)
	}
	return nil
}

func qInt(r *http.Request, k string, def int64) int64 {
	if v, err := strconv.ParseInt(r.URL.Query().Get(k), 10, 64); err == nil {
		return v
	}
	return def
}

func cleanPath(p string) string { return strings.Trim(strings.TrimSpace(p), `"`) }

// ---- DBの開閉・最近使ったファイル ----

func (a *App) apiState(r *http.Request) (any, error) {
	a.mu.RLock()
	s, job := a.store, a.job
	a.mu.RUnlock()
	out := map[string]any{"version": a.Version, "projectsDir": a.ProjectsDir, "checks": fsdb.CheckDefs}
	if s != nil {
		meta, _ := s.Meta()
		out["db"] = map[string]any{"path": s.Path, "meta": meta, "settings": s.Settings(), "rules": s.Rules(), "code": s.EditorCode()}
	}
	if job != nil {
		out["job"] = job.snapshot()
	}
	return out, nil
}

func (a *App) recentFile() string { return filepath.Join(a.ProjectsDir, "recent.json") }

func (a *App) readRecent() []string {
	var list []string
	b, err := os.ReadFile(a.recentFile())
	if err == nil {
		json.Unmarshal(b, &list)
	}
	return list
}

func (a *App) addRecent(path string) {
	a.recentMu.Lock()
	defer a.recentMu.Unlock()
	abs, _ := filepath.Abs(path)
	list := []string{abs}
	for _, p := range a.readRecent() {
		if !strings.EqualFold(p, abs) && len(list) < 15 {
			list = append(list, p)
		}
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	os.MkdirAll(a.ProjectsDir, 0o755)
	os.WriteFile(a.recentFile(), b, 0o644)
}

func (a *App) apiRecent(r *http.Request) (any, error) {
	out := []map[string]any{}
	for _, p := range a.readRecent() {
		item := map[string]any{"path": p, "name": filepath.Base(p)}
		if st, err := os.Stat(p); err == nil {
			item["modified"] = st.ModTime().Format("2006/01/02 15:04")
			item["size"] = st.Size()
		} else {
			item["missing"] = true
		}
		out = append(out, item)
	}
	return out, nil
}

func (a *App) openStore(path string) error {
	s, err := fsdb.Open(path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	old := a.store
	a.store = s
	a.mu.Unlock()
	if old != nil {
		old.Close()
	}
	a.addRecent(path)
	go s.Summary() // 初回のサマリー集計を先に済ませておく(大規模DBで数秒かかるため)
	return nil
}

func (a *App) closeStore() {
	a.mu.Lock()
	old := a.store
	a.store = nil
	a.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// OpenInitial は起動引数で渡されたDBを開く。
func (a *App) OpenInitial(path string) error { return a.openStore(path) }

func (a *App) apiOpen(r *http.Request) (any, error) {
	var req struct{ Path string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p := cleanPath(req.Path)
	if _, err := os.Stat(p); err != nil {
		return nil, badRequest("ファイルが見つかりません: %s", p)
	}
	if err := a.openStore(p); err != nil {
		return nil, badRequest("DBを開けません: %v", err)
	}
	return a.apiState(r)
}

func (a *App) apiClose(r *http.Request) (any, error) {
	a.closeStore()
	return a.apiState(r)
}

func (a *App) apiDialog(r *http.Request) (any, error) {
	var req struct{ Kind, Initial string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p, err := osutil.Dialog(osutil.DialogKind(req.Kind), req.Initial)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"path": p, "paths": strings.FieldsFunc(p, func(c rune) bool { return c == '\n' || c == '\r' })}, nil
}

// resolveDBPath は保存先DBパスを決める(名前だけなら projects フォルダに置く)。
func (a *App) resolveDBPath(name, fallback string) (string, error) {
	name = cleanPath(name)
	if name == "" {
		name = fallback
	}
	if !strings.EqualFold(filepath.Ext(name), ".db") {
		name += ".db"
	}
	if !filepath.IsAbs(name) && !strings.ContainsAny(name, `\/`) {
		if err := os.MkdirAll(a.ProjectsDir, 0o755); err != nil {
			return "", err
		}
		name = filepath.Join(a.ProjectsDir, name)
	}
	return name, nil
}

func samePath(a, b string) bool {
	x, _ := filepath.Abs(a)
	y, _ := filepath.Abs(b)
	return strings.EqualFold(x, y)
}

// ---- バックグラウンド処理(スキャン・統合) ----

func (a *App) startJob(kind, dbPath string, run func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error)) (any, error) {
	a.mu.Lock()
	if a.job != nil && !a.job.Finished {
		a.mu.Unlock()
		return nil, httpError{409, "別の処理が実行中です"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{Kind: kind, Phase: "開始", Started: time.Now(), DBPath: dbPath, cancel: cancel}
	a.job = job
	reopen := a.store != nil && samePath(a.store.Path, dbPath)
	a.mu.Unlock()
	if reopen {
		a.closeStore() // 上書き対象のDBを開いている場合は閉じる(Windowsでは開いたまま置換できない)
	}
	go func() {
		res, err := run(ctx, func(phase string, done, total int64) {
			job.mu.Lock()
			job.Phase, job.Done, job.Total = phase, done, total
			job.mu.Unlock()
		})
		if err == nil {
			err = a.openStore(dbPath)
		}
		job.mu.Lock()
		job.Finished = true
		job.Result = res
		if err != nil {
			if errors.Is(err, context.Canceled) {
				job.Err = "キャンセルしました"
			} else {
				job.Err = err.Error()
			}
		} else {
			job.Phase = "完了"
		}
		job.mu.Unlock()
	}()
	return job.snapshot(), nil
}

func (j *Job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"kind": j.Kind, "phase": j.Phase, "done": j.Done, "total": j.Total, "finished": j.Finished,
		"error": j.Err, "result": j.Result, "dbPath": j.DBPath, "elapsed": time.Since(j.Started).Round(time.Second).String()}
}

func (a *App) apiScan(r *http.Request) (any, error) {
	var req struct {
		Root    string
		DB      string
		Workers int
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	root := cleanPath(req.Root)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, badRequest("フォルダが見つかりません: %s", root)
	}
	base := filepath.Base(filepath.Clean(root))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "scan"
	}
	db, err := a.resolveDBPath(req.DB, strings.Trim(base, `:\`)+"_"+time.Now().Format("20060102_1504"))
	if err != nil {
		return nil, err
	}
	return a.startJob("scan", db, func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error) {
		return ingest.Scan(ctx, root, db, req.Workers, prog)
	})
}

func (a *App) apiMerge(r *http.Request) (any, error) {
	var req struct {
		DBs    []string
		DB     string
		Prefer string // newest | order
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	var dbs []string
	for _, p := range req.DBs {
		if p = cleanPath(p); p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return nil, badRequest("DBが見つかりません: %s", p)
		}
		dbs = append(dbs, p)
	}
	if len(dbs) < 2 {
		return nil, badRequest("統合するDBを2つ以上指定してください")
	}
	db, err := a.resolveDBPath(req.DB, "merged_"+time.Now().Format("20060102_1504"))
	if err != nil {
		return nil, err
	}
	for _, p := range dbs {
		if samePath(p, db) {
			return nil, badRequest("保存先に統合元と同じDBは指定できません")
		}
	}
	return a.startJob("merge", db, func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error) {
		return ingest.Merge(ctx, dbs, db, req.Prefer != "order", prog)
	})
}

// apiDBInfo は統合候補DBのルート・取得日時・作業者コードを返す(画面の一覧表示用)。
func (a *App) apiDBInfo(r *http.Request) (any, error) {
	var req struct{ Paths []string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out := []map[string]string{}
	for _, p := range req.Paths {
		p = cleanPath(p)
		s, err := fsdb.Open(p)
		if err != nil {
			out = append(out, map[string]string{"path": p, "error": err.Error()})
			continue
		}
		m, _ := s.Meta()
		code := s.EditorCode()
		s.Close()
		date := m["scanned_at"]
		for _, k := range []string{"merged_at", "built_at"} {
			if date == "" {
				date = m[k]
			}
		}
		out = append(out, map[string]string{"path": p, "root": m["root"], "date": date, "source": m["source"], "code": code,
			"masterId": m["master_id"], "masterRev": m["master_rev"]})
	}
	return out, nil
}

func (a *App) apiJob(r *http.Request) (any, error) {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job == nil {
		return map[string]any{}, nil
	}
	return job.snapshot(), nil
}

func (a *App) apiJobCancel(r *http.Request) (any, error) {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job != nil {
		job.cancel()
	}
	return map[string]any{}, nil
}

// ---- 自動終了(コンソールを出さないため、画面が閉じられたら終了する) ----

const byeGrace = 15 * time.Second // タブを閉じてから終了するまで(再読み込みなら直後のハートビートで取り消し)

func (a *App) apiHeartbeat(r *http.Request) (any, error) {
	a.lastBeat.Store(time.Now().UnixNano())
	return map[string]any{}, nil
}

// apiBye はタブを閉じたとき(pagehide)に送られる。猶予後に終了させる。
func (a *App) apiBye(r *http.Request) (any, error) {
	a.byeAt.Store(time.Now().UnixNano())
	return map[string]any{}, nil
}

// WatchIdle は画面からのハートビートが timeout 以上途絶えたら onIdle を呼ぶ。
// スキャン・統合の実行中は終了しない。
func (a *App) WatchIdle(timeout time.Duration, onIdle func()) {
	a.lastBeat.Store(time.Now().UnixNano()) // 起動直後はブラウザが開くまでの猶予
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			a.mu.RLock()
			busy := a.job != nil && !a.job.Finished
			a.mu.RUnlock()
			if busy {
				continue
			}
			last := a.lastBeat.Load()
			idle := time.Since(time.Unix(0, last)) > timeout
			if bye := a.byeAt.Load(); bye > last && time.Since(time.Unix(0, bye)) > byeGrace {
				idle = true // タブが閉じられ、その後ハートビートが来ていない
			}
			if idle {
				a.closeStore()
				onIdle()
				return
			}
		}
	}()
}

func (a *App) apiShutdown(r *http.Request) (any, error) {
	if a.Shutdown != nil {
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.closeStore()
			a.Shutdown()
		}()
	}
	return map[string]any{}, nil
}

func timeStamp() string { return time.Now().Format("20060102_1504") }
