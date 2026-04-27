package proxyharbor

import "context"

// providersAPI exposes the lower-level provider HTTP API.
type providersAPI struct{ c *Client }

// AddProvider creates a provider with just an ID.
//
//	c.AddProvider(ctx, "my-dc")
func (c *Client) AddProvider(ctx context.Context, id string) (ProviderDTO, error) {
	return c.Providers.Create(ctx, ProviderDTO{ID: id})
}

// Create registers a new provider.
func (a *providersAPI) Create(ctx context.Context, p ProviderDTO) (ProviderDTO, error) {
	var out ProviderDTO
	err := a.c.do(ctx, requestSpec{
		method: "POST",
		path:   "/v1/providers",
		body:   p,
		auth:   authAdmin,
		out:    &out,
	})
	return out, err
}

// Get returns a provider by ID.
func (a *providersAPI) Get(ctx context.Context, id string) (ProviderDTO, error) {
	var out ProviderDTO
	err := a.c.do(ctx, requestSpec{
		method: "GET",
		path:   "/v1/providers/" + id,
		auth:   authAdmin,
		out:    &out,
	})
	return out, err
}

// Update upserts a provider by ID.
func (a *providersAPI) Update(ctx context.Context, id string, p ProviderDTO) (ProviderDTO, error) {
	var out ProviderDTO
	err := a.c.do(ctx, requestSpec{
		method: "PUT",
		path:   "/v1/providers/" + id,
		body:   p,
		auth:   authAdmin,
		out:    &out,
	})
	return out, err
}

// Delete removes a provider by ID.
func (a *providersAPI) Delete(ctx context.Context, id string) error {
	return a.c.do(ctx, requestSpec{
		method: "DELETE",
		path:   "/v1/providers/" + id,
		auth:   authAdmin,
	})
}
