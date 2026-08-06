package redis

import (
	"context"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/storage"
	goredis "github.com/redis/go-redis/v9"
)

func init() {
	storage.Register("redis", func(url string) (storage.Store, error) {
		return New(url)
	})
}

var _ storage.Store = (*Client)(nil)

// Client implements storage.Store backed by Redis/Valkey.
type Client struct {
	rdb *goredis.Client
}

// New creates a Redis-backed Store from a connection URL.
func New(redisURL string) (*Client, error) {
	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	return &Client{rdb: goredis.NewClient(opts)}, nil
}

func (c *Client) Get(ctx context.Context, key string) (string, error) {
	result, err := c.rdb.Get(ctx, key).Result()
	if err == goredis.Nil {
		return "", nil
	}
	return result, err
}

func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *Client) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	return c.rdb.IncrBy(ctx, key, delta).Result()
}

func (c *Client) HashIncr(ctx context.Context, key, field string, delta int64) (int64, error) {
	return c.rdb.HIncrBy(ctx, key, field, delta).Result()
}

func (c *Client) HashGet(ctx context.Context, key string) (map[string]string, error) {
	result, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) HashSetNX(ctx context.Context, key, field, value string) (bool, error) {
	return c.rdb.HSetNX(ctx, key, field, value).Result()
}

func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, key, ttl).Err()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}
