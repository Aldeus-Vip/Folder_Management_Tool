// Package server はブラウザUIとJSON APIを提供するローカルHTTPサーバー。
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
}

// Job はバックグラウンドで実行中の取込/スキャン。
type Job struct {
	mu       sync.Mutex
	Kind     string         `json:"kind"`
	Phase    string         `json:"phase"`
	Done     int64          `json:"done"`
	Total    int64          `json:"total"`
	Started  time.Time      `json:"started"`
	Finished bool           `json:"finished"`
	Err      string         `json:"error,omitempty"`
	Result   *ingest.Result `json:"result,omitempty"`
	DBPath   string         `json:"dbPath"`
	cancel   context.CancelFunc
}

func New(projectsDir, version string) *App {
	b := make([]byte, 16)
	rand.Read(b)
	return &App{ProjectsDir: projectsDir, Version: version, token: hex.EncodeToString(b)}
}

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
	api := map[string]func(*http.Request) (any, error){
		"GET /api/state":         a.apiState,
		"GET /api/projects":      a.apiProjects,
		"POST /api/open":         a.apiOpen,
		"POST /api/close":        a.apiClose,
		"POST /api/import-excel": a.apiImportExcel,
		"POST /api/scan":         a.apiScan,
		"POST /api/merge":        a.apiMerge,
		"POST /api/dbinfo":       a.apiDBInfo,
		"GET /api/job":           a.apiJob,
		"POST /api/job/cancel":   a.apiJobCancel,
		"POST /api/dialog":       a.apiDialog,
		"GET /api/summary":       a.withStore(a.apiSummary),
		"POST /api/settings":     a.withStore(a.apiSettings),
		"GET /api/node":          a.withStore(a.apiNode),
		"GET /api/children":      a.withStore(a.apiChildren),
		"GET /api/subtree":       a.withStore(a.apiSubtree),
		"GET /api/find":          a.withStore(a.apiFind),
		"GET /api/search":        a.withStore(a.apiSearch),
		"GET /api/exts":          a.withStore(a.apiExts),
		"GET /api/dups":          a.withStore(a.apiDups),
		"POST /api/notes":        a.withStore(a.apiNotes),
		"POST /api/notes/filter": a.withStore(a.apiNotesFilter),
		"POST /api/notes/import": a.withStore(a.apiNotesImport),
		"POST /api/hash":         a.withStore(a.apiHash),
		"POST /api/reveal":       a.withStore(a.apiReveal),
		"POST /api/shutdown":     a.apiShutdown,
		"POST /api/heartbeat":    a.apiHeartbeat,
		"POST /api/bye":          a.apiBye,
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

func (a *App) jsonHandler(fn func(*http.Request) (any, error)) http.HandlerFunc {
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

func (a *App) withStore(fn func(*http.Request, *fsdb.Store) (any, error)) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		a.mu.RLock()
		s := a.store
		a.mu.RUnlock()
		if s == nil {
			return nil, httpError{409, "DBが開かれていません。ホーム画面でDBを開くか、取込・スキャンを実行してください"}
		}
		return fn(r, s)
	}
}

