// Package embedcheck 校验"编译进二进制的资产"本身是完整的。
//
// # 为什么需要它
//
// schema 与 mapping 都是 go:embed 进二进制的,好的一面是"推了二进制就一定
// 带了模型";坏的一面是**编译期完全看不出资产缺失**:
//
//   - schema 目录少一个模型 → 编译照样过,运行时该 model 静默不可查
//   - mapping 少一个模型 → main.go 里 loader.Get(name) 返回 false,只打一行
//     INFO 日志就 continue,启动**零报错**,healthz 全绿
//   - mapping 存在但内容为空 → 列名一个都没重写,查询能跑通但结果全是空值
//
// 这三种在生产上表现完全一样:"这个表查不到 / 查出来是空的",而排查第一步
// 永远是"是不是文件没推上去"。2026-10-09 那次 hbposv7 静默返回错数据,就是
// 这类缺口的直接后果(另一份 query 实现忽略 filters,调用方拿到 data[0])。
//
// 所以这里把"资产完整性"变成**编译不过 / 测试不过**,而不是运行期日志。
package embedcheck

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/fieldmapping"

	models "github.com/YunBright/cube/sixun-models"
)

// Check 校验内嵌 mapping 与内嵌 schema 双向齐全,且每个 mapping 非空。
//
// mappingFS 必须是 fs.Sub(..., "mapping") 之后的 FS(即以 mapping-*.yaml
// 为直接子文件的那一层)。
//
// 校验项:
//  1. 每个 model 都能读到 schema 且能解析(解析失败 ⇒ 报出**原因**);
//  2. 每个 schema 的 storage 都是 live(2026-10-23 去本地副本之后唯一的合法值);
//  3. 每个 model 都有 mapping(缺 → 该 model 静默不可查);
//  4. 每个 mapping 至少映射了一个字段(空 mapping = 列名一个没重写);
//  5. 反向:每个 mapping-*.yaml 都对应一个存在的 schema(防孤儿 mapping,
//     拼错 model 名会命中这条)。
func Check(mappingFS fs.FS) error {
	names, err := models.ModelNames()
	if err != nil {
		return fmt.Errorf("列出内嵌 schema: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("内嵌 schema 一个都没有 —— 检查 sixun-models 的 //go:embed 模式")
	}

	loader, err := fieldmapping.NewLoaderFS(mappingFS)
	if err != nil {
		return fmt.Errorf("加载内嵌 mapping: %w", err)
	}

	var problems []string

	schemaSet := make(map[string]bool, len(names))
	for _, name := range names {
		schemaSet[name] = true

		b, err := models.ReadSchema(name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("schema %s 读不到: %v", name, err))
			continue
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			problems = append(problems, fmt.Sprintf("schema %s 是空文件", name))
			continue
		}

		// ⚠️ 2026-10-23:schema 必须**解析得过**,而且 storage 必须是 live。
		//
		// 这条以前是**没有**的 ⇒ 有人写 `storage: duck` 时 schema **静默加载成功**,
		// 直到查询那一刻才炸 "model X is not a live model" —— 报错点离真正的原因
		// 隔了"加载 → 注册 → 编译 → 打源库"整条链路。
		//
		// 现在 cubeschema.Load() 会在**加载期**就拒掉非 live 的 storage,
		// 而这里是**第二道**:把这条约束钉进**永远会跑的**测试
		// (cmd/sixun-*/embed_test.go 两个 family 各跑一次),
		// 而不是只在"机器上有 config.yaml"时才生效的 config_test.go 里。
		schemaProblems := checkSchema(name, b)
		problems = append(problems, schemaProblems...)
		if len(schemaProblems) > 0 {
			continue
		}

		mp, ok := loader.Get(name)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"model %q 有 schema 但**没有 mapping** —— 该 model 会静默不可查"+
					"(main.go 只打一行 INFO 就跳过,启动不报错)", name))
			continue
		}
		if n := len(mp.TargetsWithType()); n == 0 {
			problems = append(problems, fmt.Sprintf(
				"model %q 的 mapping 是空的 —— 列名一个都不会被重写,"+
					"查询能跑通但结果是错的", name))
		}
	}

	// 反向:孤儿 mapping(model 名拼错时的典型症状)
	var orphans []string
	for _, m := range loader.Models() {
		if !schemaSet[m] {
			orphans = append(orphans, m)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		problems = append(problems, fmt.Sprintf(
			"有 mapping 但没有对应 schema(拼错了 model 名?): %s", strings.Join(orphans, ", ")))
	}

	if len(problems) > 0 {
		return fmt.Errorf("内嵌资产不完整:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// checkSchema 校验一个 schema 的字节:能解析 + storage 是 live。
//
// 独立成函数是为了**可测**:Check() 读的是**内嵌**资产(今天全是好的),
// 想证明"坏 schema 会被抓住"就得能喂坏的进来 —— 所以测试直接调它。
//
// 返回空切片 = 这个 schema 没问题。
func checkSchema(name string, b []byte) []string {
	schema, err := cubeschema.Load(b)
	if err != nil {
		return []string{fmt.Sprintf(
			"schema %s 不合法: %v —— **测试不过**,而不是运行期才炸", name, err)}
	}
	if schema.Storage != cubeschema.StorageLive {
		return []string{fmt.Sprintf(
			"model %q 的 storage=%q —— 全仓去掉本地副本之后**只有** live 可用",
			name, schema.Storage)}
	}
	return nil
}
