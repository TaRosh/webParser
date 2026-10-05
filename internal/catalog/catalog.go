// Package catalog извлекает данные о книгах со страниц books.toscrape.com.
//
// Собираем только факты: название, цену, рейтинг, наличие, категорию, UPC
// и ссылку. Описания книг не собираем — это авторский текст
// (см. docs/PRE_ORDER_CHECKLIST.md).
package catalog

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// ErrLayoutChanged — на странице не нашлось ожидаемых элементов.
// Обычно значит, что сайт поменял вёрстку и селекторы нужно обновить —
// самая частая причина "парсер сломался" в поддержке заказов.
var ErrLayoutChanged = errors.New("не найдены ожидаемые элементы — возможно, изменилась вёрстка сайта")

// Book — данные о книге. Поля Quantity, Category и UPC есть только
// на странице самой книги; после разбора каталога они пустые.
type Book struct {
	Title    string
	Price    Price
	Rating   int // 1–5
	InStock  bool
	Quantity int // сколько экземпляров в наличии; 0 — неизвестно
	Category string
	UPC      string
	URL      string
}

// Listing — страница каталога: книги и ссылка на следующую страницу.
type Listing struct {
	Books []Book
	Next  string // "" на последней странице
	// Problems — карточки, которые не удалось разобрать (пропущены).
	// Одна странная карточка не должна останавливать весь парсинг,
	// но и молча терять данные нельзя.
	Problems []string
}

// ParseListing разбирает страницу каталога. pageURL нужен, чтобы превратить
// относительные ссылки ("page-2.html") в полные.
func ParseListing(pageURL *url.URL, r io.Reader) (Listing, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return Listing{}, err
	}

	var l Listing
	cards := doc.Find("article.product_pod")
	cards.Each(func(i int, card *goquery.Selection) {
		b, err := parseCard(pageURL, card)
		if err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("карточка %d: %v", i+1, err))
			return
		}
		l.Books = append(l.Books, b)
	})
	if cards.Length() == 0 {
		return Listing{}, fmt.Errorf("каталог %s: %w", pageURL, ErrLayoutChanged)
	}

	if href, ok := doc.Find("ul.pager li.next a").Attr("href"); ok {
		if next, err := pageURL.Parse(href); err == nil {
			l.Next = next.String()
		}
	}
	return l, nil
}

func parseCard(pageURL *url.URL, card *goquery.Selection) (Book, error) {
	link := card.Find("h3 a")
	href, _ := link.Attr("href")
	// Текст ссылки обрезан ("A Light in the ..."), полное название — в title.
	title, _ := link.Attr("title")
	if href == "" || title == "" {
		return Book{}, errors.New("нет ссылки или названия")
	}
	u, err := pageURL.Parse(href)
	if err != nil {
		return Book{}, fmt.Errorf("ссылка %q: %w", href, err)
	}

	price, err := ParsePrice(card.Find(".price_color").Text())
	if err != nil {
		return Book{}, err
	}
	rating, err := parseRating(card.Find(".star-rating"))
	if err != nil {
		return Book{}, err
	}
	inStock, _ := parseAvailability(card.Find(".availability").Text())

	return Book{
		Title:   strings.TrimSpace(title),
		Price:   price,
		Rating:  rating,
		InStock: inStock,
		URL:     u.String(),
	}, nil
}

// ParseBook разбирает страницу книги — тут есть все поля.
func ParseBook(pageURL *url.URL, r io.Reader) (Book, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return Book{}, err
	}
	main := doc.Find(".product_main")
	title := strings.TrimSpace(main.Find("h1").First().Text())
	if title == "" {
		return Book{}, fmt.Errorf("книга %s: нет названия: %w", pageURL, ErrLayoutChanged)
	}

	price, err := ParsePrice(main.Find(".price_color").First().Text())
	if err != nil {
		return Book{}, fmt.Errorf("книга %s: %w", pageURL, err)
	}
	rating, err := parseRating(main.Find(".star-rating"))
	if err != nil {
		return Book{}, fmt.Errorf("книга %s: %w", pageURL, err)
	}

	// Таблица "Product Information": собираем в map "заголовок → значение",
	// чтобы не зависеть от порядка строк.
	info := make(map[string]string)
	doc.Find("table tr").Each(func(_ int, tr *goquery.Selection) {
		info[strings.TrimSpace(tr.Find("th").Text())] = strings.TrimSpace(tr.Find("td").Text())
	})
	inStock, qty := parseAvailability(info["Availability"])

	// Хлебные крошки: Home → Books → <категория> → книга.
	category := strings.TrimSpace(doc.Find("ul.breadcrumb li").Eq(2).Find("a").Text())

	return Book{
		Title:    title,
		Price:    price,
		Rating:   rating,
		InStock:  inStock,
		Quantity: qty,
		Category: category,
		UPC:      info["UPC"],
		URL:      pageURL.String(),
	}, nil
}

var ratingWords = map[string]int{"One": 1, "Two": 2, "Three": 3, "Four": 4, "Five": 5}

// parseRating: рейтинг записан CSS-классом — <p class="star-rating Three">.
func parseRating(s *goquery.Selection) (int, error) {
	class, _ := s.Attr("class")
	for _, c := range strings.Fields(class) {
		if n, ok := ratingWords[c]; ok {
			return n, nil
		}
	}
	return 0, fmt.Errorf("не найден рейтинг в class=%q", class)
}

var availableRe = regexp.MustCompile(`\((\d+) available\)`)

// parseAvailability: "In stock (22 available)" → true, 22; "In stock" → true, 0.
func parseAvailability(s string) (inStock bool, quantity int) {
	s = strings.TrimSpace(s)
	inStock = strings.HasPrefix(strings.ToLower(s), "in stock")
	if m := availableRe.FindStringSubmatch(s); m != nil {
		quantity, _ = strconv.Atoi(m[1])
	}
	return inStock, quantity
}
