package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
)

// Scan は root 配下を並列に走査してDBを作る。ネットワークドライブでは通信待ちが
// ボトルネックになるため、複数フォルダを同時に読み込む(workers 並列)。
// シンボリックリンク・ジャンクションはたどらない(ループ防止)。
func Scan(ctx context.Context, root, dbPath string, workers int, prog fsdb.Progress) (*Result, error) {
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
		res.Warnings = append(res.Warnings, fmt.Sprintf("アクセスできなかったフォルダが %d 件あります(「アクセス不可」で検索できます)", errs.Load()))
	}
	return res, nil
}
