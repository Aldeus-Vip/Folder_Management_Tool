package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
	"github.com/aldeus-vip/folder_management_tool/internal/ingest"
	"github.com/aldeus-vip/folder_management_tool/internal/osutil"
)

// ---- 閲覧 ----

func (a *App) apiSummary(r *http.Request, s *fsdb.Store) (any, error)  { return s.Summary() }
func (a *App) apiProgress(r *http.Request, s *fsdb.Store) (any, error) { return s.Progress() }

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

func (a *App) apiRules(r *http.Request, s *fsdb.Store) (any, error) {
	var ru fsdb.Rules
	if err := decode(r, &ru); err != nil {
		return nil, err
	}
	if err := s.SaveRules(ru); err != nil {
		return nil, badRequest("%v", err)
	}
	return s.Rules(), nil
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

func flag(r *http.Request, k string) bool { return r.URL.Query().Get(k) == "1" }

// treeFilter は整理画面の左ツリーの絞り込み(owner = 担当、tf = hold / rule / unhandled)。
func treeFilter(r *http.Request, s *fsdb.Store) (*fsdb.TreeFilter, error) {
	tf, err := s.TreeFilterFor(r.URL.Query().Get("owner"), r.URL.Query().Get("tf"))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return tf, nil
}

func (a *App) apiChildren(r *http.Request, s *fsdb.Store) (any, error) {
	tf, err := treeFilter(r, s)
	if err != nil {
		return nil, err
	}
	return s.Children(qInt(r, "id", 1), flag(r, "dirs"), flag(r, "hide"), tf)
}

func (a *App) apiSubtree(r *http.Request, s *fsdb.Store) (any, error) {
	tf, err := treeFilter(r, s)
	if err != nil {
		return nil, err
	}
	ns, err := s.Subtree(qInt(r, "id", 1), int(qInt(r, "depth", 1)), flag(r, "dirs"), flag(r, "hide"), 200000, tf)
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

func (a *App) apiTags(r *http.Request, s *fsdb.Store) (any, error) { return s.AllTags() }

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
		f, err := os.Open(a.localPath(s, n.Path))
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

// apiOpenFile はファイルを既定のアプリで開く(中身を確認するため。このPCからアクセスできる場合のみ)。
func (a *App) apiOpenFile(r *http.Request, s *fsdb.Store) (any, error) {
	var req struct{ ID int64 }
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, err := s.Node(req.ID)
	if err != nil {
		return nil, err
	}
	if n.IsDir {
		return nil, badRequest("フォルダは開けません")
	}
	lp := a.localPath(s, n.Path)
	if st, err := os.Stat(lp); err != nil || st.IsDir() {
		return nil, badRequest("このPCからはアクセスできません(移動・削除済み、権限がない、または「このPCでの実際の場所」の設定が違う可能性があります): %s", lp)
	}
	if err := osutil.OpenFile(lp); err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{}, nil
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
	lp := a.localPath(s, n.Path)
	if _, err := os.Stat(lp); err != nil {
		return nil, badRequest("このPCからはアクセスできません: %s", lp)
	}
	if err := osutil.Reveal(lp, n.IsDir); err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{}, nil
}

// ---- 仮想フォルダ構成 ----

func (a *App) apiVNode(r *http.Request, s *fsdb.Store) (any, error) {
	return s.VNode(qStr(r, "uuid", fsdb.VRoot))
}

func qStr(r *http.Request, k, def string) string {
	if v := r.URL.Query().Get(k); v != "" {
		return v
	}
	return def
}

func (a *App) apiVChildren(r *http.Request, s *fsdb.Store) (any, error) {
	return s.VChildren(qStr(r, "uuid", fsdb.VRoot))
}

func (a *App) apiVReal(r *http.Request, s *fsdb.Store) (any, error) {
	return s.VReal(qInt(r, "id", 1), int(qInt(r, "depth", 1)))
}

func (a *App) apiVTree(r *http.Request, s *fsdb.Store) (any, error) { return s.VTreeAll() }

func (a *App) apiVWarnings(r *http.Request, s *fsdb.Store) (any, error) {
	w, err := s.SimilarWarnings()
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = [][2]string{}
	}
	return w, nil
}

type vreq struct {
	UUID, Parent, Name, Memo string
}

func (a *App) apiVCreate(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q vreq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	id, is, err := s.VCreate(q.Parent, q.Name, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"uuid": id, "issue": is}, nil
}

func (a *App) apiVRename(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q vreq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	is, err := s.VRename(q.UUID, q.Name, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"issue": is}, nil
}

func (a *App) apiVMemo(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q vreq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	return map[string]any{}, s.VSetMemo(q.UUID, q.Memo, code)
}

func (a *App) apiVMove(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q vreq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	is, err := s.VMove(q.UUID, q.Parent, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"issue": is}, nil
}

func (a *App) apiVDelete(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q vreq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	n, err := s.VDelete(q.UUID, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"cleared": n}, nil
}

// ---- アクション・タグ ----

type idsReq struct {
	IDs    []int64  `json:"ids"`
	Target string   `json:"target"`
	Owner  []string `json:"owner"` // 担当(部 / 課 / 担当 / 担当者)
}

func (a *App) apiPlanHold(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q idsReq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	return s.PlanHold(q.IDs, code)
}

func (a *App) apiPlanOwner(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q idsReq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	return s.PlanOwner(q.IDs, q.Owner, code)
}

func (a *App) apiPlanDelete(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q idsReq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	return s.PlanDelete(q.IDs, code)
}

func (a *App) apiPlanMove(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q idsReq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	rep, err := s.PlanMove(q.IDs, q.Target, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return rep, nil
}

func (a *App) apiPlanClear(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q idsReq
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	return s.PlanClear(q.IDs, code)
}

func (a *App) apiPlanFields(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q struct {
		IDs []int64 `json:"ids"`
		fsdb.PlanFields
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	rep, err := s.PlanSetFields(q.IDs, q.PlanFields, code)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return rep, nil
}

// apiPlanFilter は検索結果すべてに同じ操作をする。
func (a *App) apiPlanFilter(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q struct {
		Query  string `json:"query"` // 検索画面と同じURLクエリ文字列
		Op     string `json:"op"`    // delete | move | hold | clear | tag | owner
		Target string `json:"target"`
		Tag    string `json:"tag"`
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	v, err := url.ParseQuery(q.Query)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	ids, err := s.SearchIDs(fsdb.FilterFromQuery(v), 200000)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	switch q.Op {
	case "delete":
		return s.PlanDelete(ids, code)
	case "move":
		rep, err := s.PlanMove(ids, q.Target, code)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		return rep, nil
	case "hold":
		return s.PlanHold(ids, code)
	case "owner":
		return s.PlanOwner(ids, strings.Split(q.Tag, "/"), code) // 「部/課/担当/担当者」
	case "clear":
		return s.PlanClear(ids, code)
	case "tag":
		n, err := s.SetTags(ids, []string{q.Tag}, nil)
		return &fsdb.Report{Applied: n}, err
	}
	return nil, badRequest("不明な操作: %s", q.Op)
}

func (a *App) apiSetTags(r *http.Request, s *fsdb.Store, code string) (any, error) {
	var q struct {
		IDs    []int64  `json:"ids"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	n, err := s.SetTags(q.IDs, q.Add, q.Remove)
	if err != nil {
		return nil, err
	}
	return map[string]int{"updated": n}, nil
}

// ---- 複数人での編集 ----

func (a *App) apiWorkCopy(r *http.Request, s *fsdb.Store) (any, error) {
	var q struct{ Code, Path string }
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	stem := strings.TrimSuffix(filepath.Base(s.Path), filepath.Ext(s.Path))
	dst, err := a.resolveDBPath(q.Path, stem+"_"+strings.TrimSpace(q.Code))
	if err != nil {
		return nil, err
	}
	if err := s.WorkCopy(dst, q.Code); err != nil {
		return nil, badRequest("%v", err)
	}
	if err := a.openStore(dst); err != nil {
		return nil, err
	}
	return a.apiState(r)
}

func (a *App) apiSetCode(r *http.Request, s *fsdb.Store) (any, error) {
	var q struct{ Code string }
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	if err := s.SetCode(q.Code); err != nil {
		return nil, badRequest("%v", err)
	}
	return a.apiState(r)
}

func (a *App) apiActMergeAnalyze(r *http.Request) (any, error) {
	var q struct {
		DBs    []string
		Master string // 統合先のマスター(空なら新しいファイルとして保存)
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	var dbs []string
	for _, p := range q.DBs {
		if p = cleanPath(p); p != "" {
			dbs = append(dbs, p)
		}
	}
	master := cleanPath(q.Master)
	if master != "" {
		if _, err := os.Stat(master); err != nil {
			return nil, badRequest("統合先のマスターが見つかりません: %s", master)
		}
	}
	ma, err := fsdb.AnalyzeActionMerge(dbs, master)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	a.mu.Lock()
	if a.analysis != nil {
		a.analysis.Close()
	}
	a.analysis = ma
	a.analysisDBs = dbs
	a.mu.Unlock()
	return ma, nil
}

func (a *App) apiActMergeApply(r *http.Request) (any, error) {
	var q struct {
		Out     string         `json:"out"`
		Choices map[string]int `json:"choices"`
		Refresh bool           `json:"refresh"` // 統合後、開いている作業用コピーを新しいマスターから作り直す
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	a.mu.Lock()
	ma, dbs := a.analysis, a.analysisDBs
	a.analysis = nil
	a.mu.Unlock()
	if ma == nil {
		return nil, badRequest("先に「比較する」を実行してください")
	}
	out := ma.MasterPath
	if out == "" {
		var err error
		if out, err = a.resolveDBPath(q.Out, "master_"+timeStamp()); err != nil {
			ma.Close()
			return nil, err
		}
	}
	// 作り直す作業用コピー(今開いているDBが統合元の1つである場合)
	var copyPath, code string
	if s := a.current(); s != nil && q.Refresh {
		for _, p := range dbs {
			if samePath(p, s.Path) {
				copyPath, code = s.Path, s.EditorCode()
			}
		}
	}
	a.closeStore() // 統合元・統合先のファイルを開いたままにしない(マスターの置き換えができなくなるため)
	open := out
	if copyPath != "" {
		open = copyPath
	}
	return a.startJob("actmerge", open, func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error) {
		defer ma.Close()
		prog("統合中", 0, 0)
		warns, err := ma.Apply(out, q.Choices)
		if err != nil {
			return nil, err
		}
		if copyPath != "" {
			prog("作業用コピーを作り直し中", 0, 0)
			m, err := fsdb.Open(out)
			if err != nil {
				return nil, err
			}
			err = m.WorkCopy(copyPath, code)
			m.Close()
			if err != nil {
				return nil, fmt.Errorf("統合は完了しましたが、作業用コピーを作り直せませんでした: %v", err)
			}
			warns = append(warns, "作業用コピーを新しいマスターから作り直しました。このまま作業を続けられます")
		}
		return &ingest.Result{Warnings: warns}, nil
	})
}

// ---- 記録用のパス(SharePoint の URL など)と、このPCでの実際の場所 ----

func (a *App) pathMapFile() string { return filepath.Join(a.ProjectsDir, "pathmap.json") }

func (a *App) readPathMap() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(a.pathMapFile()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// localPath は記録用のパスを、このPCでの実際のパスに読み替える。
func (a *App) localPath(s *fsdb.Store, p string) string {
	m, err := s.Meta()
	if err != nil || m["alias_root"] == "" {
		return p
	}
	return fsdb.LocalPath(p, m, a.readPathMap()[m["alias_root"]])
}

func (a *App) apiAlias(r *http.Request, s *fsdb.Store) (any, error) {
	var q struct{ Alias string }
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	if err := s.SetAlias(q.Alias); err != nil {
		return nil, badRequest("%v", err)
	}
	return a.apiState(r)
}

func (a *App) apiLocalRoot(r *http.Request, s *fsdb.Store) (any, error) {
	var q struct{ Local string }
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	m, _ := s.Meta()
	if m["alias_root"] == "" {
		return nil, badRequest("先に「記録用のパス」を設定してください")
	}
	local := cleanPath(q.Local)
	if local != "" {
		if st, err := os.Stat(local); err != nil || !st.IsDir() {
			return nil, badRequest("フォルダが見つかりません: %s", local)
		}
	}
	pm := a.readPathMap()
	if local == "" {
		delete(pm, m["alias_root"])
	} else {
		pm[m["alias_root"]] = local
	}
	b, _ := json.MarshalIndent(pm, "", "  ")
	if err := os.WriteFile(a.pathMapFile(), b, 0o644); err != nil {
		return nil, err
	}
	return a.apiState(r)
}
