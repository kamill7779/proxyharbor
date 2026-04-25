package auth

import (
	"net/http"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

const HeaderName = "ProxyHarbor-Key"

type Authenticator struct {
	Key string
}

func New(key string) *Authenticator {
	return &Authenticator{Key: key}
}

func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil || a.Key == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	if r.Header.Get(HeaderName) != a.Key {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	return domain.Principal{ID: "configured-key", Type: "header_key", TenantID: "default"}, nil
}
