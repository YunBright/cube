// Package dataloader 把 storage: duck 的 model 物化进 DuckDB,并负责定时重拉。
//
// 为什么抽成共用包:这段逻辑原本在 sixun-ysx/main.go 与 sixun-hbposv7/main.go
// 各写了一份。两份迟早会漂移(一个加了刷新、一个没加;一个漏了 mapping、
// 一个漏了),而这类漂移的症状是"某个门店的数据比另一个新",极难归因。
//
// storage: live 的 model 不在这里出现 —— 它们查询时透传源库,本来就没有快照。
package dataloader

import (
	"context"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"
)

// Meta 是一个 model 的装配元信息(原 main.go 里的 schemaMeta)。
type Meta struct {
	Schema  *cubeschema.Model
	Columns []fieldmapping.FieldDef
	// Mapper 把 schema 的 canonical 列名反查成源库列名(live 透传时用)。
	Mapper *fieldmapping.Mapper
	// Live 为真表示该 model 不落盘,查询时透传源库。
	Live bool
	// SourceTable 是 live model 的源库表名,来自实例 config。
	SourceTable string
}

// Spec 描述一个要物化的 duck model。
type Spec struct {
	Name  string
	Fetch func(context.Context) ([]map[string]any, error)
	Build func(context.Context, *duckdb.Engine, string) error
}

// Recorder 接收每个 model 每次物化的结果。
//
// 用接口而不是直接依赖 internal/freshness,是为了让这个包不关心"谁在消费结果":
// 生产是 *freshness.Tracker(喂给 /healthz),测试可以是个假的收集器。
// 传 nil 表示不记录。
type Recorder interface {
	Record(model string, err error)
}

// Load 把所有 storage: duck 的 model 物化进 DuckDB,返回成功物化的 model 名。
//
// 失败只记日志不中断:一个 model 拉不到,不该让整个实例起不来 ——
// 其余 model 仍然可用,故障也留在日志里可见。
//
// **但"仍然可用"不等于"健康"**:所以每次结果都交给 rec 记录,
// /healthz 据此把 status 打成 degraded。失败不能既被容忍又被藏起来。
func Load(ctx context.Context, db *duckdb.Engine, schemas map[string]*Meta, specs []Spec, lg *log.Logger, rec Recorder) []string {
	loaded := []string{}
	for _, s := range specs {
		meta, ok := schemas[s.Name]
		if !ok || meta == nil || meta.Live {
			lg.Info("skip: 非 duck model 不物化", "model", s.Name)
			continue
		}
		err := loadOne(ctx, db, meta, s, lg)
		if rec != nil {
			rec.Record(s.Name, err)
		}
		if err != nil {
			lg.Info("load failed", "model", s.Name, "err", err.Error())
			continue
		}
		loaded = append(loaded, s.Name)
	}
	return loaded
}

func loadOne(ctx context.Context, db *duckdb.Engine, meta *Meta, s Spec, lg *log.Logger) error {
	raw, err := s.Fetch(ctx)
	if err != nil {
		return err
	}
	mapped := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		mr, err := meta.Mapper.Apply(row)
		if err != nil {
			continue
		}
		mapped = append(mapped, mr)
	}
	// ⚠️ 不要因为 mapped 为空就跳过 BuildPreAgg:那会让"上一次拉到 3000 行、
	// 这次拉到 0 行"退化成"继续用旧的 3000 行",并把数据变旧这件事藏起来。
	// 宁可让预聚合表变成空的(查询返回 0 行,调用方看得见)。
	rawTable := s.Name + "_raw"
	if err := db.LoadFrom(rawTable, meta.Columns, toRows(mapped, meta.Columns)); err != nil {
		return err
	}
	if err := s.Build(ctx, db, "SELECT * FROM "+rawTable); err != nil {
		return err
	}
	lg.Info("loaded to DuckDB", "model", s.Name, "rows", len(mapped))
	return nil
}

// toRows 把 map 形态的行转成按列序排列的切片(DuckDB LoadFrom 要列序)。
func toRows(mapped []map[string]any, cols []fieldmapping.FieldDef) [][]any {
	out := make([][]any, 0, len(mapped))
	for _, row := range mapped {
		one := make([]any, 0, len(cols))
		for _, c := range cols {
			one = append(one, row[c.Target])
		}
		out = append(out, one)
	}
	return out
}
