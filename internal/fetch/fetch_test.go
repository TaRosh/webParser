package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// site — фальшивый сайт: robots.txt и обработчик страниц, считает обращения.
type site struct {
	srv        *httptest.Server
	robotsHits atomic.Int32
	pageHits   atomic.Int32
	userAgent  atomic.Value
}

// newSite поднимает сайт. robotsStatus/robotsBody — ответ на /robots.txt,
// page — обработчик всех остальных путей (n — номер обращения, с 1).
func newSite(t *testing.T, robotsStatus int, robotsBody string, page func(w http.ResponseWriter, r *http.Request, n int)) *site {
	t.Helper()
	s := &site{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			s.robotsHits.Add(1)
			w.WriteHeader(robotsStatus)
			_, _ = io.WriteString(w, robotsBody)
			return
		}
		s.userAgent.Store(r.Header.Get("User-Agent"))
		page(w, r, int(s.pageHits.Add(1)))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func ok(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "<html>ok</html>") }

func status(code int) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(code) }
}

// testClient — клиент с короткими паузами, чтобы тесты шли быстро.
func testClient() *Client {
	return New(Config{
		MinDelay:    time.Millisecond,
		BackoffBase: time.Millisecond,
		MaxRetries:  2,
		MaxWait:     time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestGetOK(t *testing.T) {
	s := newSite(t, http.StatusNotFound, "", ok)
	page, err := testClient().Get(context.Background(), s.srv.URL+"/books")
	if err != nil {
		t.Fatal(err)
	}
	if string(page.Body) != "<html>ok</html>" {
		t.Errorf("body = %q", page.Body)
	}
	if ua, _ := s.userAgent.Load().(string); !strings.HasPrefix(ua, "webParser/") || !strings.Contains(ua, "github.com") {
		t.Errorf("User-Agent = %q, want honest name with contact link", ua)
	}
}

func TestRobots(t *testing.T) {
	tests := []struct {
		name         string
		robotsStatus int
		robotsBody   string
		path         string
		wantErr      error
		wantPageHits int32
	}{
		{"no robots.txt (404) — allowed", 404, "", "/a", nil, 1},
		{"allowed by robots", 200, "User-agent: *\nDisallow: /private\n", "/public", nil, 1},
		{"disallowed — page not requested", 200, "User-agent: *\nDisallow: /private\n", "/private/x", ErrDisallowed, 0},
		{"our own group applies", 200, "User-agent: webParser\nDisallow: /\n\nUser-agent: *\nAllow: /\n", "/a", ErrDisallowed, 0},
		{"query is checked too", 200, "User-agent: *\nDisallow: /*?sort=\n", "/list?sort=price", ErrDisallowed, 0},
		{"robots.txt 5xx — whole site closed, real reason reported", 503, "", "/a", ErrRobotsUnavailable, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t, tt.robotsStatus, tt.robotsBody, ok)
			_, err := testClient().Get(context.Background(), s.srv.URL+tt.path)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := s.pageHits.Load(); got != tt.wantPageHits {
				t.Errorf("page requests = %d, want %d", got, tt.wantPageHits)
			}
		})
	}
}

func TestRobotsFetchedOnceConcurrently(t *testing.T) {
	s := newSite(t, 200, "User-agent: *\nAllow: /\n", ok)
	c := testClient()

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := c.Get(context.Background(), s.srv.URL+"/a"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := s.robotsHits.Load(); n != 1 {
		t.Errorf("robots.txt requested %d times, want 1", n)
	}
}

func TestRateLimit(t *testing.T) {
	tests := []struct {
		name     string
		minDelay time.Duration
		robots   string
		wantGap  time.Duration
	}{
		{"min delay", 40 * time.Millisecond, "", 40 * time.Millisecond},
		{"crawl-delay is larger — it wins", time.Millisecond, "User-agent: *\nCrawl-delay: 0.06\n", 60 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				times []time.Time
			)
			s := newSite(t, 200, tt.robots, func(w http.ResponseWriter, r *http.Request, n int) {
				mu.Lock()
				times = append(times, time.Now())
				mu.Unlock()
				ok(w, r, n)
			})
			c := New(Config{MinDelay: tt.minDelay}, slog.New(slog.NewTextHandler(io.Discard, nil)))

			// Даже параллельные запросы к одному сайту идут с паузой.
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					if _, err := c.Get(context.Background(), s.srv.URL+"/a"); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()

			mu.Lock()
			defer mu.Unlock()
			for i := 1; i < len(times); i++ {
				// Небольшой допуск на точность таймеров.
				if gap := times[i].Sub(times[i-1]); gap < tt.wantGap-5*time.Millisecond {
					t.Errorf("gap between requests %d and %d = %v, want >= %v", i, i+1, gap, tt.wantGap)
				}
			}
		})
	}
}

