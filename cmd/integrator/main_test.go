package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCLI_DryRunThenApplyAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过需要构建子进程的用例")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changeSet := map[string]any{
		"changes": []map[string]any{{
			"id":      "c1",
			"message": "replace a",
			"files": []map[string]any{{
				"path": "f.txt",
				"hunks": []map[string]any{{
					"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
					"lines": []map[string]any{
						{"op": "-", "text": "a"},
						{"op": "+", "text": "A"},
					},
				}},
			}},
		}},
	}
	csJSON, _ := json.Marshal(changeSet)
	csPath := filepath.Join(dir, "changes.json")
	if err := os.WriteFile(csPath, csJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "integrator")
	build := exec.Command("go", "build", "-o", bin, "./cmd/integrator")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 CLI 失败: %v\n%s", err, out)
	}

	// dry-run：退出码 0，文件不得被改。
	dry := exec.Command(bin, "-mode", "dry-run", "-root", dir, "-file", csPath)
	if out, err := dry.CombinedOutput(); err != nil {
		t.Fatalf("dry-run 应成功: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "a\nb\n" {
		t.Fatalf("dry-run 不得改文件，得到 %q", data)
	}

	// apply：退出码 0，文件被改。
	app := exec.Command(bin, "-mode", "apply", "-root", dir, "-file", csPath)
	if out, err := app.CombinedOutput(); err != nil {
		t.Fatalf("apply 应成功: %v\n%s", err, out)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "A\nb\n" {
		t.Fatalf("apply 后文件错误: %q", data)
	}

	// 再次 apply：全部已应用，仍退出 0（跳过不是失败）。
	again := exec.Command(bin, "-mode", "apply", "-root", dir, "-file", csPath)
	if out, err := again.CombinedOutput(); err != nil {
		t.Fatalf("重复 apply 应成功（幂等跳过）: %v\n%s", err, out)
	}
}

func TestCLI_RejectExitCode2(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 同一 ID 出现两次 → duplicate → 退出码 2。
	changeSet := map[string]any{"changes": []map[string]any{
		{
			"id": "x", "files": []map[string]any{{
				"path": "f.txt", "hunks": []map[string]any{{
					"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
					"lines": []map[string]any{{"op": "-", "text": "a"}, {"op": "+", "text": "1"}},
				}},
			}},
		},
		{
			"id": "x", "files": []map[string]any{{
				"path": "f.txt", "hunks": []map[string]any{{
					"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1,
					"lines": []map[string]any{{"op": "-", "text": "a"}, {"op": "+", "text": "2"}},
				}},
			}},
		},
	}}
	csJSON, _ := json.Marshal(changeSet)
	csPath := filepath.Join(dir, "changes.json")
	_ = os.WriteFile(csPath, csJSON, 0o644)

	bin := filepath.Join(t.TempDir(), "integrator")
	build := exec.Command("go", "build", "-o", bin, "./cmd/integrator")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 CLI 失败: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "-mode", "apply", "-root", dir, "-file", csPath)
	err := cmd.Run()
	if err == nil {
		t.Fatal("重复变更集必须以非零退出码失败")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("拒绝应退出码 2，得到 %v", err)
	}
}
