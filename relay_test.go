package integrator

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// relayChain 构造一条 n 节的接力链：第 1 节把基准行 "v0" 改成 "v1"，
// 第 i 节依赖第 i-1 节，把 "v{i-1}" 改成 "v{i}"，全部落在同一锚点行。
func relayChain(n int, path string, anchor int) []RawChange {
	var out []RawChange
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("r%02d", i)
		var deps []string
		if i > 1 {
			deps = []string{fmt.Sprintf("r%02d", i-1)}
		}
		out = append(out, RawChange{
			ID:        id,
			DependsOn: deps,
			Files: []RawFileChange{{Path: path, OldStart: anchor,
				OldBlock: []string{fmt.Sprintf("v%d", i-1)},
				NewBlock: []string{fmt.Sprintf("v%d", i)}}},
		})
	}
	return out
}

// TestRelay_ChainAppliesInOrder 同一条依赖链接力修改同一处：
// 三条变更依次成功应用，最终内容等于最后一步的结果。
func TestRelay_ChainAppliesInOrder(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"head", "v0", "tail"})
	eng, logBuf := newTestEngine(t, ws)

	rep, err := eng.Apply(relayChain(3, "f.txt", 2))
	if err != nil {
		t.Fatalf("relay chain must apply cleanly: %v", err)
	}
	if !rep.Completed || len(rep.Outcomes) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	for _, o := range rep.Outcomes {
		if o.Status != StatusApplied {
			t.Fatalf("change %s status = %s, want applied", o.ChangeID, o.Status)
		}
	}
	got, _, _ := ws.Read("f.txt")
	want := []string{"head", "v3", "tail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("final content = %v, want %v (last step wins)", got, want)
	}
	t.Logf("RELAY CHAIN log:\n%s", logBuf.String())
	// 日志必须能看出每条变更的标识与接力判定依据
	for _, id := range []string{"r01", "r02", "r03"} {
		if !strings.Contains(logBuf.String(), "change="+id) {
			t.Fatalf("log missing change id %s:\n%s", id, logBuf.String())
		}
	}
	if !strings.Contains(logBuf.String(), "decision=relay") ||
		!strings.Contains(logBuf.String(), "basis=precondition_verified_at_anchor") {
		t.Fatalf("log missing relay basis:\n%s", logBuf.String())
	}
}

// TestRelay_LongChainStable 几十节的链路也要稳定成立；重复提交全部跳过且内容不变。
func TestRelay_LongChainStable(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"v0"})
	eng, logBuf := newTestEngine(t, ws)

	const n = 40
	set := relayChain(n, "f.txt", 1)
	rep, err := eng.Apply(set)
	if err != nil {
		t.Fatalf("long relay chain: %v", err)
	}
	if len(rep.Outcomes) != n {
		t.Fatalf("outcomes = %d, want %d", len(rep.Outcomes), n)
	}
	for _, o := range rep.Outcomes {
		if o.Status != StatusApplied {
			t.Fatalf("change %s status = %s", o.ChangeID, o.Status)
		}
	}
	got, _, _ := ws.Read("f.txt")
	if want := []string{fmt.Sprintf("v%d", n)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("final content = %v, want %v", got, want)
	}

	// 反复提交：全部识别为已应用并跳过，不得回退或重复应用。
	snapshot := ws.Snapshot()
	for round := 0; round < 3; round++ {
		rep2, err := eng.Apply(set)
		if err != nil {
			t.Fatalf("resubmission round %d: %v", round, err)
		}
		for _, o := range rep2.Outcomes {
			if o.Status != StatusSkipped {
				t.Fatalf("round %d change %s status = %s, want skipped", round, o.ChangeID, o.Status)
			}
		}
		if !reflect.DeepEqual(ws.Snapshot(), snapshot) {
			t.Fatalf("round %d mutated workspace on resubmission", round)
		}
	}
	t.Logf("LONG CHAIN log (tail):\n%s", lastLines(logBuf.String(), 8))
}

