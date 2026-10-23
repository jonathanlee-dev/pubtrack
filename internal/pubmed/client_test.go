package pubmed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a client at h, removes the rate limit, and records
// backoff sleeps instead of sleeping.
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("pubtrack-test", "test@example.com", "")
	c.BaseURL = srv.URL
	c.limiter = NewLimiter(1000)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func TestRetriesOn429And5xx(t *testing.T) {
	var calls int32
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Write([]byte("ok"))
		}
	})
	body, err := c.get(context.Background(), "esearch.fcgi", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(*slept) != 2 {
		t.Errorf("backoff sleeps = %d, want 2", len(*slept))
	}
}

func TestNoRetryOn400(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	})
	_, err := c.get(context.Background(), "esearch.fcgi", url.Values{})
	var herr *HTTPError
	if !errors.As(err, &herr) || herr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want HTTP 400", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestRetryAfterHonored(t *testing.T) {
	var calls int32
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("ok"))
	})
	if _, err := c.get(context.Background(), "esearch.fcgi", url.Values{}); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] < 7*time.Second {
		t.Errorf("slept %v, want at least 7s", *slept)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.get(context.Background(), "esearch.fcgi", url.Values{})
	if err == nil || !strings.Contains(err.Error(), "gave up after 5 attempts") {
		t.Fatalf("err = %v", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5 (1 + MaxRetries 4)", calls)
	}
}

func TestSendsIdentification(t *testing.T) {
	var q url.Values
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		w.Write([]byte(`{"esearchresult":{"count":"0","idlist":[]}}`))
	})
	c.APIKey = "k123"
	if _, err := c.Search(context.Background(), "x", 7); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"tool": "pubtrack-test", "email": "test@example.com", "api_key": "k123",
		"db": "pubmed", "datetype": "edat", "reldate": "7", "retmode": "json",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestErrorsDoNotLeakAPIKey(t *testing.T) {
	// Connection error: *url.Error would print the full URL, api_key included.
	srv := httptest.NewServer(http.NotFoundHandler())
	closedURL := srv.URL
	srv.Close()
	c := New("t", "e@example.org", "SECRET123")
	c.BaseURL = closedURL
	c.limiter = NewLimiter(1000)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.MaxRetries = 1
	_, err := c.Search(context.Background(), "x", 7)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRET123") {
		t.Errorf("connection error leaks api key: %v", err)
	}

	// Error body: NCBI's 429 body echoes the key back.
	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"API rate limit exceeded","api-key":"SECRET123"}`))
	})
	c2.APIKey = "SECRET123"
	c2.MaxRetries = 0
	_, err = c2.Search(context.Background(), "x", 7)
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Errorf("error body leaks api key: %v", err)
	}
}

func TestFetchBatches(t *testing.T) {
	var sizes []int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sizes = append(sizes, len(strings.Split(r.URL.Query().Get("id"), ",")))
		w.Write([]byte(`<PubmedArticleSet></PubmedArticleSet>`))
	})
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "1"
	}
	if _, err := c.Fetch(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 50 {
		t.Errorf("batch sizes = %v, want [100 100 50]", sizes)
	}
}

func TestLimiterSpacesRequests(t *testing.T) {
	l := NewLimiter(50) // 20ms apart
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 78*time.Millisecond {
		t.Errorf("5 requests at 50/s took %v, want at least 80ms", elapsed)
	}
}

func TestLimiterRespectsCancel(t *testing.T) {
	l := NewLimiter(1)
	l.Wait(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
