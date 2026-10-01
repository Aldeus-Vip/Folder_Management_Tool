package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// MergeSource は統合元のDB。
type MergeSource struct {
	Path   string
	Root   string
	Sep    string
	Date   string // データ取得日時(scanned_at / imported_at / built_at)
	Source string
}

// ReadMergeSource は統合元DBのメタ情報を読む。
func ReadMergeSource(path string) (*MergeSource, error) {
	s, err := fsdb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer s.Close()
	m, err := s.Meta()
	if err != nil {
		return nil, err
	}
	d := m["scanned_at"]
	if d == "" {
		d = m["imported_at"]
	}
	if d == "" {
		d = m["built_at"]
	}
	return &MergeSource{Path: path, Root: m["root"], Sep: m["sep"], Date: d, Source: m["source"]}, nil
}

// Merge は複数のDBをフルパスを基準に1つのツリーへ統合する。
//   - 同じパスが複数のDBにある場合は優先度の高いDBの情報を採用する
//     (preferNewest=true: データ取得日時が新しいDB優先 / false: 指定順で先のDB優先)
//   - 共通の親フォルダがあればそれをルートにし、無ければ仮想ルートの下に並べる
//   - アクション・担当・メモ(注記)も同じ優先順で引き継ぐ
func Merge(ctx context.Context, dbs []string, dbPath string, preferNewest bool, prog fsdb.Progress) (*Result, error) {
	if prog == nil {
		prog = func(string, int64, int64) {}
	}
	if len(dbs) < 2 {
		return nil, fmt.Errorf("統合するDBを2つ以上指定してください")
	}
	srcs := make([]*MergeSource, 0, len(dbs))
	seen := map[string]bool{}
	for _, p := range dbs {
		abs, _ := filepath.Abs(p)
		if seen[strings.ToLower(abs)] {
			continue
		}
		seen[strings.ToLower(abs)] = true
		if out, _ := filepath.Abs(dbPath); strings.EqualFold(out, abs) {
			return nil, fmt.Errorf("保存先に統合元と同じDB(%s)は指定できません", filepath.Base(p))
		}
		ms, err := ReadMergeSource(p)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, ms)
	}
	if len(srcs) < 2 {
		return nil, fmt.Errorf("統合するDBを2つ以上指定してください")
	}
	sep := srcs[0].Sep
	for _, s := range srcs {
		if s.Sep != sep {
			return nil, fmt.Errorf("パス区切り文字が異なるDB(Windows/それ以外)は統合できません")
		}
	}
	if preferNewest {
		sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].Date > srcs[j].Date })
	}

	// 共通ルート(全DBのルートに共通する親フォルダ)を決める
	key := strings.ToLower
	trim := func(p string) string {
		if t := strings.TrimRight(p, sep); t != "" && !strings.HasSuffix(t, ":") {
			return t
		}
		return p
	}
	roots := make([]string, len(srcs))
	for i, s := range srcs {
		roots[i] = trim(s.Root)
	}
	root, virtual := commonRoot(roots, sep)

	b := fsdb.NewBuilder(root, sep)
	b.VirtualRoot = virtual
	idx := map[string]int32{}
	if !virtual {
		idx[key(root)] = 0
		idx[key(trim(root))] = 0
	}
	// 仮想ルートの直下に置く「最上位のルート」(他のDBのルートの配下でないもの)
	tops := map[string]bool{}
	for _, r := range roots {
		top := true
		for _, o := range roots {
			if len(o) < len(r) && strings.HasPrefix(key(r), key(o)+sep) {
				top = false
			}
		}
		if top {
			tops[key(r)] = true
		}
	}
	var ensureDir func(p string) int32
	ensureDir = func(p string) int32 {
		k := key(p)
		if i, ok := idx[k]; ok {
			return i
		}
		parent, name := int32(0), p
		if !(virtual && tops[k]) {
			if j := strings.LastIndex(p, sep); j > 0 {
				parent, name = ensureDir(trim(p[:j+1])), p[j+1:]
			}
		}
		i := b.Add(fsdb.Record{Parent: parent, IsDir: true, Name: name})
		idx[k] = i
		return i
	}

	var overlap, conflict int64
	for si, s := range srcs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		phase := fmt.Sprintf("読み込み中 %d/%d: %s", si+1, len(srcs), filepath.Base(s.Path))
		db, err := sql.Open("sqlite", "file:"+s.Path+"?mode=ro")
		if err != nil {
			return nil, err
		}
		var total int64
		db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&total)
		rows, err := db.Query(`SELECT path, is_dir, size, mtime, flags FROM nodes ORDER BY id`)
		if err != nil {
			db.Close()
			return nil, err
		}
		var n int64
		for rows.Next() {
			var p string
			var isDir bool
			var size, mtime int64
			var flags int
			if err := rows.Scan(&p, &isDir, &size, &mtime, &flags); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			if n++; n%50000 == 0 {
				prog(phase, n, total)
			}
			p = trim(p)
			acc := flags & fsdb.FlagAccess
			if i, ok := idx[key(p)]; ok {
				r := &b.Recs[i]
				if r.IsDir != isDir {
					conflict++ // 同じパスでフォルダとファイルが食い違う → 優先DB側を採用
					continue
				}
				if isDir {
					if r.Mtime == 0 {
						r.Mtime = mtime // 中間フォルダとして補完した場合は情報を埋める
					}
					r.Flags |= acc
				} else {
					overlap++
				}
				continue
			}
			if isDir {
				i := ensureDir(p)
				b.Recs[i].Mtime, b.Recs[i].Flags = mtime, acc
				continue
			}
			j := strings.LastIndex(p, sep)
			if j <= 0 {
				continue
			}
			parent := ensureDir(trim(p[:j+1]))
			idx[key(p)] = b.Add(fsdb.Record{Parent: parent, Size: size, Mtime: mtime, Name: p[j+1:], Flags: acc})
		}
		err = rows.Err()
		rows.Close()
		db.Close()
		if err != nil {
			return nil, err
		}
	}
	idx = nil

	names := make([]string, len(srcs))
	for i, s := range srcs {
		names[i] = s.Path
	}
	b.Meta["source"] = "merge"
	b.Meta["source_path"] = strings.Join(names, "\n")
	b.Meta["merged_at"] = time.Now().Format("2006/01/02 15:04:05")
	if err := b.Finalize(ctx, dbPath, prog); err != nil {
		return nil, err
	}

	// 注記の引継ぎ: 優先度の低いDBから順に上書きし、最後に優先度の高いDBの内容が残るようにする
	prog("注記を引継ぎ中", 0, 0)
	st, err := fsdb.Open(dbPath)
	if err != nil {
		return nil, err
	}
	var notes int64
	for i := len(srcs) - 1; i >= 0; i-- {
		n, err := st.ImportNotes(srcs[i].Path)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("注記の引継ぎに失敗(%s): %w", filepath.Base(srcs[i].Path), err)
		}
		notes += n
	}
	st.Close()

	res := &Result{Items: int64(len(b.Recs))}
	order := make([]string, len(srcs))
	for i, s := range srcs {
		order[i] = fmt.Sprintf("%d. %s(%s)", i+1, filepath.Base(s.Path), s.Date)
	}
	res.Warnings = append(res.Warnings, "優先順: "+strings.Join(order, " / "))
	if virtual {
		res.Warnings = append(res.Warnings, "共通の親フォルダが無いため、仮想ルート「"+root+"」の下に並べました")
	}
	if overlap > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("複数のDBにあった同一パスのファイル %d 件は、優先度の高いDBの情報を採用しました", overlap))
	}
	if conflict > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("フォルダ/ファイルの種別が食い違うパス %d 件は、優先度の高いDBの情報を採用しました", conflict))
	}
	if notes > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("注記(アクション・担当・メモ)を %d 件引継ぎました", notes))
	}
	return res, nil
}

const virtualRootName = "(統合ルート)"

// commonRoot は全ルートに共通する親フォルダを返す。共通部分が無ければ仮想ルート。
func commonRoot(roots []string, sep string) (string, bool) {
	split := func(p string) []string {
		parts := strings.Split(p, sep)
		for len(parts) > 1 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		return parts
	}
	common := split(roots[0])
	for _, r := range roots[1:] {
		ps := split(r)
		n := 0
		for n < len(common) && n < len(ps) && strings.EqualFold(common[n], ps[n]) {
			n++
		}
		common = common[:n]
	}
	// 意味のある共通部分か判定(UNCの "\\" だけ、空、などは不可)
	meaningful := 0
	for _, c := range common {
		if c != "" {
			meaningful++
		}
	}
	if len(common) > 0 && strings.HasPrefix(roots[0], sep+sep) && meaningful < 2 {
		meaningful = 0 // UNCはサーバー名+共有名まで一致して初めて共通とみなす
	}
	if meaningful == 0 {
		if sep == "/" && len(common) > 0 {
			return "/", false
		}
		return virtualRootName, true
	}
	p := strings.Join(common, sep)
	if strings.HasSuffix(p, ":") {
		p += sep
	}
	return p, false
}
