package fsdb

import (
	"fmt"
	"strings"
)

// ---- 記録用のパス(共有パス)と、このPCでの実際の場所 ----
// OneDrive の同期フォルダ(C:\Users\個人名\...)をスキャンすると、パスに個人のローカルパスが入ってしまう。
// そこで、DBには SharePoint の URL などの「記録用のパス(alias)」で保存し、実際にファイルを開くときは
// PCごとの実際の場所(local)に読み替える。全員が同じパスで作業・統合できる。
//   meta: root = 記録用のパス, alias_root = 記録用のパス(設定時のみ),
//         local_root / local_sep = スキャンした実際の場所と区切り文字

// AliasSep は記録用のパスに使う区切り文字(URLなら /)。
func AliasSep(alias string) string {
	if strings.Contains(alias, "://") || (strings.Contains(alias, "/") && !strings.Contains(alias, `\`)) {
		return "/"
	}
	return `\`
}

func trimRoot(p, sep string) string {
	t := strings.TrimRight(strings.TrimSpace(p), `\/`)
	if t == "" || strings.HasSuffix(t, ":") {
		return t + sep
	}
	return t
}

func joinRoot(root, sep, rest string) string {
	if rest == "" {
		return root
	}
	if strings.HasSuffix(root, sep) {
		return root + rest
	}
	return root + sep + rest
}

// LocalPath は記録用のパスを、このPCでの実際のパスに読み替える(local が空ならスキャンした場所)。
func LocalPath(p string, m map[string]string, local string) string {
	a := m["alias_root"]
	if a == "" {
		return p
	}
	if local == "" {
		local = m["local_root"]
	}
	sep, lsep := m["sep"], m["local_sep"]
	if lsep == "" {
		lsep = `\`
	}
	if p == a {
		return local
	}
	prefix := a
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	return joinRoot(local, lsep, strings.ReplaceAll(p[len(prefix):], sep, lsep))
}

// SetAlias は記録用のパスを変更する(空にするとスキャンした実際の場所に戻す)。すべての項目のパスを書き換える。
func (s *Store) SetAlias(alias string) error {
	m, err := s.Meta()
	if err != nil {
		return err
	}
	if m["virtual"] == "true" {
		return fmt.Errorf("共通の親フォルダが無い統合DBには設定できません")
	}
	oldRoot, oldSep := m["root"], m["sep"]
	local, lsep := m["local_root"], m["local_sep"]
	if local == "" {
		local, lsep = oldRoot, oldSep // 初めて設定する: 今のルートが実際の場所
	}
	newRoot, newSep := local, lsep
	if strings.TrimSpace(alias) != "" {
		newSep = AliasSep(alias)
		newRoot = trimRoot(alias, newSep)
	}
	if newRoot == oldRoot && newSep == oldSep {
		return nil
	}
	start := runeLen(oldRoot) + 1
	if !strings.HasSuffix(oldRoot, oldSep) {
		start++
	}
	prefix := newRoot
	if !strings.HasSuffix(prefix, newSep) {
		prefix += newSep
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE nodes SET path = ? || replace(substr(path, ?), ?, ?) WHERE id > 1`, prefix, start, oldSep, newSep); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET path = ?, name = ? WHERE id = 1`, newRoot, newRoot); err != nil {
		return err
	}
	aliasRoot := ""
	if strings.TrimSpace(alias) != "" {
		aliasRoot = newRoot
	}
	for k, v := range map[string]string{"root": newRoot, "sep": newSep, "alias_root": aliasRoot, "local_root": local, "local_sep": lsep} {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.summary, s.vt = nil, nil
	s.mu.Unlock()
	return nil
}

// AliasRoot は記録用のパスのルートと区切り文字を返す(スキャン時に使用)。
func AliasRoot(alias string) (string, string) {
	sep := AliasSep(alias)
	return trimRoot(alias, sep), sep
}
