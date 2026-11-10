//go:build windows

package ingest

import (
	"strings"
	"sync"
	"unsafe"

	"github.com/aldeus-vip/folder_management_tool/internal/fsdb"
	"golang.org/x/sys/windows"
)

// アカウント名の解決結果(SID → DOMAIN\name)。同じ SID が大量に出るためキャッシュする。
var sidNames sync.Map

func sidName(sid *windows.SID) (string, string) {
	key := sid.String()
	if v, ok := sidNames.Load(key); ok {
		return key, v.(string)
	}
	name := ""
	if acc, dom, _, err := sid.LookupAccount(""); err == nil {
		name = acc
		if dom != "" {
			name = dom + `\` + acc
		}
	}
	sidNames.Store(key, name) // 解決できない(削除されたアカウントなど)場合も空で覚える
	return key, name
}

// longPath は 260 文字を超えるパスでも読めるよう \\?\ 形式にする。
func longPath(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\`):
		return p
	case strings.HasPrefix(p, `\\`):
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// readACL はファイル・フォルダのアクセス権限(DACL)を読む。
func readACL(path string) (*fsdb.ACL, error) {
	sd, err := windows.GetNamedSecurityInfo(longPath(path), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return nil, err
	}
	a := &fsdb.ACL{Protected: ctl&windows.SE_DACL_PROTECTED != 0, ACEs: []fsdb.ACE{}}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil { // DACL なし = 全員にフルコントロール(制限なし)
		return a, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			continue
		}
		t := ace.Header.AceType
		if t != windows.ACCESS_ALLOWED_ACE_TYPE && t != windows.ACCESS_DENIED_ACE_TYPE {
			continue // オブジェクト ACE・監査用などはファイルサーバーではほぼ使われないため対象外
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		s, name := sidName(sid)
		a.ACEs = append(a.ACEs, fsdb.ACE{
			Deny:      t == windows.ACCESS_DENIED_ACE_TYPE,
			SID:       s,
			Principal: name,
			Rights:    uint32(ace.Mask),
			Inherited: ace.Header.AceFlags&windows.INHERITED_ACE != 0,
			Flags:     ace.Header.AceFlags & 0x0F,
		})
	}
	return a, nil
}
