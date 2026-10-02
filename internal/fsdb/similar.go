package fsdb

import (
	"strings"
	"unicode"
)

// normName は表記ゆれを吸収した比較用の名前(全角英数→半角、ひらがな→カタカナ、小文字化、空白・記号の除去)。
func normName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E: // 全角ASCII
			r -= 0xFEE0
		case r >= 0x3041 && r <= 0x3096: // ひらがな→カタカナ
			r += 0x60
		case r == 0x3000:
			r = ' '
		}
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return -1
		}
		return r
	}, s)
}

func bigrams(s string) map[string]int {
	rs := []rune(s)
	m := map[string]int{}
	for i := 0; i+1 < len(rs); i++ {
		m[string(rs[i:i+2])]++
	}
	return m
}

// simKey は類似判定用に前処理した名前。
type simKey struct {
	norm, digits string
	n            int
	grams        map[string]int
}

func mkSim(name string) simKey {
	n := normName(name)
	d := stripDigits(n)
	return simKey{norm: n, digits: d, n: len([]rune(d)), grams: bigrams(d)}
}

// Similar は2つのフォルダ名が「同じ目的の別名」らしいかを判定する。
// 数字だけが違う(2023年度/2024年度 など)ものは似ていないとみなす。
func Similar(a, b string) (bool, string) { return simPair(mkSim(a), mkSim(b)) }

func simPair(a, b simKey) (bool, string) {
	if a.norm == "" || b.norm == "" {
		return false, ""
	}
	if a.norm == b.norm {
		return true, "表記ゆれ"
	}
	if a.digits == b.digits {
		return false, "" // 数字(年度・連番)だけの違い
	}
	if a.n >= 2 && b.n >= 2 && (strings.Contains(a.digits, b.digits) || strings.Contains(b.digits, a.digits)) {
		return true, "一方を含む"
	}
	if len(a.grams) == 0 || len(b.grams) == 0 {
		return false, ""
	}
	inter, total := 0, 0
	for k, v := range a.grams {
		inter += min(v, b.grams[k])
		total += v
	}
	for _, v := range b.grams {
		total += v
	}
	if float64(2*inter)/float64(total) >= 0.6 {
		return true, "類似"
	}
	return false, ""
}

func similarAmong(name string, others []string) []string {
	k := mkSim(name)
	var out []string
	for _, o := range others {
		if ok, _ := simPair(k, mkSim(o)); ok {
			out = append(out, o)
		}
	}
	return out
}
