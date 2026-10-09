package main

import (
	"io/fs"
	"testing"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/embedcheck"
)

// TestEmbeddedAssetsAreComplete 锁住"编译进 sixun-hbposv7 二进制的 schema +
// mapping 是齐全的"。理由同 sixun-ysx 那份。
func TestEmbeddedAssetsAreComplete(t *testing.T) {
	mappingFS, err := fs.Sub(mappingAssets, "mapping")
	if err != nil {
		t.Fatalf("fs.Sub(mappingAssets, \"mapping\"): %v", err)
	}
	if err := embedcheck.Check(mappingFS); err != nil {
		t.Fatal(err)
	}
}
