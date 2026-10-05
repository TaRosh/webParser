package robots

import (
	"strings"
	"testing"
	"time"
)

func TestWildcard(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"*", "", true},
		{"*", "anything", true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"a*c", "abbbc", true},
		{"a*c", "abbbd", false},
		{"*.php", "/index.php", true},
		{"/a*b*c", "/aXXbYYc", true},
		{"/a*b*c", "/aXXcYYb", false},
		{"**a", "bba", true},
		{"a*", "a", true},
		{"*a*b", "xaxbxb", true}, // нужен откат: первая 'b' не последняя
	}
	for _, tt := range tests {
		if got := wildcard(tt.pattern, tt.s); got != tt.want {
			t.Errorf("wildcard(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		// Без '$' — совпадение по префиксу.
		{"/fish", "/fish", true},
		{"/fish", "/fish.html", true},
		{"/fish", "/fish/salmon.html", true},
		{"/fish", "/Fish.asp", false}, // регистр в путях важен
		{"/fish", "/catfish", false},
		// '/fish/' — только содержимое папки.
		{"/fish/", "/fish/", true},
		{"/fish/", "/fish", false},
		// '*' внутри шаблона.
		{"/*.php", "/index.php", true},
		{"/*.php", "/folder/any.php.file.html", true},
		{"/*.php", "/windows.PHP", false},
		// '$' — конец пути.
		{"/*.php$", "/filename.php", true},
		{"/*.php$", "/filename.php?parameters", false},
		{"/*.php$", "/filename.php/", false},
		// Query-параметры — часть пути.
		{"/*?page=", "/catalog?page=2", true},
		{"/*?page=", "/catalog", false},
	}
	for _, tt := range tests {
		if got := match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

const sample = `
# Общие правила
User-agent: *
Disallow: /private/
Disallow: /*?sort=
Allow: /private/public-page.html
Crawl-delay: 2

# Отдельные правила для нашего робота: две строки user-agent — одна группа
User-agent: webParser
User-agent: OtherBot
Disallow: /catalog/
Allow: /catalog/books/
Disallow: /tmp

User-agent: webParser
Crawl-delay: 0.5
Disallow: /admin
`

func TestGroupSelectionAndAllowed(t *testing.T) {
	rules := Parse(strings.NewReader(sample))

	tests := []struct {
		name  string
		agent string
		path  string
		want  bool
	}{
		// Для webParser есть своя группа — правила "*" к нему не применяются.
		{"own group: disallowed", "webParser", "/catalog/", false},
		{"own group: longest match wins", "webParser", "/catalog/books/1.html", true},
		{"own group: '*' rules ignored", "webParser", "/private/x", true},
		{"agent name is case-insensitive", "WEBPARSER", "/catalog/", false},
		{"groups with same agent are merged", "webParser", "/admin/users", false},
		{"two user-agent lines share a group", "OtherBot", "/tmp/file", false},
		{"no rule matches — allowed", "webParser", "/about", true},

		// Неизвестный робот получает правила "*".
		{"star group: disallowed", "SomeBot", "/private/secret", false},
		{"star group: more specific allow", "SomeBot", "/private/public-page.html", true},
		{"star group: query pattern", "SomeBot", "/books?sort=price", false},
		{"robots.txt always allowed", "SomeBot", "/robots.txt", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules.Group(tt.agent).Allowed(tt.path); got != tt.want {
				t.Errorf("Allowed(%q) for %q = %v, want %v", tt.path, tt.agent, got, tt.want)
			}
		})
	}
}

func TestCrawlDelay(t *testing.T) {
	rules := Parse(strings.NewReader(sample))
	tests := []struct {
		agent string
		want  time.Duration
	}{
		{"webParser", 500 * time.Millisecond}, // из объединённых групп webParser
		{"SomeBot", 2 * time.Second},          // из группы "*"
	}
	for _, tt := range tests {
		if got := rules.Group(tt.agent).CrawlDelay; got != tt.want {
			t.Errorf("CrawlDelay(%s) = %v, want %v", tt.agent, got, tt.want)
		}
	}
}

func TestEqualLengthAllowWins(t *testing.T) {
	// RFC 9309: при одинаковой длине правил побеждает менее строгое (Allow).
	rules := Parse(strings.NewReader("User-agent: *\nDisallow: /page\nAllow: /page\n"))
	if !rules.Group("x").Allowed("/page") {
		t.Error("equal-length allow must win")
	}
}

func TestEdgeCases(t *testing.T) {
	tests := []struct {
		name, robots, path string
		want               bool
	}{
		{"empty file", "", "/anything", true},
		{"empty disallow allows all", "User-agent: *\nDisallow:\n", "/x", true},
		{"disallow all", "User-agent: *\nDisallow: /\n", "/x", false},
		{"rules before any user-agent ignored", "Disallow: /\nUser-agent: *\nAllow: /\n", "/x", true},
		{"no group for us and no star", "User-agent: Google\nDisallow: /\n", "/x", true},
		{"keys are case-insensitive, comments stripped", "USER-AGENT: * # все\nDISALLOW: /x # закрыто\n", "/x/1", false},
		{"windows line endings", "User-agent: *\r\nDisallow: /x\r\n", "/x", false},
		{"cyrillic path in robots", "User-agent: *\nDisallow: /каталог\n", "/%d0%ba%d0%b0%d1%82%d0%b0%d0%bb%d0%be%d0%b3/1", false},
		{"garbage lines ignored", "<html>not robots</html>\nfoo bar\n", "/x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Parse(strings.NewReader(tt.robots)).Group("webParser").Allowed(tt.path); got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestAllowAllDisallowAll(t *testing.T) {
	if !AllowAll().Allowed("/x") {
		t.Error("AllowAll must allow")
	}
	if DisallowAll().Allowed("/x") || DisallowAll().Allowed("/robots.txt") {
		t.Error("DisallowAll must disallow everything")
	}
}

func TestParseLimitsSize(t *testing.T) {
	// Правило после 512 КиБ мусора не должно учитываться.
	big := "User-agent: *\n" + strings.Repeat("# padding\n", MaxSize/10) + "Disallow: /late\n"
	if !Parse(strings.NewReader(big)).Group("x").Allowed("/late") {
		t.Error("rules beyond MaxSize must be ignored")
	}
}
