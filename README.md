# change-integrator-go

面向持续集成的**变更集成引擎**：接收一组待入主干的变更（提交/补丁），在本地基准上做
**预演（rehearse）**与**实际应用（apply）**，给出可复现、可解释的结论，并支持中断续跑
与重复提交识别。

- 结论只可能是 `applied` / `skipped` / `conflicted` / `mismatch`，且预演与应用对同一前置状态给出**完全一致**的结论。
- 顺序无法确定、缺少前置、目标重复时，变更集会被**整体拒绝**并返回可区分的拒绝原因，绝不静默丢弃或按到达顺序硬套。
- 冲突时立即停止；已干净应用的变更**不回退、不重复应用**。
- 同一条依赖链上的**接力式演进**（后一条在前一条落地结果上继续改同一处）依次成功应用，最终内容等于最后一步的结果。
- 任何运行中断或同一变更集再次提交时，已成功应用的变更被识别并跳过，未完成部分从正确位置继续。

## 构建与验证

沿用 Go 标准工具链（无第三方依赖）：

```bash
go build ./...
go vet ./...
gofmt -l .      # 应无输出
go test ./...   # 34 个测试 + 1 个 Example，覆盖冲突、接力演进、重复提交、中断续跑
go test ./... -v   # 查看单测日志：每条变更的标识、判定依据、冲突位置
```

## 快速上手

```go
ws := integrator.NewDirWorkspace("/path/to/worktree")
eng := integrator.NewEngine(ws, "/path/to/state/journal.json", integrator.Options{
    Logger: log.New(os.Stdout, "", 0),
})

changes := []integrator.RawChange{ /* 提交或补丁归一化前的输入 */ }

// 预演：不落盘，结论与应用一致
r1, err := eng.Rehearse(changes)

// 应用：写入工作区与状态日志；可安全重入
r2, err := eng.Apply(changes)
```

## 变更模型

输入 `RawChange` 描述一个提交/补丁：

| 字段 | 含义 |
| --- | --- |
| `ID` | 变更唯一标识（必须非空）。重复提交必须复用原 ID。 |
| `DependsOn` | 前置变更 ID 列表，构成有向依赖边。 |
| `Files[]` | 对各文件的块级改动（hunk）。 |

每个 hunk（`RawFileChange`）包含：

| 字段 | 含义 |
| --- | --- |
| `Path` | 目标文件相对路径（禁止绝对路径与 `..` 越界）。 |
| `OldStart` | 旧块在**基准文件**中的 1-based 锚点行号。 |
| `OldBlock` | 被替换的旧行；为空表示纯插入。 |
| `NewBlock` | 替换后的新行。 |
| `Create` | 目标文件在基准上不存在（创建），此时不得携带旧块/锚点。 |

`Normalize` 将输入归一为 `Change` 并完成：

1. **校验**：ID/路径非空、锚点合法、创建语义合法。
2. **作用范围推导**：每个 hunk 对应基准文件的一个半开区间 `[Start, End)`（0-based），
   `End = Start + len(OldBlock)`；文件创建用整文件范围表示。
3. **内容指纹**：对补丁内容（路径、创建/修改、锚点、旧块、新块）做 SHA-256，
   **与 ID/摘要/依赖无关**。两条不同 ID 但内容完全相同的变更得到相同指纹。

## 归一、去重与顺序判定

`BuildPlan` 对变更集依次做四件事，任一失败都会在 `Rejection` 中给出**可区分原因**，
引擎返回 `*RejectedError` 且**整个集合不执行**（工作区零改动）：

| 拒绝原因 | 触发条件 |
| --- | --- |
| `missing_dependency` | `DependsOn` 引用的前置不在本次变更集中（自依赖同样拒绝）。 |
| `order_undecidable` | 依赖图存在环，Kahn 拓扑排序无法排出全序；环中每个节点都被标注。 |
| `duplicate_id` | 同一变更 ID 在同一变更集中出现多次。 |
| `duplicate_change` | 不同 ID 但内容指纹相同——重复提交必须复用原 ID，而不是换新 ID 重发。 |

顺序规则：**依赖边决定先后**（前置必先应用）；互不依赖的变更按 **ID 字典序**打破平局，
使顺序完全确定、跨运行可复现——不会因为到达顺序不同而改变结果。

注意：**作用范围重叠不是计划期拒绝项**，而是在应用阶段检测为冲突（因为重叠是否成立取决于
此前已应用的内容，且需要给出冲突位置与来源）。

