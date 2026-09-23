# change-integrator-go

面向持续集成的变更集成引擎：归一变更集 → 判定确定性顺序 → 预演 / 应用，
输出可复现、可解释的结论，并支持中断续跑与重复提交（不重复应用、不静默丢失）。

- 核心包：`pkg/integrator`
- 命令行：`cmd/integrator`
- 设计与规则文档：[docs/integrator.md](docs/integrator.md)

## 快速开始

```bash
go build ./...          # 构建
go vet ./...            # 静态检查
go test ./...           # 全部测试（含磁盘与 CLI 端到端用例）
go test -short ./...    # 仅快速单测
```

```bash
go run ./cmd/integrator -mode dry-run -root ./worktree -file changes.json
go run ./cmd/integrator -mode apply    -root ./worktree -file changes.json
```

退出码：`0` 成功（可含跳过）、`1` 冲突、`2` 变更集被拒绝、`3` 运行时错误。
