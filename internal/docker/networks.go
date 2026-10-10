package docker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

type NetworkConfig struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver,omitempty"`
	// Internal networks have no route to the outside world.
	Internal bool              `json:"Internal,omitempty"`
	Labels   map[string]string `json:"Labels,omitempty"`
	Options  map[string]string `json:"Options,omitempty"`
}

type NetworkInfo struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
	IPAM struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// NetworkCreate creates a network and returns its ID.
func (c *Client) NetworkCreate(ctx context.Context, cfg NetworkConfig) (string, error) {
	if cfg.Driver == "" {
		cfg.Driver = "bridge"
	}
	var resp struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, http.MethodPost, "/networks/create", nil, cfg, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// NetworkInspect returns a network by name or ID; ErrNotFound if missing.
func (c *Client) NetworkInspect(ctx context.Context, idOrName string) (*NetworkInfo, error) {
	var info NetworkInfo
	if err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(idOrName), nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// NetworkRemove deletes a network; a missing one is not an error.
func (c *Client) NetworkRemove(ctx context.Context, idOrName string) error {
	err := c.call(ctx, http.MethodDelete, "/networks/"+url.PathEscape(idOrName), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// NetworkConnect attaches a container to a network.
func (c *Client) NetworkConnect(ctx context.Context, network, container string, ep *EndpointSettings) error {
	body := map[string]any{"Container": container}
	if ep != nil {
		body["EndpointConfig"] = ep
	}
	return c.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}

// NetworkDisconnect detaches a container from a network.
func (c *Client) NetworkDisconnect(ctx context.Context, network, container string, force bool) error {
	body := map[string]any{"Container": container, "Force": force}
	return c.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil, body, nil)
}
