// Package fetch — «вежливый» HTTP-клиент для парсинга: соблюдает robots.txt,
// ограничивает частоту запросов, повторяет при временных ошибках и
// останавливается, если сайт ограничил доступ. Отключить эти правила нельзя.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/TaRosh/webParser/internal/robots"
)

var (
	// ErrDisallowed — страница закрыта в robots.txt; мы её не скачиваем.
	ErrDisallowed = errors.New("страница закрыта в robots.txt")
	// ErrBlocked — сайт ответил 403 или продолжает отвечать 429.
	// Обходить ограничение мы не будем: парсинг этого сайта останавливается.
	ErrBlocked = errors.New("сайт ограничил доступ — парсинг остановлен")
	// errRedirect — редирект запрещён нашими правилами; повтор не поможет.
	errRedirect = errors.New("редирект запрещён")
)

// StatusError — сайт ответил кодом, который не исправить повтором (например, 404).
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.Code) }

// Config — настройки клиента. Нулевые значения заменяются безопасными умолчаниями.
type Config struct {
	// Agent — имя робота для поиска своей группы в robots.txt.
	Agent string
	// UserAgent — заголовок User-Agent: честное имя и ссылка на описание бота.
	UserAgent string
	// MinDelay — минимальная пауза между запросами к одному сайту.
	// Если в robots.txt указан больший Crawl-delay — используется он.
	MinDelay time.Duration
	// MaxRetries — сколько раз повторить запрос при 5xx, 429 и сетевых ошибках.
	MaxRetries int
	// BackoffBase — первая пауза перед повтором; дальше она удваивается.
	BackoffBase time.Duration
	// MaxWait — дольше этого не ждём, даже если сайт просит (Retry-After).
	MaxWait time.Duration
	// MaxBodySize — максимальный размер страницы.
	MaxBodySize int64
	// Timeout — таймаут одного запроса.
	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Agent == "" {
		c.Agent = "webParser"
	}
	if c.UserAgent == "" {
		c.UserAgent = "webParser/0.1 (+https://github.com/TaRosh/webParser)"
	}
	if c.MinDelay <= 0 {
		c.MinDelay = time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	} else if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.MaxWait <= 0 {
		c.MaxWait = 2 * time.Minute
	}
	if c.MaxBodySize <= 0 {
		c.MaxBodySize = 10 << 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	return c
}

// Page — скачанная страница.
type Page struct {
	URL  *url.URL // итоговый адрес (после редиректов)
	Body []byte
}

// Client безопасен для использования из нескольких горутин.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	mu    sync.Mutex
	hosts map[string]*host // состояние каждого сайта: robots.txt, лимитер, блокировка
}

type host struct {
	once    sync.Once // robots.txt скачивается один раз, даже при параллельных запросах
	rules   robots.Group
	limiter *rate.Limiter
	blocked atomic.Bool
}

func New(cfg Config, log *slog.Logger) *Client {
	c := &Client{cfg: cfg.withDefaults(), log: log, hosts: make(map[string]*host)}
	c.http = &http.Client{Timeout: c.cfg.Timeout, CheckRedirect: c.checkRedirect}
	return c
}

