package providers

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	for _, test := range []struct {
		header string
		want   time.Duration
	}{{"120", 2 * time.Minute}, {now.Add(time.Minute).Format(http.TimeFormat), time.Minute}, {"-1", 0}, {"garbage", 0}, {"999999999999999999", 30 * 24 * time.Hour}} {
		if got := RetryAfter(test.header, now); got != test.want {
			t.Errorf("%q: %s", test.header, got)
		}
	}
}
func TestResponseBound(t *testing.T) {
	for _, size := range []int{100, 64 << 10, (64 << 10) + 1} {
		body := LimitedBody(io.NopCloser(strings.NewReader(strings.Repeat("x", size))))
		raw, err := io.ReadAll(body)
		_ = body.Close()
		if size <= 64<<10 && (err != nil || len(raw) != size) {
			t.Fatal("valid response rejected")
		}
		if size > 64<<10 && err == nil {
			t.Fatal("oversized response accepted")
		}
	}
}
