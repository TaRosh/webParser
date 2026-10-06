// Command webparser собирает каталог книг с books.toscrape.com и сохраняет
// его в Excel, CSV и/или Google Таблицу. Соблюдает robots.txt, ограничивает
// частоту запросов и останавливается, если сайт ограничил доступ.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/api/sheets/v4"

	"github.com/TaRosh/webParser/internal/crawl"
	"github.com/TaRosh/webParser/internal/export"
	"github.com/TaRosh/webParser/internal/fetch"
)

const minDelay = time.Second // меньше — уже неуважение к чужому серверу

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// После первого Ctrl+C возвращаем стандартную реакцию на сигнал:
	// программа сохраняет собранное, а второй Ctrl+C завершает её сразу.
	go func() { <-ctx.Done(); stop() }()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "Остановлено пользователем.")
		os.Exit(130) // общепринятый код выхода после Ctrl+C
	default:
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}
}

type config struct {
	url         string
	details     bool
	limit       int
	workers     int
	delay       time.Duration
	out         string
	sheet       string
	sheetName   string
	credentials string
	verbose     bool
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("webparser", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.url, "url", "https://books.toscrape.com/catalogue/page-1.html", "первая страница каталога")
	fs.BoolVar(&c.details, "details", false, "скачать страницу каждой книги: категория, UPC, количество (+1 запрос на книгу)")
	fs.IntVar(&c.limit, "limit", 0, "сколько книг собрать (0 — все)")
	fs.IntVar(&c.workers, "workers", 4, "параллельная обработка книг в режиме -details (1–16)")
	fs.DurationVar(&c.delay, "delay", minDelay, "пауза между запросами к сайту (не меньше 1s)")
	fs.StringVar(&c.out, "out", "out/books.xlsx", "файл результата: .xlsx или .csv (пусто — не сохранять)")
	fs.StringVar(&c.sheet, "sheet", "", "ссылка на Google Таблицу для выгрузки")
	fs.StringVar(&c.sheetName, "sheet-name", "Книги", "лист Google Таблицы")
	fs.StringVar(&c.credentials, "credentials", "credentials.json", "JSON-ключ сервисного аккаунта Google")
	fs.BoolVar(&c.verbose, "v", false, "подробный лог")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Использование: webparser [флаги]\n\nФлаги:")
		fs.PrintDefaults()
		fmt.Fprintln(stderr, "\nПримеры:\n  webparser -limit 100 -out out/books.csv\n  webparser -details -sheet https://docs.google.com/spreadsheets/d/<ID>/edit")
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("лишние аргументы: %v", fs.Args())
	}

	// Все проверки — до начала обхода: обидно ждать 17 минут
	// и упасть на опечатке в расширении файла.
	var errs []error
	if c.delay < minDelay {
		errs = append(errs, fmt.Errorf("-delay %v: меньше %v нельзя — это правило вежливости к сайту", c.delay, minDelay))
	}
	if c.workers < 1 || c.workers > 16 {
		errs = append(errs, fmt.Errorf("-workers %d: допустимо от 1 до 16", c.workers))
	}
	if c.limit < 0 {
		errs = append(errs, fmt.Errorf("-limit %d: не может быть отрицательным", c.limit))
	}
	if ext := strings.ToLower(filepath.Ext(c.out)); c.out != "" && ext != ".xlsx" && ext != ".csv" {
		errs = append(errs, fmt.Errorf("-out %q: используйте расширение .xlsx или .csv", c.out))
	}
	if c.out == "" && c.sheet == "" {
		errs = append(errs, errors.New("результат некуда сохранить: укажите -out и/или -sheet"))
	}
	if c.sheet != "" {
		id, err := export.ParseSpreadsheetID(c.sheet)
		if err != nil {
			errs = append(errs, fmt.Errorf("-sheet: %w", err))
		}
		c.sheet = id
	}
	return c, errors.Join(errs...)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	level := slog.LevelWarn // по умолчанию в консоли только прогресс и итог
	if cfg.verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	// Подключение к Google — тоже до обхода: неверный ключ видно сразу.
	var sheetsSvc *sheets.Service
	if cfg.sheet != "" {
		if sheetsSvc, err = export.NewSheetsService(ctx, cfg.credentials); err != nil {
			return err
		}
	}

	client := fetch.New(fetch.Config{MinDelay: cfg.delay}, log)
	start := time.Now()
	res, crawlErr := crawl.Run(ctx, client, crawl.Options{
		StartURL: cfg.url,
		Details:  cfg.details,
		Limit:    cfg.limit,
		Workers:  cfg.workers,
		Progress: progressPrinter(stderr),
	}, log)
	fmt.Fprintln(stderr) // перевод строки после строки прогресса

	if crawlErr != nil {
		if len(res.Books) == 0 {
			return crawlErr
		}
		fmt.Fprintf(stderr, "Обход прерван: %v\nСохраняю то, что успели собрать.\n", crawlErr)
	}

	// Сохраняем даже после ошибки: собранное за 10 минут не должно пропасть
	// из-за того, что на 11-й минуте сайт попросил остановиться.
	if err := save(context.WithoutCancel(ctx), cfg, sheetsSvc, res, stdout); err != nil {
		return errors.Join(crawlErr, err)
	}

	fmt.Fprintf(stdout, "Готово: %d книг, %d стр. каталога за %s\n",
		len(res.Books), res.Pages, time.Since(start).Round(time.Second))
	printProblems(stdout, res.Problems)
	return crawlErr
}

func save(ctx context.Context, cfg config, svc *sheets.Service, res crawl.Result, stdout io.Writer) error {
	cols := export.Columns(cfg.details)
	if cfg.out != "" {
		if err := export.Save(cfg.out, res.Books, cols); err != nil {
			return fmt.Errorf("сохранение %s: %w", cfg.out, err)
		}
		fmt.Fprintf(stdout, "Файл: %s\n", cfg.out)
	}
	if svc != nil {
		if err := export.WriteSheets(ctx, svc, cfg.sheet, cfg.sheetName, res.Books, cols); err != nil {
			return fmt.Errorf("выгрузка в Google Таблицу: %w", err)
		}
		fmt.Fprintf(stdout, "Google Таблица: https://docs.google.com/spreadsheets/d/%s/edit (лист «%s»)\n", cfg.sheet, cfg.sheetName)
	}
	return nil
}

// progressPrinter печатает прогресс в одну строку ("\r" возвращает курсор
// в начало строки). Вызывается из нескольких горутин — отсюда мьютекс.
func progressPrinter(w io.Writer) func(crawl.Progress) {
	var mu sync.Mutex
	return func(p crawl.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Phase == "книги" {
			fmt.Fprintf(w, "\rКниги: %d / %d   ", p.Done, p.Total)
			return
		}
		fmt.Fprintf(w, "\rКаталог: %d стр., найдено книг: %d   ", p.Done, p.Books)
	}
}

func printProblems(w io.Writer, problems []string) {
	if len(problems) == 0 {
		return
	}
	fmt.Fprintf(w, "Пропущено с проблемами: %d\n", len(problems))
	for i, p := range problems {
		if i == 5 {
			fmt.Fprintf(w, "  … и ещё %d (подробности: флаг -v)\n", len(problems)-5)
			break
		}
		fmt.Fprintln(w, "  -", p)
	}
}
