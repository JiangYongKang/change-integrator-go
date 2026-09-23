package integrator

import (
	"fmt"
	"strings"
	"testing"
)

// bufLogger 捕获引擎日志，用于断言日志中包含变更标识、判定依据与冲突位置。
type bufLogger struct{ b strings.Builder }

func (l *bufLogger) Printf(format string, args ...any) {
	fmt.Fprintf(&l.b, format+"\n", args...)
}
func (l *bufLogger) String() string { return l.b.String() }

// buildReplaceChange 构造一个对 f.txt 做简单行替换的变更。
func buildReplaceChange(id, old, neu string, deps ...string) Change {
	return Change{
		ID:      id,
		Message: "change " + id,
		Depends: deps,
		Files: []FilePatch{{
			Path: "f.txt",
			Hunks: []Hunk{{
				OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
				Lines: []HunkLine{{OpDel, old}, {OpAdd, neu}},
			}},
		}},
	}
}

func TestEngine_DryRunAndApply_IdenticalConclusions(t *testing.T) {
	// 三个串行依赖、依次改行的变更。
	base := map[string]string{"f.txt": "a\nb\nc"}
	c1 := buildReplaceChange("c1", "a", "A")
	c2 := buildReplaceChange("c2", "A", "AA", "c1")
	c3 := buildReplaceChange("c3", "AA", "AAA", "c2")
	set := []Change{c1, c2, c3}

	// 预演
	dryWS := NewMemWorkspace(base)
	dryLog := &bufLogger{}
	dryReport, err := New(dryWS, dryLog).Plan(set)
	if err != nil {
		t.Fatal(err)
	}
	// 预演不得写盘
	if got := dryWS.Snapshot()["f.txt"]; got != "a\nb\nc" {
		t.Fatalf("dry-run 不得修改基准，得到 %q", got)
	}
	if st, _ := dryWS.LoadState(); st != nil {
		t.Fatal("dry-run 不得写入续跑状态")
	}

	// 应用
	applyWS := NewMemWorkspace(base)
	applyLog := &bufLogger{}
	applyReport, err := New(applyWS, applyLog).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if got := applyWS.Snapshot()["f.txt"]; got != "AAA\nb\nc" {
		t.Fatalf("apply 结果错误: %q", got)
	}

	assertSameConclusions(t, dryReport, applyReport)
	if want := []string{"c1", "c2", "c3"}; !eqStrings(dryReport.AppliedIDs(), want) {
		t.Fatalf("dry-run applied=%v", dryReport.AppliedIDs())
	}
}

