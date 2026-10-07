# 03 · 工具与编排：把可用能力变成可控行动

工具定义解决“模型怎样提出请求”，执行器解决“这次请求是否能运行、实际发生了什么”。

[阅读目录](README.md) · [技术展开](#技术展开) · [基础自测](#自测)

## 先认识术语

| 术语 | 通俗解释 |
| --- | --- |
| Schema | 输入字段的类型、必填项和范围约束 |
| Executor | 真正运行工具的程序 |
| Dependency | 后一步需要前一步结果 |
| Concurrency | 独立工作在时间上重叠执行 |
| Side effect | 改变外部状态，例如写文件或创建单据 |
| tool_call_id | 用来把一次模型调用请求与它的结果配对的 ID |

## 一个工具有三个不同对象

**定义**是接口说明；**调用**是这一次的工具名和参数；**结果**是实际执行的返回。

用 calculator 举例：定义要求 expression 是字符串，调用给出“3 × 180”，结果返回 540。定义不会计算，模型输出参数也不会计算，计算发生在执行器调用的函数里。

一个好的定义要说明：

- 什么时候适合使用，以及不能做什么。
- 参数是什么，允许什么格式，是否有上限。
- 返回内容代表什么，失败和资料不足怎么表达。
- 是否修改外部状态，以及是否需要审批。

“这是一个搜索工具”太笼统；“查询制度资料，返回片段、来源与相关性；没有有效证据时返回资料不足”更容易被正确使用。

## 执行前要过三道检查

| 检查 | 要回答的问题 | 例子 |
| --- | --- | --- |
| 结构 | 参数能否解析、类型是否正确 | expression 必须是字符串 |
| 业务 | 这组值是否有意义 | 除数不能为零；日期区间不能倒置 |
| 权限 | 当前身份是否可以执行 | 能查资料，但不能替别人提交报销 |

Schema 能证明字段形状，不能证明用户拥有权限，也不能证明计算符合业务规则。

关键边界要在实际执行端再次检查。工具服务可能被其他 client 访问，不能依赖某个上游曾经校验过。

## 先判断依赖，再决定并行

```text
查补贴规则 ──→ 算金额 ──→ 整理说明
查当前日期 ─────────────→ 整理说明
```

金额依赖规则，不能和规则检索一起提前执行。日期与规则独立，可以并行。工具调用的并发上限限制正在执行的数量，不会自动判断业务依赖。

如果两项独立工作分别花 2 秒和 3 秒，理想并行耗时接近 3 秒，再加调度开销。它不保证所有动作都变快：资源争用、限流和等待可能抵消收益。

写同一个文件的两条命令，即使参数不同，也可能相互覆盖。是否并行还要看共享状态和副作用。

## 每项调用都要有自己的结果

模型一次提出两个相同名字的工具调用，也要用两个 ID 分别配对：

```text
call_a：计算交通补贴 → 540
call_b：计算住宿补贴 → 900
```

结果不能只写成“calculator 成功”。模型需要知道哪组参数对应哪个数。

同批调用部分失败时，应逐项记录：成功的保留结果，失败的返回原因，未执行的说明没有执行，无法确定的说明结果未知。只报“整批失败”会抹掉已经发生的动作。

有依赖的结果要按协议写回。不要因为某个工具更快返回，就把它错配给前面的调用。

## 定义加载与工具执行是两件事

加载工具是让程序或模型知道有哪些接口。执行工具是响应一次具体请求。

工具定义可在启动时获取，也可按任务按需选择；看到一个工具名字并不代表它已经运行。也不必为每个工具单独启动服务，本地函数和远程协议工具可以共享同一个执行器入口。

## 技术展开

### 进阶术语：DAG、背压与规范化

| 术语 | 含义 |
| --- | --- |
| DAG | 有向无环依赖图，前置动作完成后后续才能开始 |
| 信号量 | 限量许可，控制同时进入执行区的调用 |
| 背压 | 接收方繁忙时让上游等待或拒绝，避免无限积累 |
| Canonicalization | 按规则规范化输入表示，便于比较或生成键 |

### 参数校验要区分类型、缺失与多余内容

Go 的 json.Unmarshal 不会自动证明“必填项都有”。把缺失的 int 字段解码成 0，还会与用户真的填 0 混淆。

下面是一个最小的参数解析函数，只展示字段级约束：

```go
type SearchArgs struct {
    Query string `json:"query"`
    K     *int   `json:"k"`
}

func decodeSearch(raw string) (SearchArgs, error) {
    var args SearchArgs
    dec := json.NewDecoder(strings.NewReader(raw))
    dec.DisallowUnknownFields()
    if err := dec.Decode(&args); err != nil {
        return args, err
    }
    var extra any
    if err := dec.Decode(&extra); err != io.EOF {
        return args, errors.New("参数后还有内容，或不是单个JSON值")
    }
    if strings.TrimSpace(args.Query) == "" || args.K == nil {
        return args, errors.New("query与k必填")
    }
    if *args.K < 1 || *args.K > 10 {
        return args, errors.New("k超出范围")
    }
    return args, nil
}
```

这是可编译函数片段，需要 encoding/json、strings、io、errors。它仍不是完整的 JSON Schema 校验器；例如重复对象键、大小写匹配、业务授权需要另行确定规则。

第二次 Decode 检查 EOF，避免接受“第一个 JSON 后面还藏着其他内容”。不能用第一次解析成功代替整个输入有效。

### 并行执行怎样保持正确关联

核心不是“启动很多 goroutine”，而是许可、结果槽和完成等待。结构示意：

```go
type Call struct{ ID string }
type Result struct{ ID, Status string }
type Invoke func(context.Context, Call) Result

func runGroup(ctx context.Context, calls []Call, p int, invoke Invoke) []Result {
    if p < 1 { panic("并发上限必须为正") }
    out := make([]Result, len(calls))
    sem := make(chan struct{}, p)
    var wg sync.WaitGroup
    for i, call := range calls {
        i, call := i, call
        wg.Add(1)
        go func() {
            defer wg.Done()
            select {
            case sem <- struct{}{}:
                defer func() { <-sem }()
                out[i] = invoke(ctx, call)
            case <-ctx.Done():
                out[i] = Result{ID: call.ID, Status: "not_run"}
            }
        }()
    }
    wg.Wait()
    return out
}
```

需要 context、sync。各 goroutine 写不同槽位，最后等待所有写入完成，再按原顺序返回。

这个片段没有统一处理 panic、强制超时或未知结果；invoke 必须配合取消，否则 Wait 仍可能一直等待。信号量限制实际执行数量，却不限制已创建的等待 goroutine 数，所以巨大批次需要队列或固定 worker。

### 依赖和共享资源不是同一种约束

A→C、B→C 的任务，A 与 B 可并行，C 需要两项结果。一个批次的理论时间近似为：

```text
关键路径时间 + 调度/排队开销
```

但两个无数据依赖的动作若写同一文件，也需要锁或串行。依赖图约束“缺了哪个结果”，资源规则约束“同时修改什么”。

对有副作用工具可以设置资源键，如“任务工作目录”，同一键的动作顺序执行；只靠工具名一律串行可能过于保守。

### 重复动作的键怎样生成

参数字段顺序不同应归为同一个对象。可以解析后重编码，拼成：

```text
action_key = 工具名 + ":" + 规范化参数
```

保留数字词法可以避免大整数先转 float64 失真，但 1 与 1.0 未必被合并。默认值、空字段和数组顺序的含义也应由工具契约决定，不能随便全部排序。

通用 JSON 规范化与业务等价是不同问题：两个查询说法不同，可能仍是同一无进展动作。

### 对照已有实现

[ExecuteBatch、actionKey](../internal/agent/react.go)使用结果槽、许可与 WaitGroup；同批 Bash 由 sequential 规则顺序运行，actionKey 使用 UseNumber。

当前规范化主要处理 JSON 表示，不是完整业务等价判断；也没有按任意资源键生成执行 DAG。上述算法用于理解改造边界。

### 进阶推演

1. p=2 的批次里，第一项正在退避等待，这个许可要不要释放？  
   两种策略都可以。持有许可限制整个逻辑调用、减少重试风暴；释放提高利用率，但需要另控重试流量。必须明确并发统计对象。

2. 为什么结果槽不用 mutex，也不代表“并发访问都不需要锁”？  
   每个槽只有一个写者，读者在 Wait 后才读；共享 map、同一槽多写或提前读取仍需要同步。


## 自测

1. 两个工具都只读文件，能否总是并行？  
   仍需检查依赖、资源和访问范围。只读降低副作用风险，但不消除依赖。

2. tool_call_id 能代替“创建报销单”的业务幂等键吗？  
   不能。它只关联一次模型请求；新的调用 ID 不代表新的业务意图。

3. 工具结果为 error 时，执行器应该隐藏它继续重试吗？  
   应先分类。确定性错误回填让模型修正；只有适合重试的情况才进行有限重试。

## 延伸阅读

- [Day 1 笔记](../docs/day-01/day-01-notes.md)
- [Day 7 校验](../docs/day-07/day-07-notes.md)
- [Day 2 动手](../docs/day-02/day-02-lab.md)

[上一篇：模型调用与执行循环](02-models-and-loop.md) · [下一篇：上下文管理](04-context-management.md) · [术语表](TERMS.md)
