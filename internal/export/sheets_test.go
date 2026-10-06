package export

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// fakeSheets — фальшивый Google Sheets API: запоминает запросы,
// отвечает как настоящий (минимально). Сеть и ключи не нужны.
type fakeSheets struct {
	sheets []string // названия существующих листов
	status int      // если не 0 — так отвечает на любой запрос (имитация ошибки)

	mu       sync.Mutex
	calls    []string // "МЕТОД путь" по порядку
	written  [][]any  // строки из values.update
	batches  []sheets.BatchUpdateSpreadsheetRequest
	inputOpt string
}

func (f *fakeSheets) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		f.calls = append(f.calls, r.Method+" "+path)
		w.Header().Set("Content-Type", "application/json")

		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
			return
		}

		switch {
		case r.Method == http.MethodGet: // spreadsheets.get
			var props []string
			for i, s := range f.sheets {
				props = append(props, `{"properties":{"sheetId":`+strconv.Itoa(i)+`,"title":"`+s+`"}}`)
			}
			_, _ = io.WriteString(w, `{"sheets":[`+strings.Join(props, ",")+`]}`)
		case strings.HasSuffix(path, ":batchUpdate"):
			var req sheets.BatchUpdateSpreadsheetRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			f.batches = append(f.batches, req)
			if len(req.Requests) > 0 && req.Requests[0].AddSheet != nil {
				_, _ = io.WriteString(w, `{"replies":[{"addSheet":{"properties":{"sheetId":777,"title":"Книги"}}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(path, ":clear"):
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPut: // values.update
			var vr sheets.ValueRange
			if err := json.NewDecoder(r.Body).Decode(&vr); err != nil {
				t.Error(err)
			}
			f.written = vr.Values
			f.inputOpt = r.URL.Query().Get("valueInputOption")
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newFakeService(t *testing.T, f *fakeSheets) *sheets.Service {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	svc, err := sheets.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestWriteSheetsExistingSheet(t *testing.T) {
	f := &fakeSheets{sheets: []string{"Лист1", "Книги"}}
	svc := newFakeService(t, f)

	if err := WriteSheets(context.Background(), svc, "SHEET_ID", "Книги", books, Columns(false)); err != nil {
		t.Fatal(err)
	}

	// Порядок: найти лист → очистить → записать → оформить.
	wantCalls := []string{"GET", ":clear", "PUT", ":batchUpdate"}
	if len(f.calls) != len(wantCalls) {
		t.Fatalf("calls = %v", f.calls)
	}
	for i, w := range wantCalls {
		if !strings.Contains(f.calls[i], w) {
			t.Errorf("call %d = %s, want %s", i, f.calls[i], w)
		}
	}

	if len(f.written) != 3 || f.written[0][0] != "Название" || f.written[1][0] != "A Light in the Attic" {
		t.Fatalf("written = %v", f.written)
	}
	if price, ok := f.written[1][1].(float64); !ok || price != 51.77 {
		t.Errorf("price = %#v, want number 51.77", f.written[1][1])
	}
	if f.inputOpt != "RAW" {
		t.Errorf("valueInputOption = %q, want RAW (no formula injection)", f.inputOpt)
	}

	// Форматирование применено к найденному листу (sheetId 1) и цена в колонке B.
	reqs := f.batches[0].Requests
	if reqs[0].UpdateSheetProperties.Properties.SheetId != 1 ||
		reqs[0].UpdateSheetProperties.Properties.GridProperties.FrozenRowCount != 1 {
		t.Errorf("freeze request = %+v", reqs[0].UpdateSheetProperties)
	}
	if price := reqs[2].RepeatCell; price.Range.StartColumnIndex != 1 || price.Cell.UserEnteredFormat.NumberFormat.Pattern != "0.00" {
		t.Errorf("price format request = %+v", price)
	}
}

func TestWriteSheetsCreatesSheet(t *testing.T) {
	f := &fakeSheets{sheets: []string{"Лист1"}}
	svc := newFakeService(t, f)

	if err := WriteSheets(context.Background(), svc, "SHEET_ID", "Книги", books, Columns(false)); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 2 || f.batches[0].Requests[0].AddSheet.Properties.Title != "Книги" {
		t.Fatalf("sheet was not created: %+v", f.batches)
	}
	if id := f.batches[1].Requests[0].UpdateSheetProperties.Properties.SheetId; id != 777 {
		t.Errorf("formatting applied to sheet %d, want the new sheet 777", id)
	}
}

func TestWriteSheetsPermissionHint(t *testing.T) {
	f := &fakeSheets{status: http.StatusForbidden}
	svc := newFakeService(t, f)

	err := WriteSheets(context.Background(), svc, "SHEET_ID", "Книги", books, Columns(false))
	if err == nil || !strings.Contains(err.Error(), "client_email") {
		t.Fatalf("err = %v, want hint about sharing with the service account", err)
	}
}

func TestParseSpreadsheetID(t *testing.T) {
	const id = "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgvE2upms"
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://docs.google.com/spreadsheets/d/" + id + "/edit#gid=0", id, false},
		{"https://docs.google.com/spreadsheets/d/" + id, id, false},
		{"  " + id + "  ", id, false},
		{"https://example.com/table", "", true},
		{"short", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := ParseSpreadsheetID(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseSpreadsheetID(%q) = %q, %v", tt.in, got, err)
		}
	}
}
