// Package pubmed is a small client for the NCBI E-utilities API (esearch,
// elink, efetch) with rate limiting and retry with backoff.
package pubmed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/"

const (
	maxBody       = 32 << 20
	maxRetryAfter = 2 * time.Minute
)

type Client struct {
	BaseURL    string
	HTTP       *http.Client
	Tool       string
	Email      string
	APIKey     string
	MaxRetries int

	limiter *Limiter
	backoff func(attempt int) time.Duration
	sleep   func(ctx context.Context, d time.Duration) error
}

// New returns a client limited to 3 requests/second, or 10 with an API key,
// which are NCBI's published limits.
func New(tool, email, apiKey string) *Client {
	rate := 3
	if apiKey != "" {
		rate = 10
	}
	return &Client{
		BaseURL:    DefaultBaseURL,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		Tool:       tool,
		Email:      email,
		APIKey:     apiKey,
		MaxRetries: 4,
		limiter:    NewLimiter(rate),
		backoff:    expBackoff,
		sleep:      sleepCtx,
	}
}

type HTTPError struct {
	Endpoint   string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Endpoint, e.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Endpoint, e.StatusCode, e.Body)
}

// get calls an E-utilities endpoint, retrying 429s, 5xx responses, and
// connection errors with exponential backoff (or Retry-After, if longer).
func (c *Client) get(ctx context.Context, endpoint string, params url.Values) ([]byte, error) {
	params.Set("tool", c.Tool)
	params.Set("email", c.Email)
	if c.APIKey != "" {
		params.Set("api_key", c.APIKey)
	}
	u := strings.TrimSuffix(c.BaseURL, "/") + "/" + endpoint + "?" + params.Encode()

	for attempt := 0; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		body, retryAfter, retryable, err := c.do(ctx, endpoint, u)
		if err == nil {
			return body, nil
		}
		if !retryable || attempt >= c.MaxRetries {
			if attempt > 0 {
				return nil, fmt.Errorf("%w (gave up after %d attempts)", err, attempt+1)
			}
			return nil, err
		}
		wait := c.backoff(attempt)
		if retryAfter > wait {
			wait = retryAfter
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *Client) do(ctx context.Context, endpoint, u string) (body []byte, retryAfter time.Duration, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, false, fmt.Errorf("%s: %w", endpoint, stripURL(err))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
		return nil, 0, true, fmt.Errorf("%s: %w", endpoint, stripURL(err))
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, 0, true, fmt.Errorf("%s: reading response: %w", endpoint, err)
	}
	if resp.StatusCode == http.StatusOK {
		return body, 0, false, nil
	}
	retryable = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return nil, parseRetryAfter(resp.Header.Get("Retry-After")), retryable,
		&HTTPError{Endpoint: endpoint, StatusCode: resp.StatusCode, Body: c.redact(snippet(body))}
}

// stripURL drops the request URL from a *url.Error. Its message prints the
// full URL, which carries the api_key and email parameters.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redact removes the API key from text NCBI sends back; its 429 body echoes it.
func (c *Client) redact(s string) string {
	if c.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.APIKey, "[redacted]")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func expBackoff(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
