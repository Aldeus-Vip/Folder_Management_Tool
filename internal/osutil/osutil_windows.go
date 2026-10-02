//go:build windows

package osutil

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Dialog はWindows標準のファイル/フォルダ選択ダイアログを表示し、選択されたパスを返す(キャンセル時は空)。
func Dialog(kind DialogKind, initial string) (string, error) {
	var body string
	switch kind {
	case OpenExcel:
		body = `$d = New-Object System.Windows.Forms.OpenFileDialog; $d.Filter = 'Excel (*.xlsx)|*.xlsx|すべて (*.*)|*.*'`
	case OpenDB:
		body = `$d = New-Object System.Windows.Forms.OpenFileDialog; $d.Filter = 'フォルダ整理DB (*.db)|*.db|すべて (*.*)|*.*'`
	case OpenDBs:
		body = `$d = New-Object System.Windows.Forms.OpenFileDialog; $d.Multiselect = $true; $d.Filter = 'フォルダ整理DB (*.db)|*.db|すべて (*.*)|*.*'`
	case SaveDB:
		body = `$d = New-Object System.Windows.Forms.SaveFileDialog; $d.Filter = 'フォルダ整理DB (*.db)|*.db'; $d.OverwritePrompt = $true`
	case PickFolder:
		body = `$d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description = 'スキャンするフォルダを選択'; $d.ShowNewFolderButton = $false`
	default:
		return "", fmt.Errorf("unknown dialog %q", kind)
	}
	if initial != "" && kind != OpenDBs {
		if kind == PickFolder {
			body += "; $d.SelectedPath = " + psQuote(initial)
		} else {
			body += "; $d.FileName = " + psQuote(initial)
		}
	}
	prop := "FileName"
	if kind == PickFolder {
		prop = "SelectedPath"
	} else if kind == OpenDBs {
		prop = "FileNames -join [Environment]::NewLine"
	}
	script := `[Console]::OutputEncoding = [Text.Encoding]::UTF8; Add-Type -AssemblyName System.Windows.Forms; ` +
		`$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true; ShowInTaskbar = $false}; ` +
		body + `; if ($d.ShowDialog($owner) -eq 'OK') { Write-Output ($d.` + prop + `) }`
	out, err := hidden(exec.Command("powershell.exe", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-Command", script)).Output()
	if err != nil {
		return "", fmt.Errorf("ダイアログを表示できません: %w", err)
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff")), nil
}

func OpenBrowser(url string) error {
	return hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

// Reveal はエクスプローラーで項目を選択した状態で開く。
func Reveal(path string, isDir bool) error {
	cmd := exec.Command("explorer.exe")
	if isDir {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + path + `"`}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	}
	return cmd.Start()
}

// Alert はメッセージボックスを表示する(コンソールを出さないため、エラーはここで知らせる)。
func Alert(msg string, isError bool) {
	text, _ := syscall.UTF16PtrFromString(msg)
	title, _ := syscall.UTF16PtrFromString("FolderManager")
	flags := uintptr(0x40) // MB_ICONINFORMATION
	if isError {
		flags = 0x10 // MB_ICONERROR
	}
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), flags|0x40000) // MB_TOPMOST
}
