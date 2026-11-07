package providers

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func Transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 5 * time.Second
	tr.ResponseHeaderTimeout = 10 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	tr.MaxConnsPerHost = 4
	tr.MaxIdleConnsPerHost = 4
	tr.MaxIdleConns = 8
	tr.MaxResponseHeaderBytes = 16 << 10
	return tr
}
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
func RetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64((30*24*time.Hour)/time.Second) {
			return 30 * 24 * time.Hour
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		d := when.Sub(now)
		if d > 30*24*time.Hour {
			return 30 * 24 * time.Hour
		}
		return d
	}
	return 0
}

// LimitedBody makes SDK reads bounded as well as reads performed by our adapters.
// Exceeding the limit returns an error rather than accepting truncated JSON.
func LimitedBody(body io.ReadCloser) io.ReadCloser {
	return &limitedBody{body: body, remaining: 64 << 10}
}

type limitedBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, errors.New("provider response exceeds limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}
func (b *limitedBody) Close() error { return b.body.Close() }
