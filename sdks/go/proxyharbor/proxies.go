package proxyharbor

import "context"

// proxiesAPI exposes the lower-level proxy inventory HTTP API.
type proxiesAPI struct{ c *Client }

// AddProxy adds a proxy to the pool. endpoint is the only required field.
//
//	c.AddProxy(ctx, "1.2.3.4:3128")
func (c *Client) AddProxy(ctx context.Context, endpoint string) (ProxyDTO, error) {
	return c.Proxies.Upsert(ctx, ProxyDTO{Endpoint: endpoint, Healthy: true})
}

// AddProxyWithProvider adds a proxy bound to a specific provider.
//
//	c.AddProxyWithProvider(ctx, "1.2.3.4:3128", "my-dc")
func (c *Client) AddProxyWithProvider(ctx context.Context, endpoint, providerID string) (ProxyDTO, error) {
	return c.Proxies.Upsert(ctx, ProxyDTO{Endpoint: endpoint, ProviderID: providerID, Healthy: true})
}

// Upsert creates or updates a proxy. When p.ID is set the SDK PUTs to
// /v1/proxies/{id} and falls back to POST when the record does not exist
// yet; otherwise it POSTs to /v1/proxies.
func (a *proxiesAPI) Upsert(ctx context.Context, p ProxyDTO) (ProxyDTO, error) {
	var out ProxyDTO
	if p.ID != "" {
		err := a.c.do(ctx, requestSpec{
			method: "PUT",
			path:   "/v1/proxies/" + p.ID,
			body:   p,
			auth:   authAdmin,
			out:    &out,
		})
		if err == nil {
			return out, nil
		}
		if !IsNotFound(err) {
			return ProxyDTO{}, err
		}
	}
	err := a.c.do(ctx, requestSpec{
		method: "POST",
		path:   "/v1/proxies",
		body:   p,
		auth:   authAdmin,
		out:    &out,
	})
	return out, err
}

// Get fetches a proxy by ID.
func (a *proxiesAPI) Get(ctx context.Context, id string) (ProxyDTO, error) {
	var out ProxyDTO
	err := a.c.do(ctx, requestSpec{
		method: "GET",
		path:   "/v1/proxies/" + id,
		auth:   authAdmin,
		out:    &out,
	})
	return out, err
}

// Delete removes a proxy by ID.
func (a *proxiesAPI) Delete(ctx context.Context, id string) error {
	return a.c.do(ctx, requestSpec{
		method: "DELETE",
		path:   "/v1/proxies/" + id,
		auth:   authAdmin,
	})
}
