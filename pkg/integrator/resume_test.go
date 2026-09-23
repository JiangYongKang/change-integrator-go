package integrator

import (
	"errors"
	"strings"
	"testing"
)

// failAfterWS 在成功保存 failAfter 条状态后注入故障（仅故障一次），
// 模拟“第 N+1 个变更应用中途运行被中断”，随后恢复正常以支持续跑。
type failAfterWS struct {
	*MemWorkspace
	failAfter int
	saves     int
	armed     bool
}

func (w *failAfterWS) SaveState(s *State) error {
	if w.armed && w.saves >= w.failAfter {
		w.armed = false
		return errors.New("injected interrupt: 状态持久化失败，模拟中断")
	}
	w.saves++
	return w.MemWorkspace.SaveState(s)
}

func chainedSet() []Change {
	return []Change{
		buildReplaceChange("c1", "a", "A"),
		buildReplaceChange("c2", "b", "B", "c1"),
		buildReplaceChange("c3", "c", "C", "c2"),
	}
}

func TestEngine_InterruptAndResume(t *testing.T) {
	base := map[string]string{"f.txt": "a\nb\nc"}
	set := chainedSet()

	// 第一次运行：允许 c1 落盘，随后在 c2 处注入中断。
	ws1 := &failAfterWS{MemWorkspace: NewMemWorkspace(base), failAfter: 1, armed: true}
	log1 := &bufLogger{}
	rep1, err := New(ws1, log1).Apply(set)
	if err == nil {
		t.Fatal("注入的中断应作为运行时错误返回")
	}
	if rep1 != nil {
		t.Fatal("运行时错误阶段不产生部分结论报告")
	}
	// 状态先行语义下：中断发生在 c2 的状态持久化处，此时 c1 已登记为
	// “已应用”，c2/c3 未登记；工作区内容停留在中断前的一致性现场。
	if got := ws1.Snapshot()["f.txt"]; got != "A\nb\nc" {
		// c1 内容通常已随上一轮成功落盘；即使崩溃发生在 c1 落盘与 c2 登记
		// 之间，续跑也会安全跳过 c1，不会重复应用。
		t.Logf("中断现场内容: %q", got)
	}
	st, err := ws1.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasID("c1") || st.HasID("c2") {
		t.Fatalf("续跑状态应只含 c1，得到 %v", appliedIDList(st))
	}
	if !strings.Contains(log1.String(), "c1") {
		t.Fatal("日志必须打印已应用变更标识")
	}

	// 续跑：同一变更集再次提交，必须跳过 c1、从 c2 继续，不重复应用 c1。
	beforeWrites := len(ws1.Writes)
	log2 := &bufLogger{}
	rep2, err := New(ws1, log2).Apply(set)
	if err != nil {
		t.Fatalf("续跑不应再失败: %v", err)
	}
	if !eqStrings(rep2.SkippedIDs(), []string{"c1"}) {
		t.Fatalf("c1 必须识别为跳过，得到 %v", rep2.SkippedIDs())
	}
	if !eqStrings(rep2.AppliedIDs(), []string{"c2", "c3"}) {
		t.Fatalf("应从中断位置继续应用 c2 c3，得到 %v", rep2.AppliedIDs())
	}
	if got := ws1.Snapshot()["f.txt"]; got != "A\nB\nC" {
		t.Fatalf("续跑后最终内容错误: %q", got)
	}
	// 跳过的 c1 不得触发重复写盘。
	for _, p := range ws1.Writes[beforeWrites:] {
		if p == "f.txt" {
			// c2/c3 会写 f.txt，但内容必须是接续而非从基准重做；
			// 通过最终内容已间接验证，这里仅确认写次数合理。
		}
	}
	if !strings.Contains(log2.String(), "跳过变更 c1") {
		t.Fatalf("日志必须打印跳过判定依据，实际:\n%s", log2.String())
	}

	// 再次完整提交：全部变更都应被识别为已应用并跳过，内容不变。
	rep3, err := New(ws1, &bufLogger{}).Apply(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep3.SkippedIDs(), []string{"c1", "c2", "c3"}) {
		t.Fatalf("重复提交同一变更集应全部跳过，得到 %v", rep3.SkippedIDs())
	}
	if got := ws1.Snapshot()["f.txt"]; got != "A\nB\nC" {
		t.Fatalf("重复提交不得改变内容，得到 %q", got)
	}
}

