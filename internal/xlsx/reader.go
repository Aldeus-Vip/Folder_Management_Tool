// Package xlsx は巨大な .xlsx を省メモリでストリーミング読み込みする最小限のリーダー。
// 値(文字列/数値)だけを扱い、書式・数式は無視する。
package xlsx

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

type File struct {
	zr       *zip.ReadCloser
	files    map[string]*zip.File
	Sheets   []Sheet
	shared   []string
	sharedOK bool
}

type Sheet struct {
	Name string
	Path string
	Size int64 // 展開後のXMLサイズ(進捗表示用)
}

func Open(name string) (*File, error) {
	zr, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("xlsxとして開けません: %w", err)
	}
	f := &File{zr: zr, files: map[string]*zip.File{}}
	for _, zf := range zr.File {
		f.files[strings.TrimPrefix(zf.Name, "/")] = zf
	}
	if err := f.readWorkbook(); err != nil {
		zr.Close()
		return nil, err
	}
	return f, nil
}

func (f *File) Close() error { return f.zr.Close() }

func (f *File) open(name string) (io.ReadCloser, error) {
	zf, ok := f.files[name]
	if !ok {
		return nil, fmt.Errorf("%s が見つかりません", name)
	}
	return zf.Open()
}

func (f *File) readWorkbook() error {
	rels := map[string]string{}
	if rc, err := f.open("xl/_rels/workbook.xml.rels"); err == nil {
		t := newTokenizer(rc)
		for t.next() {
			if t.kind == tokStart && t.name == "Relationship" {
				target := t.attr("Target")
				if strings.HasPrefix(target, "/") {
					target = strings.TrimPrefix(target, "/")
				} else {
					target = path.Join("xl", target)
				}
				rels[t.attr("Id")] = target
			}
		}
		rc.Close()
	}
	rc, err := f.open("xl/workbook.xml")
	if err != nil {
		return err
	}
	defer rc.Close()
	t := newTokenizer(rc)
	for t.next() {
		if t.kind == tokStart && t.name == "sheet" {
			id := t.attr("r:id")
			if id == "" {
				id = t.attrLocal("id")
			}
			p := rels[id]
			if zf, ok := f.files[p]; ok {
				f.Sheets = append(f.Sheets, Sheet{Name: t.attr("name"), Path: p, Size: int64(zf.UncompressedSize64)})
			}
		}
	}
	if t.err != nil {
		return t.err
	}
	if len(f.Sheets) == 0 {
		return fmt.Errorf("シートが見つかりません")
	}
	return nil
}

// loadShared は共有文字列テーブルを読み込む(Excelで保存し直したファイル用)。
func (f *File) loadShared() error {
	if f.sharedOK {
		return nil
	}
	f.sharedOK = true
	rc, err := f.open("xl/sharedStrings.xml")
	if err != nil {
		return nil // 共有文字列なし(inlineStr のみ)
	}
	defer rc.Close()
	t := newTokenizer(rc)
	var sb strings.Builder
	inT, inPh := false, 0
	for t.next() {
		switch t.kind {
		case tokStart:
			switch t.name {
			case "si":
				sb.Reset()
			case "t":
				inT = true
			case "rPh":
				inPh++
			}
		case tokEnd:
			switch t.name {
			case "si":
				f.shared = append(f.shared, sb.String())
			case "t":
				inT = false
			case "rPh":
				inPh--
			}
		case tokText:
			if inT && inPh == 0 {
				sb.Write(t.text)
			}
		}
	}
	return t.err
}

// Rows はシートを1行ずつ読み、fn に列値(0始まりの列番号順、欠けた列は空文字)を渡す。
// 読み込み済みバイト数は progress に通知する。fn が false を返すと中断する。
func (f *File) Rows(sh Sheet, progress func(read int64), fn func(row []string) bool) error {
	if err := f.loadShared(); err != nil {
		return err
	}
	rc, err := f.open(sh.Path)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &countReader{r: rc}
	t := newTokenizer(cr)
	var row []string
	var col int
	var typ string
	var val strings.Builder
	inV, inT, inPh := false, false, 0
	var lastReport int64
	for t.next() {
		switch t.kind {
		case tokStart:
			switch t.name {
			case "row":
				row = row[:0]
				col = 0
			case "c":
				if r := t.attr("r"); r != "" {
					col = colIndex(r)
				}
				typ = t.attr("t")
				val.Reset()
			case "v":
				inV = true
			case "t":
				inT = true
			case "rPh":
				inPh++
			}
		case tokEnd:
			switch t.name {
			case "v":
				inV = false
			case "t":
				inT = false
			case "rPh":
				inPh--
			case "c":
				for len(row) < col {
					row = append(row, "")
				}
				v := val.String()
				if typ == "s" {
					if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(f.shared) {
						v = f.shared[i]
					}
				}
				row = append(row, v)
				col++
			case "row":
				if !fn(row) {
					return nil
				}
				if progress != nil && cr.n-lastReport > 1<<20 {
					lastReport = cr.n
					progress(cr.n)
				}
			}
		case tokText:
			if inV || (inT && inPh == 0) {
				val.Write(t.text)
			}
		}
	}
	return t.err
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// colIndex は "AB12" → 27 (0始まり)。
func colIndex(ref string) int {
	n := 0
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
	}
	return n - 1
}

// ---- 最小XMLトークナイザ(encoding/xml より高速。xlsxのXMLに必要な範囲のみ対応) ----

type tokKind int

const (
	tokStart tokKind = iota
	tokEnd
	tokText
)

type tokenizer struct {
	r       *bufio.Reader
	kind    tokKind
	name    string // ローカル名(名前空間接頭辞を除いたもの)
	attrs   []byte
	text    []byte
	buf     []byte
	pendEnd bool // 自己終了タグ <x/> の終了イベントを次に返す
	err     error
}

