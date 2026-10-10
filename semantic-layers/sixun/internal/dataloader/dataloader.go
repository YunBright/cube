// Package dataloader 把 storage: duck 的 model 物化进 DuckDB,并负责定时重拉。
//
// 为什么抽成共用包:这段逻辑原本在 sixun-ysx/main.go 与 sixun-hbposv7/main.go
// 各写了一份。两份迟早会漂移(一个加了刷新、一个没加;一个漏了 mapping、
// 一个漏了),而这类漂移的症状是"某个门店的数据比另一个新",极难归因。
//
// storage: live 的 model 不在这里出现 —— 它们查询时透传源库,本来就没有快照。
//
// # 全量 vs 增量
//
// 实测(2026-10-10,两个库):
//
//	                 总行数     1天改动   7天改动   30天改动
//	t_bd_item_info   27304/44316    0        70/49      410
//
// 商品档案每天实际只改约 10 行,却每 30 分钟全量重拉 2.7~4.4 万行。
// 源库是 2008 R2 的生产思迅库,这个扫描成本不该白付。
//
// 所以:常规刷新走**增量**(只拉 modify_date >= 水位 的行,按主键 upsert),
// 夜间做一次**全量校准**兜底。增量天然会漏 —— 有些客户端写库时不更新
// modify_date,新建商品的 modify_date 也常为 NULL(实测 ysx 近 7 天新建
// 商品 80% 是 NULL)—— 全量校准正是为这些兜底。
//
// # 水位什么时候推进
//
// **只在数据成功落进 DuckDB 且预聚合重建成功之后**。写失败却推进水位
// = 那段变更永久丢失,而且没有任何报错。宁可下一轮重拉(upsert 幂等)。
//
// 新水位取的是**源库**时间且在**拉取之前**读(见 loadIncremental)。
package dataloader

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YunBright/cube/pkg/cubeschema"
	"github.com/YunBright/cube/pkg/duckdb"
	"github.com/YunBright/cube/pkg/fieldmapping"
	"github.com/YunBright/cube/pkg/log"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/source"
)

// stateTable 是水位表。
//
// 为什么放在 DuckDB 而不是进程内存 / 外部文件:
//  1. 跟着 .duckdb 文件走,部署重启、进程重启都不会丢水位 ——
//     水位丢了不会立刻出错,它只会**静默地**让下一轮从零开始拉全表。
//  2. 不引入第二个状态存储,也就没有"两个文件哪个更新"的判断题。
//
// watermark 是 VARCHAR 而不是 TIMESTAMP:水位全程在字符串域里传递
// (见 internal/source/incremental.go 关于时区的说明),这里保持一致,
// 免得读回来又变成 time.Time 再引入一次转换。
const stateTable = "_sync_state"

// Mode 是一次刷新的策略。
type Mode string

const (
	// ModeFull 全量重拉 + 重建表。夜间校准与首次 bootstrap 走它。
	ModeFull Mode = "full"
	// ModeIncremental 只拉水位之后的变更行,按主键 upsert。
	ModeIncremental Mode = "incremental"
)

// IncrementalSource 是增量刷新需要的源库能力。
//
// 与 source.Connector 的同名方法签名一致,所以 *ysx.Connector 与
// *hbposv7.Connector 直接满足它 —— 这里刻意**不重新声明一遍**接口,
// 避免出现"两边签名对不上、要改两个地方"的分叉。
type IncrementalSource interface {
	FetchSince(ctx context.Context, model string, cols source.IncrementalCols, since string) ([]map[string]any, error)
	SourceNow(ctx context.Context) (string, error)
}

// Recorder 接收每个 model 每次物化的结果。
//
// 用接口而不是直接依赖 internal/freshness,是为了让这个包不关心"谁在消费结果":
// 生产是 *freshness.Tracker(喂给 /healthz),测试可以是个假的收集器。
// 传 nil 表示不记录。
type Recorder interface {
	Record(model string, err error)
}

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