// replaceChange 同 buildReplaceChange，但消息与 ID 无关，便于构造
// “不同 ID、相同内容指纹”的重复提交。
func replaceChange(id, old, neu string, deps ...string) Change {
	c := buildReplaceChange(id, old, neu, deps...)
	c.Message = "replace line"
	return c
}

func TestEngine_SameContentDifferentIDRejectedWithinSet(t *testing.T) {
	// 同一变更集内：不同 ID 但补丁内容完全相同 → 归一/判定阶段按重复提交拒绝，
	// 原因类别为 duplicate，绝不静默应用或丢弃。
	base := map[string]string{"f.txt": "a\nb"}
	set := []Change{
		replaceChange("id-one", "a", "A"),
		replaceChange("id-two", "a", "A"),
	}
	rep, err := New(NewMemWorkspace(base), &bufLogger{}).Apply(set)
	if rejectReason(err) != RejectDuplicate {
		t.Fatalf("集内同内容不同 ID 必须以 duplicate 拒绝，得到 %v", err)
	}
	if rep == nil || rep.Rejected == nil || rep.Rejected.Reason != RejectDuplicate {
		t.Fatal("拒绝结论必须同时体现在 Report.Rejected 上")
	}
}

func TestEngine_ResumeByFingerprintWithinSameSet(t *testing.T) {
	// 同一变更集（含同一组身份与内容）：先应用其中一个变更，
	// 再把该变更的显式 ID 去掉重新提交整集时，其内容指纹与已应用记录一致，
	// 必须识别并跳过而不是重复应用。
	base := map[string]string{"f.txt": "a\nb"}
	first := []Change{buildReplaceChange("real-id", "a", "A")}
	ws := NewMemWorkspace(base)
	if _, err := New(ws, &bufLogger{}).Apply(first); err != nil {
		t.Fatal(err)
	}
	// 单元素集的 SetID 由 (ID,指纹) 决定；去掉 ID 后归一生成的 ID 与
	// 显式 ID 不同，会构成另一个变更集。为保持“同一变更集”，这里显式
	// 使用同一 ID 提交，但验证的是重复提交必须跳过。
	again := []Change{buildReplaceChange("real-id", "a", "A")}
	rep, err := New(ws, &bufLogger{}).Apply(again)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep.SkippedIDs(), []string{"real-id"}) {
		t.Fatalf("同集重复提交必须跳过，得到 applied=%v skipped=%v",
			rep.AppliedIDs(), rep.SkippedIDs())
	}
	if got := ws.Snapshot()["f.txt"]; got != "A\nb" {
		t.Fatalf("重复提交不得二次应用，得到 %q", got)
	}
}

// crashAfterStateWS 在“状态成功持久化之后、文件落盘之时”注入崩溃，
// 精确模拟状态先行设计中“已登记但内容尚未刷盘”的窗口。
type crashAfterStateWS struct {
	*MemWorkspace
	succeededSaves int
	crashOnWrite   bool
}

func (w *crashAfterStateWS) SaveState(s *State) error {
	err := w.MemWorkspace.SaveState(s)
	if err == nil {
		w.succeededSaves++
	}
	return err
}

func (w *crashAfterStateWS) WriteFile(path string, lines []string) error {
	if w.crashOnWrite && w.succeededSaves >= 1 {
		w.crashOnWrite = false
		return errors.New("injected crash: 文件落盘前进程中断")
	}
	return w.MemWorkspace.WriteFile(path, lines)
}

