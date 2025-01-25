package server

import (
	"time"

	"github.com/kelseyhightower/envconfig"
)

const (
	defaultReadTimeout  = time.Second * 15
	defaultWriteTimeout = time.Second * 15
	defaultStaticPath   = "/static"
)

type HttpConfig struct {
	BindAddress     string `split_words:"true" default:":8080"`
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	MaxBodyLimit    string   `split_words:"true" default:"51200M"`
	StaticPath      string   `split_words:"true" default:""`
	AlllowedOrigins []string `split_words:"true" default:"*"`
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

	if h.StaticPath == "" {
		h.StaticPath = defaultStaticPath
	}

	return &h, nil
}
