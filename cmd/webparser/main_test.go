package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFlagsValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // подстрока; "" — без ошибки
	}{
		{"defaults are valid", nil, ""},
		{"csv output", []string{"-out", "a.csv"}, ""},
		{"delay too small", []string{"-delay", "200ms"}, "правило вежливости"},
		{"workers out of range", []string{"-workers", "100"}, "-workers"},
		{"negative limit", []string{"-limit", "-1"}, "-limit"},
		{"unknown output format", []string{"-out", "books.json"}, ".xlsx или .csv"},
		{"nowhere to save", []string{"-out", ""}, "некуда сохранить"},
		{"bad sheet link", []string{"-sheet", "https://example.com"}, "-sheet"},
		{"stray argument", []string{"extra"}, "лишние аргументы"},
		{"all errors at once", []string{"-delay", "1ms", "-workers", "0"}, "-workers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFlags(tt.args, io.Discard)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"-h"}, io.Discard, &stderr)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "Примеры") {
		t.Fatalf("err = %v, output:\n%s", err, stderr.String())
	}
}

// shopServer — мини-копия books.toscrape.com: 1 страница каталога, 2 книги.
func shopServer(t *testing.T) *httptest.Server {
	t.Helper()
	card := func(slug, title, price string) string {
		return fmt.Sprintf(`<article class="product_pod"><h3><a href="%s/index.html" title="%s">x</a></h3>`+
			`<p class="star-rating Four"></p><p class="price_color">%s</p>`+
			`<p class="instock availability">In stock</p></article>`, slug, title, price)
	}
	book := func(title string) string {
		return `<ul class="breadcrumb"><li><a>Home</a></li><li><a>Books</a></li><li><a>Poetry</a></li></ul>` +
			`<div class="product_main"><h1>` + title + `</h1><p class="price_color">£10.00</p><p class="star-rating Four"></p></div>` +
			`<table><tr><th>UPC</th><td>u1</td></tr><tr><th>Availability</th><td>In stock (5 available)</td></tr></table>`
	}
	pages := map[string]string{
		"/catalogue/page-1.html":    card("one", "Book One", "£10.00") + card("two", "Book, Two", "£0.50"),
		"/catalogue/one/index.html": book("Book One"),
		"/catalogue/two/index.html": book("Book, Two"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r) // в т.ч. /robots.txt — ограничений нет
			return
		}
		_, _ = io.WriteString(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Сквозной тест: флаги → обход → файл. Пауза 1 с — минимальная по правилам,
// поэтому тест занимает несколько секунд.
func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("медленный сквозной тест")
	}
	srv := shopServer(t)

	tests := []struct {
		name     string
		details  bool
		wantCols int
		wantRow  []string
	}{
		{"catalogue only", false, 6, []string{"Book One", "10.00", "GBP", "4", "да"}},
		{"with details", true, 9, []string{"Book One", "10.00", "GBP", "4", "да", "5", "Poetry", "u1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "books.csv")
			args := []string{"-url", srv.URL + "/catalogue/page-1.html", "-out", out}
			if tt.details {
				args = append(args, "-details")
			}

			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), args, &stdout, &stderr); err != nil {
				t.Fatalf("run: %v\nstderr:\n%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Готово: 2 книг") {
				t.Errorf("stdout:\n%s", stdout.String())
			}

			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")))).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 || len(rows[0]) != tt.wantCols {
				t.Fatalf("rows = %v", rows)
			}
			if got := strings.Join(rows[1][:len(tt.wantRow)], "|"); got != strings.Join(tt.wantRow, "|") {
				t.Errorf("row = %s", got)
			}
			if rows[2][0] != "Book, Two" {
				t.Errorf("comma in title broken: %q", rows[2][0])
			}
		})
	}
}

func TestRunSiteUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // robots.txt 5xx → сайт закрыт
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "books.xlsx")
	err := run(context.Background(), []string{"-url", srv.URL + "/catalogue/page-1.html", "-out", out}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "robots.txt") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("empty result must not create a file")
	}
}

func TestRunBadCredentialsFailsBeforeCrawl(t *testing.T) {
	err := run(context.Background(), []string{
		"-url", "http://127.0.0.1:1/never-requested",
		"-sheet", "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms",
		"-credentials", filepath.Join(t.TempDir(), "missing.json"),
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Google Sheets") {
		t.Fatalf("err = %v", err)
	}
}
