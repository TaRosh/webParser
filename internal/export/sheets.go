package export

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/TaRosh/webParser/internal/catalog"
)

// NewSheetsService подключается к Google Sheets API по ключу сервисного
// аккаунта (JSON-файл из Google Cloud Console, см. docs/GOOGLE_SHEETS.md).
//
// Используем WithAuthCredentialsFile с явным типом ServiceAccount: старая
// WithCredentialsFile устарела — она принимает файл учётных данных любого
// типа без проверки, что небезопасно.
func NewSheetsService(ctx context.Context, credentialsFile string) (*sheets.Service, error) {
	svc, err := sheets.NewService(ctx,
		option.WithAuthCredentialsFile(option.ServiceAccount, credentialsFile),
		option.WithScopes(sheets.SpreadsheetsScope),
	)
	if err != nil {
		return nil, fmt.Errorf("подключение к Google Sheets: %w", err)
	}
	return svc, nil
}

var spreadsheetIDRe = regexp.MustCompile(`/spreadsheets/d/([A-Za-z0-9_-]+)`)

// ParseSpreadsheetID принимает ссылку на таблицу из адресной строки
// или сам идентификатор и возвращает идентификатор.
func ParseSpreadsheetID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if m := spreadsheetIDRe.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`).MatchString(s) {
		return s, nil
	}
	return "", fmt.Errorf("не похоже на ссылку на Google Таблицу: %q", s)
}

// WriteSheets заменяет содержимое листа sheetTitle таблицы spreadsheetID.
// Если листа нет — создаёт его.
func WriteSheets(ctx context.Context, svc *sheets.Service, spreadsheetID, sheetTitle string, books []catalog.Book, cols []Column) error {
	sheetID, err := ensureSheet(ctx, svc, spreadsheetID, sheetTitle)
	if err != nil {
		return explain(err)
	}

	// Название листа в A1-нотации берётся в кавычки (в нём могут быть пробелы),
	// а кавычки внутри названия удваиваются.
	rangeRef := "'" + strings.ReplaceAll(sheetTitle, "'", "''") + "'"

	// Очищаем лист: иначе при выгрузке 500 книг вместо прежних 1000
	// внизу остались бы 500 старых строк.
	if _, err := svc.Spreadsheets.Values.Clear(spreadsheetID, rangeRef, &sheets.ClearValuesRequest{}).
		Context(ctx).Do(); err != nil {
		return explain(err)
	}

	rows := make([][]any, 0, len(books)+1)
	header := make([]any, len(cols))
	for i, c := range cols {
		header[i] = c.Header
	}
	rows = append(rows, header)
	for _, b := range books {
		row := make([]any, len(cols))
		for i, c := range cols {
			row[i] = sheetValue(c.Value(b))
		}
		rows = append(rows, row)
	}

	// RAW — значения как есть: строка "=SUM(...)" в названии книги
	// не превратится в формулу (защита от "formula injection").
	if _, err := svc.Spreadsheets.Values.Update(spreadsheetID, rangeRef+"!A1", &sheets.ValueRange{Values: rows}).
		ValueInputOption("RAW").Context(ctx).Do(); err != nil {
		return explain(err)
	}

	if _, err := svc.Spreadsheets.BatchUpdate(spreadsheetID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: formatRequests(sheetID, cols),
	}).Context(ctx).Do(); err != nil {
		return explain(err)
	}
	return nil
}

func sheetValue(v any) any {
	if p, ok := v.(catalog.Price); ok {
		return float64(p.Minor) / 100 // число — в таблице его можно суммировать
	}
	return v
}

// ensureSheet возвращает числовой id листа, создавая лист при необходимости.
func ensureSheet(ctx context.Context, svc *sheets.Service, spreadsheetID, title string) (int64, error) {
	ss, err := svc.Spreadsheets.Get(spreadsheetID).Fields("sheets.properties").Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	for _, s := range ss.Sheets {
		if s.Properties != nil && s.Properties.Title == title {
			return s.Properties.SheetId, nil
		}
	}

	resp, err := svc.Spreadsheets.BatchUpdate(spreadsheetID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{AddSheet: &sheets.AddSheetRequest{
			Properties: &sheets.SheetProperties{Title: title},
		}}},
	}).Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	if len(resp.Replies) == 0 || resp.Replies[0].AddSheet == nil {
		return 0, errors.New("в ответе Google Sheets нет созданного листа")
	}
	return resp.Replies[0].AddSheet.Properties.SheetId, nil
}

// formatRequests — оформление: закреплённая жирная шапка и формат цены.
func formatRequests(sheetID int64, cols []Column) []*sheets.Request {
	reqs := []*sheets.Request{
		{UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
			Properties: &sheets.SheetProperties{
				SheetId:        sheetID,
				GridProperties: &sheets.GridProperties{FrozenRowCount: 1},
			},
			Fields: "gridProperties.frozenRowCount",
		}},
		{RepeatCell: &sheets.RepeatCellRequest{
			Range: &sheets.GridRange{SheetId: sheetID, StartRowIndex: 0, EndRowIndex: 1},
			Cell: &sheets.CellData{UserEnteredFormat: &sheets.CellFormat{
				TextFormat: &sheets.TextFormat{Bold: true},
			}},
			Fields: "userEnteredFormat.textFormat.bold",
		}},
	}
	for i, c := range cols {
		if c.Header != "Цена" {
			continue
		}
		reqs = append(reqs, &sheets.Request{RepeatCell: &sheets.RepeatCellRequest{
			Range: &sheets.GridRange{
				SheetId: sheetID, StartRowIndex: 1,
				StartColumnIndex: int64(i), EndColumnIndex: int64(i + 1),
			},
			Cell: &sheets.CellData{UserEnteredFormat: &sheets.CellFormat{
				NumberFormat: &sheets.NumberFormat{Type: "NUMBER", Pattern: "0.00"},
			}},
			Fields: "userEnteredFormat.numberFormat",
		}})
	}
	return reqs
}

// explain добавляет к частым ошибкам API подсказку, что делать.
func explain(err error) error {
	var ge *googleapi.Error
	if errors.As(err, &ge) && (ge.Code == http.StatusForbidden || ge.Code == http.StatusNotFound) {
		return fmt.Errorf("%w\nПодсказка: откройте таблицу в браузере → «Настройки доступа» → добавьте "+
			"email сервисного аккаунта (поле client_email в JSON-ключе) с правами редактора; "+
			"проверьте, что ссылка на таблицу верная", err)
	}
	return err
}
