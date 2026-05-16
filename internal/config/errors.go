package config

import (
	"errors"
)

var (
	ErrBothSecretsSet    = errors.New("both secrets are set, please use one or the other")
	ErrBothSecretsNotSet = errors.New("neither secrets are set, please use one or the other")
	ErrInvalidHttpUrl    = errors.New("invalid HTTP URL")
	ErrInvalidGitUrl     = errors.New("invalid Git URL")
	ErrInvalidOciUrl     = errors.New("invalid OCI artifact reference")
	ErrParseConfigFailed = errors.New("failed to parse config from environment")
)
