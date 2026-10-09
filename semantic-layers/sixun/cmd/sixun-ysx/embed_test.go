package main

import (
	"io/fs"
	"testing"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/embedcheck"
)

// TestEmbeddedAssetsAreComplete 锁住"编译进 sixun-ysx 二进制的 schema +
// mapping 是齐全的"。
//
// 这条测试存在的理由:asset 缺失在编译期完全看不出来,而运行期症状是
// "某个 model 静默查不了 / 查出空值",启动零报错。把它变成测试失败,
// 就不必等到生产现场才发现。
func TestEmbeddedAssetsAreComplete(t *testing.T) {
	mappingFS, err := fs.Sub(mappingAssets, "mapping")
	if err != nil {
		t.Fatalf("fs.Sub(mappingAssets, \"mapping\"): %v", err)
	}
	if err := embedcheck.Check(mappingFS); err != nil {
		t.Fatal(err)
	}
}
