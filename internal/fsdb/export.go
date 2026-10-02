package fsdb

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ActionLabel はアクションの表示名。
func ActionLabel(a string) string {
	switch a {
	case ActDelete:
		return "削除"
	case ActMove:
		return "移動"
	case ActHold:
		return "保留"
	}
	return ""
}

func fmtTime(t int64) string {
	if t <= 0 {
		return ""
	}
	return time.Unix(t, 0).Format("2006/01/02 15:04")
}

// WarnLabels はフラグと閾値から警告ラベルを組み立てる。
func WarnLabels(n *Node, st Settings) string {
	var w []string
	for _, c := range []struct {
		f int
		s string
	}{{FlagBadChar, "禁止文字"}, {FlagTrailing, "末尾が.かスペース"}, {FlagReserved, "予約語"}, {FlagAccess, "アクセス不可"},
		{FlagEmptyDir, "空フォルダ"}, {FlagTempFile, "一時ファイル"}, {FlagCopyName, "コピー名"}, {FlagVersionName, "版管理名"},
		{FlagSingleChild, "中身がフォルダ1つ"}, {FlagDup, "重複候補"}} {
		if n.Flags&c.f != 0 {
			w = append(w, c.s)
		}
	}
	if n.PathLen > st.PathLimit {
		w = append(w, "パス長")
	}
	if n.Depth >= st.DeepDepth {
		w = append(w, "深い階層")
	}
	if n.IsDir && n.Children > int64(st.ManyFiles) {
		w = append(w, "項目過多")
	}
	if !n.IsDir && n.Mtime > 0 && n.Mtime < time.Now().AddDate(-st.OldYears, 0, 0).Unix() {
		w = append(w, fmt.Sprintf("更新から%d年以上", st.OldYears))
	}
	return strings.Join(w, " / ")
}

// WriteCSV は検索結果をExcelで開けるCSV(UTF-8 BOM付き)で書き出す。
func (s *Store) WriteCSV(w io.Writer, f Filter) error {
	st := s.Settings()
	r := s.Rules()
	io.WriteString(w, "\uFEFF")
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	cw.Write([]string{"ID", "種別", "名前", "拡張子", "サイズ(KB)", "更新日時", "階層", "ファイル数(配下)", "パス文字数", "警告",
		"アクション", "継承(親フォルダの設定)", "移動先(整理後)", "新しい名前", "期限", "タグ", "担当", "メモ", "作業者", "5Sルール", "フルパス"})
	err := s.SearchEach(f, func(n *Node) error {
		kind := "ファイル"
		files := ""
		if n.IsDir {
			kind = "フォルダ"
			files = strconv.FormatInt(n.Files, 10)
		}
		dest := ""
		if n.Action == ActMove {
			name := n.Name
			if n.NewName != "" {
				name = n.NewName
			}
			dest = joinV(r.RootName, n.VPath, name)
		}
		owner := n.Owner
		if owner == "" && n.IOwner != "" {
			owner = n.IOwner + "(親フォルダ)"
		}
		inh := ""
		if n.Action == "" && n.IAction != "" {
			inh = ActionLabel(n.IAction)
			if n.IVPath != "" {
				inh += " → " + n.IVPath
			}
		}
		return cw.Write([]string{strconv.FormatInt(n.ID, 10), kind, n.Name, n.Ext, strconv.FormatFloat(float64(n.Size)/1024, 'f', 1, 64),
			fmtTime(n.Mtime), strconv.Itoa(n.Depth), files, strconv.Itoa(n.PathLen), WarnLabels(n, st),
			ActionLabel(n.Action), inh, dest, n.NewName, n.Due, strings.Join(n.Tags, ", "), owner, n.Memo, n.Editor, strings.ReplaceAll(n.RuleMsg, "\n", " / "), n.Path})
	})
	cw.Flush()
	if err != nil {
		return err
	}
	return cw.Error()
}