## 冲突处理规则

引擎严格按计划顺序处理，累加每个已应用变更在**基准**上的作用范围。应用下一个变更前，
先将其范围与已占用范围比较（重叠对象若全部属于该变更的传递依赖闭包，则按
**接力演进**处理，见下节）：

- **同路径行区间相交**（`[a1,a2)` 与 `[b1,b2)` 满足 `a1 < b2 && b1 < a2`）→ 冲突；
  端点相接（如 `[1,3)` 与 `[3,5)`）**不冲突**。
- **两个零长度插入落在同一锚点**（如都在第 3 行前插入）→ 冲突，因为合并后的相对顺序无法确定。
- **同路径文件创建与任何改动（含另一次创建）**→ 冲突。
- 不同路径互不影响；同一变更内部的多个 hunk 总是视为兼容。

冲突结论包含可解释的 `Conflict`：`ChangeID`（当前变更）、`AgainstID`（来源变更）、
`Path`、`BaseStart`/`BaseEnd`（基准行区间，0-based 半开）、`Detail`。

冲突发生时：

1. 引擎在首个冲突处**立即停止**，返回 `*ConflictError`；
2. 之前已干净应用的变更**保留在工作区中，不回退**，也不会被重复应用；
3. 冲突变更及其后的变更本次不应用——续跑时从该变更重新判定。

应用原语层面还有两类可区分失败：`ErrContextMismatch`（旧块在锚点与全文都找不到，
或全文匹配到多处，引擎拒绝猜测位置）、创建已存在文件/修改不存在文件。

## 接力式依赖改动（relay）

CI 里常见一类提交：同一条依赖链上的变更**接力修改同一处内容**。例如某行先被
`a` 从 `v0` 改成 `v1`，`b`（依赖 `a`）再把 `v1` 改成 `v2`，`c`（依赖 `b`）继续改成
`v3`。这种情况下几条变更应依次成功应用，最终内容等于最后一步的结果，链路拉到
几十条也稳定成立。

### 适用范围与判定规则

引擎按计划顺序累加已应用变更的基准作用范围；当前变更与已占范围重叠时，按
重叠对象的来源分两类处理：

- **重叠对象全部落在当前变更的传递依赖闭包内**（含已在此前运行中应用、本次未
  提交的前置）→ 判定为**接力演进**，不做冲突。日志打印
  `decision=relay basis=overlaps_only_own_dependencies`。
- **存在任何一个不在依赖闭包内的重叠对象** → 仍是**冲突**（`conflicted`），
  给出具体位置与来源。也就是说：互不依赖的变更改到同一处，结论与接力引入前
  完全一致；接力豁免只覆盖“自己的前置”，**不会掩护与无关变更的重叠**。

接力变更的应用走**严格模式**：

1. hunk 必须在声明的锚点处精确匹配**预期前置内容**（即前序变更落地后的内容），
   匹配成功才应用，日志打印 `basis=precondition_verified_at_anchor`；
2. 锚点处内容对不上时，**绝不静默跳过、也绝不换到文件里别的位置套用**
   （不做全文件搜索回退），而是给出可区分的失败结论 `mismatch`：
   返回 `*RelayMismatchError`（`errors.Is(err, ErrRelayPrecondition)` 成立），
   其中携带变更标识、路径、锚点行号、预期内容与实际内容；
3. 接力创建文件时目标已存在、或接力修改时目标不存在，同样判为 `mismatch`。

### 与无依赖重叠改动如何区分

| 场景 | 结论 | 错误类型 |
| --- | --- | --- |
| 互不依赖的变更改到同一处 | `conflicted`，含位置与来源 | `*ConflictError` |
| 依赖链上接力改同一处，前置内容对得上 | `applied`（严格模式） | — |
| 声明了依赖，但预期前置内容与实际对不上 | `mismatch`，含预期/实际内容 | `*RelayMismatchError`（`ErrRelayPrecondition`） |

`mismatch` 与 `conflicted` 一样会立即停止本次运行；之前已干净应用的变更保留
不回退。预演与应用对接力场景（含失败路径）给出完全一致的结论；中断续跑时，
已落地的接力前缀被识别并跳过，剩余部分从正确状态继续接力，只提交剩余变更
（前置已在状态日志中）同样成立。

### 本地验证

```bash
go test ./... -run 'TestRelay_' -v
```

