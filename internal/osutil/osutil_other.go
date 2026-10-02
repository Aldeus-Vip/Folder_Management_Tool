//go:build !windows

package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func Dialog(kind DialogKind, initial string) (string, error) { return "", ErrUnsupported }

func OpenBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func Reveal(path string, isDir bool) error { return ErrUnsupported }

func Alert(msg string, isError bool) { fmt.Fprintln(os.Stderr, msg) }
