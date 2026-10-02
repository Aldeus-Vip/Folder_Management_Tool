package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// Scan は root 配下を並列に走査してDBを作る。ネットワークドライブでは通信待ちが
// ボトルネックになるため、複数フォルダを同時に読み込む(workers 並列)。
// シンボリックリンク・ジャンクションはたどらない(ループ防止)。
//
// alias を指定すると、DBにはそのパス(SharePoint の URL など)で記録する。OneDrive の同期フォルダのように
// 人によって実際の場所が異なるフォルダを、全員が同じパスで扱うため。
func Scan(ctx context.Context, root, dbPath string, workers int, alias string, prog fsdb.Progress) (*Result, error) {
	if prog == nil {
		prog = func(string, int64, int64) {}
	}
	root = filepath.Clean(root)
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("フォルダを開けません: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("フォルダではありません: %s", root)
	}
	if workers <= 0 {
		workers = 16
	}
	b := fsdb.NewBuilder(root, string(filepath.Separator))
	if strings.TrimSpace(alias) != "" {
		ar, sep := fsdb.AliasRoot(alias)
		b = fsdb.NewBuilder(ar, sep)
		b.Meta["alias_root"], b.Meta["local_root"], b.Meta["local_sep"] = ar, root, string(filepath.Separator)
	}
	b.Recs[0].Mtime = st.ModTime().Unix()

	var mu sync.Mutex
	var wg sync.WaitGroup
	var dirsDone, files, errs atomic.Int64
	sem := make(chan struct{}, workers)

	var visit func(id int32, dir string)
	visit = func(id int32, dir string) {
		defer wg.Done()
		if ctx.Err() != nil {
			return
		}
		sem <- struct{}{}
		ents, err := os.ReadDir(dir)
		type item struct {
			rec  fsdb.Record
			path string
		}
		items := make([]item, 0, len(ents))
		for _, e := range ents {
			r := fsdb.Record{Parent: id, Name: e.Name()}
			info, ierr := e.Info()
			if ierr == nil {
				r.Mtime = info.ModTime().Unix()
				r.Size = info.Size()
			} else {
				r.Flags |= fsdb.FlagAccess
				r.Err = errMessage(ierr)
				errs.Add(1)
			}
			r.IsDir = e.IsDir() && e.Type()&os.ModeSymlink == 0
			if r.IsDir {
				r.Size = 0
			}
			items = append(items, item{r, filepath.Join(dir, e.Name())})
		}
		<-sem
		mu.Lock()
		if err != nil {
			b.Recs[id].Flags |= fsdb.FlagAccess
			b.Recs[id].Err = errMessage(err)
			errs.Add(1)
		}
		ids := make([]int32, len(items))
		for i := range items {
			ids[i] = b.Add(items[i].rec)
		}
		mu.Unlock()
		dirsDone.Add(1)
		for i, it := range items {
			if it.rec.IsDir {
				wg.Add(1)
				go visit(ids[i], it.path)
			} else {
				files.Add(1)
			}
		}
	}

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				prog(fmt.Sprintf("スキャン中(フォルダ %d / ファイル %d / エラー %d)", dirsDone.Load(), files.Load(), errs.Load()), dirsDone.Load()+files.Load(), 0)
			}
		}
	}()
	wg.Add(1)
	visit(0, root)
	wg.Wait()
	close(done)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	b.Meta["source"] = "scan"
	b.Meta["source_path"] = root
	b.Meta["scanned_at"] = time.Now().Format("2006/01/02 15:04:05")
	if err := b.Finalize(ctx, dbPath, prog); err != nil {
		return nil, err
	}
	res := &Result{Items: int64(len(b.Recs)), Errors: errs.Load()}
	if errs.Load() > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("アクセスできなかった項目が %d 件あります(🔒 表示。中身・サイズは不明です。サマリーの「アクセス不可」から一覧できます)", errs.Load()))
	}
	return res, nil
}

// errMessage は読み込みエラーを利用者向けの文に直す(パスは行に表示されるので省く)。
func errMessage(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "アクセス権がありません(アクセスが拒否されました)"
	case errors.Is(err, fs.ErrNotExist):
		return "見つかりません(スキャン中に移動・削除された可能性があります)"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return "読み込めません: " + pe.Err.Error()
	}
	return "読み込めません: " + err.Error()
}
