package integrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskWorkspace_ApplyAndResume(t *testing.T) {
	dir := t.TempDir()
	base := "a\nb\nc\n"
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := NewDiskWorkspace(dir)
	set := chainedSet()

	log := &bufLogger{}
	rep, err := New(ws, log).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep.AppliedIDs(), []string{"c1", "c2", "c3"}) {
		t.Fatalf("应用集合错误: %v", rep.AppliedIDs())
	}
	data, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(data), "\n") != "A\nB\nC" {
		t.Fatalf("磁盘内容错误: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, ".integrator-state.json")); err != nil {
		t.Fatalf("续跑状态文件必须落盘: %v", err)
	}

	// 再次提交同一变更集：全部跳过，内容不变（重复提交幂等）。
	rep2, err := New(ws, &bufLogger{}).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep2.SkippedIDs(), []string{"c1", "c2", "c3"}) {
		t.Fatalf("重复提交应全部跳过，得到 %v", rep2.SkippedIDs())
	}

	// 预演也必须识别已应用状态，但不改动状态文件。
	rep3, err := New(ws, &bufLogger{}).Plan(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep3.SkippedIDs(), []string{"c1", "c2", "c3"}) {
		t.Fatalf("预演应识别续跑状态并跳过，得到 %v", rep3.SkippedIDs())
	}
}

func TestDiskWorkspace_ConflictExitsReportAndKeepsApplied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := NewDiskWorkspace(dir)
	set := []Change{
		buildReplaceChange("c1", "a", "A"),
		buildReplaceChange("c2", "ZZZ", "X", "c1"),
	}
	rep, err := New(ws, &bufLogger{}).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep.AppliedIDs(), []string{"c1"}) || !eqStrings(rep.ConflictIDs(), []string{"c2"}) {
		t.Fatalf("结论错误: applied=%v conflict=%v", rep.AppliedIDs(), rep.ConflictIDs())
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if strings.TrimRight(string(data), "\n") != "A\nb\nc" {
		t.Fatalf("冲突不得回退已应用的 c1，得到 %q", data)
	}
}