func TestEngine_ConflictStopsAndDoesNotRollBack(t *testing.T) {
	// c1 能干净应用；c2 期望的旧内容在基准中不存在 → 冲突；
	// c3 依赖 c2，必须不被应用；c1 必须保持落盘、不回退、不重复应用。
	base := map[string]string{"f.txt": "a\nb\nc"}
	c1 := buildReplaceChange("c1", "a", "A")
	c2 := buildReplaceChange("c2", "ZZZ", "X", "c1") // 上下文找不到
	c3 := buildReplaceChange("c3", "X", "Y", "c2")
	set := []Change{c1, c2, c3}

	ws := NewMemWorkspace(base)
	log := &bufLogger{}
	rep, err := New(ws, log).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep.AppliedIDs(), []string{"c1"}) {
		t.Fatalf("仅 c1 应应用，得到 %v", rep.AppliedIDs())
	}
	if !eqStrings(rep.ConflictIDs(), []string{"c2"}) {
		t.Fatalf("仅 c2 应冲突，得到 %v", rep.ConflictIDs())
	}
	// c3 既不在 applied 也不在 conflict：按设计在冲突点停止，绝不静默丢弃——
	// 它未出现在结论中即表示“尚未处理”，续跑时会从该位置继续。
	if len(rep.Results) != 2 {
		t.Fatalf("冲突后应停止处理后续变更，得到 %d 条结论", len(rep.Results))
	}
	if got := ws.Snapshot()["f.txt"]; got != "A\nb\nc" {
		t.Fatalf("已应用的 c1 不得回退，得到 %q", got)
	}
	// 冲突定位必须包含位置与来源
	c2res := rep.Results[1]
	if len(c2res.Conflicts) != 1 {
		t.Fatalf("c2 应有 1 个冲突定位，得到 %d", len(c2res.Conflicts))
	}
	loc := c2res.Conflicts[0]
	if loc.Path != "f.txt" || loc.Line < 1 {
		t.Fatalf("冲突位置不完整: %+v", loc)
	}
	if !strings.Contains(log.String(), "c2") || !strings.Contains(log.String(), "f.txt") {
		t.Fatalf("日志必须打印变更标识与冲突位置，实际:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "判定顺序") {
		t.Fatalf("日志必须打印判定依据（顺序），实际:\n%s", log.String())
	}

	// 对同构的全新基准做预演：同一初始状态（无续跑记录）下，
	// dry-run 与 apply 的 applied/conflict 集合必须完全一致。
	freshRep, err := New(NewMemWorkspace(base), &bufLogger{}).Plan(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(freshRep.ConflictIDs(), rep.ConflictIDs()) {
		t.Fatalf("同初始状态下冲突集合不一致: dry=%v apply=%v",
			freshRep.ConflictIDs(), rep.ConflictIDs())
	}
	if !eqStrings(freshRep.AppliedIDs(), rep.AppliedIDs()) {
		t.Fatalf("同初始状态下应用集合不一致: dry=%v apply=%v",
			freshRep.AppliedIDs(), rep.AppliedIDs())
	}

	// 在已 apply 的现场再做 dry-run：c1 必须作为 skipped 被识别（读状态，不写）。
	onSiteRep, err := New(ws, &bufLogger{}).Plan(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(onSiteRep.SkippedIDs(), []string{"c1"}) {
		t.Fatalf("已应用现场预演应识别 c1 为 skipped，得到 %v", onSiteRep.SkippedIDs())
	}
	if !eqStrings(onSiteRep.ConflictIDs(), []string{"c2"}) {
		t.Fatalf("已应用现场预演仍应在 c2 处冲突，得到 %v", onSiteRep.ConflictIDs())
	}
}

func TestEngine_RejectsAreDistinguishable(t *testing.T) {
	base := map[string]string{"f.txt": "a"}

	// 重复 ID
	dup := []Change{buildReplaceChange("x", "a", "1"), buildReplaceChange("x", "b", "2")}
	_, err := New(NewMemWorkspace(base), nil).Apply(dup)
	if rejectReason(err) != RejectDuplicate {
		t.Fatalf("期望 duplicate，得到 %v", err)
	}

	// 缺少前置
	missing := []Change{buildReplaceChange("x", "a", "1", "ghost")}
	_, err = New(NewMemWorkspace(base), nil).Apply(missing)
	if rejectReason(err) != RejectMissingDep {
		t.Fatalf("期望 missing-dependency，得到 %v", err)
	}

	// 顺序不可确定：同文件无依赖
	undec := []Change{buildReplaceChange("x", "a", "1"), buildReplaceChange("y", "a", "2")}
	_, err = New(NewMemWorkspace(base), nil).Apply(undec)
	if rejectReason(err) != RejectOrderUndecidable {
		t.Fatalf("期望 order-undecidable，得到 %v", err)
	}
}

func assertSameConclusions(t *testing.T, a, b *Report) {
	t.Helper()
	if !eqStrings(a.Order, b.Order) {
		t.Fatalf("顺序不一致: %v vs %v", a.Order, b.Order)
	}
	if !eqStrings(a.AppliedIDs(), b.AppliedIDs()) {
		t.Fatalf("applied 集合不一致: %v vs %v", a.AppliedIDs(), b.AppliedIDs())
	}
	if !eqStrings(a.SkippedIDs(), b.SkippedIDs()) {
		t.Fatalf("skipped 集合不一致: %v vs %v", a.SkippedIDs(), b.SkippedIDs())
	}
	if !eqStrings(a.ConflictIDs(), b.ConflictIDs()) {
		t.Fatalf("conflict 集合不一致: %v vs %v", a.ConflictIDs(), b.ConflictIDs())
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
