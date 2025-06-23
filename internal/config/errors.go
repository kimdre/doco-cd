package config

import (
	"errors"

	"gopkg.in/validator.v2"
)

var (
	ErrInvalidLogLevel   = validator.TextErr{Err: errors.New("invalid log level, must be one of debug, info, warn, error")}
	ErrBothSecretsSet    = errors.New("both secrets are set, please use one or the other")
	ErrBothSecretsNotSet = errors.New("neither secrets are set, please use one or the other")
	ErrInvalidHttpUrl    = errors.New("invalid HTTP URL")
)
