package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/config"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
)

func TestOpenSelectorUsesLocalWhenRedisIsOptional(t *testing.T) {
	sel, closeSelector, err := openSelector(context.Background(), config.Config{Selector: selector.NameZFair}, slog.Default())
	if err != nil {
		t.Fatalf("openSelector() error = %v", err)
	}
	defer closeSelector()
	if got := selector.Name(sel); got != selector.NameLocal {
		t.Fatalf("selector.Name() = %q, want local", got)
	}
}

func TestOpenSelectorFailsFastWhenRedisIsRequired(t *testing.T) {
	_, _, err := openSelector(context.Background(), config.Config{Selector: selector.NameZFair, SelectorRedisRequired: true}, slog.Default())
	if err == nil {
		t.Fatal("openSelector() error = nil, want redis required error")
	}
}

func TestOpenCacheFallsBackWhenRedisIsOptional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cacheImpl, closeCache, err := openCache(ctx, config.Config{RedisAddr: "127.0.0.1:1"}, logger)
	if err != nil {
		t.Fatalf("openCache() error = %v, want optional Redis fallback", err)
	}
	defer closeCache()
	if _, ok := cacheImpl.(cache.Noop); !ok {
		t.Fatalf("openCache() = %T, want cache.Noop", cacheImpl)
	}
}

func TestOpenCacheFailsFastWhenRedisIsRequired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, _, err := openCache(ctx, config.Config{RedisAddr: "127.0.0.1:1", SelectorRedisRequired: true}, logger)
	if err == nil {
		t.Fatal("openCache() error = nil, want required Redis error")
	}
}
