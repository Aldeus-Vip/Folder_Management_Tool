package fsdb

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 名前・構造から静的に決まる警告フラグ(DBの flags 列に保存)。
// 更新日の古さ・パス長・階層の深さ・ファイル過多は閾値を後から変更できるよう、
// フラグではなくクエリ時に列の値から判定する。
const (
	FlagBadChar     = 1 << 0 // SharePoint禁止文字
	FlagTrailing    = 1 << 1 // 末尾が「.」または半角スペース
	FlagReserved    = 1 << 2 // 予約語(CON, PRN, ...)
	FlagAccess      = 1 << 3 // アクセス不可
	FlagEmptyDir    = 1 << 4 // 空フォルダ
	FlagTempFile    = 1 << 5 // 一時・システムファイル(~$xxx, Thumbs.db 等)
	FlagCopyName    = 1 << 6 // コピー的な名称(「- コピー」「(1)」「新しいフォルダー」等)
	FlagVersionName = 1 << 7 // 版管理的な名称(旧, old, bk, _v2, 最新 等)
	FlagSingleChild = 1 << 8 // 中身がフォルダ1つだけのフォルダ(階層を浅くできる候補)
	FlagDup         = 1 << 9 // 重複候補(同名・同サイズのファイルが他にもある)
)

const badChars = `\/:*?"<>|#%`

var reservedNames = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for i := '1'; i <= '9'; i++ {
		m["COM"+string(i)] = true
		m["LPT"+string(i)] = true
	}
	return m
}()

var tempNames = map[string]bool{"thumbs.db": true, ".ds_store": true, "desktop.ini": true, "ehthumbs.db": true}
var tempExts = map[string]bool{"tmp": true, "temp": true, "~tmp": true}

var (
	reCopyNum    = regexp.MustCompile(`[(（]\d{1,3}[)）]$`)
	copyWords    = []string{"コピー", " - copy", "copy of ", "新しいフォルダー", "新しいフォルダ", "new folder", "名称未設定", "無題", "untitled"}
	reVersionEng = regexp.MustCompile(`(^|[^a-z])(old|bk|bak|bkup|backup|final|latest)([^a-z]|$)|[_\-\s]v(er)?\.?\d+([^a-z0-9]|$)`)
	versionWords = []string{"旧", "最新", "最終", "修正版", "確定版", "バックアップ", "作業用"}
)

// NameFlags はファイル/フォルダ名だけから判定できるフラグを返す。
func NameFlags(name string, isDir bool) int {
	f := 0
	if strings.ContainsAny(name, badChars) || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 }) >= 0 {
		f |= FlagBadChar
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		f |= FlagTrailing
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToUpper(strings.TrimSpace(base))] {
		f |= FlagReserved
	}
	lc := strings.ToLower(name)
	stem := lc
	if !isDir {
		if i := strings.LastIndexByte(stem, '.'); i > 0 {
			stem = stem[:i]
		}
		if strings.HasPrefix(lc, "~$") || strings.HasPrefix(lc, ".~lock.") || tempNames[lc] || tempExts[Ext(name)] {
			f |= FlagTempFile
		}
	}
	for _, w := range copyWords {
		if strings.Contains(lc, w) {
			f |= FlagCopyName
			break
		}
	}
	if reCopyNum.MatchString(strings.TrimSpace(stem)) {
		f |= FlagCopyName
	}
	if reVersionEng.MatchString(stem) {
		f |= FlagVersionName
	} else {
		for _, w := range versionWords {
			if strings.Contains(stem, w) {
				f |= FlagVersionName
				break
			}
		}
	}
	if !isDir {
		if e := Ext(name); e == "bak" || e == "old" {
			f |= FlagVersionName
		}
	}
	return f
}

// Ext は小文字・ドットなしの拡張子を返す(".gitignore" のようなドットファイルは拡張子なし)。
func Ext(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	e := strings.ToLower(name[i+1:])
	if utf8.RuneCountInString(e) > 16 || strings.ContainsAny(e, " 　") {
		return ""
	}
	return e
}
