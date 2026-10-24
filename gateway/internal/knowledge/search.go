package knowledge

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// 关键词检索：BM25。中文没有空格，按相邻两个字切（“产品经理” → 产品、品经、经理），
// 拉丁字母和数字按词；标题、summary、标签、相关的人权重更高。不调模型，也不需要向量。

// Tokens 把文字切成检索用的词。
func Tokens(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var word, cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			out = append(out, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

type index struct {
	paths []string
	tf    []map[string]float64
	dl    []float64
	avgdl float64
	df    map[string]int
}

const maxIndexedBody = 20000

func buildIndex(entries []Entry) *index {
	ix := &index{df: map[string]int{}}
	total := 0.0
	for _, e := range entries {
		tf := map[string]float64{}
		add := func(text string, w float64) {
			for _, t := range Tokens(text) {
				tf[t] += w
			}
		}
		add(e.Title, 3)
		add(e.Summary, 2)
		add(strings.Join(e.Tags, " "), 2)
		add(strings.Join(e.People, " "), 2)
		body := e.Body
		if len(body) > maxIndexedBody {
			body = body[:maxIndexedBody]
		}
		add(body, 1)
		l := 0.0
		for t, n := range tf {
			l += n
			ix.df[t]++
		}
		ix.paths = append(ix.paths, e.Path)
		ix.tf = append(ix.tf, tf)
		ix.dl = append(ix.dl, l)
		total += l
	}
	if len(entries) > 0 {
		ix.avgdl = total / float64(len(entries))
	}
	return ix
}

type scored struct {
	path    string
	score   float64
	matched int // 命中了几个不同的查询词
}

func (ix *index) search(query string) ([]scored, int) {
	seen := map[string]bool{}
	var terms []string
	for _, t := range Tokens(query) {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
		if len(terms) >= 64 {
			break
		}
	}
	if len(terms) == 0 || len(ix.paths) == 0 {
		return nil, len(terms)
	}
	const k1, b = 1.2, 0.75
	n := float64(len(ix.paths))
	var out []scored
	for i, tf := range ix.tf {
		s := scored{path: ix.paths[i]}
		for _, t := range terms {
			f := tf[t]
			if f == 0 {
				continue
			}
			df := float64(ix.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			s.score += idf * f * (k1 + 1) / (f + k1*(1-b+b*ix.dl[i]/max(ix.avgdl, 1)))
			s.matched++
		}
		if s.matched > 0 {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].path < out[j].path
	})
	return out, len(terms)
}

// Hit 是一条搜索结果。
type Hit struct {
	Entry
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
	Matched int     `json:"-"`
	Terms   int     `json:"-"`
}

// snippet 在正文里找第一个命中的词，取它前后一段；找不到就用索引那一句。
func snippet(e Entry, query string, n int) string {
	body := []rune(e.Body)
	lower := []rune(strings.ToLower(e.Body))
	best := -1
	for _, t := range Tokens(query) {
		tr := []rune(t)
		for i := 0; i+len(tr) <= len(lower); i++ {
			if string(lower[i:i+len(tr)]) == t {
				if best < 0 || i < best {
					best = i
				}
				break
			}
		}
	}
	if best < 0 {
		return e.Line()
	}
	start := max(0, best-n/3)
	end := min(len(body), start+n)
	s := strings.TrimSpace(strings.ReplaceAll(string(body[start:end]), "\n", " "))
	if start > 0 {
		s = "…" + s
	}
	if end < len(body) {
		s += "…"
	}
	return s
}