// Loader 封装一次刷新所需的全部依赖,并按 Mode 执行。
//
// 全量与增量共用同一份 schema / mapping / fetcher —— 分成两个入口的
// 后果就是"全量那条路修了、增量那条路没修",而症状要到真实数据上才出现。
type Loader struct {
	db      *duckdb.Engine
	schemas map[string]*Meta
	specs   []Spec

	// conn 提供增量拉取与源库时间。nil 表示本实例不支持增量,
	// 此时所有刷新都是全量(本地裸跑 / 单测常用)。
	conn IncrementalSource
	// incrCols 是 model 名 → 增量时间列(主列 + 可选回退列)。
	//
	// 用**源库列名**而不是 canonical 名,因为 WHERE 子句打在源库上;
	// canonical 名(DuckDB 侧)与源库列名在实测中恰好同名,但那只是巧合。
	// 回退列的语义见 source.IncrementalCols。
	incrCols map[string]source.IncrementalCols

	lg  *log.Logger
	rec Recorder

	// wmMu 串行化水位的读改写。
	//
	// 在途守卫(refreshschedule)已经保证同一进程里只有一次刷新在跑,
	// 但首次 bootstrap 与启动加载也可能并发,这里不假设调用方守规矩。
	wmMu sync.Mutex
}

// NewLoader 构造 Loader。incrCols 传 nil 等价于"全部走全量"。
func NewLoader(db *duckdb.Engine, schemas map[string]*Meta, specs []Spec,
	conn IncrementalSource, incrCols map[string]source.IncrementalCols, lg *log.Logger, rec Recorder) *Loader {
	return &Loader{
		db:       db,
		schemas:  schemas,
		specs:    specs,
		conn:     conn,
		incrCols: incrCols,
		lg:       lg,
		rec:      rec,
	}
}

// Run 按 mode 刷新所有 storage: duck 的 model,返回成功物化的 model 名。
//
// 失败只记日志不中断:一个 model 拉不到,不该让整个刷新作废 ——
// 其余 model 仍然可用,故障也留在日志里可见。
//
// **但"仍然可用"不等于"健康"**:所以每次结果都交给 rec 记录,
// /healthz 据此把 status 打成 degraded。失败不能既被容忍又被藏起来。
func (l *Loader) Run(ctx context.Context, mode Mode) []string {
	if err := l.ensureStateTable(ctx); err != nil {
		l.lg.Info("水位表初始化失败,本轮全部按全量处理(不会丢数据,只是慢)", "err", err.Error())
		mode = ModeFull
	}

	loaded := []string{}
	for _, s := range l.specs {
		meta, ok := l.schemas[s.Name]
		if !ok || meta == nil || meta.Live {
			l.lg.Info("skip: 非 duck model 不物化", "model", s.Name)
			continue
		}

		var err error
		if mode == ModeIncremental && l.canIncrement(s.Name, meta) {
			err = l.loadIncremental(ctx, meta, s)
		} else {
			if mode == ModeIncremental {
				l.lg.Info("本 model 走全量", "model", s.Name, "原因", l.noIncrementReason(s.Name, meta))
			}
			err = l.loadFull(ctx, meta, s)
		}

		if l.rec != nil {
			l.rec.Record(s.Name, err)
		}
		if err != nil {
			l.lg.Info("load failed", "model", s.Name, "mode", string(mode), "err", err.Error())
			continue
		}
		loaded = append(loaded, s.Name)
	}
	return loaded
}

// canIncrement 判断某 model 本轮能否走增量。
//
// 三个条件缺一不可,任何一条不满足都必须回落到全量 ——
// **绝不允许"没有主键也照样 upsert"**,那会产生不可控的行重复。
func (l *Loader) canIncrement(model string, meta *Meta) bool {
	return l.noIncrementReason(model, meta) == ""
}

// incrColsFor 返回某 model 的增量列配置,以及它是否真的配了主列。
//
// 只看 map 里有没有这个 key 不够 —— 配置可能写了这个 model 却是空字符串
// (例如 incremental_fallback 配了但 incremental 没配),那样生成的 SQL
// 会变成 `WHERE [] >= ...`,直接语法错误。
func (l *Loader) incrColsFor(model string) (source.IncrementalCols, bool) {
	c, ok := l.incrCols[model]
	if !ok || strings.TrimSpace(c.Primary) == "" {
		return source.IncrementalCols{}, false
	}
	return c, true
}

// hasPrimaryCol 是 incrColsFor 的 bool 版,用在只需要判断的地方。
func hasPrimaryCol(cols map[string]source.IncrementalCols, model string) bool {
	c, ok := cols[model]
	return ok && strings.TrimSpace(c.Primary) != ""
}

func (l *Loader) noIncrementReason(model string, meta *Meta) string {
	switch {
	case l.conn == nil:
		return "本实例未接入增量源"
	case !hasPrimaryCol(l.incrCols, model):
		return "未配置增量列(refresh.incremental." + model + ")"
	case meta.Schema == nil:
		return "schema 未装配"
	default:
		if _, ok := meta.Schema.PrimaryKeyDimension(); !ok {
			return "schema 未声明 primary_key,无法安全 upsert"
		}
	}
	return ""
}

