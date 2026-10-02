package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func (a *App) apiChildren(r *http.Request, s *fsdb.Store) (any, error) {
	return s.Children(qInt(r, "id", 1), flag(r, "dirs"), flag(r, "hide"))
}

func (a *App) apiSubtree(r *http.Request, s *fsdb.Store) (any, error) {
	ns, err := s.Subtree(qInt(r, "id", 1), int(qInt(r, "depth", 1)), flag(r, "dirs"), flag(r, "hide"), 200000)
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
	IDs    []int64 `json:"ids"`
	Target string  `json:"target"`
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
		Op     string `json:"op"`    // delete | move | clear | tag
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
	var q struct{ DBs []string }
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	var dbs []string
	for _, p := range q.DBs {
		if p = cleanPath(p); p != "" {
			dbs = append(dbs, p)
		}
	}
	ma, err := fsdb.AnalyzeActionMerge(dbs)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	a.mu.Lock()
	if a.analysis != nil {
		a.analysis.Close()
	}
	a.analysis = ma
	a.mu.Unlock()
	return ma, nil
}

func (a *App) apiActMergeApply(r *http.Request) (any, error) {
	var q struct {
		Out     string         `json:"out"`
		Choices map[string]int `json:"choices"`
	}
	if err := decode(r, &q); err != nil {
		return nil, err
	}
	a.mu.Lock()
	ma := a.analysis
	a.analysis = nil
	a.mu.Unlock()
	if ma == nil {
		return nil, badRequest("先に「比較する」を実行してください")
	}
	out, err := a.resolveDBPath(q.Out, "master_"+timeStamp())
	if err != nil {
		ma.Close()
		return nil, err
	}
	return a.startJob("actmerge", out, func(ctx context.Context, prog fsdb.Progress) (*ingest.Result, error) {
		defer ma.Close()
		prog("統合中", 0, 0)
		warns, err := ma.Apply(out, q.Choices)
		if err != nil {
			return nil, err
		}
		return &ingest.Result{Warnings: warns}, nil
	})
}
