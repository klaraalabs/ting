package delivery

import (
	"math/rand/v2"
	"time"
)

func retryDelay(c Config, attempt int, retryAfter time.Duration) time.Duration {
	delay := c.RetryBase
	// Saturate before multiplying, including after a database lease is recovered.
	for n := 1; n < attempt && delay < c.RetryMax; n++ {
		if delay > c.RetryMax/2 {
			delay = c.RetryMax
			break
		}
		delay *= 2
	}
	if delay > c.RetryMax {
		delay = c.RetryMax
	}
	// Equal jitter avoids immediate retry storms while keeping exponential bounds.
	delay = delay/2 + time.Duration(rand.Int64N(int64(delay-delay/2)+1))
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}
