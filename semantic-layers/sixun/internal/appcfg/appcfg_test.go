package appcfg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolve_LocalWins 锁住优先级:config.local.yaml 存在时必须压过 config.yaml。
//
// 这条规则的**反面**危险更大 —— 如果哪天 app 在服务器上读到了一份遗留的
// config.local.yaml,就会盖住刚部署下去的 config.yaml,表现为
// "部署改了配置但没生效"且没有任何报错。所以它必须被测试钉住。
func TestResolve_LocalWins(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, SharedFile), "version: from-shared\n")
	write(t, filepath.Join(dir, LocalFile), "version: from-local\n")

	if got, want := Resolve(dir), filepath.Join(dir, LocalFile); got != want {
		t.Fatalf("Resolve 应优先返回 %s,实际 %s", want, got)
	}
}

// TestResolve_FallsBackToShared 只有 config.yaml 时必须回落到它,
// 且 Resolve 不要求文件真的存在(存在性由读取动作报错)。
func TestResolve_FallsBackToShared(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, SharedFile), "version: from-shared\n")

	if got, want := Resolve(dir), filepath.Join(dir, SharedFile); got != want {
		t.Fatalf("Resolve 应返回 %s,实际 %s", want, got)
	}

	empty := t.TempDir()
	if got, want := Resolve(empty), filepath.Join(empty, SharedFile); got != want {
		t.Fatalf("目录为空时应返回 %s,实际 %s", want, got)
	}
}

// TestResolve_IgnoresDirectoryNamedLocal 目录名同名不算数,
// 否则有人 mkdir config.local.yaml 就会把覆盖逻辑静默关掉。
func TestResolve_IgnoresDirectoryNamedLocal(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, SharedFile), "version: from-shared\n")
	if err := os.Mkdir(filepath.Join(dir, LocalFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := Resolve(dir), filepath.Join(dir, SharedFile); got != want {
		t.Fatalf("同名目录不应被当作覆盖文件,应返回 %s,实际 %s", want, got)
	}
}

// TestLoad_ReportsUsedPath 保证调用方拿得到"实际读了哪份文件"。
//
// app 的启动日志要靠这个字段暴露用的是哪份配置;字段没了就等于把上面那条
// 静默覆盖风险重新变成不可见,所以这里把它当契约测。
func TestLoad_ReportsUsedPath(t *testing.T) {
	type cfg struct {
		Version string `yaml:"version"`
	}

	dir := t.TempDir()
	write(t, filepath.Join(dir, SharedFile), "version: shared\n")
	got, used, err := Load[cfg](dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "shared" || used != filepath.Join(dir, SharedFile) {
		t.Fatalf("只有 config.yaml 时应读到 shared 且路径正确,实际 version=%q used=%q", got.Version, used)
	}

	write(t, filepath.Join(dir, LocalFile), "version: local\n")
	got, used, err = Load[cfg](dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "local" || used != filepath.Join(dir, LocalFile) {
		t.Fatalf("加了 config.local.yaml 后应读到 local 且路径正确,实际 version=%q used=%q", got.Version, used)
	}
}

// TestLoad_MissingFileIsAnError 缺文件必须**报错**并带上路径,
// 不能静默返回一个零值配置 —— 那会让 app 拿着空 DSN 去连库。
func TestLoad_MissingFileIsAnError(t *testing.T) {
	type cfg struct{}
	dir := t.TempDir()
	_, used, err := Load[cfg](dir)
	if err == nil {
		t.Fatalf("目录里没有任何配置时应报错,实际返回 used=%q", used)
	}
	if want := filepath.Join(dir, SharedFile); used != want {
		t.Fatalf("报错时应带上尝试的路径 %s,实际 %s", want, used)
	}
}

// TestLoadFrom_BadYAMLIsAnError 解析失败也要报错并带路径,
// 不能"读到了文件就算成功"。
func TestLoadFrom_BadYAMLIsAnError(t *testing.T) {
	type cfg struct {
		Version string `yaml:"version"`
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.yaml")
	write(t, bad, "version: [unclosed\n")
	if _, _, err := LoadFrom[cfg](bad); err == nil {
		t.Fatal("非法 YAML 应报错")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
