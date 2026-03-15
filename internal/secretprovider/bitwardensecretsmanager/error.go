package bitwardensecretsmanager

import "errors"

var ErrNotSupported = errors.New("bitwarden secrets manager is not supported in this build")
