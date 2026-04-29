package main

import (
	"context"
	"log/slog"
	"testing"

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