func TestRetriesAndBlocking(t *testing.T) {
	tests := []struct {
		name      string
		page      func(http.ResponseWriter, *http.Request, int)
		wantErr   error // nil — успех
		wantCode  int   // ожидаемый StatusError, если wantErr == nil и успеха нет
		wantHits  int32
		thenBlock bool // следующий запрос к сайту отклоняется сразу
	}{
		{
			name: "503 twice, then success",
			page: func(w http.ResponseWriter, r *http.Request, n int) {
				if n <= 2 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				ok(w, r, n)
			},
			wantHits: 3,
		},
		{
			name:     "500 always — give up after retries",
			page:     status(500),
			wantCode: 500,
			wantHits: 3, // 1 попытка + 2 повтора
		},
		{
			name: "429 with Retry-After, then success",
			page: func(w http.ResponseWriter, r *http.Request, n int) {
				if n == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				ok(w, r, n)
			},
			wantHits: 2,
		},
		{
			name:      "429 always — stop, do not evade",
			page:      status(429),
			wantErr:   ErrBlocked,
			wantHits:  3,
			thenBlock: true,
		},
		{
			name: "Retry-After longer than we agree to wait — stop at once",
			page: func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantErr:   ErrBlocked,
			wantHits:  1,
			thenBlock: true,
		},
		{
			name:      "403 — stop immediately",
			page:      status(403),
			wantErr:   ErrBlocked,
			wantHits:  1,
			thenBlock: true,
		},
		{
			name:     "404 — no retry",
			page:     status(404),
			wantCode: 404,
			wantHits: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t, 404, "", tt.page)
			c := testClient()
			ctx := context.Background()

			_, err := c.Get(ctx, s.srv.URL+"/a")

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantCode != 0:
				var se *StatusError
				if !errors.As(err, &se) || se.Code != tt.wantCode {
					t.Fatalf("err = %v, want HTTP %d", err, tt.wantCode)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if got := s.pageHits.Load(); got != tt.wantHits {
				t.Errorf("requests = %d, want %d", got, tt.wantHits)
			}

			if tt.thenBlock {
				_, err := c.Get(ctx, s.srv.URL+"/other")
				if !errors.Is(err, ErrBlocked) {
					t.Errorf("next request: err = %v, want ErrBlocked", err)
				}
				if got := s.pageHits.Load(); got != tt.wantHits {
					t.Errorf("blocked site was requested again (%d requests)", got)
				}
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	other := newSite(t, 404, "", ok)

	tests := []struct {
		name    string
		target  string // куда редиректит /start
		wantErr error  // nil — успех
	}{
		{"same site — followed", "/final", nil},
		{"to disallowed page — refused", "/private/x", ErrDisallowed},
		{"to another site — refused", other.srv.URL + "/x", errRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t, 200, "User-agent: *\nDisallow: /private\n", func(w http.ResponseWriter, r *http.Request, n int) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, tt.target, http.StatusFound)
					return
				}
				ok(w, r, n)
			})
			page, err := testClient().Get(context.Background(), s.srv.URL+"/start")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && page.URL.Path != "/final" {
				t.Errorf("final URL = %s", page.URL)
			}
			if tt.wantErr != nil && s.pageHits.Load() != 1 {
				t.Errorf("refused redirect was retried: %d requests", s.pageHits.Load())
			}
			if other.pageHits.Load() != 0 {
				t.Error("request leaked to another site")
			}
		})
	}
}

func TestBodySizeLimit(t *testing.T) {
	s := newSite(t, 404, "", func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, strings.Repeat("x", 2000))
	})
	c := New(Config{MinDelay: time.Millisecond, MaxBodySize: 1000}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var se *StatusError
	if _, err := c.Get(context.Background(), s.srv.URL+"/big"); !errors.As(err, &se) || se.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want too large", err)
	}
}

func TestInvalidURL(t *testing.T) {
	for _, u := range []string{"", "ftp://example.com/x", "not a url", "/relative/path"} {
		if _, err := testClient().Get(context.Background(), u); err == nil {
			t.Errorf("Get(%q): expected error", u)
		}
	}
}

func TestContextCancelDuringWait(t *testing.T) {
	s := newSite(t, 404, "", ok)
	c := New(Config{MinDelay: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, err := c.Get(ctx, s.srv.URL+"/a"); err != nil { // первый запрос — сразу
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Get(ctx, s.srv.URL+"/b") // второй ждал бы час — отменяем
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v, want quick context error", err, time.Since(start))
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},
		{"-1", 0},
		{"soon", 0},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0}, // дата в прошлом
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestBackoffGrows(t *testing.T) {
	c := New(Config{BackoffBase: 100 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for attempt, min := range []time.Duration{100, 200, 400, 800} {
		min *= time.Millisecond
		got := c.backoff(attempt)
		if got < min || got > min+50*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want [%v, %v]", attempt, got, min, min+50*time.Millisecond)
		}
	}
}

// Сайт не отвечает вообще (как при блокировке): robots.txt недоступен —
// сайт закрыт, а в ошибке настоящая причина, а не "закрыто в robots.txt".
func TestRobotsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // соединение будет отклонено

	_, err := testClient().Get(context.Background(), url+"/page")
	if !errors.Is(err, ErrRobotsUnavailable) || errors.Is(err, ErrDisallowed) {
		t.Fatalf("err = %v, want ErrRobotsUnavailable (and not ErrDisallowed)", err)
	}
	if strings.Count(err.Error(), url+"/page") != 1 {
		t.Errorf("URL must appear once: %v", err)
	}
}
