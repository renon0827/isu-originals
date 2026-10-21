package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

var sharedTransport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:        0,
	MaxIdleConnsPerHost: 512,
	IdleConnTimeout:     60 * time.Second,
}

// Agent は 1 人のユーザー（ブラウザ）を表す。Cookie と簡易 HTTP キャッシュを持つ。
type Agent struct {
	base   string
	host   string
	client *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	etag         string
	lastModified string
	expires      time.Time
	body         []byte
}

func NewAgent(base, host string, timeout time.Duration) *Agent {
	jar, _ := cookiejar.New(nil)
	return &Agent{
		base: strings.TrimSuffix(base, "/"),
		host: host,
		client: &http.Client{
			Transport: sharedTransport,
			Jar:       jar,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cache: map[string]cacheEntry{},
	}
}

// Response は計測情報つきのレスポンス。
type Response struct {
	Status  int
	Body    []byte
	Latency time.Duration
	Header  http.Header
	// FromCache はブラウザキャッシュ（max-age / 304）で済んだ場合 true
	FromCache bool
	// LongPoll は価格確認 API（ロングポーリングで待たされてよい）の場合 true
	LongPoll bool
}

var ErrTimeout = errors.New("timeout")

func (a *Agent) Do(ctx context.Context, method, path string, body any) (*Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, err
	}
	if a.host != "" {
		req.Host = a.host
	}
	req.Header.Set("User-Agent", "isuauction-bench")
	req.Header.Set("Accept-Encoding", "gzip")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	res, err := a.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s %s: %w", method, path, ErrTimeout)
		}
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	b, err := readBody(res)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	return &Response{Status: res.StatusCode, Body: b, Latency: time.Since(start), Header: res.Header,
		LongPoll: strings.Contains(path, "/price?")}, nil
}

// GetStatic はブラウザと同様に Cache-Control / ETag / Last-Modified を尊重して取得する。
func (a *Agent) GetStatic(ctx context.Context, path string) (*Response, error) {
	a.mu.Lock()
	ce, ok := a.cache[path]
	a.mu.Unlock()
	if ok && time.Now().Before(ce.expires) {
		return &Response{Status: 200, Body: ce.body, FromCache: true}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return nil, err
	}
	if a.host != "" {
		req.Host = a.host
	}
	req.Header.Set("User-Agent", "isuauction-bench")
	req.Header.Set("Accept-Encoding", "gzip")
	if ok {
		if ce.etag != "" {
			req.Header.Set("If-None-Match", ce.etag)
		}
		if ce.lastModified != "" {
			req.Header.Set("If-Modified-Since", ce.lastModified)
		}
	}
	start := time.Now()
	res, err := a.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("GET %s: %w", path, ErrTimeout)
		}
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer res.Body.Close()
	b, err := readBody(res)
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", path, err)
	}
	r := &Response{Status: res.StatusCode, Body: b, Latency: time.Since(start), Header: res.Header}
	if res.StatusCode == http.StatusNotModified && ok {
		r.Status, r.Body, r.FromCache = 200, ce.body, true
	}
	if r.Status == 200 {
		ne := cacheEntry{etag: res.Header.Get("ETag"), lastModified: res.Header.Get("Last-Modified"), body: r.Body}
		if ma := maxAge(res.Header.Get("Cache-Control")); ma > 0 {
			ne.expires = time.Now().Add(ma)
		}
		if ne.etag != "" || ne.lastModified != "" || !ne.expires.IsZero() {
			a.mu.Lock()
			a.cache[path] = ne
			a.mu.Unlock()
		}
	}
	return r, nil
}

func maxAge(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "max-age=") {
			var sec int
			fmt.Sscanf(strings.TrimPrefix(part, "max-age="), "%d", &sec)
			return time.Duration(sec) * time.Second
		}
		if part == "no-store" || part == "no-cache" {
			return 0
		}
	}
	return 0
}

func readBody(res *http.Response) ([]byte, error) {
	var r io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gz, err := newGzipReader(res.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return io.ReadAll(r)
}

func decode[T any](r *Response) (T, error) {
	var v T
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return v, fmt.Errorf("invalid json: %w", err)
	}
	return v, nil
}
