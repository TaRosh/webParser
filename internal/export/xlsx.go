package export

import (
	"io"

	"github.com/xuri/excelize/v2"

	"github.com/TaRosh/webParser/internal/catalog"
)

const sheetName = "Книги"

// WriteXLSX пишет таблицу Excel, удобную для человека: цена — число
// (можно суммировать и сортировать), шапка закреплена и с фильтрами,
// ссылки кликабельные.
func WriteXLSX(w io.Writer, books []catalog.Book, cols []Column) error {
	f := excelize.NewFile()
	defer f.Close()

	if err := f.SetSheetName("Sheet1", sheetName); err != nil {
		return err
	}
	styles, err := newStyles(f)
	if err != nil {
		return err
	}

	// Шапка.
	header := make([]any, len(cols))
	for i, c := range cols {
		header[i] = c.Header
		col, _ := excelize.ColumnNumberToName(i + 1)
		if err := f.SetColWidth(sheetName, col, col, c.Width); err != nil {
			return err
		}
	}
	if err := f.SetSheetRow(sheetName, "A1", &header); err != nil {
		return err
	}
	last, _ := excelize.CoordinatesToCellName(len(cols), 1)
	if err := f.SetCellStyle(sheetName, "A1", last, styles.header); err != nil {
		return err
	}

	// Данные.
	for r, b := range books {
		for i, c := range cols {
			cell, _ := excelize.CoordinatesToCellName(i+1, r+2)
			if err := setCell(f, cell, c, b, styles); err != nil {
				return err
			}
		}
	}

	// Закрепить шапку: при прокрутке она остаётся на месте.
	if err := f.SetPanes(sheetName, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return err
	}
	// Кнопки фильтра в шапке.
	if len(books) > 0 {
		lastData, _ := excelize.CoordinatesToCellName(len(cols), len(books)+1)
		if err := f.AutoFilter(sheetName, "A1:"+lastData, nil); err != nil {
			return err
		}
	}
	return f.Write(w)
}

func setCell(f *excelize.File, cell string, c Column, b catalog.Book, s styles) error {
	switch v := c.Value(b).(type) {
	case catalog.Price:
		// Число с двумя знаками. float64 здесь — только для отображения
		// в ячейке; все расчёты в программе идут в целых копейках.
		if err := f.SetCellValue(sheetName, cell, float64(v.Minor)/100); err != nil {
			return err
		}
		return f.SetCellStyle(sheetName, cell, cell, s.money)
	case string:
		if c.Header == "Ссылка" && v != "" {
			if err := f.SetCellValue(sheetName, cell, v); err != nil {
				return err
			}
			if err := f.SetCellHyperLink(sheetName, cell, v, "External"); err != nil {
				return err
			}
			return f.SetCellStyle(sheetName, cell, cell, s.link)
		}
		return f.SetCellValue(sheetName, cell, v)
	default:
		return f.SetCellValue(sheetName, cell, v)
	}
}

type styles struct{ header, money, link int }

func newStyles(f *excelize.File) (styles, error) {
	var (
		s   styles
		err error
	)
	if s.header, err = f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"2F5597"}},
	}); err != nil {
		return s, err
	}
	moneyFmt := "0.00"
	if s.money, err = f.NewStyle(&excelize.Style{CustomNumFmt: &moneyFmt}); err != nil {
		return s, err
	}
	s.link, err = f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "0563C1", Underline: "single"}})
	return s, err
}
