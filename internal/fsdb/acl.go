package fsdb

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- アクセス権限(NTFS の ACL) ----
// SharePoint では「親から権限を引き継がない(固有の権限を持つ)」フォルダ・ファイルの数に上限がある
// (ライブラリあたり 50,000)。ファイルサーバーの権限を移行すると、継承が切れている場所・独自の付与がある場所が
// そのまま固有の権限になるため、スキャン時に ACL を読み取って数える。
//
// DB: acl_defs(ACL の内容。同じ内容は1行にまとめる) / acls(項目ごとの参照と、継承の状態)

// ACE は付与1件。
type ACE struct {
	Deny      bool   `json:"deny"`      // 拒否
	SID       string `json:"sid"`       // S-1-5-…
	Principal string `json:"principal"` // DOMAIN\name(解決できなければ空)
	Rights    uint32 `json:"rights"`    // アクセスマスク
	Inherited bool   `json:"inherited"` // 親から引き継いだ付与
	Flags     uint8  `json:"flags"`     // 継承の範囲(OBJECT_INHERIT 0x1 / CONTAINER_INHERIT 0x2 / INHERIT_ONLY 0x8)
}

// ACL は項目のアクセス権限。
type ACL struct {
	Protected bool  `json:"protected"` // 親からの継承を無効にしている
	ACEs      []ACE `json:"aces"`
}

// Explicit はこの項目に直接付与された(引き継いでいない)ACE の数。
func (a *ACL) Explicit() int {
	n := 0
	for _, e := range a.ACEs {
		if !e.Inherited {
			n++
		}
	}
	return n
}

// Unique は SharePoint で固有の権限になるか(継承を切っている、または独自の付与がある)。
func (a *ACL) Unique() bool { return a.Protected || a.Explicit() > 0 }

// Orphan は名前を解決できないアカウント(削除されたユーザーなど)を含むか。
func (a *ACL) Orphan() bool {
	for _, e := range a.ACEs {
		if e.Principal == "" {
			return true
		}
	}
	return false
}

// encode は DB に保存する形(1行1件: 種別 TAB SID TAB 名前 TAB 権限 TAB 継承 TAB 範囲)。
func (a *ACL) encode() string {
	var b strings.Builder
	for i, e := range a.ACEs {
		if i > 0 {
			b.WriteByte('\n')
		}
		t, inh := "A", "0"
		if e.Deny {
			t = "D"
		}
		if e.Inherited {
			inh = "1"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%x\t%s\t%x", t, e.SID, strings.ReplaceAll(e.Principal, "\t", " "), e.Rights, inh, e.Flags)
	}
	return b.String()
}

// DecodeACL はDBに保存した形から戻す(DBの統合用)。
func DecodeACL(protected bool, text string) *ACL {
	return &ACL{Protected: protected, ACEs: decodeACEs(text)}
}

func decodeACEs(s string) []ACE {
	out := []ACE{}
	if s == "" {
		return out
	}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 6 {
			continue
		}
		r, _ := strconv.ParseUint(f[3], 16, 32)
		fl, _ := strconv.ParseUint(f[5], 16, 8)
		out = append(out, ACE{Deny: f[0] == "D", SID: f[1], Principal: f[2], Rights: uint32(r), Inherited: f[4] == "1", Flags: uint8(fl)})
	}
	return out
}

// RightsLabel はアクセスマスクを、エクスプローラーの「セキュリティ」タブと同じ呼び方にする。
func RightsLabel(m uint32) string {
	const (
		genericAll     = 0x10000000
		genericExecute = 0x20000000
		genericWrite   = 0x40000000
		genericRead    = 0x80000000
	)
	switch {
	case m&genericAll != 0 || m&0x1F01FF == 0x1F01FF:
		return "フルコントロール"
	case m&0x1301BF == 0x1301BF:
		return "変更"
	case m&genericRead != 0 && m&genericWrite != 0:
		return "変更"
	case m&0x1201BF == 0x1201BF:
		return "読み取りと実行 + 書き込み"
	case m&0x1200A9 == 0x1200A9 || (m&genericRead != 0 && m&genericExecute != 0):
		return "読み取りと実行"
	case m&0x120089 == 0x120089 || m&genericRead != 0:
		return "読み取り"
	case m&0x100116 == 0x100116 || m&genericWrite != 0:
		return "書き込み"
	}
	return fmt.Sprintf("特殊なアクセス許可(0x%X)", m)
}

// ScopeLabel は継承の範囲(どこに適用されるか)。
func ScopeLabel(f uint8) string {
	oi, ci, io := f&0x1 != 0, f&0x2 != 0, f&0x8 != 0
	switch {
	case oi && ci && io:
		return "サブフォルダーとファイルのみ"
	case oi && ci:
		return "このフォルダー、サブフォルダー、ファイル"
	case ci && io:
		return "サブフォルダーのみ"
	case oi && io:
		return "ファイルのみ"
	case ci:
		return "このフォルダーとサブフォルダー"
	case oi:
		return "このフォルダーとファイル"
	}
	return "このフォルダーのみ"
}

// ACLView は画面表示用。
type ACLView struct {
	Scanned   bool      `json:"scanned"`   // 権限を取得したか
	Protected bool      `json:"protected"` // 親からの継承を無効にしている
	Unique    bool      `json:"unique"`    // SharePoint で固有の権限になる
	NoDetail  bool      `json:"noDetail"`  // 固有かどうかだけ取得(SharePoint の一覧CSV。付与先の内訳なし)
	ACEs      []ACEView `json:"aces"`
}

type ACEView struct {
	ACE
	RightsLabel string `json:"rightsLabel"`
	ScopeLabel  string `json:"scopeLabel"`
}

// NodeACL は項目のアクセス権限(取得していなければ Scanned=false)。
func (s *Store) NodeACL(id int64) (*ACLView, error) {
	var prot, expl int
	var text string
	err := s.DB.QueryRow(`SELECT a.protected, a.explicit, d.text FROM acls a JOIN acl_defs d ON d.id=a.def WHERE a.node_id=?`, id).Scan(&prot, &expl, &text)
	if err != nil {
		return &ACLView{ACEs: []ACEView{}}, nil
	}
	v := &ACLView{Scanned: true, Protected: prot == 1, Unique: prot == 1 || expl > 0, ACEs: []ACEView{}}
	if text == "" {
		var mode string
		s.DB.QueryRow(`SELECT value FROM meta WHERE key='acl_mode'`).Scan(&mode)
		v.NoDetail = mode == "spcsv"
	}
	for _, e := range decodeACEs(text) {
		v.ACEs = append(v.ACEs, ACEView{ACE: e, RightsLabel: RightsLabel(e.Rights), ScopeLabel: ScopeLabel(e.Flags)})
	}
	return v, nil
}

// ACLScanned は権限を取得したDBか。
func (s *Store) ACLScanned() bool {
	var n int
	s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM acls)`).Scan(&n)
	return n > 0
}

const DefaultPermLimit = 50000

func (s *Store) PermLimit() int64 {
	var v string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key='perm_limit'`).Scan(&v)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return n
	}
	return DefaultPermLimit
}

func (s *Store) SetPermLimit(n int64) error {
	if n <= 0 {
		return fmt.Errorf("上限は1以上を指定してください")
	}
	return s.SetMeta(map[string]string{"perm_limit": strconv.FormatInt(n, 10)})
}
