package server

import (
	"errors"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestRandomHexFailsClosedWhenEntropyUnavailable(t *testing.T) {
	oldReader := cryptoRandReader
	cryptoRandReader = errReader{}
	t.Cleanup(func() { cryptoRandReader = oldReader })

	if got, err := randomHex(8); err == nil || got != "" {
		t.Fatalf("randomHex should fail closed, got %q err=%v", got, err)
	}
	if got, err := generateKey(); err == nil || got != "" {
		t.Fatalf("generateKey should fail closed, got %q err=%v", got, err)
	}
}