// TestRelay_PreconditionMismatch 声明了依赖、但预期前置内容与实际应用后的
// 内容对不上：必须给出与冲突可区分的 mismatch 失败，且前序变更保留不回退。
func TestRelay_PreconditionMismatch(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"head", "v0", "tail"})
	eng, logBuf := newTestEngine(t, ws)

	set := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"v0"}, NewBlock: []string{"v1"}}}},
		// b 依赖 a，但预期前置内容写错（应为 "v1" 却写成 "vX"）。
		{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"vX"}, NewBlock: []string{"v2"}}}},
	}
	rep, err := eng.Apply(set)
	if err == nil {
		t.Fatalf("expected RelayMismatchError, got nil")
	}
	// 必须与“两方抢同一处”的冲突可区分
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("err must not be ConflictError: %v", err)
	}
	var rm *RelayMismatchError
	if !errors.As(err, &rm) {
		t.Fatalf("err = %v, want *RelayMismatchError", err)
	}
	if !errors.Is(err, ErrRelayPrecondition) {
		t.Fatalf("err = %v, want errors.Is ErrRelayPrecondition", err)
	}
	if rm.ChangeID != "b" || rm.Path != "f.txt" || rm.Anchor != 2 {
		t.Fatalf("mismatch detail = %+v", rm)
	}
	if !reflect.DeepEqual(rm.Expected, []string{"vX"}) || !reflect.DeepEqual(rm.Actual, []string{"v1"}) {
		t.Fatalf("mismatch expected/actual = %q/%q", rm.Expected, rm.Actual)
	}
	if len(rep.Outcomes) != 2 || rep.Outcomes[0].Status != StatusApplied ||
		rep.Outcomes[1].Status != StatusMismatch {
		t.Fatalf("outcomes = %+v", rep.Outcomes)
	}
	// 不得静默跳过、不得换位置套用：a 的结果保留，b 未生效。
	got, _, _ := ws.Read("f.txt")
	if want := []string{"head", "v1", "tail"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("file after mismatch = %v, want %v", got, want)
	}
	t.Logf("MISMATCH log:\n%s", logBuf.String())
	if !strings.Contains(logBuf.String(), "decision=mismatch") ||
		!strings.Contains(logBuf.String(), "change=b") {
		t.Fatalf("log missing mismatch decision:\n%s", logBuf.String())
	}
}

// TestRelay_NoRelocateToOtherPosition 预期前置内容在文件别处唯一存在、
// 但不在锚点处：接力变更必须失败，不得换到别的位置套用。
func TestRelay_NoRelocateToOtherPosition(t *testing.T) {
	ws := NewMemoryWorkspace()
	// 第 2 行是 "v0"；"v1" 唯一存在于第 4 行。
	ws.SeedFile("f.txt", []string{"head", "v0", "mid", "v1", "tail"})
	eng, logBuf := newTestEngine(t, ws)

	set := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"v0"}, NewBlock: []string{"other"}}}},
		// b 依赖 a 且与 a 同锚点重叠（接力）；其旧块 "v1" 只在第 4 行唯一出现，
		// 宽松模式会换到第 4 行套用，接力严格模式必须拒绝。
		{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"v1"}, NewBlock: []string{"v2"}}}},
	}
	_, err := eng.Apply(set)
	var rm *RelayMismatchError
	if !errors.As(err, &rm) {
		t.Fatalf("err = %v, want *RelayMismatchError (no relocation)", err)
	}
	// 第 4 行的 "v1" 不得被改动；a 的结果保留。
	got, _, _ := ws.Read("f.txt")
	if want := []string{"head", "other", "mid", "v1", "tail"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relay must not relocate to another position: %v", got)
	}
	t.Logf("NO-RELOCATE log:\n%s", logBuf.String())
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestRelay_RehearseApplyIdentical 接力场景下预演与应用必须给出完全一致的结论，
// 且预演不得改动工作区。
func TestRelay_RehearseApplyIdentical(t *testing.T) {
	mk := func() (*MemoryWorkspace, []RawChange) {
		ws := NewMemoryWorkspace()
		ws.SeedFile("f.txt", []string{"head", "v0", "tail"})
		return ws, relayChain(4, "f.txt", 2)
	}

	ws1, set := mk()
	eng1, log1 := newTestEngine(t, ws1)
	rep1, err := eng1.Rehearse(set)
	if err != nil {
		t.Fatalf("rehearse: %v", err)
	}
	t.Logf("REHEARSE log:\n%s", log1.String())

	ws2, _ := mk()
	eng2, log2 := newTestEngine(t, ws2)
	rep2, err := eng2.Apply(set)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Logf("APPLY log:\n%s", log2.String())

	if !sameConclusions(rep1, rep2) {
		t.Fatalf("conclusions differ:\nrehearse=%+v\napply=%+v", rep1.Outcomes, rep2.Outcomes)
	}
	if got, _, _ := ws1.Read("f.txt"); !reflect.DeepEqual(got, []string{"head", "v0", "tail"}) {
		t.Fatalf("rehearse must not mutate workspace: %v", got)
	}
	if got, _, _ := ws2.Read("f.txt"); !reflect.DeepEqual(got, []string{"head", "v4", "tail"}) {
		t.Fatalf("apply result = %v", got)
	}

	// 失败路径同样一致：接力前置不匹配时，两种模式都给出 mismatch。
	bad := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"v0"}, NewBlock: []string{"v1"}}}},
		{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"vX"}, NewBlock: []string{"v2"}}}},
	}
	ws3, _ := mk()
	eng3, _ := newTestEngine(t, ws3)
	repR, errR := eng3.Rehearse(bad)
	ws4, _ := mk()
	eng4, _ := newTestEngine(t, ws4)
	repA, errA := eng4.Apply(bad)
	var rmR, rmA *RelayMismatchError
	if !errors.As(errR, &rmR) || !errors.As(errA, &rmA) {
		t.Fatalf("rehearse err = %v, apply err = %v, both want *RelayMismatchError", errR, errA)
	}
	if !sameConclusions(repR, repA) {
		t.Fatalf("mismatch conclusions differ:\nrehearse=%+v\napply=%+v", repR.Outcomes, repA.Outcomes)
	}
	if got, _, _ := ws3.Read("f.txt"); !reflect.DeepEqual(got, []string{"head", "v0", "tail"}) {
		t.Fatalf("rehearse mismatch must not mutate workspace: %v", got)
	}
}

