package engine

// DisciplinePrompt 是纪律注入提示词(impl §9,一期唯一的注入腿):
// `iknowledge status --prompt` 打印,供粘贴进 CLAUDE.md / codex 指令 / aibridge 模板。
const DisciplinePrompt = `本仓库配有 knowledge MCP。规则:
0. knowledge 工具不可用(服务未启动)时:照常干活,任务尾提醒用户运行 ` + "`iknowledge serve`" + `;
1. 定位任何功能前,先 kb_recall 或 kb_map,不要盲目 grep;若 recall 空手、随后用
   grep 找到了目标,把你用过的查询词 kb_remember 进该节点的 keywords(回填索引);
2. 修改任何函数前,必须 kb_recall(node, mode=history) 查看来时路与负知识;
3. 知识只用于导航,修改前必须阅读原文(知识与原文冲突时以原文为准,并勘误知识);
4. 每个逻辑修改单元收尾时,必须 kb_record_change(一次重构 = 一条记录,
   nodes 列出主节点与全部波及节点;改了什么/为什么/否决了什么),否则任务不算完成;
5. 读懂一段费了功夫的代码或发现代码上看不出的约定后,kb_remember 沉淀(一眼懂的不存);
6. 上下文卫生:大范围分析定位交给 kb_investigate(把简报原样交给子代理执行),
   结论先蒸馏(remember / kb_task)再动手;修改阶段不依赖分析期的记忆,重读目标原文;
7. 开始多步任务先 kb_task start(声明 touching),收尾 kb_task complete 归档。`

// InitializeInstructions 是 initialize 返回的最短纪律(增强而非依赖——
// 纪律的正身是上面的粘贴提示词;客户端是否注入 instructions 需实测,impl §7.1)。
const InitializeInstructions = "代码知识库:读前 kb_recall/kb_map 导航,改后 kb_record_change 记账(一个逻辑修改一条),悟到的坑 kb_remember 沉淀。知识仅导航,修改前必读原文。"
