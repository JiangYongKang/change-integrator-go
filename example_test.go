package integrator_test

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	integrator "github.com/highcumontoa/change-integrator-go"
)

func Example() {
	// 1. 准备基准工作区与状态目录。
	dir, _ := os.MkdirTemp("", "ci-example-")
	defer os.RemoveAll(dir)
	ws := integrator.NewDirWorkspace(filepath.Join(dir, "worktree"))
	_ = ws.Write("app.txt", []string{"line1", "line2", "line3"})

	eng := integrator.NewEngine(ws, filepath.Join(dir, "state", "journal.json"), integrator.Options{
		Logger: log.New(os.Stdout, "", 0),
	})

	// 2. 一组待入主干的变更：b 依赖 a，且二者改动互不重叠。
	changes := []integrator.RawChange{
		{
			ID: "b", DependsOn: []string{"a"},
			Files: []integrator.RawFileChange{{
				Path: "app.txt", OldStart: 3,
				OldBlock: []string{"line3"}, NewBlock: []string{"line3-b"},
			}},
		},
		{
			ID: "a",
			Files: []integrator.RawFileChange{{
				Path: "app.txt", OldStart: 1,
				OldBlock: []string{"line1"}, NewBlock: []string{"line1-a"},
			}},
		},
	}

	// 3. 预演：不落盘，但结论与 Apply 一致。
	if r, err := eng.Rehearse(changes); err != nil {
		fmt.Println("rehearse error:", err)
	} else {
		fmt.Println("rehearse order:", r.Order)
	}

	// 4. 应用：拓扑序为 a -> b。
	if _, err := eng.Apply(changes); err != nil {
		fmt.Println("apply error:", err)
	}
	lines, _, _ := ws.Read("app.txt")
	fmt.Println("applied:", lines)

	// 5. 同一变更集再次提交：全部识别并跳过，不重复应用。
	r2, err := eng.Apply(changes)
	if err != nil {
		fmt.Println("resubmit error:", err)
	} else {
		for _, o := range r2.Outcomes {
			fmt.Println("resubmit:", o.ChangeID, o.Status)
		}
	}

	// Output:
	// change=a deps=[] position=0 decision=applied mode=rehearse
	// change=b deps=[a] position=1 decision=applied mode=rehearse
	// rehearse order: [a b]
	// change=a deps=[] position=0 decision=applied mode=apply
	// change=b deps=[a] position=1 decision=applied mode=apply
	// applied: [line1-a line2 line3-b]
	// change=a deps=[] position=0 decision=skipped basis=journal_record order=0
	// change=b deps=[a] position=1 decision=skipped basis=journal_record order=1
	// resubmit: a skipped
	// resubmit: b skipped
}
