package gamma

import "time"

// Config holds gamma's tunable settings.
type Config struct {
	Addr           string
	DefaultChannel string
	SendTimeout    time.Duration
	ShutdownGrace  time.Duration
}

// DefaultConfig is what gamma runs with when nothing overrides it.
func DefaultConfig() Config {
	return Config{
		Addr:           ":8083",
		DefaultChannel: "email",
		SendTimeout:    sendTimeout,
		ShutdownGrace:  45 * time.Second,
	}
}

// Validate reports whether the config can be served.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return errConfig("addr is required")
	case !knownChannel(c.DefaultChannel):
		return errConfig("default channel is not a known channel")
	case c.SendTimeout <= 0:
		return errConfig("send timeout must be positive")
	}
	return nil
}

type errConfig string

func (e errConfig) Error() string { return "gamma config: " + string(e) }
