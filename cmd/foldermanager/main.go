// FolderManager: 大規模フォルダの棚卸し・整理(5S)ツール。
// ローカルにWebサーバーを立ててブラウザでUIを表示する。データはSQLite(.db)に保存する。
// Windows版はコンソールを表示しない(-H windowsgui でビルド)。ブラウザのタブを閉じると自動で終了する。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aldeus-vip/folder_management_tool/internal/osutil"
	"github.com/aldeus-vip/folder_management_tool/internal/server"
)

var version = "0.1.0"

func main() {
	port := flag.Int("port", 8765, "待ち受けポート(使用中なら空きポートを自動選択)")
	noBrowser := flag.Bool("no-browser", false, "ブラウザを自動で開かない(この場合は自動終了もしない)")
	projects := flag.String("projects", "", "DBの保存フォルダ(既定: exeと同じ場所の projects)")
	// 画面からのハートビートがこの時間途絶えたら終了する。
	// ブラウザは背景タブのタイマーを最大1分間隔まで間引くため、余裕を持たせている。
	idle := flag.Duration("idle", 3*time.Minute, "画面が閉じられてから自動終了するまでの最大時間")
	flag.Parse()

	dir := *projects
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			exe = "."
		}
		dir = filepath.Join(filepath.Dir(exe), "projects")
	}
	os.MkdirAll(dir, 0o755)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		// すでに起動中なら、その画面をブラウザで開いて終了する(二重起動しない)
		existing := fmt.Sprintf("http://127.0.0.1:%d/", *port)
		if isRunning(existing) && flag.NArg() == 0 {
			if !*noBrowser {
				osutil.OpenBrowser(existing)
			}
			return
		}
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			osutil.Alert("起動できません: "+err.Error(), true)
			os.Exit(1)
		}
	}

	app := server.New(dir, version)
	srv := &http.Server{Handler: app.Handler()}
	app.Shutdown = func() { srv.Shutdown(context.Background()) }

	// DBファイルをexeにドラッグ&ドロップ、または引数で渡した場合はそのDBを開く
	if flag.NArg() > 0 {
		if err := app.OpenInitial(flag.Arg(0)); err != nil {
			osutil.Alert("DBを開けません: "+err.Error(), true)
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	fmt.Printf("FolderManager %s\n  画面: %s\n  DB保存先: %s\n", version, url, dir)
	if !*noBrowser {
		if err := osutil.OpenBrowser(url); err != nil {
			osutil.Alert("ブラウザを開けませんでした。次のアドレスをブラウザで開いてください:\n"+url, true)
		}
		app.WatchIdle(*idle, app.Shutdown)
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		osutil.Alert("エラーで終了しました: "+err.Error(), true)
		os.Exit(1)
	}
}

func isRunning(url string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(url + "ping")
	if err != nil {
		return false
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64))
	return string(b) == "FolderManager"
}
