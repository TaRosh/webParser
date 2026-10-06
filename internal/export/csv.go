package export

import (
	"encoding/csv"
	"fmt"
	"io"

	"github.com/TaRosh/webParser/internal/catalog"
)

// bom — метка порядка байтов UTF-8. По ней Excel понимает, что файл в UTF-8;
// без неё он открывает CSV как Windows-1251, и кириллица превращается в "РќР°Р·...".
const bom = "\xEF\xBB\xBF"

// WriteCSV пишет CSV по RFC 4180: разделитель — запятая, значения с запятыми
// и кавычками экранирует encoding/csv. Цена — текстом с точкой ("51.77").
//
// CSV — формат для программ и импорта. Людям лучше отдавать .xlsx: русский
// Excel ждёт в CSV разделитель ";" и открывает такой файл одной колонкой.
func WriteCSV(w io.Writer, books []catalog.Book, cols []Column) error {
	if _, err := io.WriteString(w, bom); err != nil {
		return err
	}
	cw := csv.NewWriter(w)

	row := make([]string, len(cols))
	for i, c := range cols {
		row[i] = c.Header
	}
	if err := cw.Write(row); err != nil {
		return err
	}
	for _, b := range books {
		for i, c := range cols {
			row[i] = csvValue(c.Value(b))
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush() // csv.Writer буферизует — без Flush хвост файла потеряется
	return cw.Error()
}

func csvValue(v any) string {
	switch v := v.(type) {
	case catalog.Price:
		return v.Decimal()
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}
