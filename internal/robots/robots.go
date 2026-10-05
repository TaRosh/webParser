// Package robots разбирает robots.txt по стандарту RFC 9309
// (https://www.rfc-editor.org/rfc/rfc9309) и отвечает, можно ли скачивать путь.
package robots

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// MaxSize — сколько байт robots.txt читаем. RFC требует разбирать
// не меньше 500 КиБ; всё, что дальше, отбрасывается.
const MaxSize = 512 << 10

// Rules — разобранный robots.txt целиком.
type Rules struct {
	groups []group
}

type group struct {
	agents     []string // имена роботов в нижнем регистре
	rules      []rule
	crawlDelay time.Duration
}

type rule struct {
	allow   bool
	pattern string
}

// Group — правила, которые относятся к конкретному роботу.
type Group struct {
	rules []rule
	// CrawlDelay — пауза между запросами, которую просит сайт (0 — не задана).
	// Директива не входит в RFC 9309, но её используют многие сайты.
	CrawlDelay  time.Duration
	disallowAll bool
}

// AllowAll — правила "можно всё": robots.txt нет (ответ 4xx).
func AllowAll() Group { return Group{} }

// DisallowAll — правила "нельзя ничего": robots.txt недоступен (5xx, сеть).
// RFC требует в этом случае считать сайт полностью закрытым.
func DisallowAll() Group { return Group{disallowAll: true} }

// Parse разбирает robots.txt. Ошибок не бывает: непонятные строки по
// стандарту просто игнорируются.
func Parse(r io.Reader) *Rules {
	var (
		rules   Rules
		cur     *group
		inRules bool // уже были правила после user-agent — следующий user-agent начинает новую группу
	)
	sc := bufio.NewScanner(io.LimitReader(r, MaxSize))
	sc.Buffer(make([]byte, 64<<10), MaxSize)

	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#") // комментарии
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if cur == nil || inRules {
				rules.groups = append(rules.groups, group{})
				cur = &rules.groups[len(rules.groups)-1]
				inRules = false
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
		case "allow", "disallow":
			if cur == nil {
				continue // правило до первого user-agent ни к кому не относится
			}
			inRules = true
			if value == "" {
				continue // пустой "Disallow:" означает "ничего не запрещено"
			}
			cur.rules = append(cur.rules, rule{allow: key == "allow", pattern: normalize(value)})
		case "crawl-delay":
			if cur == nil {
				continue
			}
			inRules = true
			if sec, err := strconv.ParseFloat(value, 64); err == nil && sec > 0 {
				cur.crawlDelay = time.Duration(sec * float64(time.Second))
			}
		}
		// Остальные директивы (sitemap, host, clean-param) нам не нужны.
	}
	return &rules
}

// Group выбирает правила для робота с именем agent (например, "webParser").
// Если о нём ничего не сказано — берутся правила для всех ("*").
// Несколько групп с одним именем объединяются (так требует RFC).
func (r *Rules) Group(agent string) Group {
	agent = strings.ToLower(agent)
	if g, ok := r.merge(agent); ok {
		return g
	}
	if g, ok := r.merge("*"); ok {
		return g
	}
	return AllowAll()
}

func (r *Rules) merge(agent string) (Group, bool) {
	var (
		res   Group
		found bool
	)
	for _, g := range r.groups {
		for _, a := range g.agents {
			if a == agent {
				found = true
				res.rules = append(res.rules, g.rules...)
				res.CrawlDelay = max(res.CrawlDelay, g.crawlDelay)
				break
			}
		}
	}
	return res, found
}

// Allowed сообщает, можно ли скачать путь. path — путь с query, как в URL:
// "/catalog/page-2.html" или "/search?q=go".
//
// Правило выбора: побеждает самое длинное совпавшее правило (самое
// конкретное); при равной длине Allow сильнее Disallow.
func (g Group) Allowed(path string) bool {
	if g.disallowAll {
		return false
	}
	if path == "/robots.txt" {
		return true // сам robots.txt всегда можно читать
	}
	path = normalize(path)

	bestLen, allowed := -1, true // ни одно правило не совпало — можно
	for _, r := range g.rules {
		if !match(r.pattern, path) {
			continue
		}
		n := len(r.pattern)
		if n > bestLen || (n == bestLen && r.allow) {
			bestLen, allowed = n, r.allow
		}
	}
	return allowed
}

// match проверяет путь по шаблону robots.txt: '*' — любые символы,
// '$' в конце — конец пути. Без '$' шаблон совпадает с началом пути (префикс).
func match(pattern, path string) bool {
	if p, ok := strings.CutSuffix(pattern, "$"); ok {
		return wildcard(p, path)
	}
	return wildcard(pattern+"*", path) // префикс = шаблон + "что угодно дальше"
}

// wildcard — сопоставление с '*' жадным алгоритмом с откатом:
// идём по строке и шаблону двумя указателями; встретив '*', запоминаем,
// где она была, и сначала считаем, что она "съела" 0 символов. При
// несовпадении откатываемся к последней '*' и даём ей съесть на 1 символ больше.
// Время O(len(s) * len(p)) в худшем случае, память O(1).
func wildcard(p, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0 // позиция последней '*' в шаблоне и в строке на тот момент
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' { // хвостовые звёздочки совпадают с пустой строкой
		pi++
	}
	return pi == len(p)
}

// normalize приводит путь и шаблоны к одному виду перед сравнением:
// не-ASCII символы (например, кириллица) кодируются как %XX, а уже
// закодированные %xx переводятся в верхний регистр. Тогда "/каталог"
// в robots.txt совпадёт с "/%D0%BA%D0%B0..." в адресе.
func normalize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(s[i+1 : i+3]))
			i += 2
		case c >= 0x80 || c == ' ':
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0F])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