func newTokenizer(r io.Reader) *tokenizer {
	return &tokenizer{r: bufio.NewReaderSize(r, 1<<20)}
}

func (t *tokenizer) next() bool {
	if t.pendEnd {
		t.pendEnd = false
		t.kind = tokEnd
		return true
	}
	for {
		c, err := t.r.ReadByte()
		if err != nil {
			if err != io.EOF {
				t.err = err
			}
			return false
		}
		if c != '<' {
			t.r.UnreadByte()
			raw, err := t.r.ReadSlice('<')
			if err == bufio.ErrBufferFull {
				t.buf = append(t.buf[:0], raw...)
				for err == bufio.ErrBufferFull {
					raw, err = t.r.ReadSlice('<')
					t.buf = append(t.buf, raw...)
				}
				raw = t.buf
			}
			if err != nil && err != io.EOF {
				t.err = err
				return false
			}
			if err == nil {
				raw = raw[:len(raw)-1]
				t.r.UnreadByte()
			}
			t.text = unescape(raw)
			t.kind = tokText
			return true
		}
		raw, err := t.readTag()
		if err != nil {
			t.err = err
			return false
		}
		if len(raw) == 0 {
			continue
		}
		switch raw[0] {
		case '?':
			continue
		case '!':
			if bytes.HasPrefix(raw, []byte("![CDATA[")) {
				t.text = append([]byte(nil), bytes.TrimSuffix(raw[8:], []byte("]]"))...)
				t.kind = tokText
				return true
			}
			continue // コメント・DOCTYPE
		case '/':
			t.name = localName(bytes.TrimSpace(raw[1:]))
			t.kind = tokEnd
			return true
		}
		self := raw[len(raw)-1] == '/'
		if self {
			raw = raw[:len(raw)-1]
		}
		i := bytes.IndexAny(raw, " \t\r\n")
		if i < 0 {
			t.name = localName(raw)
			t.attrs = nil
		} else {
			t.name = localName(raw[:i])
			t.attrs = raw[i:]
		}
		t.kind = tokStart
		t.pendEnd = self
		return true
	}
}

// readTag は '<' の直後から '>' までを読む(属性値内の '>' を考慮)。
func (t *tokenizer) readTag() ([]byte, error) {
	t.buf = t.buf[:0]
	var quote byte
	// コメントは "-->" まで、CDATA は "]]>" まで読む
	for {
		c, err := t.r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("XMLが途中で終わっています")
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
		} else if c == '"' || c == '\'' {
			if !bytes.HasPrefix(t.buf, []byte("!")) {
				quote = c
			}
		} else if c == '>' {
			b := t.buf
			if bytes.HasPrefix(b, []byte("!--")) && !bytes.HasSuffix(b, []byte("--")) {
				t.buf = append(t.buf, c)
				continue
			}
			if bytes.HasPrefix(b, []byte("![CDATA[")) && !bytes.HasSuffix(b, []byte("]]")) {
				t.buf = append(t.buf, c)
				continue
			}
			return b, nil
		}
		t.buf = append(t.buf, c)
	}
}

func localName(b []byte) string {
	if i := bytes.LastIndexByte(b, ':'); i >= 0 {
		b = b[i+1:]
	}
	return string(b)
}

// attr は属性値を返す(名前は接頭辞込みで完全一致)。
func (t *tokenizer) attr(name string) string {
	a := t.attrs
	for len(a) > 0 {
		a = bytes.TrimLeft(a, " \t\r\n")
		eq := bytes.IndexByte(a, '=')
		if eq < 0 {
			return ""
		}
		k := string(bytes.TrimSpace(a[:eq]))
		a = bytes.TrimLeft(a[eq+1:], " \t\r\n")
		if len(a) == 0 {
			return ""
		}
		q := a[0]
		end := bytes.IndexByte(a[1:], q)
		if end < 0 {
			return ""
		}
		v := a[1 : 1+end]
		a = a[2+end:]
		if k == name {
			return string(unescape(v))
		}
	}
	return ""
}

// attrLocal は接頭辞を無視して属性を探す。
func (t *tokenizer) attrLocal(local string) string {
	a := string(t.attrs)
	for _, part := range strings.Fields(a) {
		if eq := strings.IndexByte(part, '='); eq > 0 {
			k := part[:eq]
			if i := strings.LastIndexByte(k, ':'); i >= 0 {
				k = k[i+1:]
			}
			if k == local {
				return strings.Trim(part[eq+1:], `"'`)
			}
		}
	}
	return ""
}

func unescape(b []byte) []byte {
	if bytes.IndexByte(b, '&') < 0 {
		return append([]byte(nil), b...)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '&' {
			out = append(out, b[i])
			continue
		}
		semi := bytes.IndexByte(b[i:], ';')
		if semi < 0 {
			out = append(out, b[i])
			continue
		}
		ent := string(b[i+1 : i+semi])
		switch ent {
		case "amp":
			out = append(out, '&')
		case "lt":
			out = append(out, '<')
		case "gt":
			out = append(out, '>')
		case "quot":
			out = append(out, '"')
		case "apos":
			out = append(out, '\'')
		default:
			var r int64 = -1
			if strings.HasPrefix(ent, "#x") || strings.HasPrefix(ent, "#X") {
				r, _ = strconv.ParseInt(ent[2:], 16, 32)
			} else if strings.HasPrefix(ent, "#") {
				r, _ = strconv.ParseInt(ent[1:], 10, 32)
			}
			if r <= 0 {
				out = append(out, b[i:i+semi+1]...)
			} else {
				out = append(out, string(rune(r))...)
			}
		}
		i += semi
	}
	return out
}
