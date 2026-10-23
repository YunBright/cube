package main

// 对**实际会部署的那份 config.yaml** 的约束。
//
// # 为什么需要它
//
// config.yaml 不在版本库(.gitignore),而且 deploy-cube.ps1 会用**打包机工作区**
// 里那份覆盖服务器上的。后果是:代码里的默认值和约束单测全绿,但部署出去的配置
// 可能根本没配某个源表名 —— 此时该 model 会**静默不可查**
// (main.go 只打一行 Info 日志然后 continue,healthz 照样全绿)。
//
// 这类"配置文件与代码约定漂移"的问题,只有**直接断言配置文件本身**才拦得住。
//
// 文件不存在时 skip 而不是 fail:CI 与其他开发机上没有这份文件(含 DSN 口令),
// 在那里断言等于制造假红。

import (
	"os"
	"strings"
	"testing"

	"github.com/YunBright/cube/pkg/cubeschema"
	models "github.com/YunBright/cube/sixun-models"

	"github.com/YunBright/cube/semantic-layers/sixun/internal/appcfg"
)

func loadDeployedConfig(t *testing.T) appConfig {
	t.Helper()
	const path = "config.yaml"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s 不存在(不在版本库,只有打包机上有),跳过", path)
	}
	cfg, used, err := appcfg.LoadFrom[appConfig](path)
	if err != nil {
		t.Fatalf("加载 %s 失败: %v", path, err)
	}
	if used != path {
		// config.local.yaml 优先级更高,留在服务器上会盖住部署下去的版本
		// (deploy-cube.ps1 的 prune 步骤就是专门删它的)。
		t.Fatalf("实际生效的配置是 %s 而不是 %s —— 本次断言的不是会部署的那份", used, path)
	}
	return cfg
}

// TestDeployedConfigCoversEveryLiveModel 锁"每个 live model 都有源"。
//
// ⚠️ 2026-10-23 去掉本地副本之后,**全部 model 都是 live** ⇒ 源表名(config 的
// table_<model>)就是它们**唯一**的取数入口 —— 少一条,那个 model 就静默不可查。
//
// 症状特别难查:main.go 只打一行 "live model 缺 source.table_*,该 model 不可查询",
// 然后 continue ⇒ healthz 照样全绿、网关照样注册成功,
// 只是"查这个表"永远 404 MODEL_NOT_FOUND —— 而那看起来像"这个 source 没有该能力"。
//
// ⇒ 这条测试把"少配一条表名"从运行期的静默失败,提前到 CI 的红灯。
func TestDeployedConfigCoversEveryLiveModel(t *testing.T) {
	cfg := loadDeployedConfig(t)
	live, err := liveTables(&cfg)
	if err != nil {
		t.Fatalf("构造 live 表名失败: %v", err)
	}
	names, err := models.ModelNames()
	if err != nil {
		t.Fatalf("列 model 失败: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("一个 model 都没有 —— 嵌入式资产没加载上")
	}
	for _, name := range names {
		b, err := models.ReadSchema(name)
		if err != nil {
			t.Errorf("读 schema %q 失败: %v", name, err)
			continue
		}
		schema, err := cubeschema.Load(b)
		if err != nil {
			t.Errorf("解析 schema %q 失败: %v", name, err)
			continue
		}
		if schema.Storage != cubeschema.StorageLive {
			t.Errorf("model %q 不是 live model(storage=%s)—— 全仓去掉本地副本之后应当都是 live",
				name, schema.Storage)
			continue
		}
		if strings.TrimSpace(live[name]) == "" {
			t.Errorf("live model %q 在 config 里没有源(table_%s 缺)⇒ 它会**静默不可查**",
				name, name)
		}
	}
}

// TestDeployedConfigHasMemberFragmentTables 锁"会员片段要用的四张表都配了"。
//
// ⚠️ member 的 live 源是**片段**(余额 = 充值 − 储值支付,要 JOIN 三张流水表),
// 少配任何一张 ⇒ 片段构造直接报错 ⇒ 启动失败(这是好的失败方式:
// 显式报错,而不是"会员余额恒为 0")。
func TestDeployedConfigHasMemberFragmentTables(t *testing.T) {
	cfg := loadDeployedConfig(t)
	for name, v := range map[string]string{
		"table_member":            cfg.Source.TableMember,
		"table_member_consume":    cfg.Source.TableMemberConsume,
		"table_member_paylist":    cfg.Source.TableMemberPaylist,
		"table_member_savingplus": cfg.Source.TableMemberSavingPlus,
	} {
		if strings.TrimSpace(v) == "" {
			t.Errorf("会员 live 片段需要 %s,但配置里是空的", name)
		}
	}
	// 片段构造本身也要能过(表名会被拼进 SQL 文本 ⇒ 必须过标识符白名单)。
	if _, err := liveTables(&cfg); err != nil {
		t.Fatalf("构造 live 表名失败: %v", err)
	}
}
