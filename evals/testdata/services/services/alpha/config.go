package alpha

import "time"

// Config holds alpha's tunable settings.
type Config struct {
	Addr            string
	DefaultPageSize int
	ShutdownGrace   time.Duration
	StoreTimeout    time.Duration
}

// DefaultConfig is what alpha runs with when nothing overrides it.
func DefaultConfig() Config {
	return Config{
		Addr:            ":8081",
		DefaultPageSize: 50,
		ShutdownGrace:   20 * time.Second,
		StoreTimeout:    queryTimeout,
	}
}

// Validate reports whether the config can be served.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return errConfig("addr is required")
	case c.DefaultPageSize <= 0:
		return errConfig("default page size must be positive")
	case c.DefaultPageSize > MaxPageSize:
		return errConfig("default page size exceeds the maximum")
	}
	return nil
}

type errConfig string

func (e errConfig) Error() string { return "alpha config: " + string(e) }
