package fsdb

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- 整理後の項目数(ファイル + フォルダ)の見込み ----
// 移行先の制約(例: 全体で 900,000 項目以下)を満たすかを確認するため、
// 設定したアクションを反映した「整理後に残る項目数」を数える。
//
//   - 削除   : 配下を含めて残らない
//   - 移動   : 新構成に入る(移動したフォルダの配下も含む)
//   - 未処理・保留: 今の場所に残る。フォルダは、中に残る項目がある場合(元から空のフォルダを含む)だけ数える
//                  (中身をすべて移動・削除して空になった旧フォルダは残さない前提)
//   - 仮想フォルダ: 新構成のフォルダとして数える(ルートとショートカットは数えない)

const DefaultItemLimit = 900000

type FileFolder struct {
	Files   int64 `json:"files"`
	Folders int64 `json:"folders"`
	Size    int64 `json:"size"` // ファイルの合計サイズ(バイト)
}

func (f *FileFolder) add(dir bool, size int64) {
	if dir {
		f.Folders++
	} else {
		f.Files++
		f.Size += size
	}
}

func (f FileFolder) Total() int64 { return f.Files + f.Folders }

type ItemEstimate struct {
	Limit    int64      `json:"limit"`
	Current  FileFolder `json:"current"`  // 現在(スキャン時点)
	VFolders int64      `json:"vfolders"` // 新構成の仮想フォルダ
	Moved    FileFolder `json:"moved"`    // 移動して新構成に入る項目
	Kept     FileFolder `json:"kept"`     // 未処理のまま残る項目
	Held     FileFolder `json:"held"`     // 保留のまま残る項目
	Deleted  FileFolder `json:"deleted"`  // 削除で減る項目
	Dropped  FileFolder `json:"dropped"`  // 移行しない拡張子のファイル(試算。DropOn のときだけ)
	DropExts []string   `json:"dropExts"` // 移行しない拡張子
	DropOn   bool       `json:"dropOn"`   // 移行しない拡張子を見込みから除く
	Emptied  int64      `json:"emptied"`  // 中身がすべて移動・削除されて空になる旧フォルダ(残さない)
	Total    int64      `json:"total"`    // 整理後の見込み(仮想フォルダ + 移動 + 未処理 + 保留)
	Over     int64      `json:"over"`     // 上限を超える数(0 以下なら上限内)
	// 容量(ファイルの合計サイズ)
	TotalSize int64 `json:"totalSize"` // 整理後の見込み(移動 + 未処理 + 保留)
	SizeLimit int64 `json:"sizeLimit"` // 容量の上限(バイト。0 = 指定なし)
}

func (s *Store) SizeLimit() int64 {
	var v string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='size_limit'`).Scan(&v)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// DropSetting は「移行しない拡張子」の試算の設定(拡張子と、見込みから除くかどうか)。
func (s *Store) DropSetting() ([]string, bool) {
	m, _ := s.Meta()
	exts, _ := parseExts(m["item_drop"])
	return exts, m["item_drop_on"] == "1"
}

func (s *Store) SetDropSetting(exts string, on bool) error {
	list, _ := parseExts(exts)
	v := ""
	if on {
		v = "1"
	}
	err := s.SetMeta(map[string]string{"item_drop": strings.Join(list, ","), "item_drop_on": v})
	s.mu.Lock()
	s.items = nil
	s.mu.Unlock()
	return err
}

// SetSizeLimit は容量の上限(バイト。0 = 指定なし)を保存する。
func (s *Store) SetSizeLimit(n int64) error {
	if n < 0 {
		return fmt.Errorf("容量の上限は0以上を指定してください")
	}
	return s.SetMeta(map[string]string{"size_limit": strconv.FormatInt(n, 10)})
}

func (s *Store) ItemLimit() int64 {
	var v string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='item_limit'`).Scan(&v)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return n
	}
	return DefaultItemLimit
}

func (s *Store) SetItemLimit(n int64) error {
	if n <= 0 {
		return fmt.Errorf("上限は1以上を指定してください")
	}
	return s.SetMeta(map[string]string{"item_limit": strconv.FormatInt(n, 10)})
}

// EstimateItems は整理後の項目数の見込みを数える(1回の走査。結果は次の編集までキャッシュ)。
func (s *Store) EstimateItems() (*ItemEstimate, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	pi, gen, cached := s.pidx, s.gen, s.items
	vf := int64(0)
	for _, n := range s.vt.nodes {
		if n.UUID != VRoot && n.Link == "" {
			vf++
		}
	}
	s.mu.Unlock()
	limit, slimit := s.ItemLimit(), s.SizeLimit()
	dropExts, dropOn := s.DropSetting()
	dropKey := fmt.Sprint(dropOn, dropExts)
	if cached != nil && cached.gen == gen && cached.drop == dropKey {
		e := cached.est
		e.Limit, e.Over, e.SizeLimit = limit, e.Total-limit, slimit
		return &e, nil
	}
	drop := map[string]bool{}
	if dropOn {
		for _, x := range dropExts {
			drop[x] = true
		}
	}
	var maxID int64
	if err := s.DB.QueryRow(`SELECT max(id) FROM nodes`).Scan(&maxID); err != nil {
		return nil, err
	}
	e := ItemEstimate{VFolders: vf, DropExts: dropExts, DropOn: dropOn}
	if e.DropExts == nil {
		e.DropExts = []string{}
	}
	keep := make([]bool, maxID+1) // 配下に今の場所に残る項目がある
	// 子 → 親の順(ID の降順)に見て、残る項目があるフォルダを親へ伝える
	rows, err := s.DB.Query(`SELECT id, COALESCE(parent_id,0), is_dir, flags, size, ext FROM nodes ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, size int64
		var dir bool
		var flags int
		var ext string
		if err := rows.Scan(&id, &parent, &dir, &flags, &size, &ext); err != nil {
			return nil, err
		}
		e.Current.add(dir, size)
		act := ""
		if k := pi.nearest(id); k >= 0 {
			act = pi.acts[k]
		}
		if !dir && act != ActDelete && len(drop) > 0 {
			x := strings.ToLower(ext)
			if x == "" {
				x = "(なし)"
			}
			if drop[x] { // 移行しない拡張子 → 整理後には入らない(試算)
				e.Dropped.add(false, size)
				continue
			}
		}
		switch act {
		case ActDelete:
			e.Deleted.add(dir, size)
			continue
		case ActMove:
			e.Moved.add(dir, size)
			continue
		}
		// 元から空のフォルダ・中身が分からないフォルダ(アクセス不可)はそのまま残るものとして数える
		if dir && !keep[id] && flags&(FlagEmptyDir|FlagAccess) == 0 {
			e.Emptied++ // 中身がすべて移動・削除されて空になる
			continue
		}
		if act == ActHold {
			e.Held.add(dir, size)
		} else {
			e.Kept.add(dir, size)
		}
		if parent > 0 && parent <= maxID {
			keep[parent] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	e.Total = e.VFolders + e.Moved.Total() + e.Kept.Total() + e.Held.Total()
	e.TotalSize = e.Moved.Size + e.Kept.Size + e.Held.Size
	s.mu.Lock()
	if s.gen == gen {
		s.items = &itemCache{gen: gen, drop: dropKey, est: e}
	}
	s.mu.Unlock()
	e.Limit, e.Over, e.SizeLimit = limit, e.Total-limit, slimit
	return &e, nil
}

type itemCache struct {
	gen  int64
	drop string
	est  ItemEstimate
}
