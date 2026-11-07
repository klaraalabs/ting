package admin

import (
	"errors"
	"net/url"
	"time"
)

type Config struct {
	Enabled    bool
	Origin     string
	SessionTTL time.Duration
}

func DefaultConfig() Config {
	return Config{Enabled: true, Origin: "https://localhost:8080", SessionTTL: 8 * time.Hour}
}
func (c Config) Validate() error {
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != c.Origin {
		return errors.New("TING_ADMIN_ORIGIN must be an exact HTTPS origin without a path, query, or credentials")
	}
	if c.SessionTTL < 5*time.Minute || c.SessionTTL > 24*time.Hour {
		return errors.New("TING_ADMIN_SESSION_TTL must be between 5m and 24h")
	}
	return nil
}
