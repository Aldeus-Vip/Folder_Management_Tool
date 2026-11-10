//go:build !windows

package ingest

import "github.com/aldeus-vip/folder_management_tool/internal/fsdb"

// readACL は Windows 以外では読まない(NTFS のアクセス権限は Windows でのみ取得できる)。
func readACL(string) (*fsdb.ACL, error) { return nil, nil }