func joinV(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, `\`)
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// WritePlanScript は整理計画(削除・移動)を実行するPowerShellスクリプトを生成する。
// 既定はドライラン(-Execute を付けたときだけ実際に変更)。
// localPath は記録用のパスをこのPCでの実際のパスに読み替える(nil なら読み替えない)。
func (s *Store) WritePlanScript(w io.Writer, localPath func(string) string) error {
	if localPath == nil {
		localPath = func(p string) string { return p }
	}
	if err := s.ensureIndex(); err != nil {
		return err
	}
	meta, _ := s.Meta()
	r := s.Rules()
	var ns []Node
	if err := s.SearchEach(Filter{State: "own"}, func(n *Node) error {
		if n.Action != "" && n.Action != ActHold { // 保留は何もしない
			ns = append(ns, *n)
		}
		return nil
	}); err != nil {
		return err
	}
	vfolders, err := s.VTreeAll()
	if err != nil {
		return err
	}
	base := r.BasePath
	if base == "" {
		base = "D:\\整理後"
	}
	var b strings.Builder
	b.WriteString("\uFEFF")
	fmt.Fprintf(&b, `# ==========================================================
# フォルダ整理 実行スクリプト (FolderManager が生成)
#   生成日時   : %s
#   対象ルート : %s
#   整理後ルート: %s (-TargetRoot で変更可)
#   件数       : %d 件(削除・移動)
#
# ■ 使い方
#   1) まずはそのまま実行してください(ドライラン: 何も変更しません)。
#        powershell -ExecutionPolicy Bypass -File .\このファイル.ps1
#   2) 画面とログCSVを確認し、問題なければ -Execute を付けて実行します。
#        powershell -ExecutionPolicy Bypass -File .\このファイル.ps1 -Execute
#   ※「削除」は完全削除です(ごみ箱に入りません)。
#   ※ 子→親の順に実行します(親フォルダを移動する前に、配下の個別の移動・削除を済ませるため)。
# ==========================================================
param(
  [switch]$Execute,
  [string]$TargetRoot = %s
)
$ErrorActionPreference = 'Stop'
$DryRun = -not $Execute
$log = Join-Path $PSScriptRoot ('plan_log_{0}.csv' -f (Get-Date -Format 'yyyyMMdd_HHmmss'))
function Write-Log($op, $src, $dst, $result) {
  [pscustomobject]@{ Time = (Get-Date -Format 's'); Op = $op; Source = $src; Dest = $dst; Result = $result } |
    Export-Csv -LiteralPath $log -Append -NoTypeInformation -Encoding UTF8
  $color = if ($result -eq 'OK') { 'Green' } elseif ($result -eq 'DRYRUN') { 'Cyan' } else { 'Yellow' }
  Write-Host ("[{0}] {1}: {2} {3}" -f $result, $op, $src, $(if ($dst) { "-> $dst" } else { '' })) -ForegroundColor $color
}
function Invoke-Step($op, $src, $dst, [scriptblock]$action) {
  if ($src -and -not (Test-Path -LiteralPath $src)) { Write-Log $op $src $dst 'NOTFOUND'; return }
  if ($DryRun) { Write-Log $op $src $dst 'DRYRUN'; return }
  try { & $action; Write-Log $op $src $dst 'OK' } catch { Write-Log $op $src $dst ('ERROR: ' + $_.Exception.Message) }
}
function Ensure-Dir($p) { if (-not (Test-Path -LiteralPath $p)) { New-Item -ItemType Directory -Path $p -Force | Out-Null } }
function T($rel) { if ($rel) { Join-Path $TargetRoot $rel } else { $TargetRoot } }
if ($DryRun) { Write-Host '*** ドライラン(変更しません)。実行するには -Execute を付けてください ***' -ForegroundColor Cyan }

# ---- 整理後のフォルダ構成を作成 ----
`, time.Now().Format("2006/01/02 15:04:05"), meta["root"], base, len(ns), psQuote(base))
	for _, v := range vfolders {
		if v.Kind == "vdir" && v.Path != "" {
			fmt.Fprintf(&b, "Invoke-Step 'フォルダ作成' '' (T %s) { Ensure-Dir (T %s) }\n", psQuote(v.Path), psQuote(v.Path))
		}
	}
	b.WriteString("\n# ---- 削除・移動(子→親の順) ----\n")
	for i := len(ns) - 1; i >= 0; i-- {
		n := ns[i]
		src := psQuote(localPath(n.Path))
		fmt.Fprintf(&b, "# [%s] %s", ActionLabel(n.Action), n.Path)
		if n.Memo != "" {
			fmt.Fprintf(&b, "  (メモ: %s)", strings.ReplaceAll(n.Memo, "\n", " "))
		}
		if n.Due != "" {
			fmt.Fprintf(&b, "  (期限: %s)", n.Due)
		}
		b.WriteString("\n")
		switch n.Action {
		case ActDelete:
			fmt.Fprintf(&b, "Invoke-Step '削除' %s '' { Remove-Item -LiteralPath %s -Recurse -Force }\n", src, src)
		case ActMove:
			name := n.Name
			if n.NewName != "" {
				name = n.NewName
			}
			rel := joinV(n.VPath, name)
			fmt.Fprintf(&b, "$dst = T %s\nInvoke-Step '移動' %s $dst { Ensure-Dir (Split-Path -LiteralPath $dst -Parent); Move-Item -LiteralPath %s -Destination $dst }\n", psQuote(rel), src, src)
		}
	}
	b.WriteString("Write-Host ('完了。ログ: ' + $log)\n")
	_, err = io.WriteString(w, strings.ReplaceAll(b.String(), "\n", "\r\n"))
	return err
}
