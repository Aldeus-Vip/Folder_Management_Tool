//go:build windows

package osutil

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
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
	case SaveDB:
		body = `$d = New-Object System.Windows.Forms.SaveFileDialog; $d.Filter = 'フォルダ整理DB (*.db)|*.db'; $d.OverwritePrompt = $true`
	case PickFolder:
		body = `$d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description = 'スキャンするフォルダを選択'; $d.ShowNewFolderButton = $false`
	default:
		return "", fmt.Errorf("unknown dialog %q", kind)
	}
	if initial != "" {
		if kind == PickFolder {
			body += "; $d.SelectedPath = " + psQuote(initial)
		} else {
			body += "; $d.FileName = " + psQuote(initial)
		}
	}
	prop := "FileName"
	if kind == PickFolder {
		prop = "SelectedPath"
	}
	script := `[Console]::OutputEncoding = [Text.Encoding]::UTF8; Add-Type -AssemblyName System.Windows.Forms; ` +
		`$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true; ShowInTaskbar = $false}; ` +
		body + `; if ($d.ShowDialog($owner) -eq 'OK') { Write-Output $d.` + prop + ` }`
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
