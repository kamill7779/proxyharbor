package proxyharbor

import "net/http"

const headerKey = "ProxyHarbor-Key"

type authMode int

const (
	authTenant authMode = iota
	authAdmin
	authNone
)

// applyAuth attaches the appropriate ProxyHarbor key header to req.
//
// Tenant calls fall back to the admin key when no tenant key is configured;
// the gateway accepts admin keys for tenant-scoped APIs as well.
func (c *Client) applyAuth(req *http.Request, mode authMode) error {
	switch mode {
	case authTenant:
		key := c.cfg.TenantKey
		if key == "" {
			key = c.cfg.AdminKey
		}
		if key == "" {
			return ErrNoTenantKey
		}
		req.Header.Set(headerKey, key)
	case authAdmin:
		if c.cfg.AdminKey == "" {
			return ErrNoAdminKey
		}
		req.Header.Set(headerKey, c.cfg.AdminKey)
	case authNone:
	}
	return nil
}
