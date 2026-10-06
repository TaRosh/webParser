package export

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/TaRosh/webParser/internal/catalog"
)

var books = []catalog.Book{
	{
		Title: "A Light in the Attic", Price: catalog.Price{Minor: 5177, Currency: "GBP"},
		Rating: 3, InStock: true, Quantity: 22, Category: "Poetry", UPC: "a897fe39b1053632",
		URL: "https://books.toscrape.com/catalogue/a-light-in-the-attic_1000/index.html",
	},
	{
		// Запятая и кавычки в названии — проверка экранирования CSV.
		Title: `Tipping, the "Velvet"`, Price: catalog.Price{Minor: 5, Currency: "GBP"},
		Rating: 1, InStock: false, URL: "https://books.toscrape.com/catalogue/x/index.html",
	},
}

func TestColumns(t *testing.T) {
	tests := []struct {
		details bool
		want    string
	}{
		{false, "Название,Цена,Валюта,Рейтинг,В наличии,Ссылка"},
		{true, "Название,Цена,Валюта,Рейтинг,В наличии,Количество,Категория,UPC,Ссылка"},
	}
	for _, tt := range tests {
		var h []string
		for _, c := range Columns(tt.details) {
			h = append(h, c.Header)
		}
		if got := strings.Join(h, ","); got != tt.want {
			t.Errorf("details=%v: %s", tt.details, got)
		}
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, books, Columns(true)); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if !bytes.HasPrefix(data, []byte(bom)) {
		t.Fatal("no UTF-8 BOM — Excel will show mojibake")
	}

	// Читаем обратно стандартным парсером — так проверяется и экранирование.
	rows, err := csv.NewReader(bytes.NewReader(data[len(bom):])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"Название", "Цена", "Валюта", "Рейтинг", "В наличии", "Количество", "Категория", "UPC", "Ссылка"},
		{"A Light in the Attic", "51.77", "GBP", "3", "да", "22", "Poetry", "a897fe39b1053632", books[0].URL},
		{`Tipping, the "Velvet"`, "0.05", "GBP", "1", "нет", "0", "", "", books[1].URL},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d", len(rows))
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d:\n got %q\nwant %q", i, rows[i], want[i])
		}
	}
}

func TestWriteXLSX(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteXLSX(&buf, books, Columns(false)); err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cells := []struct{ cell, want string }{
		{"A1", "Название"},
		{"A2", "A Light in the Attic"},
		{"B2", "51.77"}, // число с форматом 0.00
		{"B3", "0.05"},
		{"D2", "3"},
		{"E3", "нет"},
		{"F2", books[0].URL},
	}
	for _, c := range cells {
		got, err := f.GetCellValue(sheetName, c.cell)
		if err != nil || got != c.want {
			t.Errorf("%s = %q (err %v), want %q", c.cell, got, err, c.want)
		}
	}

	// Цена записана числом, а не текстом — её можно суммировать. В xlsx у числовых
	// ячеек нет атрибута типа (excelize видит Unset), у текстовых — тип строки.
	if typ, _ := f.GetCellType(sheetName, "B2"); typ != excelize.CellTypeNumber && typ != excelize.CellTypeUnset {
		t.Errorf("price cell type = %v, want number", typ)
	}
	if ok, link, _ := f.GetCellHyperLink(sheetName, "F2"); !ok || link != books[0].URL {
		t.Errorf("hyperlink = %v %q", ok, link)
	}
	if panes, _ := f.GetPanes(sheetName); !panes.Freeze || panes.YSplit != 1 {
		t.Errorf("header row is not frozen: %+v", panes)
	}
}

func TestSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out") // папки ещё нет
	tests := []struct {
		name    string
		file    string
		wantErr bool
	}{
		{"csv", "books.csv", false},
		{"xlsx", "books.xlsx", false},
		{"upper-case extension", "BOOKS.XLSX", false},
		{"unknown format", "books.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.file)
			err := Save(path, books, Columns(true))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr {
				return
			}
			if info, err := os.Stat(path); err != nil || info.Size() == 0 {
				t.Fatalf("file not written: %v", err)
			}
		})
	}

	// Временных файлов после записи не остаётся.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".export-") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

func TestSaveOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.csv")
	for _, n := range []int{2, 1} {
		if err := Save(path, books[:n], Columns(false)); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if lines := strings.Count(string(data), "\n"); lines != 2 { // шапка + 1 книга
		t.Errorf("lines = %d, file was not overwritten", lines)
	}
}
