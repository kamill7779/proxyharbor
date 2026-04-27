package proxyharbor

import "context"

// healthAPI exposes /healthz and /readyz.
type healthAPI struct{ c *Client }

// Ready checks the ProxyHarbor /readyz endpoint.
func (a *healthAPI) Ready(ctx context.Context) error {
	return a.c.do(ctx, requestSpec{
		method: "GET",
		path:   "/readyz",
		auth:   authNone,
	})
}

// Healthz checks the ProxyHarbor /healthz endpoint.
func (a *healthAPI) Healthz(ctx context.Context) error {
	return a.c.do(ctx, requestSpec{
		method: "GET",
		path:   "/healthz",
		auth:   authNone,
	})
}