// loadFull 全量重拉:重建 _raw 表 + 重建预聚合表。
//
// ⚠️ 空结果会把表清空,这是**有意的**:上次拉到 3000 行、这次拉到 0 行,
// 若继续用旧数据就是把"数据变空"这件事藏起来。宁可让查询返回 0 行。
// (增量的空结果语义相反,见 loadIncremental。)
func (l *Loader) loadFull(ctx context.Context, meta *Meta, s Spec) error {
	raw, err := s.Fetch(ctx)
	if err != nil {
		return err
	}
	mapped := mapRows(raw, meta)

	rawTable := s.Name + "_raw"
	if err := l.db.LoadFrom(rawTable, meta.Columns, toRows(mapped, meta.Columns)); err != nil {
		return err
	}
	if err := s.Build(ctx, l.db, "SELECT * FROM "+rawTable); err != nil {
		return err
	}
	l.lg.Info("loaded to DuckDB", "model", s.Name, "mode", "full", "rows", len(mapped))

	// 全量已经把当前这一刻之前的数据全量覆盖了,所以水位必须跟着推进。
	// 不推进的话:夜里全量校准之后,紧接着的增量会从很旧的水位开始拉,
	// 于是"为了省一次全量"反而拉了整整一个周期的全量数据量。
	if l.conn != nil && hasPrimaryCol(l.incrCols, s.Name) {
		if err := l.advanceWatermark(ctx, s.Name); err != nil {
			// 水位推进失败**不影响本轮数据**:数据已经是全量最新的,
			// 只是下一轮增量会重复拉(upsert 幂等,只是浪费)。
			l.lg.Info("全量成功但水位推进失败,下轮增量会重复拉(幂等,不影响数据)",
				"model", s.Name, "err", err.Error())
		}
	}
	return nil
}

// loadIncremental 增量拉取 + 按主键 upsert。
func (l *Loader) loadIncremental(ctx context.Context, meta *Meta, s Spec) error {
	wm, err := l.readWatermark(ctx, s.Name)
	if err != nil {
		return err
	}
	if wm == "" {
		// 没有水位 = 从没成功同步过 = 没有基线,增量无从谈起。
		// 这时直接全量,而不是"从零开始增量"(那会只拉到 1970 年之后的
		// 恰好全部数据 —— 巧合地正确,但依赖于所有人的时间戳都在 1970 之后)。
		l.lg.Info("无水位,先做一次全量 bootstrap", "model", s.Name)
		return l.loadFull(ctx, meta, s)
	}

	// ⚠️ 新水位必须在**拉取之前**从源库读。
	//
	// 反过来做(先拉后读时间)会把"拉取耗时期间产生的变更"也算进已处理区间,
	// 而那些变更的 modify_date 可能小于我们随后读到的时间 → 永久丢失。
	// 先读时间再拉,那些变更落在 [上次水位, 本次时间) 之外,下轮一定会被拉到。
	newWM, err := l.conn.SourceNow(ctx)
	if err != nil {
		return fmt.Errorf("读源库时间失败(增量水位无法确定,本轮不做): %w", err)
	}

	cols, _ := l.incrColsFor(s.Name)
	rows, err := l.conn.FetchSince(ctx, s.Name, cols, wm)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// **空结果不动表**,与全量的空结果语义相反。
		// 这里只说明"这个时间窗内没有变更",清表就是纯粹的数据丢失。
		// 水位照常推进 —— "确认过没有变更"本身就是要记住的信息。
		if err := l.writeWatermark(ctx, s.Name, newWM, ModeIncremental); err != nil {
			return err
		}
		l.lg.Info("增量窗口内无变更", "model", s.Name, "since", wm, "rows", 0)
		return nil
	}

	mapped := mapRows(rows, meta)
	pkDim, _ := meta.Schema.PrimaryKeyDimension()
	rawTable := s.Name + "_raw"
	touched, err := l.db.UpsertInto(rawTable, pkDim.Name, meta.Columns, toRows(mapped, meta.Columns))
	if err != nil {
		return err
	}
	if err := s.Build(ctx, l.db, "SELECT * FROM "+rawTable); err != nil {
		// 数据已经 upsert 进去了,但水位不推进 → 下轮重拉同一批(幂等)。
		// 这是刻意选的失败方向:重复 vs 丢失,重复便宜得多。
		return fmt.Errorf("增量写入后重建预聚合失败(水位不推进,下轮重拉): %w", err)
	}
	if err := l.writeWatermark(ctx, s.Name, newWM, ModeIncremental); err != nil {
		return err
	}

	l.lg.Info("loaded to DuckDB", "model", s.Name, "mode", "incremental",
		"since", wm, "rows", len(mapped), "upserted", touched, "watermark", newWM)
	return nil
}

