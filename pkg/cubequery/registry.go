// registry.go 让 Query → SQL 的编译器能看到**不止一个** model。
//
// 单表编译只需要一个 *cubeschema.Model;一旦支持 join,编译器就必须能查到
// "被 join 的那个 model"的 schema、源表名与 mapping —— 否则生成的 SQL 里
// 会出现没法解析的列名。
package cubequery

import (
	"sort"

	"github.com/YunBright/cube/pkg/cubeschema"
)

// ModelEntry 是 join 需要从一个 model 里取到的全部信息。
type ModelEntry struct {
	Schema *cubeschema.Model
	// SourceTable 是该 model 的物理表名。
	//   - live model:实例 config 的 table_*(透传源库,必须真实表名)
	//   - duck model :schema.SQLTable(DuckDB 侧表名)
	SourceTable string
	// Mapper 把该 model 的 canonical 列名解析成物理列名。
	Mapper ColumnResolver
	// LegacyTSQL 表示目标实例是 **SQL Server 2008 及更早**(compat level 80)。
	//
	// 这不是假想:hbposv7(hbposepro)实测就是 compat 80,那里
	// DATEFROMPARTS(2012+)报"不是可以识别的内置函数"、
	// OFFSET/FETCH(2012+)报"'OFFSET' 附近有语法错误"。
	// 而 ysx(hbposv10)是 2014,两者都支持。
	//
	// 同一个思迅产品线两个实例能力不同 —— 所以必须**探测**,不能假设。
	LegacyTSQL bool
}

// ModelRegistry 让编译器按名字查 model,以及列出全部 model。
//
// Names() 不是可选的:编译器要**反查**某个成员属于哪个 model(维度 `supplier.name`
// 里的 `supplier` 就是 model 名),没有它就无法把成员分派给各自的表。
type ModelRegistry interface {
	Get(name string) (ModelEntry, bool)
	Names() []string
}

// mapRegistry 是 ModelRegistry 的标准实现。
type mapRegistry struct {
	entries map[string]ModelEntry
	names   []string
}

func (r *mapRegistry) Get(name string) (ModelEntry, bool) {
	e, ok := r.entries[name]
	return e, ok
}

func (r *mapRegistry) Names() []string { return r.names }

// NewRegistry 由"名字 → ModelEntry"的 map 构造 registry。名字会排序,
// 保证编译出的 SQL 稳定。
func NewRegistry(m map[string]ModelEntry) ModelRegistry {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return &mapRegistry{entries: m, names: names}
}

// baseEntry 把当前正在编译的 model 作为 registry 里的唯一条目。
//
// 单表查询走这条路径 —— 它让 BuildTSQL 的既有行为一字不变(不加 JOIN、不加别名)。
func baseEntry(schema *cubeschema.Model, sourceTable string, mapper ColumnResolver) ModelRegistry {
	entries := map[string]ModelEntry{}
	if schema != nil {
		entries[schema.Name] = ModelEntry{
			Schema: schema, SourceTable: sourceTable, Mapper: mapper,
		}
	}
	return NewRegistry(entries)
}

// ownerOf 反查 `<model>.<ref>` 的归属:先按前缀找,前缀找不到就按 ref 在
// 全部 model 里找(容忍调用方写成不带前缀的裸 ref)。
//
// 两个 model 有同名 ref 时**必须靠前缀区分** —— 这也正是把 model 名写全的原因。
func ownerOf(reg ModelRegistry, qualified string, baseName string) (ModelEntry, string, bool) {
	if i := indexDotStr(qualified); i > 0 {
		if e, ok := reg.Get(qualified[:i]); ok && e.Schema != nil {
			return e, qualified[i+1:], true
		}
	}
	if e, ok := reg.Get(baseName); ok && e.Schema != nil {
		if _, ok := e.Schema.FindDimension(qualified); ok {
			return e, qualified, true
		}
		if _, ok := e.Schema.FindMeasure(qualified); ok {
			return e, qualified, true
		}
	}
	return ModelEntry{}, "", false
}

func indexDotStr(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}
