package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
)

// dialWait is how long a call waits for the helper to listen (it starts
// alongside the agent, or is being restarted by an update).
const dialWait = 10 * time.Second

// Client is the agent's side: the same operations as Helper, carried out
// by the helper process.
type Client struct {
	path string

	mu   sync.Mutex
	peer *agentproto.Peer
	conn net.Conn
}

func NewClient(path string) *Client { return &Client{path: path} }

func (c *Client) HostIdentity(ctx context.Context) error {
	return c.call(ctx, MsgHostIdentity, struct{}{}, nil)
}

func (c *Client) EnsureSiteUser(ctx context.Context, id int64) (uid, gid int, err error) {
	var u SiteUser
	if err := c.call(ctx, MsgSiteUserEnsure, SiteRequest{SiteID: id}, &u); err != nil {
		return -1, -1, err
	}
	return u.UID, u.GID, nil
}

func (c *Client) DeleteSiteUser(ctx context.Context, id int64) error {
	return c.call(ctx, MsgSiteUserDelete, SiteRequest{SiteID: id}, nil)
}

func (c *Client) ApplySitePHP(ctx context.Context, s SitePHP) error {
	return c.call(ctx, MsgSitePHPApply, s, nil)
}

func (c *Client) RemoveSitePHP(ctx context.Context, id int64) error {
	return c.call(ctx, MsgSitePHPRemove, SiteRequest{SiteID: id}, nil)
}

func (c *Client) ApplyLaravel(ctx context.Context, s SitePHP, l php.Laravel) error {
	return c.call(ctx, MsgSiteLaravelApply, SiteLaravel{SitePHP: s, Laravel: l}, nil)
}

func (c *Client) LaravelProcesses(ctx context.Context, req agentproto.SiteLaravel) (agentproto.SiteLaravelResponse, error) {
	var resp agentproto.SiteLaravelResponse
	err := c.call(ctx, MsgSiteLaravel, req, &resp)
	return resp, err
}

func (c *Client) RestartUpdatedPHP(ctx context.Context) ([]int64, error) {
	var resp RestartedSites
	err := c.call(ctx, MsgPHPRestartUpdated, struct{}{}, &resp)
	return resp.SiteIDs, err
}

func (c *Client) WriteMariaDBConfig(ctx context.Context, cfg MariaDBConfig) error {
	return c.call(ctx, MsgMariaDBConfig, cfg, nil)
}

func (c *Client) RestartMariaDB(ctx context.Context) error {
	return c.call(ctx, MsgMariaDBRestart, struct{}{}, nil)
}

// Close drops the connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.peer, c.conn = nil, nil
	}
}

// call sends a request; a connection found broken (the helper restarted)
// is replaced and the request sent once more: every request is idempotent.
func (c *Client) call(ctx context.Context, msgType string, req, resp any) error {
	for attempt := 0; ; attempt++ {
		p, err := c.connect(ctx)
		if err == nil {
			err = p.Call(ctx, msgType, req, resp)
			if re := (*agentproto.RemoteError)(nil); errors.As(err, &re) {
				return errors.New(re.Msg)
			}
			if err == nil {
				return nil
			}
			c.drop(p)
		}
		if attempt > 0 || ctx.Err() != nil {
			return fmt.Errorf("privileged helper: %w", err)
		}
	}
}

func (c *Client) connect(ctx context.Context) (*agentproto.Peer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peer != nil {
		select {
		case <-c.peer.Done():
			c.conn.Close()
			c.peer, c.conn = nil, nil
		default:
			return c.peer, nil
		}
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	p := agentproto.NewPeer(conn, conn, nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("the agent serves no requests of the helper")
	})
	go p.Run(context.Background())
	c.peer, c.conn = p, conn
	return p, nil
}

// dial connects, waiting for a helper that is not listening yet.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialWait)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", c.path)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Client) drop(p *agentproto.Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peer == p {
		c.conn.Close()
		c.peer, c.conn = nil, nil
	}
}
