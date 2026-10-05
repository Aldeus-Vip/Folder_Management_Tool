package fsdb

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- 作業指示CSV(判断と実行を分離するための正本) ----
// 実行は「固定の汎用スクリプトがこのCSVを読み込む」形にする(SharePoint は PnP PowerShell、
// ファイルサーバーの削除は PowerShell、ファイルサーバー → SharePoint は SPMT)。
// 行は「子 → 親」の実行順(ExecOrder)に並べる。

// MigratedExts は初動移行(IT部門が SharePoint へそのまま移行した分)の対象拡張子(小文字・ドットなし)。
func (s *Store) MigratedExts() []string {
	var v string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='migrated_exts'`).Scan(&v)
	return splitExts(v)
}

func (s *Store) SetMigratedExts(list string) error {
	return s.SetMeta(map[string]string{"migrated_exts": strings.Join(splitExts(list), ",")})
}

func splitExts(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == ';' || r == '、'
	}) {
		x = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(x), "."))
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

var workOrderAction = map[string]string{ActMove: "Move", ActDelete: "Delete", ActHold: "Hold"}

// SharePoint で使えない文字(移行先の名前の確認用)
const spBadChars = `"*:<>?/\|`

// WriteWorkOrder は作業指示CSVを書き出す。
//
//   - 個別に設定したアクション(移動・削除・保留)を1行ずつ。親の「削除」に含まれる配下の「削除」は省略する
//   - 親を削除する場合でも、配下の「移動」は残す(先に移動してから親を削除する)。配下の「保留」は Note で警告する
//   - Location: ファイルは拡張子で一次判定(初動移行の対象なら SharePoint)、フォルダは Both
//   - 承認(Approval)は Pending で出力する。実行スクリプトは Approved の行だけを実行する
func (s *Store) WriteWorkOrder(w io.Writer) error {
	if err := s.ensureIndex(); err != nil {
		return err
	}
	r := s.Rules()
	meta, _ := s.Meta()
	sep := meta["sep"]
	if sep == "" {
		sep = `\`
	}
	// 判断の元になったスキャンの日時。実行時に、これより後に更新された項目がある削除は止められるようにする
	scanned := meta["scanned_at"]
	if scanned == "" {
		scanned = meta["merged_at"]
	}
	if t, err := time.ParseInLocation("2006/01/02 15:04:05", scanned, time.Local); err == nil {
		scanned = t.Format("2006-01-02T15:04:05")
	}
	exts := map[string]bool{}
	for _, e := range s.MigratedExts() {
		exts[e] = true
	}
	var ns []Node
	if err := s.SearchEach(Filter{State: "own"}, func(n *Node) error {
		if n.Action != "" {
			ns = append(ns, *n)
		}
		return nil
	}); err != nil {
		return err
	}
	// 削除フォルダの配下にある移動・保留の数(警告用)
	type sub struct{ moves, holds int }
	under := map[int64]*sub{}
	var delStack []*Node
	for i := range ns { // ns は ID 順(行きがけ順)
		n := &ns[i]
		for len(delStack) > 0 && delStack[len(delStack)-1].End < n.ID {
			delStack = delStack[:len(delStack)-1]
		}
		if len(delStack) > 0 {
			d := delStack[len(delStack)-1]
			if under[d.ID] == nil {
				under[d.ID] = &sub{}
			}
			switch n.Action {
			case ActMove:
				under[d.ID].moves++
			case ActHold:
				under[d.ID].holds++
			}
		}
		if n.Action == ActDelete && n.IsDir {
			delStack = append(delStack, n)
		}
	}
	io.WriteString(w, "\uFEFF")
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	cw.Write([]string{"Id", "ExecOrder", "SourcePath", "ItemType", "Action", "TargetRelPath", "Location", "Reason", "DecidedBy", "Approval",
		"SizeBytes", "FileCount", "LastModified", "ScannedAt", "Due", "Owner", "Note", "Result", "ExecutedAt"})
	order := 0
	for i := len(ns) - 1; i >= 0; i-- { // 子 → 親
		n := &ns[i]
		if n.Action == ActDelete && n.IAction == ActDelete {
			continue // 親フォルダの削除に含まれる
		}
		order++
		itemType, loc := "File", "FileServer"
		if n.IsDir {
			itemType, loc = "Folder", "Both"
		} else if exts[strings.ToLower(n.Ext)] {
			loc = "SharePoint"
		}
		var target string
		var notes []string
		if n.Action == ActMove {
			name := n.Name
			if n.NewName != "" {
				name = n.NewName
			}
			target = joinV(n.VPath, name)
			if strings.ContainsAny(name, spBadChars) {
				notes = append(notes, "移動先の名前にSharePointで使えない文字があります")
			}
			full := joinV(strings.TrimRight(r.BasePath, `\/`), target)
			if l := utf8.RuneCountInString(full); r.BasePath != "" && l > 400 {
				notes = append(notes, fmt.Sprintf("移動先のパスが%d文字(SharePointの上限400文字を超過)", l))
			}
		}
		if u := under[n.ID]; u != nil {
			if u.moves > 0 {
				notes = append(notes, fmt.Sprintf("配下の移動%d件を先に実行してから削除", u.moves))
			}
			if u.holds > 0 {
				notes = append(notes, fmt.Sprintf("配下に保留%d件あり(削除すると保留の項目も消えます)", u.holds))
			}
		}
		if n.Err != "" {
			notes = append(notes, "スキャン時にアクセスできませんでした: "+n.Err)
		}
		if n.Action == ActHold {
			notes = append(notes, "保留(実行しない)")
		}
		reason := n.Memo
		if reason == "" {
			reason = strings.Join(n.Tags, ", ")
		}
		owner := splitOwner(n.IOwner).Label()
		mtime := ""
		if n.Mtime > 0 {
			mtime = time.Unix(n.Mtime, 0).Format("2006-01-02T15:04:05")
		}
		if sep != `\` {
			target = strings.ReplaceAll(target, `\`, sep)
		}
		if err := cw.Write([]string{strconv.FormatInt(n.ID, 10), strconv.Itoa(order), n.Path, itemType, workOrderAction[n.Action], target, loc,
			reason, n.Editor, "Pending", strconv.FormatInt(n.InnerS, 10), strconv.FormatInt(n.Inner, 10), mtime, scanned, n.Due, owner,
			strings.Join(notes, " / "), "", ""}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTargetStructure は新構成のフォルダ一覧(仮想フォルダ。整理後のルートからの相対パス)を書き出す。
// 親 → 子の順なので、上から順に作成すればよい。
func (s *Store) WriteTargetStructure(w io.Writer) error {
	rows, err := s.VTreeAll()
	if err != nil {
		return err
	}
	io.WriteString(w, "\uFEFF")
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	cw.Write([]string{"TargetRelPath", "Depth", "Memo"})
	for _, v := range rows {
		if v.UUID == VRoot {
			continue
		}
		if err := cw.Write([]string{v.Path, strconv.Itoa(v.Depth), v.Memo}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
