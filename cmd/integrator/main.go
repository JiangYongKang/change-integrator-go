// Command integrator 是变更集成引擎的命令行入口，供 CI 流水线调用。
//
// 变更集以 JSON 描述（见 docs/integrator.md 的“输入格式”），从 -file 指定的
// 文件读取，或在 file="-"（默认）时从标准输入读取。
//
// 用法：
//
//	integrator -mode dry-run -root . -file changes.json
//	integrator -mode apply    -root /path/to/worktree -file changes.json
//
// 退出码：0 全部应用（允许存在跳过项）；1 出现冲突；2 变更集被拒绝（
// 归一/顺序判定失败）；3 运行时错误。结论同时以 JSON 打印到标准输出。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/highcumontoa/change-integrator-go/pkg/integrator"
)

func main() {
	mode := flag.String("mode", "dry-run", "运行模式：dry-run 或 apply")
	root := flag.String("root", ".", "基准工作区根目录")
	file := flag.String("file", "-", "变更集 JSON 文件路径，- 表示标准输入")
	statePath := flag.String("state", "", "续跑状态文件路径（默认 <root>/.integrator-state.json）")
	flag.Parse()

	var m integrator.Mode
	switch *mode {
	case "dry-run":
		m = integrator.ModeDryRun
	case "apply":
		m = integrator.ModeApply
	default:
		fmt.Fprintf(os.Stderr, "未知模式 %q（仅支持 dry-run / apply）\n", *mode)
		os.Exit(3)
	}

	data, err := readInput(*file)
	if err != nil {
		fail(err)
	}
	var in struct {
		Changes []integrator.Change `json:"changes"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		fail(fmt.Errorf("解析变更集 JSON 失败: %w", err))
	}

	ws := integrator.NewDiskWorkspace(*root)
	if *statePath != "" {
		ws.SetStatePath(*statePath)
	}
	eng := integrator.New(ws, logWriter{})
	report, err := eng.Integrate(m, in.Changes)
	if err != nil {
		if re, ok := err.(*integrator.RejectError); ok {
			fmt.Fprintf(os.Stderr, "变更集被拒绝（%s）：%s\n", re.Reason, re.Message)
			os.Exit(2)
		}
		fail(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fail(err)
	}
	if len(report.ConflictIDs()) > 0 {
		os.Exit(1)
	}
}

func readInput(p string) ([]byte, error) {
	if p == "-" {
		return readAll(os.Stdin)
	}
	return os.ReadFile(p)
}

func readAll(f *os.File) ([]byte, error) {
	return io.ReadAll(f)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(3)
}

// logWriter 把引擎判定日志转发到标准错误，避免污染标准输出的 JSON 结论。
type logWriter struct{}

func (logWriter) Printf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
