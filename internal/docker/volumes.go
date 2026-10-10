package docker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

type Volume struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// VolumeCreate creates a named local volume; an existing one is returned
// unchanged.
func (c *Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Driver": "local", "Labels": labels}
	return c.call(ctx, http.MethodPost, "/volumes/create", nil, body, nil)
}

// VolumeList returns volumes carrying every given label.
func (c *Client) VolumeList(ctx context.Context, labels map[string]string) ([]Volume, error) {
	q := url.Values{}
	if len(labels) > 0 {
		q.Set("filters", labelFilter(labels))
	}
	var resp struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.call(ctx, http.MethodGet, "/volumes", q, nil, &resp)
	return resp.Volumes, err
}

// VolumeRemove deletes a volume and its data; a missing one is not an error.
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// VolumeInspect returns a volume; ErrNotFound if it does not exist.
func (c *Client) VolumeInspect(ctx context.Context, name string) (*Volume, error) {
	var v Volume
	if err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
