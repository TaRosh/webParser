package catalog

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture открывает сохранённую страницу из testdata — тесты не ходят в сеть.
func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const base = "https://books.toscrape.com/catalogue/"

func TestParseListingFirstPage(t *testing.T) {
	l, err := ParseListing(mustURL(t, base+"page-1.html"), fixture(t, "listing_first.html"))
	if err != nil {
		t.Fatal(err)
	}

	if len(l.Books) != 20 || len(l.Problems) != 0 {
		t.Fatalf("books = %d, problems = %v; want 20 books, no problems", len(l.Books), l.Problems)
	}
	want := Book{
		Title:   "A Light in the Attic",
		Price:   Price{Minor: 5177, Currency: "GBP"},
		Rating:  3,
		InStock: true,
		URL:     base + "a-light-in-the-attic_1000/index.html",
	}
	if l.Books[0] != want {
		t.Errorf("first book:\n got %+v\nwant %+v", l.Books[0], want)
	}
	if l.Next != base+"page-2.html" {
		t.Errorf("next = %q", l.Next)
	}

	for _, b := range l.Books {
		if strings.HasSuffix(b.Title, "...") {
			t.Errorf("truncated title %q — must come from the title attribute", b.Title)
		}
		if b.Rating < 1 || b.Rating > 5 || b.Price.Minor <= 0 || !strings.HasPrefix(b.URL, base) {
			t.Errorf("suspicious book %+v", b)
		}
	}
}

func TestParseListingLastPage(t *testing.T) {
	l, err := ParseListing(mustURL(t, base+"page-50.html"), fixture(t, "listing_last.html"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Next != "" {
		t.Errorf("last page must have no next link, got %q", l.Next)
	}
	if len(l.Books) != 20 {
		t.Errorf("books = %d", len(l.Books))
	}
}

func TestParseBook(t *testing.T) {
	pageURL := base + "a-light-in-the-attic_1000/index.html"
	b, err := ParseBook(mustURL(t, pageURL), fixture(t, "book.html"))
	if err != nil {
		t.Fatal(err)
	}
	want := Book{
		Title:    "A Light in the Attic",
		Price:    Price{Minor: 5177, Currency: "GBP"},
		Rating:   3,
		InStock:  true,
		Quantity: 22,
		Category: "Poetry",
		UPC:      "a897fe39b1053632",
		URL:      pageURL,
	}
	if b != want {
		t.Errorf("\n got %+v\nwant %+v", b, want)
	}
}

// Синтетические страницы для ситуаций, которых нет на живом сайте.
func card(title, price, rating, stock string) string {
	return `<article class="product_pod"><h3><a href="book_1/index.html" title="` + title + `">x</a></h3>` +
		`<p class="star-rating ` + rating + `"></p><p class="price_color">` + price + `</p>` +
		`<p class="instock availability">` + stock + `</p></article>`
}

func TestParseListingEdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		html         string
		wantErr      error
		wantBooks    int
		wantProblems int
	}{
		{"no cards — layout changed", `<html><body><div class="new-design"></div></body></html>`, ErrLayoutChanged, 0, 0},
		{"broken price skipped", card("A", "£51.77", "One", "In stock") + card("B", "free", "Two", "In stock"), nil, 1, 1},
		{"unknown rating skipped", card("A", "£1.00", "Zero", "In stock"), nil, 0, 1},
		{"no title skipped", card("", "£1.00", "One", "In stock"), nil, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := ParseListing(mustURL(t, base), strings.NewReader(tt.html))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(l.Books) != tt.wantBooks || len(l.Problems) != tt.wantProblems {
				t.Errorf("books = %d, problems = %v", len(l.Books), l.Problems)
			}
		})
	}
}

func TestParseListingOutOfStock(t *testing.T) {
	l, err := ParseListing(mustURL(t, base), strings.NewReader(card("A", "£1.00", "Five", "Out of stock")))
	if err != nil || len(l.Books) != 1 {
		t.Fatalf("books = %v, err = %v", l.Books, err)
	}
	if l.Books[0].InStock {
		t.Error("book must be out of stock")
	}
}

func TestParseBookErrors(t *testing.T) {
	tests := []struct {
		name, html string
		wantErr    error // nil — любая ошибка
	}{
		{"no title", `<div class="product_main"><p class="price_color">£1.00</p></div>`, ErrLayoutChanged},
		{"bad price", `<div class="product_main"><h1>A</h1><p class="price_color">N/A</p></div>`, errPrice},
		{"no rating", `<div class="product_main"><h1>A</h1><p class="price_color">£1.00</p></div>`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseBook(mustURL(t, base), strings.NewReader(tt.html))
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseAvailability(t *testing.T) {
	tests := []struct {
		in      string
		inStock bool
		qty     int
	}{
		{"In stock (22 available)", true, 22},
		{"  In stock (1 available)\n", true, 1},
		{"In stock", true, 0},
		{"Out of stock", false, 0},
		{"", false, 0},
	}
	for _, tt := range tests {
		inStock, qty := parseAvailability(tt.in)
		if inStock != tt.inStock || qty != tt.qty {
			t.Errorf("parseAvailability(%q) = %v, %d; want %v, %d", tt.in, inStock, qty, tt.inStock, tt.qty)
		}
	}
}
