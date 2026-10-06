package crawl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaRosh/webParser/internal/catalog"
	"github.com/TaRosh/webParser/internal/fetch"
)

const site = "https://shop.test/"

// fakeSite — сайт в памяти: адрес → HTML или ошибка. Считает запросы.
type fakeSite struct {
	pages  map[string]string
	errs   map[string]error
	jitter bool // случайная задержка ответа — перемешивает порядок завершения воркеров

	mu    sync.Mutex
	calls []string
	count atomic.Int32
}

func (s *fakeSite) Get(ctx context.Context, u string) (*fetch.Page, error) {
	s.count.Add(1)
	s.mu.Lock()
	s.calls = append(s.calls, u)
	s.mu.Unlock()

	if s.jitter {
		time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, ok := s.errs[u]; ok {
		return nil, err
	}
	html, ok := s.pages[u]
	if !ok {
		return nil, &fetch.StatusError{URL: u, Code: 404}
	}
	parsed, _ := url.Parse(u)
	return &fetch.Page{URL: parsed, Body: []byte(html)}, nil
}

// listing — страница каталога с книгами books и ссылкой next ("" — последняя).
func listing(next string, books ...string) string {
	var b strings.Builder
	for _, name := range books {
		fmt.Fprintf(&b, `<article class="product_pod"><h3><a href="%s.html" title="%s">x</a></h3>`+
			`<p class="star-rating Two"></p><p class="price_color">£1.00</p>`+
			`<p class="instock availability">In stock</p></article>`, name, name)
	}
	if next != "" {
		fmt.Fprintf(&b, `<ul class="pager"><li class="next"><a href="%s">next</a></li></ul>`, next)
	}
	return b.String()
}

// bookPage — страница книги с категорией.
func bookPage(name string) string {
	return `<ul class="breadcrumb"><li><a>Home</a></li><li><a>Books</a></li><li><a>Cat-` + name + `</a></li></ul>` +
		`<div class="product_main"><h1>` + name + `</h1><p class="price_color">£2.00</p>` +
		`<p class="star-rating Four"></p></div>` +
		`<table><tr><th>UPC</th><td>upc-` + name + `</td></tr><tr><th>Availability</th><td>In stock (3 available)</td></tr></table>`
}

// shop — 3 страницы каталога по 2 книги + страницы всех книг.
func shop() *fakeSite {
	s := &fakeSite{pages: map[string]string{
		site + "p1": listing("p2", "a", "b"),
		site + "p2": listing("p3", "c", "d"),
		site + "p3": listing("", "e", "f"),
	}, errs: map[string]error{}}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		s.pages[site+n+".html"] = bookPage(n)
	}
	return s
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func titles(books []catalog.Book) string {
	var t []string
	for _, b := range books {
		t = append(t, b.Title)
	}
	return strings.Join(t, ",")
}

func TestListings(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*fakeSite)
		limit     int
		want      string // названия книг по порядку
		wantPages int
	}{
		{"all pages in order", nil, 0, "a,b,c,d,e,f", 3},
		{"limit stops early", nil, 3, "a,b,c", 2},
		{"loop in pagination", func(s *fakeSite) { s.pages[site+"p3"] = listing("p1", "e", "f") }, 0, "a,b,c,d,e,f", 3},
		{"duplicates across pages", func(s *fakeSite) { s.pages[site+"p2"] = listing("p3", "b", "c") }, 0, "a,b,c,e,f", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := shop()
			if tt.modify != nil {
				tt.modify(s)
			}
			res, err := Run(context.Background(), s, Options{StartURL: site + "p1", Limit: tt.limit}, quiet())
			if err != nil {
				t.Fatal(err)
			}
			if got := titles(res.Books); got != tt.want {
				t.Errorf("books = %s, want %s", got, tt.want)
			}
			if res.Pages != tt.wantPages || int(s.count.Load()) != tt.wantPages {
				t.Errorf("pages = %d, requests = %d, want %d", res.Pages, s.count.Load(), tt.wantPages)
			}
		})
	}
}

func TestListingError(t *testing.T) {
	s := shop()
	delete(s.pages, site+"p2")

	res, err := Run(context.Background(), s, Options{StartURL: site + "p1"}, quiet())
	var se *fetch.StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want 404", err)
	}
	if titles(res.Books) != "a,b" {
		t.Errorf("partial result lost: %s", titles(res.Books))
	}
}

func TestDetails(t *testing.T) {
	s := shop()
	s.jitter = true // воркеры завершаются вразнобой — порядок всё равно должен сохраниться

	var calls atomic.Int32
	res, err := Run(context.Background(), s, Options{
		StartURL: site + "p1",
		Details:  true,
		Workers:  4,
		Progress: func(Progress) { calls.Add(1) }, // вызывается из разных горутин
	}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(res.Books); got != "a,b,c,d,e,f" {
		t.Fatalf("order broken: %s", got)
	}
	for _, b := range res.Books {
		if b.Category != "Cat-"+b.Title || b.UPC != "upc-"+b.Title || b.Quantity != 3 {
			t.Errorf("book not enriched: %+v", b)
		}
	}
	if s.count.Load() != 3+6 {
		t.Errorf("requests = %d, want 9 (3 listing + 6 books)", s.count.Load())
	}
	if calls.Load() != 3+6 {
		t.Errorf("progress calls = %d", calls.Load())
	}
}

func TestDetailsBookFailureIsReported(t *testing.T) {
	s := shop()
	delete(s.pages, site+"c.html")

	res, err := Run(context.Background(), s, Options{StartURL: site + "p1", Details: true, Workers: 3}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Books) != 6 || res.Books[2].Title != "c" || res.Books[2].Category != "" {
		t.Errorf("failed book must keep catalog data: %+v", res.Books[2])
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "c.html") {
		t.Errorf("problems = %v", res.Problems)
	}
}

func TestDetailsStopsWhenBlocked(t *testing.T) {
	s := shop()
	s.errs[site+"b.html"] = fetch.ErrBlocked

	res, err := Run(context.Background(), s, Options{StartURL: site + "p1", Details: true, Workers: 1}, quiet())
	if !errors.Is(err, fetch.ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	// 3 страницы каталога + a + b; после блокировки книги c..f не запрашиваются.
	if n := s.count.Load(); n > 3+3 {
		t.Errorf("requests after block: total %d", n)
	}
	if len(res.Books) != 6 || res.Books[0].Category != "Cat-a" {
		t.Errorf("collected data lost: %+v", res.Books)
	}
}

func TestCancel(t *testing.T) {
	s := shop()
	ctx, cancel := context.WithCancel(context.Background())

	res, err := Run(ctx, s, Options{
		StartURL: site + "p1",
		Progress: func(p Progress) {
			if p.Phase == "каталог" && p.Done == 1 {
				cancel() // пользователь нажал Ctrl+C после первой страницы
			}
		},
	}, quiet())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if titles(res.Books) != "a,b" {
		t.Errorf("partial result = %s", titles(res.Books))
	}
}