日志中可看到每条变更的标识、拓扑位置、判定依据
（`decision=relay` / `basis=precondition_verified_at_anchor` /
`decision=mismatch path=... anchor=... expected=... actual=...`）。
关键用例：`TestRelay_ChainAppliesInOrder`（同区域接力成功）、
`TestRelay_LongChainStable`（40 节链路 + 反复提交稳定）、
`TestRelay_PreconditionMismatch` / `TestRelay_NoRelocateToOtherPosition`
（前置对不上且拒绝换位置）、`TestRelay_RehearseApplyIdentical`（两种模式一致）、
`TestRelay_ResumeAfterInterruption` / `TestRelay_ResumeRemainingOnly`（中断续跑）、
`TestRelay_IndependentOverlapStillConflict` /
`TestRelay_RelayDoesNotShieldUnrelatedOverlap`（无依赖重叠仍判冲突）。

## 预演与应用模式

两种模式共享 `run` 这一条执行路径（归一化 → 计划 → 冲突判定 → 应用原语），差异只有两点：

| | Rehearse（预演） | Apply（应用） |
| --- | --- | --- |
| 工作区写入 | 写入**内存覆盖层**（读穿透基准、写只进覆盖层），真实工作区零改动 | 写入真实工作区（`DirWorkspace` 或自定义 `Workspace`） |
| 状态日志 | **只读不写**，不产生持久状态 | 每条变更应用成功后**立即原子落盘**（临时文件 + rename） |

因此对同一基准与同一状态，两者的 `Order` 与每条变更的状态集合（applied/skipped/conflicted）
**完全一致**。测试 `TestEngine_RehearseAndApply_IdenticalConclusions` 对此做了断言。

## 中断与重复提交

- **状态日志**（默认 `journal.json`）记录每条已应用变更的 ID、内容指纹、应用序号、
  作用范围；每次写入都是原子替换，中断不会留下半截日志。
- 每次运行先载入日志：日志中存在且**指纹一致**的变更直接判定为 `skipped`（判定依据
  `basis=journal_record` 会打印），其作用范围仍参与后续冲突判定；然后从计划中的
  正确位置继续应用剩余变更。
- 续跑时允许**只提交剩余变更**：前置若已在此前运行中应用（存在于状态日志），计划校验视其为
  已满足的前置，不再误报 `missing_dependency`；前置既不在集合中也不在日志中时仍被拒绝。
- 同一 ID 但内容与上次应用时**指纹不一致** → 返回 `ErrFingerprintMismatch`，不会静默按新版本覆盖。
- 崩溃窗口安全网：若某变更的工作区写入已完成而日志尚未落盘，重跑时会做**内容级识别**
  （逐文件比对应用后的预期结果），确认已应用则补记日志并跳过，避免重复应用。
- 续跑时剩余变更与已应用内容若存在重叠，冲突仍会被检出（见
  `TestResume_RemainingConflictStillDetected`）。

测试中的中断通过 `Options.BeforeChange` 钩子注入（在每条变更应用前返回错误来模拟
CI 进程被 kill）。

## 结论报告与日志

`Report` 包含：

- `Mode`：rehearse / apply；
- `Order`：拓扑序变更 ID；
- `Outcomes[]`：每条变更的 `ChangeID`、`Status`（applied/skipped/conflicted/mismatch）、
  `Reason`，冲突时附带 `Conflict`；
- `Completed`：变更集是否全部走到终态。

传入 `Options.Logger` 后，每个决策都会打印结构化日志，包含**变更标识、依赖、
拓扑位置、判定依据（applied/skipped/conflicted/mismatch/relay/interrupted）与
冲突位置/来源**，便于 CI 页面解释结论。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `model.go` | 原始输入、统一变更模型、归一化校验、作用范围、内容指纹 |
| `plan.go` | 去重、依赖存在性、拓扑排序、四类拒绝原因 |
| `workspace.go` | 工作区接口（磁盘/内存）、hunk 定位与应用原语（含接力严格模式） |
| `journal.go` | 跨运行状态日志的原子持久化 |
| `engine.go` | 两种模式的统一执行流程、冲突/接力判定、跳过/续跑、覆盖层 |
| `report.go` | 模式、状态、冲突与报告的类型定义 |
| `*_test.go` | 计划校验、冲突、接力演进、模式一致性、中断续跑、工作区原语、端到端 Example |