// SetWatermark 强制设置某个 model 的增量水位,返回原水位。
//
// 这不是给测试用的后门,而是运维需要的操作:
//   - 「把水位回拨 3 天重放一次」—— 用于怀疑某段时间变更没进来时;
//   - 「清掉水位强制走一次全量」—— 用于水位表与 _raw 表怀疑不同步时。
//
// 它**不**碰 DuckDB 里的数据,只改水位表,所以是安全的、可逆的。
// 空字符串表示"清掉水位",下一轮增量会先做全量 bootstrap。
func (l *Loader) SetWatermark(ctx context.Context, model, wm string) (string, error) {
	l.wmMu.Lock()
	old, err := l.readWatermarkLocked(model)
	l.wmMu.Unlock()
	if err != nil {
		return "", err
	}
	if wm == "" {
		if err := l.db.Exec("DELETE FROM "+stateTable+" WHERE model = ?", model); err != nil {
			return old, err
		}
		return old, nil
	}
	if err := l.writeWatermark(ctx, model, wm, ModeIncremental); err != nil {
		return old, err
	}
	return old, nil
}

// Watermark 返回某个 model 当前的水位(空串 = 没有水位)。
func (l *Loader) Watermark(ctx context.Context, model string) (string, error) {
	l.wmMu.Lock()
	defer l.wmMu.Unlock()
	return l.readWatermarkLocked(model)
}

// ---- 水位表 ----

func (l *Loader) ensureStateTable(ctx context.Context) error {
	l.wmMu.Lock()
	defer l.wmMu.Unlock()
	return l.db.Exec("CREATE TABLE IF NOT EXISTS " + stateTable +
		" (model VARCHAR PRIMARY KEY, watermark VARCHAR NOT NULL, last_mode VARCHAR, updated_at VARCHAR)")
}

func (l *Loader) readWatermark(ctx context.Context, model string) (string, error) {
	l.wmMu.Lock()
	defer l.wmMu.Unlock()
	return l.readWatermarkLocked(model)
}

// readWatermarkLocked 读水位;调用方必须已持有 wmMu。
func (l *Loader) readWatermarkLocked(model string) (string, error) {
	rows, err := l.db.Query("SELECT watermark FROM "+stateTable+" WHERE model = ?", model)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", nil // 没有记录 = 没有水位
	}
	var wm string
	if err := rows.Scan(&wm); err != nil {
		return "", err
	}
	return wm, nil
}

func (l *Loader) writeWatermark(ctx context.Context, model, wm string, mode Mode) error {
	l.wmMu.Lock()
	defer l.wmMu.Unlock()
	now := sourceNowLocal()
	// DELETE + INSERT 而不是 INSERT OR REPLACE:后者在部分 DuckDB 版本上
	// 对带 PRIMARY KEY 的表行为不稳定,而这里是刷新路径,必须确定。
	if err := l.db.Exec("DELETE FROM "+stateTable+" WHERE model = ?", model); err != nil {
		return err
	}
	return l.db.Exec("INSERT INTO "+stateTable+" (model, watermark, last_mode, updated_at) VALUES (?, ?, ?, ?)",
		model, wm, string(mode), now)
}

// advanceWatermark 把水位推到源库当前时间(全量成功后用)。
func (l *Loader) advanceWatermark(ctx context.Context, model string) error {
	if l.conn == nil {
		return nil
	}
	now, err := l.conn.SourceNow(ctx)
	if err != nil {
		return err
	}
	return l.writeWatermark(ctx, model, now, ModeFull)
}

// sourceNowLocal 只用于水位表的 updated_at 审计列。
//
// 这里是**唯一**允许用本地时钟的地方,且它只是给人看的记录时间,
// 不参与任何数据取舍 —— 真正的水位一律来自源库。
func sourceNowLocal() string {
	return time.Now().Format("2006-01-02 15:04:05.000")
}

// ---- helpers ----

// mapRows 把源库行经 mapping 收敛成 canonical 行。
func mapRows(raw []map[string]any, meta *Meta) []map[string]any {
	mapped := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		mr, err := meta.Mapper.Apply(row)
		if err != nil {
			continue
		}
		mapped = append(mapped, mr)
	}
	return mapped
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
