package mcpserv

// 工具定义与端点可见性(impl §7.1/§7.2):工具可见性由端点决定,
// 这是备模式的权限控制;委派主模式下侦察兵连 main,禁令走简报纪律 + 活跃 job 校验。

// scoutTools 是 /mcp/scout/<job> 端点可见的工具(无 investigate 防套娃、
// 无 record_change——侦察兵不改码,铁律二)。
var scoutTools = map[string]bool{
	"kb_map": true, "kb_recall": true, "kb_remember": true,
	"kb_task": true, "kb_flow": true, "kb_submit_findings": true,
}

func toolVisible(role, name string) bool {
	if _, ok := allTools[name]; !ok {
		return false
	}
	if role == "scout" {
		return scoutTools[name]
	}
	return true
}

func toolDefs(role string) []any {
	var out []any
	for _, name := range toolOrder {
		if toolVisible(role, name) {
			out = append(out, allTools[name])
		}
	}
	return out
}

var toolOrder = []string{
	"kb_init", "kb_status", "kb_map", "kb_recall", "kb_remember", "kb_record_change",
	"kb_verify", "kb_task", "kb_investigate", "kb_submit_findings", "kb_adopt",
	"kb_flow", "kb_maintain",
}

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(desc string) map[string]any   { return map[string]any{"type": "string", "description": desc} }
func boolp(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func intp(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func arr(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": desc}
}

var allTools = map[string]any{
	"kb_init": map[string]any{
		"name":        "kb_init",
		"description": "骨架建立/对账(幂等):扫库建节点、StructHash 精确迁移、失配降 suspect、标孤儿。新仓库自助初始化用它。",
		"inputSchema": obj(map[string]any{
			"force":        boolp("对丢失/受损分片强制重写(不动已有知识)"),
			"reanchor_all": boolp("mass-suspect 批量出口:人工确认全局性变更(如全库 gofmt)后,全库重锚、suspect 升回 fresh"),
		}),
	},
	"kb_status": map[string]any{
		"name":        "kb_status",
		"description": "库状态:覆盖率、suspect/孤儿/冲突分片、使用日志汇总、活跃任务、维护欠账。",
		"inputSchema": obj(map[string]any{}),
	},
	"kb_map": map[string]any{
		"name":        "kb_map",
		"description": "金字塔分支摘要视图(项目→目录→文件→符号,逐层下钻)。定位『什么在哪』的第一步,不要盲目 grep。",
		"inputSchema": obj(map[string]any{
			"path":  str("分支路径(相对仓库根,正斜杠;缺省根)"),
			"depth": intp("下钻层数,默认 2"),
		}),
	},
	"kb_recall": map[string]any{
		"name":        "kb_recall",
		"description": "查知识:mode=usage(怎么用:快照+契约+坑)/ history(为什么长这样:快照+来时路+决策链,改代码前必查)/ flow(流程视图)。query 可为节点 ID(file.go#Symbol)或关键词。",
		"inputSchema": obj(map[string]any{
			"query":  str("节点 ID 或关键词(中英文均可)"),
			"mode":   map[string]any{"type": "string", "enum": []string{"usage", "history", "flow"}, "description": "缺省 usage"},
			"limit":  intp("关键词检索返回数,默认 5"),
			"before": str("history 翻页:取此 change ID 之前的更早历史"),
		}, "query"),
	},
	"kb_remember": map[string]any{
		"name":        "kb_remember",
		"description": "沉淀知识(有阈值:费了功夫才懂的、代码上看不出来的才存;一眼懂的不存)。只收锚定本仓库代码的知识——判据:代码变了它会失效吗;通用编程知识/会话偏好/任务待办三不进(待办归 kb_task,偏好归宿主 memory)。新写的函数直接报符号名,服务端自动落锚建节点。supersedes 是更新/合并旧条目的唯一入口。",
		"inputSchema": obj(map[string]any{
			"node": str("节点 ID:file.go#Symbol(方法带接收者,如 file.go#Service.Login)"),
			"entries": arr(obj(map[string]any{
				"kind":     map[string]any{"type": "string", "enum": []string{"summary", "contract", "mutation", "pitfall", "usage"}, "description": "summary=一句话概览;contract=调用契约/前置条件;mutation=是否改入参/有无副作用;pitfall=坑/易错;usage=正确用法"},
				"text":     str("知识文本(按层级有 token 预算)"),
				"based_on": arr(str("依据条目引用 node-id#entry-id"), "结论依据来自其他知识条目(非原文)时必须声明;可信度封顶 inferred"),
				"disputes": arr(str("矛盾条目引用 node-id#entry-id"), "本条与既有条目矛盾且你无法当场裁决(证据在代码外等)时声明,登记待裁决防静默共存;能自裁的直接 kb_verify refute"),
			}, "kind", "text"), "知识条目"),
			"keywords":   arr(str("检索关键词"), "整体替换语义(非追加),上限 12;recall 空手后回填你用过的检索词"),
			"supersedes": arr(str("被取代的条目 ID"), "新条目取代旧条目(更新/合并);须恰好一条新条目"),
			"base_hash":  str("可选:此前 recall 返回的锚 hash,做乐观并发校验"),
		}, "node"),
	},
	"kb_record_change": map[string]any{
		"name":        "kb_record_change",
		"description": "修改代码后的变更记录:一个逻辑修改=一条记录(一次重构改 15 个函数也是 1 条,nodes 列全)。推翻既有决定必须带 overturns+rebuttal(决策链)。每个逻辑修改单元收尾时必须调用,否则任务不算完成。",
		"inputSchema": obj(map[string]any{
			"nodes": arr(str("节点 ID"), "首位为主节点,其余为波及节点;新符号自动落锚建节点"),
			"what":  str("改了什么(一句话)"),
			"why":   str("为什么改(触发原因)"),
			"task":  str("触发这次修改的任务/问题"),
			"rejected": arr(obj(map[string]any{
				"option": str("否决的方案"),
				"reason": str("否决原因"),
			}, "option", "reason"), "负知识:否决过什么、为什么(防横跳的核心)"),
			"overturns": str("被推翻的 change ID(走回头路必须声明)"),
			"rebuttal":  str("对被推翻记录 why 的直接回应(overturns 非空时必填)"),
			"verified":  str("这次修改如何被验证(测试名等,强烈建议)"),
			"remaps": arr(obj(map[string]any{
				"from":    str("旧节点 ID"),
				"to":      arr(str("新节点 ID"), "目标节点(拆分可多个)"),
				"entries": map[string]any{"type": "object", "description": "可选:entryID→目标节点 ID 逐条指定;缺省全部归 to[0]"},
			}, "from", "to"), "重构申报:拆分/合并的知识归属(机器猜不了,谁重构谁申报)"),
			"base_hash": str("可选乐观校验;失配不拒收只警示(账本优先)"),
		}, "nodes", "what", "why"),
	},
	"kb_verify": map[string]any{
		"name":        "kb_verify",
		"description": "复核知识:confirm(读原文确认,inferred→verified;entry 传纯节点 ID + confirm 则做节点级重验、清 suspect)/ refute(驳倒,必须附原文证据,触发级联污染回收+疫苗义务)/ obsolete(没错但不再适用,体面退休)。",
		"inputSchema": obj(map[string]any{
			"entry":    str("条目引用 node-id#entry-id;或纯节点 ID(配 confirm 做节点级重验重锚)"),
			"verdict":  map[string]any{"type": "string", "enum": []string{"confirm", "refute", "obsolete"}},
			"evidence": str("refute 必填:原文证据(引用具体代码行)"),
			"reason":   str("obsolete 必填:退休原因(功能下线/约定废止)"),
		}, "entry", "verdict"),
	},
	"kb_task": map[string]any{
		"name":        "kb_task",
		"description": "任务态(WIP)台账:start/update/complete/get。进行中状态与知识严格分离;touching 声明『正在动谁』,他人触碰时自动看到;complete 自动归档为变更记录。",
		"inputSchema": obj(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"start", "update", "complete", "get"}},
			"wip": obj(map[string]any{
				"task":     str("任务一句话(含 issue 引用)"),
				"intent":   str("意图"),
				"plan":     arr(str(""), "计划步骤"),
				"done":     arr(str(""), "已完成"),
				"todo":     arr(str(""), "待办"),
				"touching": arr(str("节点 ID"), "正在动哪些节点"),
			}),
		}, "action"),
	},
	"kb_investigate": map[string]any{
		"name":        "kb_investigate",
		"description": "侦查(上下文卫生):先查库,命中直接返回;未命中秒回一份侦查简报——把简报【原样】交给一个子代理(Task 工具)执行,不要自己执行,保护你自己的上下文。侦察兵蒸馏落库后 kb_submit_findings 交卷。",
		"inputSchema": obj(map[string]any{
			"question": str("要定位的问题(如『登录偶尔失败,定位原因和修改点』)"),
			"scope":    str("可选:范围路径前缀"),
		}, "question"),
	},
	"kb_submit_findings": map[string]any{
		"name":        "kb_submit_findings",
		"description": "侦察兵交卷:落库销 job。交卷后把同样内容完整写进你的最终答复带回主 AI(委派模式的回程通道是子代理返回值)。",
		"inputSchema": obj(map[string]any{
			"job":        str("侦查简报里的 job id"),
			"conclusion": str("定位结论"),
			"locations":  arr(str("节点 ID"), "位置指针(不要复制原文)"),
			"plan":       str("建议修改计划"),
			"risks":      str("风险提示"),
		}, "job", "conclusion"),
	},
	"kb_adopt": map[string]any{
		"name":        "kb_adopt",
		"description": "孤儿节点处置:claim(认领——符号搬去了新位置,建 remap 迁移知识与血缘)/ bury(送葬——确认作废,知识快照归档进 journal 可溯)。",
		"inputSchema": obj(map[string]any{
			"orphan": str("孤儿节点 ID"),
			"action": map[string]any{"type": "string", "enum": []string{"claim", "bury"}},
			"to":     str("claim 必填:新节点 ID"),
			"reason": str("bury 必填:为什么确认作废"),
		}, "orphan", "action"),
	},
	"kb_flow": map[string]any{
		"name":        "kb_flow",
		"description": "流程/主题节点 CRUD:横向维度——一条穿过多个文件的业务/技术流程(flow:xxx),或横切关注点/全局约定(topic:xxx)。steps 引用树节点不复制内容。",
		"inputSchema": obj(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"get", "create", "update", "deprecate"}, "description": "get:读(id 空则列全部);create/update/deprecate:写。update 是整体替换,先 get 再改"},
			"flow": obj(map[string]any{
				"id":    str("flow:name 或 topic:name"),
				"title": str("标题"),
				"steps": arr(obj(map[string]any{
					"node": str("树节点 ID"),
					"note": str("该步说明"),
				}, "node"), "有序步骤(主题节点可空)"),
				"conventions":  arr(str(""), "全局约定"),
				"troubleshoot": str("排障入口说明"),
			}, "id", "title"),
		}, "action", "flow"),
	},
	"kb_maintain": map[string]any{
		"name":        "kb_maintain",
		"description": "维护欠账:next 取一条(时代摘要压缩/文件摘要落后/疑似重复/待重验 suspect/矛盾待裁决/非代码知识超期复核/置信度滞后即有 inferred 知识却已被测试验证过);complete 销账(era 债携带 era_summary 落库,负知识必须逐条保留在摘要里)。任务尾顺手偿还 ≤2 条。",
		"inputSchema": obj(map[string]any{
			"action":      map[string]any{"type": "string", "enum": []string{"next", "complete", "dismiss"}, "description": "next 取一条;complete 销账;dismiss 消解假阳性(如 dup-entries 判定实为不同)"},
			"id":          str("complete/dismiss 必填:欠账 ID"),
			"scope":       str("next 可选:路径前缀,只取本任务相关的债"),
			"era_summary": str("era-compress 债完成时提交的时代摘要文本"),
		}, "action"),
	},
}
