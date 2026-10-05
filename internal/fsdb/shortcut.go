package fsdb

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- 整理後の構成の中のショートカット ----
// 実際のファイルではなく、整理後のフォルダ構成(仮想)の中で別の場所へ飛ぶための目印。
// リンク先は仮想フォルダ("v:<uuid>")か、実フォルダ/ファイル("n:<id>")。

func parseLink(target string) (kind string, uuid string, id int64, err error) {
	switch {
	case strings.HasPrefix(target, "v:"):
		return "v", target[2:], 0, nil
	case strings.HasPrefix(target, "n:"):
		id, err = strconv.ParseInt(target[2:], 10, 64)
		if err == nil {
			return "n", "", id, nil
		}
	}
	return "", "", 0, fmt.Errorf("リンク先が不正です: %s", target)
}

// VCreateLink は仮想フォルダ parent の直下にショートカットを作る。
func (s *Store) VCreateLink(parent, target, name, editor string) (string, error) {
	if err := s.ensureIndex(); err != nil {
		return "", err
	}
	kind, uuid, id, err := parseLink(target)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("名前を入力してください")
	}
	s.mu.Lock()
	p := s.vt.nodes[parent]
	var bad string
	switch {
	case p == nil || p.Link != "":
		bad = "置き場所の仮想フォルダが見つかりません"
	case kind == "v" && (s.vt.nodes[uuid] == nil || s.vt.nodes[uuid].Link != ""):
		bad = "リンク先の仮想フォルダが見つかりません"
	}
	s.mu.Unlock()
	if bad != "" {
		return "", fmt.Errorf("%s", bad)
	}
	if kind == "n" {
		if _, err := s.Node(id); err != nil {
			return "", err
		}
	}
	u := newUUID()
	if _, err := s.DB.Exec(`INSERT INTO vnodes(uuid, parent, name, editor, updated_at, link) VALUES(?,?,?,?,?,?)`, u, parent, name, editor, nowStr(), target); err != nil {
		return "", err
	}
	s.invalidate()
	return u, nil
}

// Location はリンク先を表示する手順。
//
//	VFolder: 仮想フォルダ(整理後の構成)
//	VPlaced: 整理後の構成に置かれた実項目(Placed = 移動を設定した項目、Path = そこからリンク先までの実フォルダ)
//	Left   : 整理後の構成に無い実項目 → 現在のフォルダ構成で表示
type Location struct {
	VFolder string  `json:"vfolder,omitempty"`
	Placed  int64   `json:"placed,omitempty"`
	Path    []int64 `json:"path,omitempty"`
	Target  int64   `json:"target,omitempty"`
	Left    int64   `json:"left,omitempty"`
	Reason  string  `json:"reason,omitempty"` // Left の理由
}

// VLocate はリンク先の場所を調べる。
func (s *Store) VLocate(target string) (*Location, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	kind, uuid, id, err := parseLink(target)
	if err != nil {
		return nil, err
	}
	if kind == "v" {
		s.mu.Lock()
		n := s.vt.nodes[uuid]
		s.mu.Unlock()
		if n == nil || n.Link != "" {
			return nil, fmt.Errorf("リンク先の仮想フォルダは削除されています")
		}
		return &Location{VFolder: uuid}, nil
	}
	x, err := s.Node(id)
	if err != nil {
		return nil, fmt.Errorf("リンク先の項目が見つかりません")
	}
	placed, vp, act := s.placement(x.ID)
	if act != ActMove {
		reason := map[string]string{ActDelete: "削除を設定しています", ActHold: "保留にしています", "": "整理後の構成にまだ置いていません"}[act]
		return &Location{Left: x.ID, Reason: reason}, nil
	}
	loc := &Location{VFolder: vp, Placed: placed, Target: x.ID}
	if placed != x.ID {
		anc, err := s.Ancestors(x.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range anc {
			if a.ID > placed {
				loc.Path = append(loc.Path, a.ID)
			}
		}
	}
	return loc, nil
}

// placement は項目に及んでいるアクション(自身または親フォルダの設定)と、その設定をした項目・移動先。
func (s *Store) placement(id int64) (placed int64, vparent, act string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi := s.pidx
	k := pi.nearest(id)
	if k < 0 {
		return 0, "", ""
	}
	if pi.acts[k] == ActMove && s.vt.nodes[pi.vps[k]] == nil {
		return 0, "", ""
	}
	return pi.ids[k], pi.vps[k], pi.acts[k]
}

// linkRow はショートカットの1行。
func (s *Store) linkRow(n *vnode) VRow {
	r := VRow{Key: "v:" + n.UUID, Kind: "vlink", UUID: n.UUID, Parent: n.Parent, Name: n.Name, Depth: n.Depth, Memo: n.Memo, Editor: n.Editor, Link: n.Link}
	kind, uuid, id, err := parseLink(n.Link)
	switch {
	case err != nil:
		r.Broken = err.Error()
	case kind == "v":
		s.mu.Lock()
		t := s.vt.nodes[uuid]
		if t == nil || t.Link != "" {
			r.Broken = "リンク先の仮想フォルダは削除されています"
		} else {
			r.LinkPath = joinV(s.vt.rootName, s.vt.path(uuid))
		}
		s.mu.Unlock()
	default:
		var path string
		if s.DB.QueryRow(`SELECT path FROM nodes WHERE id=?`, id).Scan(&path) != nil {
			r.Broken = "リンク先の項目が見つかりません"
			break
		}
		r.LinkPath = path
		if _, vp, act := s.placement(id); act == ActMove {
			s.mu.Lock()
			r.LinkPath = joinV(s.vt.rootName, s.vt.path(vp)) + ` …\` + path[strings.LastIndexAny(path, `\/`)+1:]
			s.mu.Unlock()
		} else {
			r.Broken = "リンク先は整理後の構成にありません(現在の構成で表示します)"
		}
	}
	return r
}
