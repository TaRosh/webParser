// Package crawl обходит каталог: проходит страницы по ссылкам "next",
// собирает книги и (по желанию) скачивает страницу каждой книги
// пулом воркеров.
package crawl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/TaRosh/webParser/internal/catalog"
	"github.com/TaRosh/webParser/internal/fetch"
)

// Fetcher — то, что умеет скачать страницу. В программе это *fetch.Client,
// в тестах — фальшивка с готовыми страницами.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Page, error)
}

// Options — параметры обхода.
type Options struct {
	StartURL string
	// Details — скачивать страницу каждой книги (категория, UPC, количество).
	// Это +1 запрос на книгу: для 1000 книг около 17 минут при паузе 1 с.
	Details bool
	// Limit — сколько книг собрать (0 — все).
	Limit int
	// Workers — сколько книг разбирать параллельно в режиме Details.
	Workers int
	// Progress вызывается после каждой страницы. Внимание: в режиме Details —
	// из разных горутин, функция должна быть потокобезопасной.
	Progress func(Progress)
}

// Progress — текущее состояние для вывода в консоль.
type Progress struct {
	Phase string // "каталог" или "книги"
	Done  int    // страниц каталога или книг обработано
	Total int    // для "книги": сколько всего; для "каталог": 0 (неизвестно заранее)
	Books int    // книг найдено
}

// Result — собранные данные. Возвращается и вместе с ошибкой:
// при Ctrl+C или блокировке сайта уже собранное не теряется.
type Result struct {
	Books    []catalog.Book
	Problems []string // что не удалось разобрать (пропущено)
	Pages    int      // страниц каталога скачано
}

// Run выполняет обход.
func Run(ctx context.Context, f Fetcher, opt Options, log *slog.Logger) (Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = 1
	}
	if opt.Progress == nil {
		opt.Progress = func(Progress) {}
	}

	res, err := listings(ctx, f, opt, log)
	if err != nil || !opt.Details || len(res.Books) == 0 {
		return res, err
	}
	return res, details(ctx, f, &res, opt, log)
}

// listings обходит страницы каталога в ширину (BFS): очередь адресов
// + множество посещённых. Для книжного каталога очередь — цепочка "next",
// но та же схема работает и для обхода категорий или всего сайта.
func listings(ctx context.Context, f Fetcher, opt Options, log *slog.Logger) (Result, error) {
	var (
		res     Result
		queue   = []string{opt.StartURL}
		visited = make(map[string]struct{}) // множество: проверка "уже были?" за O(1)
		seen    = make(map[string]struct{}) // адреса уже найденных книг — защита от дублей
	)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		u := queue[0]
		queue = queue[1:]
		if _, ok := visited[u]; ok {
			continue // защита от зацикливания: "next" ведёт на уже пройденную страницу
		}
		visited[u] = struct{}{}

		page, err := f.Get(ctx, u)
		if err != nil {
			return res, fmt.Errorf("страница каталога %s: %w", u, err)
		}
		visited[page.URL.String()] = struct{}{} // адрес после редиректа тоже считаем пройденным

		l, err := catalog.ParseListing(page.URL, bytes.NewReader(page.Body))
		if err != nil {
			return res, err
		}
		res.Pages++
		for _, p := range l.Problems {
			res.Problems = append(res.Problems, u+": "+p)
			log.Warn("карточка пропущена", "page", u, "problem", p)
		}

		for _, b := range l.Books {
			if _, dup := seen[b.URL]; dup {
				continue
			}
			seen[b.URL] = struct{}{}
			res.Books = append(res.Books, b)
			if opt.Limit > 0 && len(res.Books) >= opt.Limit {
				opt.Progress(Progress{Phase: "каталог", Done: res.Pages, Books: len(res.Books)})
				return res, nil
			}
		}
		opt.Progress(Progress{Phase: "каталог", Done: res.Pages, Books: len(res.Books)})

		if l.Next != "" {
			queue = append(queue, l.Next)
		}
	}
	return res, nil
}

// details скачивает страницы книг пулом из opt.Workers горутин.
//
// Важно: воркеры НЕ увеличивают нагрузку на сайт — клиент всё равно
// пропускает не больше одного запроса в паузу. Параллельность нужна,
// чтобы разбор и ожидание сети одной книги перекрывались со следующей.
func details(ctx context.Context, f Fetcher, res *Result, opt Options, log *slog.Logger) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var (
		jobs = make(chan int) // номера книг в res.Books
		done atomic.Int32
		mu   sync.Mutex // защищает только res.Problems
		wg   sync.WaitGroup
	)
	total := len(res.Books)

	for range opt.Workers {
		wg.Go(func() {
			for i := range jobs {
				url := res.Books[i].URL
				b, err := book(ctx, f, url)
				switch {
				case err == nil:
					// Каждый воркер пишет только в свой элемент среза —
					// разные индексы не пересекаются, мьютекс не нужен,
					// а порядок книг совпадает с каталогом.
					res.Books[i] = b
				case errors.Is(err, fetch.ErrBlocked):
					cancel(err) // сайт попросил остановиться — останавливаем всех
					continue
				case ctx.Err() != nil:
					continue // остановка: просто дочитываем канал
				default:
					// Книга остаётся с данными из каталога, проблема — в отчёт.
					log.Warn("книга пропущена", "url", url, "err", err)
					mu.Lock()
					res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", url, err))
					mu.Unlock()
				}
				opt.Progress(Progress{Phase: "книги", Done: int(done.Add(1)), Total: total, Books: total})
			}
		})
	}

feed:
	for i := range total {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs) // воркеры выйдут из range, когда разберут оставшееся
	wg.Wait()

	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err // ErrBlocked или ошибка родительского контекста
	}
	return ctx.Err()
}

func book(ctx context.Context, f Fetcher, url string) (catalog.Book, error) {
	page, err := f.Get(ctx, url)
	if err != nil {
		return catalog.Book{}, err
	}
	return catalog.ParseBook(page.URL, bytes.NewReader(page.Body))
}