// Get скачивает страницу, соблюдая все правила вежливости.
func (c *Client) Get(ctx context.Context, rawURL string) (*Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("некорректный адрес %q", rawURL)
	}

	h := c.host(ctx, u)
	if h.blocked.Load() {
		return nil, ErrBlocked
	}
	if !h.rules.Allowed(u.RequestURI()) {
		return nil, fmt.Errorf("%s: %w", rawURL, ErrDisallowed)
	}

	for attempt := 0; ; attempt++ {
		if err := h.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		page, retryAfter, err := c.do(ctx, u)
		if err == nil {
			return page, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		var se *StatusError
		isStatus := errors.As(err, &se)
		switch {
		case errors.Is(err, ErrDisallowed), errors.Is(err, errRedirect):
			return nil, err // редирект на закрытую страницу или на другой сайт
		case isStatus && se.Code == http.StatusForbidden:
			h.blocked.Store(true)
			c.log.Warn("сайт ответил 403 — останавливаемся", "host", u.Host)
			return nil, ErrBlocked
		case isStatus && !retryable(se.Code):
			return nil, err // 404 и т.п. — повтор не поможет
		}

		// Временная ошибка: 5xx, 429 или сеть.
		tooMany := isStatus && se.Code == http.StatusTooManyRequests
		wait := retryAfter
		if wait == 0 {
			wait = c.backoff(attempt)
		}
		if attempt >= c.cfg.MaxRetries || wait > c.cfg.MaxWait {
			if tooMany {
				// Сайт продолжает просить снизить частоту — уважаем и уходим.
				h.blocked.Store(true)
				c.log.Warn("сайт продолжает отвечать 429 — останавливаемся", "host", u.Host)
				return nil, ErrBlocked
			}
			return nil, err
		}
		c.log.Warn("временная ошибка, повтор", "url", rawURL, "err", err, "attempt", attempt+1, "wait", wait)
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// do выполняет один запрос. retryAfter > 0, если сайт прислал Retry-After.
func (c *Client) do(ctx context.Context, u *url.URL) (page *Page, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Дочитываем немного тела, чтобы соединение можно было переиспользовать.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			&StatusError{URL: u.String(), Code: resp.StatusCode}
	}

	// Читаем на 1 байт больше лимита: так отличаем "ровно лимит" от "больше".
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodySize+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > c.cfg.MaxBodySize {
		return nil, 0, &StatusError{URL: u.String(), Code: http.StatusRequestEntityTooLarge}
	}
	return &Page{URL: resp.Request.URL, Body: body}, 0, nil
}

// host возвращает состояние сайта, при первом обращении скачивая robots.txt.
func (c *Client) host(ctx context.Context, u *url.URL) *host {
	key := u.Scheme + "://" + u.Host
	c.mu.Lock()
	h, ok := c.hosts[key]
	if !ok {
		h = &host{}
		c.hosts[key] = h
	}
	c.mu.Unlock()

	h.once.Do(func() {
		// Отмена контекста одного запроса не должна "навсегда" закрыть сайт:
		// robots.txt качаем с собственным таймаутом, независимо от ctx.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.Timeout)
		defer cancel()
		h.rules = c.loadRobots(rctx, key)
		delay := max(c.cfg.MinDelay, h.rules.CrawlDelay)
		// Token bucket на 1 токен, пополнение раз в delay = не чаще одного запроса в delay.
		h.limiter = rate.NewLimiter(rate.Every(delay), 1)
		c.log.Info("правила сайта", "host", u.Host, "delay", delay)
	})
	return h
}

// loadRobots скачивает и разбирает robots.txt по правилам RFC 9309:
// 2xx — разбираем; 4xx — файла нет, можно всё; 5xx или сеть — нельзя ничего.
func (c *Client) loadRobots(ctx context.Context, base string) robots.Group {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/robots.txt", nil)
	if err != nil {
		return robots.DisallowAll()
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	// Отдельный клиент: редиректы robots.txt разрешены на любой хост (RFC),
	// а наша проверка редиректов здесь не нужна.
	resp, err := (&http.Client{Timeout: c.cfg.Timeout}).Do(req)
	if err != nil {
		c.log.Warn("robots.txt недоступен — считаем сайт закрытым", "site", base, "err", err)
		return robots.DisallowAll()
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return robots.Parse(resp.Body).Group(c.cfg.Agent)
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		return robots.AllowAll()
	default:
		c.log.Warn("robots.txt вернул ошибку — считаем сайт закрытым", "site", base, "status", resp.StatusCode)
		return robots.DisallowAll()
	}
}

// checkRedirect разрешает редиректы только внутри того же сайта и только
// на страницы, не закрытые в robots.txt.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("больше 5 подряд: %w", errRedirect)
	}
	from := via[0].URL
	if req.URL.Host != from.Host || req.URL.Scheme != from.Scheme {
		return fmt.Errorf("на другой сайт %s: %w", req.URL.Host, errRedirect)
	}
	c.mu.Lock()
	h := c.hosts[from.Scheme+"://"+from.Host]
	c.mu.Unlock()
	if h != nil && !h.rules.Allowed(req.URL.RequestURI()) {
		return fmt.Errorf("редирект на %s: %w", req.URL, ErrDisallowed)
	}
	return nil
}

// backoff — экспоненциальная пауза: base, 2·base, 4·base, ... плюс случайный
// разброс до половины base. Разброс нужен, чтобы много клиентов после сбоя
// сайта не вернулись к нему одновременно одной волной.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.BackoffBase << min(attempt, 10) // ограничиваем сдвиг, чтобы не переполнить
	return d + rand.N(c.cfg.BackoffBase/2+1)   //nolint:gosec // разброс паузы не секрет, криптостойкость не нужна
}

// retryable — коды, при которых имеет смысл повторить запрос позже.
func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// parseRetryAfter разбирает заголовок Retry-After: число секунд или дата.
// Возвращает 0, если заголовка нет или он некорректен.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil {
		if sec < 0 {
			return 0
		}
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