// TestRelay_ResumeAfterInterruption 接力链路中途中断：再次提交同一批变更时，
// 已落地的部分识别并跳过，剩余部分从正确状态继续接力，最终内容正确。
func TestRelay_ResumeAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"v0"})

	var firstLog bytes.Buffer
	eng1 := newPersistentEngine(t, ws, dir, func(id string) error {
		if id == "r03" {
			return errors.New("simulated CI process killed")
		}
		return nil
	}, &firstLog)
	set := relayChain(5, "f.txt", 1)
	rep1, err := eng1.Apply(set)
	if err == nil || rep1.Completed {
		t.Fatalf("expected interruption, err=%v completed=%v", err, rep1.Completed)
	}
	statuses := map[string]Status{}
	for _, o := range rep1.Outcomes {
		statuses[o.ChangeID] = o.Status
	}
	if statuses["r01"] != StatusApplied || statuses["r02"] != StatusApplied || statuses["r03"] != "" {
		t.Fatalf("first run outcomes = %+v, want r01/r02 applied only", statuses)
	}
	t.Logf("FIRST RUN log:\n%s", firstLog.String())
	if got, _, _ := ws.Read("f.txt"); !reflect.DeepEqual(got, []string{"v2"}) {
		t.Fatalf("after interruption content = %v, want [v2]", got)
	}

	// 续跑：同一批变更再次提交。
	var secondLog bytes.Buffer
	eng2 := newPersistentEngine(t, ws, dir, nil, &secondLog)
	rep2, err := eng2.Apply(set)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Logf("RESUME log:\n%s", secondLog.String())
	if len(rep2.Outcomes) != 5 {
		t.Fatalf("resume outcomes = %d, want 5", len(rep2.Outcomes))
	}
	for i, o := range rep2.Outcomes {
		want := StatusApplied
		if i < 2 {
			want = StatusSkipped
		}
		if o.Status != want {
			t.Fatalf("resume outcome[%d] %s = %s, want %s", i, o.ChangeID, o.Status, want)
		}
	}
	if got, _, _ := ws.Read("f.txt"); !reflect.DeepEqual(got, []string{"v5"}) {
		t.Fatalf("after resume content = %v, want [v5]", got)
	}
	// 续跑日志必须能看出跳过依据与接力依据。
	if !strings.Contains(secondLog.String(), "change=r01") ||
		!strings.Contains(secondLog.String(), "decision=skipped") ||
		!strings.Contains(secondLog.String(), "basis=journal_record") {
		t.Fatalf("resume log missing skip basis:\n%s", secondLog.String())
	}
	if !strings.Contains(secondLog.String(), "decision=relay") {
		t.Fatalf("resume log missing relay basis:\n%s", secondLog.String())
	}

	// 再反复提交：全部跳过，接力结果不被改坏。
	snapshot := ws.Snapshot()
	rep3, err := eng2.Apply(set)
	if err != nil {
		t.Fatalf("resubmission: %v", err)
	}
	for _, o := range rep3.Outcomes {
		if o.Status != StatusSkipped {
			t.Fatalf("resubmission change %s = %s, want skipped", o.ChangeID, o.Status)
		}
	}
	if !reflect.DeepEqual(ws.Snapshot(), snapshot) {
		t.Fatalf("resubmission mutated workspace")
	}
}

