package fsdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Record は取込/スキャン中の1エントリ。Parent は Builder.Recs 内のインデックス(ルートは -1)。
type Record struct {
	Parent int32
	IsDir  bool
	Size   int64 // ファイルのバイト数(フォルダは集計で上書き)
	Mtime  int64 // UNIX秒(不明は0)
	Name   string
	Flags  int // 入力時点で分かっているフラグ(アクセス不可など)
}

// Builder は取込元(Excel/スキャン)に依存しない共通の構築器。
// Recs[0] は必ずルート(Name にルートのフルパス)。
type Builder struct {
	Recs []Record
	Sep  string
	Meta map[string]string
	// VirtualRoot: ルートが実在しない仮想フォルダ(共通の親がないDB同士の統合用)。
	// このとき第1階層の Name はフルパスそのものとして扱う。
	VirtualRoot bool
}

func NewBuilder(root, sep string) *Builder {
	return &Builder{
		Recs: []Record{{Parent: -1, IsDir: true, Name: root}},
		Sep:  sep,
		Meta: map[string]string{},
	}
}

func (b *Builder) Add(r Record) int32 {
	b.Recs = append(b.Recs, r)
	return int32(len(b.Recs) - 1)
}

// Progress は進捗通知(phase: 工程名, done/total: 件数)。
type Progress func(phase string, done, total int64)

const schema = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE nodes(
  id INTEGER PRIMARY KEY,   -- ツリー順(行きがけ順)の通し番号。配下は id+1..end_id の連続範囲
  parent_id INTEGER,
  end_id INTEGER,
  depth INTEGER,
  is_dir INTEGER,
  name TEXT,
  name_lc TEXT,
  ext TEXT,
  size INTEGER,             -- バイト。フォルダは配下の合計
  mtime INTEGER,
  child_count INTEGER,      -- 直下の項目数
  file_count INTEGER,       -- 配下の全ファイル数
  dir_count INTEGER,        -- 配下の全フォルダ数
  path_len INTEGER,         -- ルートからの相対パス文字数
  flags INTEGER,
  path TEXT
);
`

const indexes = `
CREATE INDEX nodes_parent ON nodes(parent_id);
CREATE INDEX nodes_dup ON nodes(name_lc, size) WHERE is_dir=0;
CREATE INDEX nodes_size ON nodes(size);
-- ノードは構築後に変わらないため、重い集計は構築時に済ませておく
CREATE TABLE dup_groups AS SELECT name_lc, size, count(*) cnt, (count(*)-1)*size wasted
  FROM nodes WHERE is_dir=0 AND size>0 AND flags & 512 != 0 GROUP BY name_lc, size HAVING cnt>1;
