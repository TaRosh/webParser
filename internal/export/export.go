// Package export сохраняет книги в CSV, Excel (xlsx) и Google Таблицы.
// Колонки описаны один раз (Columns) и используются всеми форматами.
package export

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TaRosh/webParser/internal/catalog"
)

// Column — одна колонка таблицы.
type Column struct {
	Header string
	Width  float64 // ширина в Excel, в символах
	// Value возвращает значение ячейки: string, int или catalog.Price.
	// Каждый формат сам решает, как записать цену (текстом или числом).
	Value func(catalog.Book) any
}

// Columns возвращает колонки. В быстром режиме (без страниц книг)
// количества, категории и UPC нет — пустые колонки только путают.
func Columns(details bool) []Column {
	cols := []Column{
		{"Название", 50, func(b catalog.Book) any { return b.Title }},
		{"Цена", 10, func(b catalog.Book) any { return b.Price }},
		{"Валюта", 8, func(b catalog.Book) any { return b.Price.Currency }},
		{"Рейтинг", 9, func(b catalog.Book) any { return b.Rating }},
		{"В наличии", 11, func(b catalog.Book) any { return yesNo(b.InStock) }},
	}
	if details {
		cols = append(cols,
			Column{"Количество", 12, func(b catalog.Book) any { return b.Quantity }},
			Column{"Категория", 20, func(b catalog.Book) any { return b.Category }},
			Column{"UPC", 18, func(b catalog.Book) any { return b.UPC }},
		)
	}
	return append(cols, Column{"Ссылка", 60, func(b catalog.Book) any { return b.URL }})
}

func yesNo(v bool) string {
	if v {
		return "да"
	}
	return "нет"
}

// Save пишет файл, выбирая формат по расширению: .csv или .xlsx.
//
// Запись атомарная: сначала во временный файл рядом, затем переименование.
// Если программа упадёт посреди записи, старая выгрузка останется целой,
// а не превратится в обрывок.
func Save(path string, books []catalog.Book, cols []Column) error {
	var write func(io.Writer, []catalog.Book, []Column) error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		write = WriteCSV
	case ".xlsx":
		write = WriteXLSX
	default:
		return fmt.Errorf("неизвестный формат %q: используйте .csv или .xlsx", filepath.Ext(path))
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".export-*"+filepath.Ext(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // после успешного Rename файла уже нет — ошибка игнорируется

	if err := write(tmp, books, cols); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
