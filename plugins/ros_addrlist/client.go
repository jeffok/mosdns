package ros_addrlist

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-routeros/routeros/v3"
	"go.uber.org/zap"
)

// rosClient 封装 go-routeros 长连接，自动断线重连。
type rosClient struct {
	addr      string
	user      string
	pass      string
	list      string
	timeout   string
	comment   string
	dialTO    time.Duration
	writeTO   time.Duration
	reconnDly time.Duration
	logger    *zap.Logger

	mu     sync.Mutex
	client *routeros.Client
}

func newROSClient(a *Args, logger *zap.Logger) (*rosClient, error) {
	dialTO, err := time.ParseDuration(a.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial_timeout: %w", err)
	}
	writeTO, err := time.ParseDuration(a.WriteTimeout)
	if err != nil {
		return nil, fmt.Errorf("write_timeout: %w", err)
	}
	reconnDly, err := time.ParseDuration(a.ReconnDelay)
	if err != nil {
		return nil, fmt.Errorf("reconn_delay: %w", err)
	}
	return &rosClient{
		addr:      a.Address,
		user:      a.Username,
		pass:      a.Password,
		list:      a.ListName,
		timeout:   a.Timeout,
		comment:   a.Comment,
		dialTO:    dialTO,
		writeTO:   writeTO,
		reconnDly: reconnDly,
		logger:    logger,
	}, nil
}

func (c *rosClient) get(ctx context.Context) (*routeros.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	connCtx, cancel := context.WithTimeout(ctx, c.dialTO)
	defer cancel()
	cl, err := routeros.DialContext(connCtx, c.addr, c.user, c.pass)
	if err != nil {
		c.logger.Warn("ros_addrlist dial err", zap.String("addr", c.addr), zap.Error(err))
		return nil, err
	}
	c.logger.Debug("ros_addrlist api connected", zap.String("addr", c.addr), zap.String("user", c.user))
	c.client = cl
	return cl, nil
}

func (c *rosClient) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
}

// Add 把 IP 写入 address-list。失败时会重连一次再试。
func (c *rosClient) Add(ctx context.Context, ip string) error {
	if err := c.tryAdd(ctx, ip); err != nil {
		// 连接可能断了，重连重试一次
		c.drop()
		select {
		case <-time.After(c.reconnDly):
		case <-ctx.Done():
			return ctx.Err()
		}
		return c.tryAdd(ctx, ip)
	}
	return nil
}

func (c *rosClient) tryAdd(ctx context.Context, ip string) error {
	cl, err := c.get(ctx)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, c.writeTO)
	defer cancel()
	_, err = cl.RunArgsContext(wctx, []string{
		"/ip/firewall/address-list/add",
		"=list=" + c.list,
		"=address=" + ip,
		"=timeout=" + c.timeout,
		"=comment=" + c.comment,
	})
	if err != nil {
		c.logger.Warn("ros_addrlist add err", zap.String("ip", ip), zap.Error(err))
	} else {
		c.logger.Debug("ros_addrlist add ok", zap.String("ip", ip))
	}
	return err
}

func (c *rosClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
}
