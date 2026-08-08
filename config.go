package server

import (
	"time"

	"github.com/kelseyhightower/envconfig"
)

const (
	defaultReadTimeout     = time.Second * 15
	defaultWriteTimeout    = time.Second * 15
	defaultShutdownTimeout = time.Second * 10
	defaultStaticPath      = "/static"
)

type HttpConfig struct {
	BindAddress  string `split_words:"true" default:":8080"`
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// ShutdownTimeout is the grace period in-flight requests are given to
	// finish once a termination signal arrives. Read from
	// HTTP_SHUTDOWN_TIMEOUT; defaults to defaultShutdownTimeout.
	ShutdownTimeout time.Duration `split_words:"true"`
	MaxBodyLimit    string        `split_words:"true" default:"51200M"`
	StaticPath      string        `split_words:"true" default:""`
	AlllowedOrigins []string      `split_words:"true" default:"*"`
}

func NewHttpConfig() (*HttpConfig, error) {
	var h HttpConfig
	if err := envconfig.Process("http", &h); err != nil {
		return nil, err
	}

	if h.ReadTimeout == 0 {
		h.ReadTimeout = defaultReadTimeout
	}

	if h.WriteTimeout == 0 {
		h.WriteTimeout = defaultWriteTimeout
	}

	if h.ShutdownTimeout == 0 {
		h.ShutdownTimeout = defaultShutdownTimeout
	}

	if h.StaticPath == "" {
		h.StaticPath = defaultStaticPath
	}

	return &h, nil
}