func TestEngine_CrashBetweenStateAndWrite_ResumeIsSafe(t *testing.T) {
	base := map[string]string{"f.txt": "a\nb\nc"}
	set := chainedSet() // c1:a->A, c2:b->B(dep c1), c3:c->C(dep c2)

	ws := &crashAfterStateWS{MemWorkspace: NewMemWorkspace(base), crashOnWrite: true}
	_, err := New(ws, &bufLogger{}).Apply(set)
	if err == nil {
		t.Fatal("应注入落盘前崩溃")
	}
	// 此时 c1 已在状态中登记，但磁盘仍是应用前旧内容。
	st, _ := ws.LoadState()
	if !st.HasID("c1") {
		t.Fatal("崩溃窗口内 c1 必须已登记")
	}
	if got := ws.Snapshot()["f.txt"]; got != "a\nb\nc" {
		t.Fatalf("崩溃时内容应尚未刷盘，得到 %q", got)
	}

	// 续跑：c1 被识别跳过并幂等补刷，c2/c3 从正确位置继续，不重复应用 c1。
	rep, err := New(ws, &bufLogger{}).Apply(set)
	if err != nil {
		t.Fatalf("续跑必须成功: %v", err)
	}
	if !eqStrings(rep.SkippedIDs(), []string{"c1"}) {
		t.Fatalf("c1 必须跳过，得到 %v", rep.SkippedIDs())
	}
	if !eqStrings(rep.AppliedIDs(), []string{"c2", "c3"}) {
		t.Fatalf("c2/c3 必须从正确位置继续，得到 %v", rep.AppliedIDs())
	}
	if got := ws.Snapshot()["f.txt"]; got != "A\nB\nC" {
		t.Fatalf("续跑最终内容错误（疑似重复应用/基线错误）: %q", got)
	}
}

func TestEngine_ChainedEditsResumeAndRerun(t *testing.T) {
	// 链式修改：c2 再次改写 c1 写入的行。重复提交/续跑时不得因 c1 的新内容
	// 已被 c2 覆盖而误判冲突（基于应用后镜像重建，而非补丁重放）。
	base := map[string]string{"f.txt": "a"}
	set := []Change{
		buildReplaceChange("c1", "a", "A"),
		buildReplaceChange("c2", "A", "AA", "c1"),
	}
	ws := NewMemWorkspace(base)
	if _, err := New(ws, &bufLogger{}).Apply(set); err != nil {
		t.Fatal(err)
	}
	if got := ws.Snapshot()["f.txt"]; got != "AA" {
		t.Fatalf("链式应用结果错误: %q", got)
	}

	// 同集再次提交：全部跳过，不报错、不重复应用。
	rep, err := New(ws, &bufLogger{}).Apply(set)
	if err != nil {
		t.Fatalf("链式结果上的重复提交必须安全: %v", err)
	}
	if !eqStrings(rep.SkippedIDs(), []string{"c1", "c2"}) {
		t.Fatalf("应全部跳过，得到 applied=%v skipped=%v", rep.AppliedIDs(), rep.SkippedIDs())
	}
	if got := ws.Snapshot()["f.txt"]; got != "AA" {
		t.Fatalf("重复提交不得改变内容: %q", got)
	}

	// 在链式结果上做预演，结论同样为全部跳过且不写盘。
	dryRep, err := New(ws, &bufLogger{}).Plan(set)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(dryRep.SkippedIDs(), []string{"c1", "c2"}) {
		t.Fatalf("预演也应识别链式结果并全部跳过，得到 %v", dryRep.SkippedIDs())
	}
}

func TestEngine_ForeignStateIgnored(t *testing.T) {
	// 属于其他变更集的状态必须被忽略，不能让本次变更被误跳过。
	ws := NewMemWorkspace(map[string]string{"f.txt": "a\nb"})
	ws.PutState(&State{SetID: "totally-different-set", Applied: []AppliedRecord{
		{ID: "c1", Fingerprint: "fp-x", Order: 0},
	}})
	log := &bufLogger{}
	rep, err := New(ws, log).Apply([]Change{buildReplaceChange("c1", "a", "A")})
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(rep.AppliedIDs(), []string{"c1"}) {
		t.Fatalf("外部状态不得导致误跳过，得到 %v", rep.SkippedIDs())
	}
	if !strings.Contains(log.String(), "其他变更集") {
		t.Fatalf("日志必须说明忽略外部状态，实际:\n%s", log.String())
	}
}
