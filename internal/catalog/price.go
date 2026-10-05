package catalog

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Price — цена в минимальных единицах валюты (копейках, центах, пенсах).
//
// Деньги никогда не храним во float64: 0.1 + 0.2 в двоичной дроби даёт
// 0.30000000000000004, и при суммировании тысяч цен копейки "уплывают".
// Целое число копеек считается точно.
type Price struct {
	Minor    int64  // 5177 = 51.77
	Currency string // код ISO 4217: GBP, USD, EUR, RUB
}

// String — "51.77 GBP".
func (p Price) String() string {
	return fmt.Sprintf("%d.%02d %s", p.Minor/100, p.Minor%100, p.Currency)
}

// Decimal — "51.77": для таблиц, где валюта в отдельной колонке.
func (p Price) Decimal() string { return fmt.Sprintf("%d.%02d", p.Minor/100, p.Minor%100) }

var currencies = []struct{ sign, code string }{
	// Длинные обозначения раньше коротких, иначе "руб." найдётся как "р".
	{"руб.", "RUB"}, {"руб", "RUB"}, {"р.", "RUB"}, {"₽", "RUB"}, {"RUB", "RUB"},
	{"£", "GBP"}, {"GBP", "GBP"},
	{"$", "USD"}, {"USD", "USD"},
	{"€", "EUR"}, {"EUR", "EUR"},
}

var errPrice = errors.New("не удалось разобрать цену")

// ParsePrice разбирает цену в форматах, которые встречаются на сайтах:
//
//	£51.77   $1,234.50   1 234,56 ₽   1234 руб.   € 9,99   1.234.567,89 €
//
// Разделитель дробной части — последняя точка или запятая, за которой
// идут 1–2 цифры; остальные точки, запятые и пробелы — разделители тысяч.
func ParsePrice(s string) (Price, error) {
	var p Price
	rest := strings.TrimSpace(s)
	for _, c := range currencies {
		if strings.Contains(rest, c.sign) {
			p.Currency = c.code
			rest = strings.Replace(rest, c.sign, "", 1)
			break
		}
	}

	// Оставляем только цифры и разделители; пробелы (в т.ч. неразрывные) убираем.
	var digits strings.Builder
	for _, r := range rest {
		switch {
		case unicode.IsDigit(r), r == '.', r == ',':
			digits.WriteRune(r)
		case unicode.IsSpace(r), r == ' ', r == ' ':
		default:
			return Price{}, fmt.Errorf("%w %q: лишний символ %q", errPrice, s, r)
		}
	}
	num := digits.String()
	if num == "" {
		return Price{}, fmt.Errorf("%w %q: нет цифр", errPrice, s)
	}

	intPart, fracPart := num, ""
	if i := strings.LastIndexAny(num, ".,"); i >= 0 && len(num)-i-1 <= 2 && len(num)-i-1 > 0 {
		intPart, fracPart = num[:i], num[i+1:]
	}
	intPart = strings.NewReplacer(".", "", ",", "").Replace(intPart)
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) == 1 {
		fracPart += "0" // "9,9" = 9.90
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Price{}, fmt.Errorf("%w %q: %w", errPrice, s, err)
	}
	if whole > math.MaxInt64/100 {
		// Иначе whole*100 молча переполнит int64 и вернёт мусор.
		return Price{}, fmt.Errorf("%w %q: слишком большое число", errPrice, s)
	}
	var frac int64
	if fracPart != "" {
		frac, _ = strconv.ParseInt(fracPart, 10, 64) // две цифры — ошибки быть не может
	}
	p.Minor = whole*100 + frac
	return p, nil
}
