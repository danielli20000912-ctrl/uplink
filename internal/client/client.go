// Package client is a consumer-side client for an uplink publisher.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"uplink/internal/protocol"
)

type Client struct {
	base string
	key  string
	http *http.Client
	disc *protocol.Discovery
}

func New(baseURL, key string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: key, http: &http.Client{Timeout: 30 * time.Second}}
}

// Error is a non-200 response from the publisher.
type Error struct {
	Status int
	Body   protocol.ErrorBody
}

func (e *Error) Error() string {
	return fmt.Sprintf("uplink: HTTP %d %s: %s", e.Status, e.Body.Error.Code, e.Body.Error.Message)
}

func (c *Client) get(ctx context.Context, path string, auth bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, &e.Body)
		return e
	}
	return json.Unmarshal(raw, out)
}

// Discovery fetches and caches the discovery document.
func (c *Client) Discovery(ctx context.Context) (*protocol.Discovery, error) {
	if c.disc != nil {
		return c.disc, nil
	}
	var d protocol.Discovery
	if err := c.get(ctx, "/.well-known/uplink.json", false, &d); err != nil {
		return nil, err
	}
	if d.Protocol != "uplink" {
		return nil, fmt.Errorf("uplink: %s does not publish uplink", c.base)
	}
	c.disc = &d
	return &d, nil
}

func (c *Client) endpoint(ctx context.Context, name string) (string, error) {
	d, err := c.Discovery(ctx)
	if err != nil {
		return "", err
	}
	p, ok := d.Endpoints[name]
	if !ok {
		return "", fmt.Errorf("uplink: publisher has no %s endpoint", name)
	}
	return p, nil
}

func (c *Client) Catalog(ctx context.Context) (*protocol.Catalog, error) {
	p, err := c.endpoint(ctx, "catalog")
	if err != nil {
		return nil, err
	}
	var cat protocol.Catalog
	return &cat, c.get(ctx, p, true, &cat)
}

func (c *Client) Account(ctx context.Context) (*protocol.Account, error) {
	p, err := c.endpoint(ctx, "account")
	if err != nil {
		return nil, err
	}
	var a protocol.Account
	return &a, c.get(ctx, p, true, &a)
}

func (c *Client) Status(ctx context.Context) (*protocol.Status, error) {
	p, err := c.endpoint(ctx, "status")
	if err != nil {
		return nil, err
	}
	var s protocol.Status
	return &s, c.get(ctx, p, true, &s)
}

// Usage returns every settled record with since <= created_at < until,
// splitting the range into 31-day windows and following cursors.
func (c *Client) Usage(ctx context.Context, since, until time.Time) ([]protocol.Record, error) {
	p, err := c.endpoint(ctx, "usage")
	if err != nil {
		return nil, err
	}
	var out []protocol.Record
	for from := since; from.Before(until); {
		to := from.Add(31 * 24 * time.Hour)
		if to.After(until) {
			to = until
		}
		cursor := ""
		for {
			q := url.Values{}
			q.Set("since", from.UTC().Format(time.RFC3339))
			q.Set("until", to.UTC().Format(time.RFC3339))
			q.Set("limit", "1000")
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var page protocol.UsagePage
			if err := c.get(ctx, p+"?"+q.Encode(), true, &page); err != nil {
				return nil, err
			}
			out = append(out, page.Records...)
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		from = to
	}
	return out, nil
}