CREATE INDEX dup_groups_w ON dup_groups(wasted DESC, name_lc);
CREATE TABLE ext_stats AS SELECT ext, count(*) cnt, sum(size) size FROM nodes WHERE is_dir=0 GROUP BY ext;
CREATE TABLE depth_stats AS SELECT depth, count(*) cnt, COALESCE(sum(CASE WHEN is_dir=0 THEN size END),0) size FROM nodes WHERE id>1 GROUP BY depth;`

// Finalize はツリー順の並べ替え・集計・警告判定を行い、SQLite DB(dbPath)に書き出す。
// 一時ファイルに書いてから置き換えるため、途中で失敗しても既存DBは壊れない。
func (b *Builder) Finalize(ctx context.Context, dbPath string, prog Progress) error {
	n := len(b.Recs)
	recs := b.Recs
	if prog == nil {
		prog = func(string, int64, int64) {}
	}

	// 1) 親→子のCSR構造を作り、子を「フォルダ優先・名前順」に並べる
	prog("構造を計算中", 0, int64(n))
	lc := make([]string, n)
	for i := range recs {
		lc[i] = strings.ToLower(recs[i].Name)
	}
	start := make([]int32, n+1)
	for i := 1; i < n; i++ {
		start[recs[i].Parent+1]++
	}
	for i := 1; i <= n; i++ {
		start[i] += start[i-1]
	}
	kids := make([]int32, n)
	fill := append([]int32(nil), start[:n]...)
	for i := 1; i < n; i++ {
		p := recs[i].Parent
		kids[fill[p]] = int32(i)
		fill[p]++
	}
	for p := 0; p < n; p++ {
		seg := kids[start[p]:start[p+1]]
		if len(seg) > 1 {
			sort.Slice(seg, func(a, c int) bool {
				x, y := seg[a], seg[c]
				if recs[x].IsDir != recs[y].IsDir {
					return recs[x].IsDir
				}
				if lc[x] != lc[y] {
					return lc[x] < lc[y]
				}
				return recs[x].Name < recs[y].Name
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2) 行きがけ順の採番
	order := make([]int32, 0, n) // order[ord-1] = rec index
	ord := make([]int32, n)
	depth := make([]int16, n)
	stack := []int32{0}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = append(order, i)
		ord[i] = int32(len(order))
		seg := kids[start[i]:start[i+1]]
		for k := len(seg) - 1; k >= 0; k-- {
			depth[seg[k]] = depth[i] + 1
			stack = append(stack, seg[k])
		}
	}
	if len(order) != n {
		return fmt.Errorf("ツリー構造が不正です(到達できない項目が %d 件あります)", n-len(order))
	}

	// 3) 帰りがけで配下の合計を集計
	size := make([]int64, n)
	files := make([]int64, n)
	dirs := make([]int64, n)
	cnt := make([]int32, n)
	for k := n - 1; k >= 0; k-- {
		i := order[k]
		cnt[i]++
		if !recs[i].IsDir {
			size[i] = recs[i].Size
		}
		if p := recs[i].Parent; p >= 0 {
			cnt[p] += cnt[i]
			size[p] += size[i]
			if recs[i].IsDir {
				dirs[p] += dirs[i] + 1
				files[p] += files[i]
			} else {
				files[p]++
			}
		}
	}

	// 4) 警告フラグ
	type dupKey struct {
		name string
		size int64
	}
	dupCount := map[dupKey]int32{}
	for i := 1; i < n; i++ {
		if !recs[i].IsDir && recs[i].Size > 0 {
			dupCount[dupKey{lc[i], recs[i].Size}]++
		}
	}
	flags := make([]int, n)
	for i := 0; i < n; i++ {
		r := &recs[i]
		f := r.Flags
		if i > 0 && !(b.VirtualRoot && depth[i] == 1) {
			f |= NameFlags(r.Name, r.IsDir)
		}
		if r.IsDir {
			nk := start[i+1] - start[i]
			if nk == 0 && f&FlagAccess == 0 {
				f |= FlagEmptyDir
			}
			if nk == 1 && recs[kids[start[i]]].IsDir {
				f |= FlagSingleChild
			}
		} else if r.Size > 0 && dupCount[dupKey{lc[i], r.Size}] > 1 {
			f |= FlagDup
		}
		flags[i] = f
	}

	// 5) SQLiteへ書き出し
	tmp := dbPath + ".building"
	os.Remove(tmp)
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		db.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF", "PRAGMA cache_size=-200000", "PRAGMA temp_store=MEMORY"} {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	if _, err := db.Exec(schema + planSchema); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO nodes VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	paths := make([]string, 0, 64)
	relLen := make([]int, 0, 64)
	for k := 0; k < n; k++ {
		i := order[k]
		r := &recs[i]
		d := int(depth[i])
		paths = paths[:d]
		relLen = relLen[:d]
		var p string
		var rl int
		if d == 0 || (d == 1 && b.VirtualRoot) {
			p = r.Name // 仮想ルート直下はフルパスを名前に持ち、パス文字数はそこから数える
		} else {
			pp := paths[d-1]
			if strings.HasSuffix(pp, b.Sep) {
				p = pp + r.Name
			} else {
				p = pp + b.Sep + r.Name
			}
			rl = relLen[d-1] + utf8.RuneCountInString(r.Name)
			if d > 1 && !(d == 2 && b.VirtualRoot) {
				rl++
			}
		}
		paths = append(paths, p)
		relLen = append(relLen, rl)
		var parent any
		if r.Parent >= 0 {
			parent = ord[r.Parent]
		}
		ext := ""
		if !r.IsDir {
			ext = Ext(r.Name)
		}
		isDir := 0
		if r.IsDir {
			isDir = 1
		}
		if _, err := stmt.Exec(k+1, parent, k+int(cnt[i]), d, isDir, r.Name, lc[i], ext, size[i], r.Mtime,
			start[i+1]-start[i], files[i], dirs[i], rl, flags[i], p); err != nil {
			return err
		}
		if k%20000 == 0 {
			prog("DBへ書き込み中", int64(k), int64(n))
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	stmt.Close()
	meta := map[string]string{
		"sep":        b.Sep,
		"root":       recs[0].Name,
		"built_at":   time.Now().Format("2006/01/02 15:04:05"),
		"node_count": strconv.Itoa(n),
		"version":    "1",
		"virtual":    strconv.FormatBool(b.VirtualRoot),
	}
	for k, v := range b.Meta {
		meta[k] = v
	}
	for k, v := range meta {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	prog("インデックス作成中", int64(n), int64(n))
	if _, err := db.Exec(indexes); err != nil {
		return err
	}
	if _, err := db.Exec("ANALYZE"); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	os.Remove(dbPath)
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	if err := os.Rename(tmp, dbPath); err != nil {
		return err
	}
	ok = true
	return nil
}
