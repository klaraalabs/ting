package server

import (
	"errors"
	"time"
)

type Limits struct {
	RequestsPerMinute int
	MaxPeers          int
	MaxConcurrent     int
	MaxBodyBytes      int
	RequestTimeout    time.Duration
}

func DefaultLimits() Limits {
	return Limits{RequestsPerMinute: 120, MaxPeers: 10000, MaxConcurrent: 64, MaxBodyBytes: 1 << 20, RequestTimeout: 10 * time.Second}
}
func (l Limits) Validate() error {
	if l.RequestsPerMinute < 1 || l.RequestsPerMinute > 100000 {
		return errors.New("TING_REQUESTS_PER_MINUTE must be between 1 and 100000")
	}
	if l.MaxPeers < 1 || l.MaxPeers > 100000 {
		return errors.New("TING_RATE_LIMIT_MAX_PEERS must be between 1 and 100000")
	}
	if l.MaxConcurrent < 1 || l.MaxConcurrent > 1000 {
		return errors.New("TING_MAX_CONCURRENT_REQUESTS must be between 1 and 1000")
	}
	if l.MaxBodyBytes < 4096 || l.MaxBodyBytes > 1<<20 {
		return errors.New("TING_MAX_REQUEST_BYTES must be between 4096 and 1048576")
	}
	if l.RequestTimeout < time.Second || l.RequestTimeout > 30*time.Second {
		return errors.New("TING_REQUEST_TIMEOUT must be between 1s and 30s")
	}
	return nil
}
