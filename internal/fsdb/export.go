package fsdb

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Actions は注記で選べるアクション(UIの選択肢と同じ)。
var Actions = []string{"残す", "要確認", "削除", "アーカイブ", "移動", "名前変更"}

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
	if n.Depth > st.DeepDepth {
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
	io.WriteString(w, "\uFEFF")
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	cw.Write([]string{"ID", "種別", "名前", "拡張子", "サイズ(KB)", "更新日時", "階層", "ファイル数(配下)", "パス文字数", "警告",
		"アクション", "担当", "新しい名前", "移動先", "メモ", "フルパス"})
	err := s.SearchEach(f, func(n *Node) error {
		kind := "ファイル"
		files := ""
		if n.IsDir {
			kind = "フォルダ"
			files = strconv.FormatInt(n.Files, 10)
		}
		return cw.Write([]string{strconv.FormatInt(n.ID, 10), kind, n.Name, n.Ext, strconv.FormatFloat(float64(n.Size)/1024, 'f', 1, 64),
			fmtTime(n.Mtime), strconv.Itoa(n.Depth), files, strconv.Itoa(n.PathLen), WarnLabels(n, st),
			n.Action, n.Owner, n.NewName, n.Dest, n.Memo, n.Path})
	})
	cw.Flush()
	if err != nil {
		return err
	}
	return cw.Error()
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// WritePlanScript はアクション(削除/アーカイブ/移動/名前変更)を実行するPowerShellスクリプトを生成する。
// 既定はドライラン(-Execute を付けたときだけ実際に変更)。
func (s *Store) WritePlanScript(w io.Writer) error {
	meta, _ := s.Meta()
	root := meta["root"]
	sep := meta["sep"]
	var ns []Node
	err := s.SearchEach(Filter{Action: "any"}, func(n *Node) error {
		if n.Action == "削除" || n.Action == "アーカイブ" || n.Action == "移動" || n.Action == "名前変更" {
			ns = append(ns, *n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("\uFEFF")
	fmt.Fprintf(&b, `# ==========================================================
# フォルダ整理 実行計画スクリプト (FolderManager が生成)
#   生成日時 : %s
#   対象ルート: %s
#   件数      : %d 件
#
# ■ 使い方
#   1) まずはそのまま実行してください(ドライラン: 何も変更しません)。
#        powershell -ExecutionPolicy Bypass -File .\このファイル.ps1
#      画面とログCSVで「何が起きるか」を確認できます。
#   2) 問題がなければ -Execute を付けて実行すると実際に変更されます。
#        powershell -ExecutionPolicy Bypass -File .\このファイル.ps1 -Execute
#   ※「削除」は完全削除です(ごみ箱に入りません)。不安な場合は
#     アクションを「アーカイブ」にして、退避先へ移動する運用を推奨します。
# ==========================================================
param(
  [switch]$Execute,
  [string]$ArchiveRoot = %s   # アーカイブ先(ルートからの相対パス構造を保って移動)
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
  if (-not (Test-Path -LiteralPath $src)) { Write-Log $op $src $dst 'NOTFOUND'; return }
  if ($DryRun) { Write-Log $op $src $dst 'DRYRUN'; return }
  try {
    & $action
    Write-Log $op $src $dst 'OK'
  } catch { Write-Log $op $src $dst ('ERROR: ' + $_.Exception.Message) }
}
function Ensure-Parent($p) {
  $parent = Split-Path -LiteralPath $p -Parent
  if ($parent -and -not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
}
if ($DryRun) { Write-Host '*** ドライラン(変更しません)。実行するには -Execute を付けてください ***' -ForegroundColor Cyan }

`, time.Now().Format("2006/01/02 15:04:05"), root, len(ns), psQuote(root+"_Archive"))

	// 親フォルダが削除/移動/アーカイブされる項目は、親の操作に含まれるので個別には実行しない。
	// 子→親の順(ID降順)で実行し、親の名前変更より先に子の操作が終わるようにする。
	type rng struct{ id, end int64 }
	var consumed []rng
	skip := map[int64]bool{}
	for _, n := range ns {
		for len(consumed) > 0 && consumed[len(consumed)-1].end < n.ID {
			consumed = consumed[:len(consumed)-1]
		}
		if len(consumed) > 0 {
			skip[n.ID] = true
			continue
		}
		if n.Action != "名前変更" {
			consumed = append(consumed, rng{n.ID, n.End})
		}
	}
	for i := len(ns) - 1; i >= 0; i-- {
		n := ns[i]
		src := psQuote(n.Path)
		fmt.Fprintf(&b, "# [%s] %s", n.Action, n.Path)
		if n.Memo != "" {
			fmt.Fprintf(&b, "  (メモ: %s)", strings.ReplaceAll(n.Memo, "\n", " "))
		}
		b.WriteString("\n")
		if skip[n.ID] {
			b.WriteString("#   → 親フォルダの操作に含まれるためスキップ\n\n")
			continue
		}
		switch n.Action {
		case "削除":
			fmt.Fprintf(&b, "Invoke-Step '削除' %s '' { Remove-Item -LiteralPath %s -Recurse -Force }\n", src, src)
		case "アーカイブ":
			rel := strings.TrimPrefix(strings.TrimPrefix(n.Path, root), sep)
			fmt.Fprintf(&b, "$dst = Join-Path $ArchiveRoot %s\nInvoke-Step 'アーカイブ' %s $dst { Ensure-Parent $dst; Move-Item -LiteralPath %s -Destination $dst }\n", psQuote(rel), src, src)
		case "移動":
			if n.Dest == "" {
				b.WriteString("#   → 移動先が未入力のためスキップ\n\n")
				continue
			}
			fmt.Fprintf(&b, "$dst = Join-Path %s %s\nInvoke-Step '移動' %s $dst { Ensure-Parent $dst; Move-Item -LiteralPath %s -Destination $dst }\n", psQuote(n.Dest), psQuote(n.Name), src, src)
		case "名前変更":
			if n.NewName == "" {
				b.WriteString("#   → 新しい名前が未入力のためスキップ\n\n")
				continue
			}
			fmt.Fprintf(&b, "Invoke-Step '名前変更' %s %s { Rename-Item -LiteralPath %s -NewName %s }\n", src, psQuote(n.NewName), src, psQuote(n.NewName))
		}
		b.WriteString("\n")
	}
	b.WriteString("Write-Host ('完了。ログ: ' + $log)\n")
	_, err = io.WriteString(w, strings.ReplaceAll(b.String(), "\n", "\r\n"))
	return err
}
