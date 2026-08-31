package main

import (
	"net/http"
	"strings"

	"github.com/kimdre/doco-cd/internal/restapi"
)

func (h *orchestrationHandler) requestIP(r *http.Request) string {
	if h == nil || h.appConfig == nil {
		return r.RemoteAddr
	}

	return restapi.ResolveRequestIP(
		r.RemoteAddr,
		strings.TrimSpace(h.appConfig.TrustedProxyHeader),
		r.Header,
		h.appConfig.TrustedProxyNetworks,
	)
}
