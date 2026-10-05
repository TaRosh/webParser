package catalog

import (
	"errors"
	"strconv"
	"testing"
)

func TestParsePrice(t *testing.T) {
	tests := []struct {
		in       string
		minor    int64
		currency string
		wantErr  bool
	}{
		// Как на books.toscrape.com.
		{"£51.77", 5177, "GBP", false},
		{" £0.99 ", 99, "GBP", false},
		// Английский формат: запятая — тысячи, точка — дробная часть.
		{"$1,234.50", 123450, "USD", false},
		{"USD 12", 1200, "USD", false},
		// Русский формат: пробел (в т.ч. неразрывный) — тысячи, запятая — дробная часть.
		{"1 234,56 ₽", 123456, "RUB", false},
		{"1 234 ₽", 123400, "RUB", false},
		{"1234 руб.", 123400, "RUB", false},
		{"990 р.", 99000, "RUB", false},
		// Европейский: точка — тысячи, запятая — дробная часть.
		{"1.234.567,89 €", 123456789, "EUR", false},
		{"€ 9,9", 990, "EUR", false},
		// Без валюты.
		{"100", 10000, "", false},
		{"1,000", 100000, "", false}, // 3 цифры после запятой — это тысячи, а не дробь

		{"", 0, "", true},
		{"£", 0, "", true},
		{"free", 0, "", true},
		{"N/A", 0, "", true},
		{"от 100 ₽", 0, "", true},                 // "от" — не цена, а диапазон; пусть человек решит
		{"99 999 999 999 999 999 ₽", 0, "", true}, // влезает в int64, но не ×100
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParsePrice(tt.in)
			if tt.wantErr {
				if !errors.Is(err, errPrice) {
					t.Fatalf("err = %v, want errPrice", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Minor != tt.minor || got.Currency != tt.currency {
				t.Errorf("got %+v, want %d %s", got, tt.minor, tt.currency)
			}
		})
	}
}

func TestPriceString(t *testing.T) {
	tests := []struct {
		p        Price
		str, dec string
	}{
		{Price{5177, "GBP"}, "51.77 GBP", "51.77"},
		{Price{5, "USD"}, "0.05 USD", "0.05"},
		{Price{100000, "RUB"}, "1000.00 RUB", "1000.00"},
	}
	for _, tt := range tests {
		if tt.p.String() != tt.str || tt.p.Decimal() != tt.dec {
			t.Errorf("%+v: String=%q Decimal=%q", tt.p, tt.p.String(), tt.p.Decimal())
		}
	}
}

func TestParsePriceOverflowKeepsCause(t *testing.T) {
	_, err := ParsePrice("99999999999999999999999 ₽")
	var numErr *strconv.NumError
	if !errors.Is(err, errPrice) || !errors.As(err, &numErr) {
		t.Fatalf("err = %v: want both errPrice and the strconv cause", err)
	}
}
