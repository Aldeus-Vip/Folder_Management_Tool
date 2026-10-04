package fsdb

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// テスト用ツリー(ファイルはすべて1KB)
//
//	\\srv\share
//	├ 経理\            ← フォルダ
//	│ ├ 2023\ 請求書.pdf, 見積.xlsx
//	│ ├ 2024\ 請求書.pdf
//	│ └ メモ.txt
//	├ 総務\ 規程.docx, 古い\ a.txt
//	└ readme.txt
func testDB(t *testing.T) (*Store, map[string]int64) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "t.db")
	b := NewBuilder(`\\srv\share`, `\`)
	dir := func(p int32, n string) int32 { return b.Add(Record{Parent: p, IsDir: true, Name: n}) }
	file := func(p int32, n string) { b.Add(Record{Parent: p, Name: n, Size: 1024, Mtime: 1700000000}) }
	k := dir(0, "経理")
	y23, y24 := dir(k, "2023"), dir(k, "2024")
	file(y23, "請求書.pdf")
	file(y23, "見積.xlsx")
	file(y24, "請求書.pdf")
	file(k, "メモ.txt")
	g := dir(0, "総務")
	file(g, "規程.docx")
	file(dir(g, "古い"), "a.txt")
	file(0, "readme.txt")
	if err := b.Finalize(context.Background(), db, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ids := map[string]int64{}
	rows, _ := s.DB.Query(`SELECT id, path FROM nodes`)
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		ids[strings.TrimPrefix(p, `\\srv\share\`)] = id
	}
	rows.Close()
	return s, ids
}

func node(t *testing.T, s *Store, id int64) *Node {
	t.Helper()
	n, err := s.Node(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPlanInheritanceAndRemaining(t *testing.T) {
	s, ids := testDB(t)
	if r := node(t, s, 1); r.Rem != 7 || r.Files != 7 {
		t.Fatalf("root rem=%d files=%d", r.Rem, r.Files)
	}
	vid, _, err := s.VCreate(VRoot, "経理部", "A")
	if err != nil {
		t.Fatal(err)
	}
	// 経理フォルダを移動 → 配下はすべて「親で移動」
	rep, err := s.PlanMove([]int64{ids["経理"]}, vid, "A")
	if err != nil || rep.Applied != 1 {
		t.Fatalf("move: %+v %v", rep, err)
	}
	// 配下の 2023\見積.xlsx だけ個別に削除(上書き)
	s.PlanDelete([]int64{ids[`経理\2023\見積.xlsx`]}, "A")

	if n := node(t, s, ids[`経理\2023\請求書.pdf`]); n.Action != "" || n.IAction != ActMove || n.IVPath != "経理部" || n.Rem != 0 {
		t.Fatalf("inherited: %+v", n)
	}
	if n := node(t, s, ids[`経理\2023\見積.xlsx`]); n.Action != ActDelete || n.IAction != ActMove {
		t.Fatalf("override: %+v", n)
	}
	k := node(t, s, ids["経理"])
	if k.Inner != 3 || k.VPath != "経理部" { // 4ファイル - 個別に削除した1つ
		t.Fatalf("inner=%d vpath=%q", k.Inner, k.VPath)
	}
	if r := node(t, s, 1); r.Rem != 3 { // 総務の2つ + readme
		t.Fatalf("root rem=%d", r.Rem)
	}
	pp, _ := s.Progress()
	if pp.RemFiles != 3 || pp.MoveFiles != 3 || pp.DelFiles != 1 {
		t.Fatalf("progress %+v", pp)
	}
	// 仮想ツリー: 経理部 の下に「経理」が 3ファイルで並び、その中身から見積.xlsx は除かれる
	kids, _ := s.VChildren(vid)
	if len(kids) != 1 || kids[0].Name != "経理" || kids[0].Files != 3 {
		t.Fatalf("vchildren %+v", kids)
	}
	real, _ := s.VReal(ids[`経理\2023`], 2)
	if len(real) != 1 || real[0].Name != "請求書.pdf" {
		t.Fatalf("vreal %+v", real)
	}
	if v, _ := s.VNode(vid); v.Files != 3 {
		t.Fatalf("vnode files=%d", v.Files)
	}
	// 未処理/処理済みの検索
	for state, want := range map[string]int64{"unhandled": 6, "handled": 7, ActDelete: 1, ActMove: 1} {
		_, n, err := s.Search(Filter{State: state}, 0, 10)
		if err != nil || n != want {
			t.Errorf("state %s: %d %v (want %d)", state, n, err, want)
		}
	}
	// 処理済みを非表示
	kids2, _ := s.Children(1, false, true, nil)
	for _, c := range kids2 {
		if c.Name == "経理" {
			t.Fatal("planned folder should be hidden")
		}
	}
	// 仮想フォルダを削除すると移動の設定は解除される
	if n, err := s.VDelete(vid, "A"); err != nil || n != 1 {
		t.Fatalf("vdelete %d %v", n, err)
	}
	if r := node(t, s, 1); r.Rem != 6 {
		t.Fatalf("after vdelete rem=%d", r.Rem)
	}
}

func TestRules(t *testing.T) {
	s, ids := testDB(t)
	r := s.Rules()
	r.MaxDepth = 2
	r.TagNeed = "block"
	if err := s.SaveRules(r); err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.VCreate(VRoot, "A", "x")
	b, _, _ := s.VCreate(a, "B", "x")
	if _, is, _ := s.VCreate(b, "C", "x"); len(is.Blocks) == 0 {
		t.Fatal("depth 3 vfolder should be blocked")
	}
	// タグなしは禁止
	rep, _ := s.PlanMove([]int64{ids["readme.txt"]}, a, "x")
	if rep.Applied != 0 || len(rep.Issues) != 1 {
		t.Fatalf("tag rule: %+v", rep)
	}
	s.SetTags([]int64{ids["readme.txt"], ids["経理"]}, []string{"共有"}, nil)
	if rep, _ := s.PlanMove([]int64{ids["readme.txt"]}, a, "x"); rep.Applied != 1 {
		t.Fatalf("with tag: %+v", rep)
	}
	// 経理(配下2階層)を階層2のBへ → 経理=3, 2023=4, ファイル=5 で上限超え
	if rep, _ := s.PlanMove([]int64{ids["経理"]}, b, "x"); rep.Applied != 0 {
		t.Fatalf("depth rule: %+v", rep)
	}
	// 名前変更: 禁止文字はNG、正常な名前はOK
	nn := "re:adme.txt"
	if rep, _ := s.PlanSetFields([]int64{ids["readme.txt"]}, PlanFields{NewName: &nn}, "x"); len(rep.Issues) == 0 || len(rep.Issues[0].Blocks) == 0 {
		t.Fatalf("bad rename: %+v", rep)
	}
	nn = "はじめに.txt"
	if _, err := s.PlanSetFields([]int64{ids["readme.txt"]}, PlanFields{NewName: &nn}, "x"); err != nil {
		t.Fatal(err)
	}
	if n := node(t, s, ids["readme.txt"]); n.NewName != "はじめに.txt" {
		t.Fatalf("rename: %+v", n)
	}
	// 似た名前の警告
	_, is, _ := s.VCreate(VRoot, "ａ ", "x")
	if is == nil || len(is.Blocks) == 0 { // "A" と表記ゆれで同名 → 同名は禁止
		t.Fatalf("dup name: %+v", is)
	}
	_, is, _ = s.VCreate(VRoot, "経理関係書類", "x")
	_, is, _ = s.VCreate(VRoot, "経理関係", "x")
	if len(is.Warns) == 0 {
		t.Fatalf("similar warn: %+v", is)
	}
}

func TestSimilar(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"経理", "けいり", false}, {"ＡＢＣ資料", "abc資料", true}, {"2023年度", "2024年度", false},
		{"営業資料", "営業資料_旧", true}, {"契約書", "契約書類", true}, {"人事", "経理", false},
		{"プロジェクト資料", "プロジェクト関連資料", true},
	} {
		if got, _ := Similar(c.a, c.b); got != c.want {
			t.Errorf("%s / %s = %v", c.a, c.b, got)
		}
	}
}

func TestActionMerge(t *testing.T) {
	s, ids := testDB(t)
	dir := t.TempDir()
	// マスターに共通の仮想フォルダを作ってからコピーを配布
	shared, _, _ := s.VCreate(VRoot, "共通", "")
	cA, cB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	if err := s.WorkCopy(cA, "経理部_山田"); err != nil {
		t.Fatal(err)
	}
	if err := s.WorkCopy(cB, "総務部_佐藤"); err != nil {
		t.Fatal(err)
	}
	a, _ := Open(cA)
	b, _ := Open(cB)
	if a.EditorCode() != "経理部_山田" {
		t.Fatal("code")
	}
	// A: 経理→共通へ移動、readme 削除、タグ付け
	a.PlanMove([]int64{ids["経理"]}, shared, "経理部_山田")
	a.PlanDelete([]int64{ids["readme.txt"]}, "経理部_山田")
	a.SetTags([]int64{ids["経理"]}, []string{"会計"}, nil)
	// B: 新しい仮想フォルダへ総務を移動、readme は移動(→競合)、同名「共通」はリネーム(競合)
	nv, _, _ := b.VCreate(VRoot, "総務", "総務部_佐藤")
	b.PlanMove([]int64{ids["総務"], ids["readme.txt"]}, nv, "総務部_佐藤")
	b.VRename(shared, "共有", "総務部_佐藤")
	a.VRename(shared, "共用", "経理部_山田")
	a.PlanOwner([]int64{ids[`経理\2023`]}, []string{"管理部", "", "", "山田"}, "経理部_山田") // 担当も統合される
	b.PlanHold([]int64{ids[`総務\古い`]}, "総務部_佐藤")
	a.Close()
	b.Close()

	ma, err := AnalyzeActionMerge([]string{cA, cB}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ma.Close()
	if len(ma.Conflicts) != 2 {
		t.Fatalf("conflicts: %+v", ma.Conflicts)
	}
	choices := map[string]int{}
	for _, c := range ma.Conflicts {
		choices[c.Key] = 1 // B を採用
	}
	out := filepath.Join(dir, "master2.db")
	if _, err := ma.Apply(out, choices); err != nil {
		t.Fatal(err)
	}
	m, _ := Open(out)
	defer m.Close()
	if m.EditorCode() != "" {
		t.Fatal("merged master must have no code")
	}
	if n := node(t, m, ids["経理"]); n.Action != ActMove || n.VPath != "共有" || len(n.Tags) != 1 {
		t.Fatalf("経理: %+v", n)
	}
	if n := node(t, m, ids["総務"]); n.Action != ActMove || n.VPath != "総務" {
		t.Fatalf("総務: %+v", n)
	}
	if n := node(t, m, ids[`経理\2023`]); splitOwner(n.Owner) != (OwnerFields{"管理部", "", "", "山田"}) {
		t.Fatalf("owner: %+v", n)
	}
	if n := node(t, m, ids[`総務\古い`]); n.Action != ActHold {
		t.Fatalf("hold: %+v", n)
	}
	if n := node(t, m, ids["readme.txt"]); n.Action != ActMove {
		t.Fatalf("readme (B chosen): %+v", n)
	}
	// 統合後のマスターから作り直したコピーと、古い版のコピーは統合できない
	c2 := filepath.Join(dir, "c2.db")
	if err := m.WorkCopy(c2, "X"); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeActionMerge([]string{cA, c2}, ""); err == nil {
		t.Fatal("different revisions should be rejected")
	}
}

func TestPlanScript(t *testing.T) {
	s, ids := testDB(t)
	v, _, _ := s.VCreate(VRoot, "経理部", "x")
	s.PlanMove([]int64{ids["経理"]}, v, "x")
	s.PlanDelete([]int64{ids[`経理\メモ.txt`]}, "x")
	var b strings.Builder
	if err := s.WritePlanScript(&b, nil); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	del := strings.Index(out, "Invoke-Step '削除'")
	mv := strings.Index(out, "Invoke-Step '移動'")
	if del < 0 || mv < 0 || del > mv { // 子(削除)→親(移動)の順
		t.Fatalf("order:\n%s", out)
	}
	if !strings.Contains(out, `T '経理部\経理'`) {
		t.Fatalf("dest:\n%s", out)
	}
}

// 1人分の作業用コピーを元のマスターへ統合 → 別の人が古い版のコピーから同じマスターへ統合(マスターの最新と比較)
func TestMergeIntoMaster(t *testing.T) {
	s, ids := testDB(t)
	dir := t.TempDir()
	master := filepath.Join(dir, "master.db")
	if _, err := s.DB.Exec(`VACUUM INTO ?`, master); err != nil {
		t.Fatal(err)
	}
	m, _ := Open(master)
	cA, cB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	m.WorkCopy(cA, "山田")
	m.WorkCopy(cB, "佐藤")
	m.Close()
	a, _ := Open(cA)
	if mp, _ := a.Meta(); mp["master_path"] != mustAbs(master) {
		t.Fatalf("master_path=%q", mp["master_path"])
	}
	a.PlanDelete([]int64{ids["readme.txt"]}, "山田")
	a.Close()
	// 山田さん1人分をマスターへ
	ma, err := AnalyzeActionMerge([]string{cA}, master)
	if err != nil {
		t.Fatal(err)
	}
	if len(ma.Conflicts) != 0 || ma.Auto != 1 {
		t.Fatalf("single: %+v", ma)
	}
	warns, err := ma.Apply("", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(warns)
	backups, _ := filepath.Glob(filepath.Join(dir, "master_backup_*.db"))
	if len(backups) != 1 {
		t.Fatalf("backup: %v", backups)
	}
	// 佐藤さん(古い版から作ったコピー): readme を移動 → マスターの「削除」と競合
	b, _ := Open(cB)
	v, _, _ := b.VCreate(VRoot, "総務", "佐藤")
	b.PlanMove([]int64{ids["readme.txt"], ids["総務"]}, v, "佐藤")
	b.Close()
	ma, err = AnalyzeActionMerge([]string{cB}, master)
	if err != nil {
		t.Fatal(err)
	}
	if len(ma.Conflicts) != 1 || len(ma.Warnings) == 0 {
		t.Fatalf("rebase: conflicts=%+v warns=%v", ma.Conflicts, ma.Warnings)
	}
	c := ma.Conflicts[0]
	keep := 0 // マスター(現在)の「削除」を残す
	for i, o := range c.Options {
		if strings.Contains(strings.Join(o.Editors, ","), "マスター") {
			keep = i
		}
	}
	if _, err := ma.Apply("", map[string]int{c.Key: keep}); err != nil {
		t.Fatal(err)
	}
	m2, _ := Open(master)
	defer m2.Close()
	if n := node(t, m2, ids["readme.txt"]); n.Action != ActDelete {
		t.Fatalf("readme: %+v", n)
	}
	if n := node(t, m2, ids["総務"]); n.Action != ActMove || n.VPath != "総務" {
		t.Fatalf("総務: %+v", n)
	}
	if m2.EditorCode() != "" {
		t.Fatal("master must stay code-less")
	}
}

func TestAlias(t *testing.T) {
	s, ids := testDB(t)
	url := "https://example.sharepoint.com/sites/Teams_758/Shared Documents/"
	if err := s.SetAlias(url); err != nil {
		t.Fatal(err)
	}
	n := node(t, s, ids[`経理\2023\請求書.pdf`])
	if n.Path != "https://example.sharepoint.com/sites/Teams_758/Shared Documents/経理/2023/請求書.pdf" {
		t.Fatalf("path=%s", n.Path)
	}
	if id, err := s.FindPath("https://example.sharepoint.com/sites/Teams_758/Shared Documents/経理"); err != nil || id != ids["経理"] {
		t.Fatalf("find %d %v", id, err)
	}
	m, _ := s.Meta()
	if lp := LocalPath(n.Path, m, `C:\Users\山田\OneDrive - 会社\部署 - Documents`); lp != `C:\Users\山田\OneDrive - 会社\部署 - Documents\経理\2023\請求書.pdf` {
		t.Fatalf("local=%s", lp)
	}
	if lp := LocalPath(n.Path, m, ""); lp != `\\srv\share\経理\2023\請求書.pdf` {
		t.Fatalf("scanned=%s", lp)
	}
	// 元に戻す
	if err := s.SetAlias(""); err != nil {
		t.Fatal(err)
	}
	if n := node(t, s, ids[`経理\2023\請求書.pdf`]); n.Path != `\\srv\share\経理\2023\請求書.pdf` {
		t.Fatalf("restored=%s", n.Path)
	}
}

// 第3階層までは「数字3桁_名前」、ファイルは置けない(カスタムルール)
func topRules(d int, mode string) []CustomRule {
	return []CustomRule{
		{Cat: "整頓", Mode: mode, Kind: "dir", DepthOp: "le", Depth: d, Must: Cond{"format", "num3"}},
		{Cat: "整頓", Mode: mode, Kind: "file", DepthOp: "le", Depth: d, Must: Cond{"forbid", ""}},
	}
}

func TestTopRule(t *testing.T) {
	s, ids := testDB(t)
	r := s.Rules()
	r.Custom = topRules(3, "block")
	r.MaxDepth = 8
	if err := s.SaveRules(r); err != nil {
		t.Fatal(err)
	}
	if _, is, _ := s.VCreate(VRoot, "経理", "x"); len(is.Blocks) == 0 {
		t.Fatal("name without prefix should be blocked")
	}
	a, is, _ := s.VCreate(VRoot, "０１０_経理", "x") // 全角も可
	if len(is.Blocks) > 0 {
		t.Fatalf("fullwidth: %+v", is)
	}
	b, _, _ := s.VCreate(a, "020_請求", "x")
	c, _, _ := s.VCreate(b, "030_2024年度", "x")
	if _, is, _ := s.VCreate(c, "自由な名前", "x"); len(is.Blocks) > 0 { // 第4階層は自由
		t.Fatalf("depth4: %+v", is)
	}
	// ファイルを第3階層(b の下)には置けない、第4階層(c の下)は可
	if rep, _ := s.PlanMove([]int64{ids["readme.txt"]}, b, "x"); rep.Applied != 0 {
		t.Fatalf("file at depth3: %+v", rep)
	}
	if rep, _ := s.PlanMove([]int64{ids["readme.txt"]}, c, "x"); rep.Applied != 1 {
		t.Fatalf("file at depth4: %+v", rep)
	}
	// フォルダ「総務」(直下にファイルあり)を第2階層へ: 名前・中身のファイルが違反
	rep, _ := s.PlanMove([]int64{ids["総務"]}, a, "x")
	if rep.Applied != 0 || len(rep.Issues[0].Blocks) < 2 {
		t.Fatalf("folder: %+v", rep)
	}
	// 第4階層へなら可
	if rep, _ := s.PlanMove([]int64{ids["総務"]}, c, "x"); rep.Applied != 1 {
		t.Fatalf("folder depth4: %+v", rep)
	}
	// ルールを後から厳しくした場合の違反一覧(第4階層まで → c の下のファイル・名前が違反)
	r.Custom = topRules(4, "block")
	s.SaveRules(r)
	vs, err := s.RuleViolations()
	if err != nil || len(vs) < 2 {
		t.Fatalf("violations: %+v %v", vs, err)
	}
	// 仮想フォルダの移動: c(030_2024年度)をルート直下へ → 中のファイルが第2階層になり違反
	if is, _ := s.VMove(c, VRoot, "x"); is == nil || len(is.Blocks) == 0 {
		t.Fatalf("vmove: %+v", is)
	}
}

// 旧バージョンの上位階層ルールはカスタムルールに移し替える
func TestLegacyTopRule(t *testing.T) {
	s, _ := testDB(t)
	s.SetMeta(map[string]string{"vrules": `{"rootName":"新","maxDepth":6,"depthMode":"block","pathLimit":250,"pathMode":"block","maxItems":100,"itemsMode":"warn","badName":"block","copyName":"warn","tagRequired":"off","topDepth":2,"topFormat":"custom","topPattern":"^[0-9]{2}-.+$","topNameMode":"warn","topFileMode":"block"}`})
	r := s.Rules()
	if len(r.Custom) != 2 || r.Custom[0].Must.Op != "regex" || r.Custom[0].Depth != 2 || r.Custom[1].Mode != "block" || r.RootName != "新" {
		t.Fatalf("migrated: %+v", r)
	}
	s.SetMeta(map[string]string{"vrules": `{"rootName":"新","maxDepth":6,"depthMode":"block","pathLimit":250,"pathMode":"block","maxItems":100,"itemsMode":"warn","badName":"block","copyName":"warn","tagRequired":"off"}`})
	if r := s.Rules(); len(r.Custom) != len(DefaultCustomRules) {
		t.Fatalf("defaults: %+v", r.Custom)
	}
}

func TestCustomRules(t *testing.T) {
	s, ids := testDB(t)
	r := s.Rules()
	r.Custom = []CustomRule{
		{Cat: "清掃", Mode: "block", Kind: "dir", Must: Cond{"notcontains", "古い, OLD"}},
		{Cat: "清潔", Mode: "warn", Kind: "any", DepthOp: "ge", Depth: 2, Must: Cond{"nochars", "＃ &"}},
		{Cat: "整頓", Mode: "warn", Kind: "dir", DepthOp: "eq", Depth: 1, When: Cond{"suffix", "部"}, Must: Cond{"haschild", "規定|SOP, 契約"}},
		{Cat: "整頓", Mode: "block", Kind: "file", Must: Cond{"maxlen", "8"}},
	}
	if err := s.SaveRules(r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRules(Rules{MaxDepth: 5, PathLimit: 100, MaxItems: 10, DepthMode: "off", PathMode: "off", ItemsMode: "off", BadName: "off", CopyName: "off", TagNeed: "off",
		Custom: []CustomRule{{Mode: "warn", Kind: "file", Must: Cond{"haschild", "a"}}}}); err == nil {
		t.Fatal("haschild on files should be rejected")
	}
	if _, is, _ := s.VCreate(VRoot, "old資料", "x"); len(is.Blocks) == 0 {
		t.Fatal("OLD (case-insensitive) should be blocked")
	}
	keiri, _, _ := s.VCreate(VRoot, "経理部", "x")
	sub, is, _ := s.VCreate(keiri, "A&B", "x")
	if len(is.Warns) == 0 {
		t.Fatalf("chars at depth2: %+v", is)
	}
	// 「総務」の中の「古い」フォルダが違反 → 移動できない
	if rep, _ := s.PlanMove([]int64{ids["総務"]}, sub, "x"); rep.Applied != 0 || !strings.Contains(strings.Join(rep.Issues[0].Blocks, ""), "古い") {
		t.Fatalf("subtree: %+v", rep)
	}
	// ファイル名の文字数
	if rep, _ := s.PlanMove([]int64{ids["readme.txt"]}, sub, "x"); rep.Applied != 0 {
		t.Fatalf("maxlen: %+v", rep)
	}
	// 必須フォルダ: 経理部の直下に「規定」「契約」が無い → 違反一覧に出る。作れば消える
	has := func() bool {
		vs, _ := s.RuleViolations()
		for _, v := range vs {
			if strings.Contains(v.Msg, "直下に") {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("required folders should be reported")
	}
	s.VCreate(keiri, "SOP", "x")
	s.VCreate(keiri, "契約書", "x")
	if has() {
		t.Fatal("required folders exist now")
	}
}

// 保留・担当・ツリーの絞り込み
func TestHoldOwnerFilter(t *testing.T) {
	s, ids := testDB(t)
	if _, err := s.PlanHold([]int64{ids[`経理\2023`]}, "x"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Progress()
	if p.HoldFiles != 2 || p.RemFiles != 5 {
		t.Fatalf("progress: %+v", p)
	}
	if n := node(t, s, ids[`経理\2023\見積.xlsx`]); n.IAction != ActHold {
		t.Fatalf("inherit hold: %+v", n)
	}
	// 担当: 経理=山田、経理\2024=佐藤(配下で上書き)
	// 担当: 経理=部「管理部」・担当者「山田」、経理\2024=担当者「佐藤」だけ(部は親から引き継ぐ)
	s.PlanOwner([]int64{ids["経理"]}, []string{"管理部", "", "", "山田"}, "x")
	s.PlanOwner([]int64{ids[`経理\2024`]}, []string{"", "", "", "佐藤"}, "x")
	if n := node(t, s, ids[`経理\2023\請求書.pdf`]); splitOwner(n.IOwner) != (OwnerFields{"管理部", "", "", "山田"}) || n.Owner != "" {
		t.Fatalf("owner inherit: %+v", n)
	}
	if n := node(t, s, ids[`経理\2024`]); splitOwner(n.Owner) != (OwnerFields{"", "", "", "佐藤"}) || splitOwner(n.IOwner) != (OwnerFields{"管理部", "", "", "佐藤"}) {
		t.Fatalf("owner own: %+v", n)
	}
	if _, err := s.TreeFilterFor("9:x", ""); err == nil {
		t.Fatal("bad level")
	}
	tf0, _ := s.TreeFilterFor("0:管理部", "")
	if kids, _ := s.Children(ids["経理"], false, false, tf0); len(kids) != 3 {
		t.Fatalf("level0 filter: %+v", kids)
	}
	// 担当を設定しても保留(アクション)は消えない。担当の解除で行が残らない
	s.PlanOwner([]int64{ids[`経理\2023`]}, []string{"", "", "", "山田"}, "x")
	s.PlanOwner([]int64{ids[`経理\2023`]}, nil, "x")
	if n := node(t, s, ids[`経理\2023`]); n.Action != ActHold {
		t.Fatalf("hold kept: %+v", n)
	}
	tf, err := s.TreeFilterFor("3:山田", "")
	if err != nil {
		t.Fatal(err)
	}
	kids, _ := s.Children(1, false, false, tf)
	if len(kids) != 1 || kids[0].Name != "経理" || kids[0].TF != 1 {
		t.Fatalf("filter root: %+v", kids)
	}
	sub, _ := s.Children(ids["経理"], false, false, tf)
	for _, k := range sub {
		if k.Name == "2024" {
			t.Fatal("2024 belongs to 佐藤")
		}
	}
	tf, _ = s.TreeFilterFor("3:佐藤", "")
	kids, _ = s.Children(1, false, false, tf)
	if len(kids) != 1 || kids[0].TF != 2 {
		t.Fatalf("path only: %+v", kids)
	}
	tf, _ = s.TreeFilterFor("-", "")
	kids, _ = s.Children(1, false, false, tf)
	if len(kids) != 2 { // 総務・readme.txt
		t.Fatalf("unassigned: %+v", kids)
	}
	tf, _ = s.TreeFilterFor("3:山田", "hold")
	if kids, _ := s.Children(ids["経理"], false, false, tf); len(kids) != 1 || kids[0].Name != "2023" {
		t.Fatalf("owner+hold: %+v", kids)
	}
	ns, total, err := s.Search(Filter{Owner: "3:山田", Kind: "file"}, 0, 100)
	if err != nil || total != 3 {
		t.Fatalf("search owner: %d %v %+v", total, err, ns)
	}
	if _, total, _ := s.Search(Filter{State: "ihold"}, 0, 100); total != 3 {
		t.Fatalf("search ihold: %d", total)
	}
	os, _ := s.Owners()
	// 部: 管理部(4)、担当者: 山田(3)・佐藤(1)
	if len(os) != 3 || os[0] != (OwnerCount{0, "管理部", 4}) || os[1] != (OwnerCount{3, "山田", 3}) || os[2] != (OwnerCount{3, "佐藤", 1}) {
		t.Fatalf("owners: %+v", os)
	}
}

// 現在のフォルダ構成への当てはめ
func TestRuleHits(t *testing.T) {
	s, ids := testDB(t)
	r := s.Rules()
	r.Custom = topRules(1, "block")
	r.ApplyCurrent = true
	if err := s.SaveRules(r); err != nil {
		t.Fatal(err)
	}
	// 第1階層: 経理・総務(名前が形式外)、readme.txt(ファイル)が違反
	if n := s.RuleHitCount(); n != 3 {
		t.Fatalf("hits=%d", n)
	}
	if n := node(t, s, ids["readme.txt"]); n.RuleMsg == "" {
		t.Fatal("rule msg")
	}
	if _, total, _ := s.Search(Filter{Check: "rule5s"}, 0, 10); total != 3 {
		t.Fatalf("search=%d", total)
	}
	tf, _ := s.TreeFilterFor("", "rule")
	if kids, _ := s.Children(ids["経理"], false, false, tf); len(kids) != 0 {
		t.Fatalf("children of violating folder: %+v", kids)
	}
	// ずらし: 現在のルートが整理後の第1階層 → 経理などは第2階層で対象外、ルートが対象
	r.CurrentOffset = 1
	s.SaveRules(r)
	if n := s.RuleHitCount(); n != 1 {
		t.Fatalf("offset hits=%d", n)
	}
	r.ApplyCurrent = false
	s.SaveRules(r)
	if n := s.RuleHitCount(); n != 0 {
		t.Fatalf("off hits=%d", n)
	}
}
