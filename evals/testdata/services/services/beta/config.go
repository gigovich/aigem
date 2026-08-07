package beta

import "time"

// Config holds beta's tunable settings.
type Config struct {
	Addr            string
	DefaultCurrency string
	ChargeTimeout   time.Duration
	ShutdownGrace   time.Duration
}

// DefaultConfig is what beta runs with when nothing overrides it.
func DefaultConfig() Config {
	return Config{
		Addr:            ":8082",
		DefaultCurrency: "EUR",
		ChargeTimeout:   10 * time.Second,
		ShutdownGrace:   30 * time.Second,
	}
}

// Validate reports whether the config can be served.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return errConfig("addr is required")
	case len(c.DefaultCurrency) != 3:
		return errConfig("default currency must be a 3-letter code")
	case c.ChargeTimeout <= 0:
		return errConfig("charge timeout must be positive")
	}
	return nil
}

type errConfig string

func (e errConfig) Error() string { return "beta config: " + string(e) }