// TestRelay_ResumeRemainingOnly 中断后只提交剩余变更：前置已在状态日志中，
// 接力关系仍然成立，从正确状态继续。
func TestRelay_ResumeRemainingOnly(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"v0"})

	var firstLog bytes.Buffer
	eng1 := newPersistentEngine(t, ws, dir, func(id string) error {
		if id == "r02" {
			return errors.New("kill")
		}
		return nil
	}, &firstLog)
	set := relayChain(4, "f.txt", 1)
	if _, err := eng1.Apply(set); err == nil {
		t.Fatalf("expected interruption")
	}

	var secondLog bytes.Buffer
	eng2 := newPersistentEngine(t, ws, dir, nil, &secondLog)
	rep, err := eng2.Apply(set[1:]) // 只提交 r02..r04
	if err != nil {
		t.Fatalf("resume remaining only: %v", err)
	}
	t.Logf("RESUME-REMAINING log:\n%s", secondLog.String())
	for _, o := range rep.Outcomes {
		if o.Status != StatusApplied {
			t.Fatalf("change %s = %s, want applied", o.ChangeID, o.Status)
		}
	}
	if got, _, _ := ws.Read("f.txt"); !reflect.DeepEqual(got, []string{"v4"}) {
		t.Fatalf("content = %v, want [v4]", got)
	}
}

// TestRelay_IndependentOverlapStillConflict 互不依赖的变更改到同一处仍判冲突；
// 依赖关系只豁免落在自己传递依赖闭包内的重叠。
func TestRelay_IndependentOverlapStillConflict(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"v0"})
	eng, logBuf := newTestEngine(t, ws)

	// a 与 x 互不依赖、改同一处 → 冲突（不受接力豁免影响）。
	set := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"v0"}, NewBlock: []string{"v1"}}}},
		{ID: "x", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"v0"}, NewBlock: []string{"vx"}}}},
	}
	_, err := eng.Apply(set)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if ce.Conflict.ChangeID != "x" || ce.Conflict.AgainstID != "a" {
		t.Fatalf("conflict = %+v", ce.Conflict)
	}
	t.Logf("INDEPENDENT CONFLICT log:\n%s", logBuf.String())
}

// TestRelay_RelayDoesNotShieldUnrelatedOverlap 接力变更若同时与无关变更重叠，
// 无关部分仍判冲突——接力豁免不得掩护“两方抢同一处”。
func TestRelay_RelayDoesNotShieldUnrelatedOverlap(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"v0", "keep"})
	eng, logBuf := newTestEngine(t, ws)

	set := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"v0"}, NewBlock: []string{"v1"}}}},
		// u 与 a 无依赖，改同一处。
		{ID: "u", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"v0"}, NewBlock: []string{"vu"}}}},
		// b 依赖 a（与 a 是接力），但它同时与无关的 u 重叠 → 仍判冲突。
		{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"v1"}, NewBlock: []string{"v2"}}}},
	}
	_, err := eng.Apply(set)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	// 首个冲突应是 u 对 a（u 拓扑序在 b 前）。
	if ce.Conflict.ChangeID != "u" || ce.Conflict.AgainstID != "a" {
		t.Fatalf("conflict = %+v, want u vs a", ce.Conflict)
	}
	t.Logf("NO-SHIELD log:\n%s", logBuf.String())
}