func (a *App) download(fn func(io.Writer, *http.Request, *fsdb.Store) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		s := a.store
		a.mu.RUnlock()
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

// ---- プロジェクト(DB)管理 ----

func (a *App) apiState(r *http.Request) (any, error) {
	a.mu.RLock()
	s := a.store
	job := a.job
	a.mu.RUnlock()
	out := map[string]any{"version": a.Version, "projectsDir": a.ProjectsDir, "actions": fsdb.Actions, "checks": fsdb.CheckDefs}
	if s != nil {
		meta, _ := s.Meta()
		out["db"] = map[string]any{"path": s.Path, "meta": meta, "settings": s.Settings()}
	}
	if job != nil {
		out["job"] = job.snapshot()
	}
	return out, nil
}

type projectInfo struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

func (a *App) apiProjects(r *http.Request) (any, error) {
	ents, _ := os.ReadDir(a.ProjectsDir)
	out := []projectInfo{}
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, projectInfo{filepath.Join(a.ProjectsDir, e.Name()), e.Name(), info.Size(), info.ModTime().Format("2006/01/02 15:04")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
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

func (a *App) apiOpen(r *http.Request) (any, error) {
	var req struct{ Path string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p := strings.Trim(strings.TrimSpace(req.Path), `"`)
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

// resolveDBPath は保存先DBパスを決める(名前だけなら projects フォルダに置く)。
func (a *App) resolveDBPath(name, fallback string) (string, error) {
	name = strings.Trim(strings.TrimSpace(name), `"`)
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

func samePath(a, b string) bool {
	x, _ := filepath.Abs(a)
	y, _ := filepath.Abs(b)
	return strings.EqualFold(x, y)
}

func (j *Job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"kind": j.Kind, "phase": j.Phase, "done": j.Done, "total": j.Total, "finished": j.Finished,
		"error": j.Err, "result": j.Result, "dbPath": j.DBPath, "elapsed": time.Since(j.Started).Round(time.Second).String()}
}

func (a *App) apiImportExcel(r *http.Request) (any, error) {
	var req struct{ Xlsx, DB string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	x := strings.Trim(strings.TrimSpace(req.Xlsx), `"`)
	if _, err := os.Stat(x); err != nil {
		return nil, badRequest("Excelファイルが見つかりません: %s", x)
	}
	db, err := a.resolveDBPath(req.DB, strings.TrimSuffix(filepath.Base(x), filepath.Ext(x)))
	if err != nil {
		return nil, err
	}
	return a.startJob("excel", db, func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error) {
		return ingest.ImportExcel(ctx, x, db, prog)
	})
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
	root := strings.Trim(strings.TrimSpace(req.Root), `"`)
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
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" {
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

// apiDBInfo は統合候補DBのルート・取得日時を返す(画面の一覧表示用)。
func (a *App) apiDBInfo(r *http.Request) (any, error) {
	var req struct{ Paths []string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out := []map[string]string{}
	for _, p := range req.Paths {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		ms, err := ingest.ReadMergeSource(p)
		if err != nil {
			out = append(out, map[string]string{"path": p, "error": err.Error()})
			continue
		}
		out = append(out, map[string]string{"path": p, "root": ms.Root, "date": ms.Date, "source": ms.Source})
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
// 取込・スキャン・統合の実行中は終了しない。
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

// ---- 閲覧 ----

func (a *App) apiSummary(r *http.Request, s *fsdb.Store) (any, error) { return s.Summary() }

func (a *App) apiSettings(r *http.Request, s *fsdb.Store) (any, error) {
	var st fsdb.Settings
	if err := decode(r, &st); err != nil {
		return nil, err
	}
	if err := s.SaveSettings(st); err != nil {
		return nil, badRequest("%v", err)
	}
	return s.Settings(), nil
}

func (a *App) apiNode(r *http.Request, s *fsdb.Store) (any, error) {
	n, err := s.Node(qInt(r, "id", 1))
	if err != nil {
		return nil, httpError{404, err.Error()}
	}
	anc, err := s.Ancestors(n.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"node": n, "ancestors": anc}, nil
}

func (a *App) apiChildren(r *http.Request, s *fsdb.Store) (any, error) {
	return s.Children(qInt(r, "id", 1), r.URL.Query().Get("dirs") == "1")
}

func (a *App) apiSubtree(r *http.Request, s *fsdb.Store) (any, error) {
	ns, err := s.Subtree(qInt(r, "id", 1), int(qInt(r, "depth", 1)), r.URL.Query().Get("dirs") == "1", 200000)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return ns, nil
}

func (a *App) apiFind(r *http.Request, s *fsdb.Store) (any, error) {
	id, err := s.FindPath(r.URL.Query().Get("path"))
	if err != nil {
		return nil, httpError{404, err.Error()}
	}
	return map[string]int64{"id": id}, nil
}

func (a *App) apiSearch(r *http.Request, s *fsdb.Store) (any, error) {
	limit := qInt(r, "limit", 200)
	if limit > 5000 {
		limit = 5000
	}
	ns, total, err := s.Search(fsdb.FilterFromQuery(r.URL.Query()), int(qInt(r, "offset", 0)), int(limit))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"rows": ns, "total": total}, nil
}

func (a *App) apiExts(r *http.Request, s *fsdb.Store) (any, error) {
	return s.Exts(qInt(r, "under", 0), int(qInt(r, "limit", 500)))
}

func (a *App) apiDups(r *http.Request, s *fsdb.Store) (any, error) {
	gs, total, wasted, err := s.Dups(qInt(r, "under", 0), int(qInt(r, "offset", 0)), int(qInt(r, "limit", 50)))
	if err != nil {
		return nil, err
	}
	return map[string]any{"groups": gs, "total": total, "wasted": wasted}, nil
}

// ---- 編集 ----

func (a *App) apiNotes(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct {
		IDs []int64         `json:"ids"`
		Set fsdb.NoteFields `json:"set"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := s.SetNotes(req.IDs, req.Set)
	if err != nil {
		return nil, err
	}
	return map[string]int64{"updated": n}, nil
}

func (a *App) apiNotesFilter(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct {
		Query string          `json:"query"` // 検索画面と同じURLクエリ文字列
		Set   fsdb.NoteFields `json:"set"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	q, err := url.ParseQuery(req.Query)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	n, err := s.SetNotesByFilter(fsdb.FilterFromQuery(q), req.Set)
	if err != nil {
		return nil, err
	}
	return map[string]int64{"updated": n}, nil
}

func (a *App) apiNotesImport(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct{ Path string }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	p := strings.Trim(strings.TrimSpace(req.Path), `"`)
	if samePath(p, s.Path) {
		return nil, badRequest("現在開いているDBとは別のDBを指定してください")
	}
	if _, err := os.Stat(p); err != nil {
		return nil, badRequest("ファイルが見つかりません: %s", p)
	}
	n, err := s.ImportNotes(p)
	if err != nil {
		return nil, badRequest("引継ぎに失敗しました: %v", err)
	}
	return map[string]int64{"imported": n}, nil
}

// apiHash は重複候補の中身が本当に同じかをSHA-256で確認する(実ファイルにアクセスできる場合のみ)。
func (a *App) apiHash(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct{ IDs []int64 }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, id := range req.IDs {
		n, err := s.Node(id)
		if err != nil {
			continue
		}
		key := strconv.FormatInt(id, 10)
		f, err := os.Open(n.Path)
		if err != nil {
			out[key] = "ERROR: 開けません"
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			out[key] = "ERROR: 読み込み失敗"
			continue
		}
		out[key] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

func (a *App) apiReveal(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct{ ID int64 }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := s.Node(req.ID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(n.Path); err != nil {
		return nil, badRequest("このPCからはアクセスできません: %s", n.Path)
	}
	if err := osutil.Reveal(n.Path, n.IsDir); err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{}, nil
}

// OpenInitial は起動引数で渡されたDBを開く。
func (a *App) OpenInitial(path string) error { return a.openStore(path) }
