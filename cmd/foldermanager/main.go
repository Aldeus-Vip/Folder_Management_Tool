// FolderManager: 大規模フォルダの棚卸し・整理(5S)ツール。
// ローカルにWebサーバーを立ててブラウザでUIを表示する。データはSQLite(.db)に保存する。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aldeus-vip/folder_management_tool/internal/osutil"
	"github.com/aldeus-vip/folder_management_tool/internal/server"
)

var version = "0.1.0"

func main() {
	port := flag.Int("port", 8765, "待ち受けポート(使用中なら空きポートを自動選択)")
	noBrowser := flag.Bool("no-browser", false, "ブラウザを自動で開かない")
	projects := flag.String("projects", "", "DBの保存フォルダ(既定: exeと同じ場所の projects)")
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

	app := server.New(dir, version)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, "起動できません:", err)
			os.Exit(1)
		}
	}
	srv := &http.Server{Handler: app.Handler()}
	app.Shutdown = func() { srv.Shutdown(context.Background()) }

	// DBファイルをexeにドラッグ&ドロップ、または引数で渡した場合はそのDBを開く
	initial := ""
	if flag.NArg() > 0 {
		initial = flag.Arg(0)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	if initial != "" {
		if err := app.OpenInitial(initial); err != nil {
			fmt.Fprintln(os.Stderr, "DBを開けません:", err)
		}
	}
	fmt.Printf("FolderManager %s\n", version)
	fmt.Printf("  画面: %s\n", url)
	fmt.Printf("  DB保存先: %s\n", dir)
	fmt.Println("  このウィンドウを閉じるとツールが終了します。")
	if !*noBrowser {
		osutil.OpenBrowser(url)
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
