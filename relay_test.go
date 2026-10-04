package integrator

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// relayBase 是接力场景共用的基准文件：第 2 行是将被接力演进的版本号。
func relayBase() []string { return []string{"head", "V1", "tail"} }

// relayChain 构造 n 条接力变更：第 i 条把第 2 行从 "Vi" 改为 "V(i+1)"，
// 每条依赖前一条，全部落在同一基准行上。
func relayChain(n int) []RawChange {
	var out []RawChange
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("r%02d", i)
		var deps []string
		if i > 1 {
			deps = []string{fmt.Sprintf("r%02d", i-1)}
		}
		out = append(out, RawChange{
			ID: id, DependsOn: deps,
			Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
				OldBlock: []string{fmt.Sprintf("V%d", i)},
				NewBlock: []string{fmt.Sprintf("V%d", i+1)}}},
		})
	}
	return out
}

func relayWant(final int) []string {
	return []string{"head", fmt.Sprintf("V%d", final), "tail"}
}

// TestRelay_ChainAppliesInOrder 同一条依赖链在同一处接力演进：
// 每条变更依次成功应用，最终内容等于最后一步的结果。
func TestRelay_ChainAppliesInOrder(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", relayBase())
	eng, logBuf := newTestEngine(t, ws)

	rep, err := eng.Apply(relayChain(4))
	if err != nil {
		t.Fatalf("relay chain must apply cleanly: %v", err)
	}
	if !rep.Completed || len(rep.Outcomes) != 4 {
		t.Fatalf("report = %+v", rep)
	}
	for i, o := range rep.Outcomes {
		wantID := fmt.Sprintf("r%02d", i+1)
		if o.ChangeID != wantID || o.Status != StatusApplied {
			t.Fatalf("outcome[%d] = %+v, want %s applied", i, o, wantID)
		}
	}
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, relayWant(5)) {
		t.Fatalf("final content = %v, want %v (last step wins)", got, relayWant(5))
	}
	log := logBuf.String()
	t.Logf("RELAY CHAIN log:\n%s", log)
	// 日志必须能看出每条变更的标识与接力判定依据。
	for _, id := range []string{"r02", "r03", "r04"} {
		if !strings.Contains(log, "change="+id+" ") || !strings.Contains(log, "decision=relay") {
			t.Fatalf("log must show relay basis for %s:\n%s", id, log)
		}
	}
	if !strings.Contains(log, "overlaps=[r01]") {
		t.Fatalf("log must name the relay source:\n%s", log)
	}
}

// TestRelay_LongChainStableAndRepeatable 几十条链路稳定成立；
// 反复提交时已全部跳过、内容不变、结论稳定。
func TestRelay_LongChainStableAndRepeatable(t *testing.T) {
	const n = 30
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", relayBase())
	eng, logBuf := newTestEngine(t, ws)

	rep, err := eng.Apply(relayChain(n))
	if err != nil {
		t.Fatalf("long relay chain: %v", err)
	}
	if !rep.Completed || len(rep.Outcomes) != n {
		t.Fatalf("outcomes = %d, want %d", len(rep.Outcomes), n)
	}
	for _, o := range rep.Outcomes {
		if o.Status != StatusApplied {
			t.Fatalf("change %s status = %s, want applied", o.ChangeID, o.Status)
		}
	}
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, relayWant(n+1)) {
		t.Fatalf("final content = %v, want %v", got, relayWant(n+1))
	}

	// 反复提交：全部识别为已应用并跳过，工作区字节级不变。
	snapshot := ws.Snapshot()
	for round := 0; round < 3; round++ {
		rep, err := eng.Apply(relayChain(n))
		if err != nil {
			t.Fatalf("resubmission round %d: %v", round, err)
		}
		for _, o := range rep.Outcomes {
			if o.Status != StatusSkipped {
				t.Fatalf("round %d: change %s status = %s, want skipped", round, o.ChangeID, o.Status)
			}
		}
		if !reflect.DeepEqual(ws.Snapshot(), snapshot) {
			t.Fatalf("round %d: workspace mutated on resubmission", round)
		}
	}
	t.Logf("LONG CHAIN log (tail):\n%s", lastLines(logBuf.String(), 8))
}

// TestRelay_IndependentOverlapStillConflicts 互不依赖的变更改到同一处仍判冲突，
// 并给出具体位置与来源；已接力的部分保留不回退。
func TestRelay_IndependentOverlapStillConflicts(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", relayBase())
	eng, logBuf := newTestEngine(t, ws)

	set := append(relayChain(2), RawChange{
		ID: "x", // 无依赖，独立改同一行
		Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"V1"}, NewBlock: []string{"X1"}}},
	})
	rep, err := eng.Apply(set)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConflictError", err)
	}
	var rm *RelayMismatchError
	if errors.As(err, &rm) {
		t.Fatalf("independent overlap must be a conflict, not a relay mismatch: %v", err)
	}
	if ce.Conflict.ChangeID != "x" || ce.Conflict.AgainstID != "r01" {
		t.Fatalf("conflict parties = %s vs %s, want x vs r01",
			ce.Conflict.ChangeID, ce.Conflict.AgainstID)
	}
	if ce.Conflict.Path != "f.txt" || ce.Conflict.BaseStart != 1 || ce.Conflict.BaseEnd != 2 {
		t.Fatalf("conflict location = %s [%d,%d), want f.txt [1,2)",
			ce.Conflict.Path, ce.Conflict.BaseStart, ce.Conflict.BaseEnd)
	}
	// 接力链 r01->r02 已干净应用并保留。
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, relayWant(3)) {
		t.Fatalf("file after conflict = %v, want %v (relay chain kept)", got, relayWant(3))
	}
	last := rep.Outcomes[len(rep.Outcomes)-1]
	if last.ChangeID != "x" || last.Status != StatusConflicted {
		t.Fatalf("last outcome = %+v, want x conflicted", last)
	}
	t.Logf("INDEPENDENT OVERLAP log:\n%s", logBuf.String())
	if !strings.Contains(logBuf.String(), "decision=conflicted") ||
		!strings.Contains(logBuf.String(), "against=r01") {
		t.Fatalf("log must show conflict source:\n%s", logBuf.String())
	}
}

// TestRelay_PreconditionMismatchIsStale 声明了依赖、但预期前置内容与实际落地
// 结果对不上：给出与冲突可区分的 stale 结论，不静默跳过、不换位置套用。
func TestRelay_PreconditionMismatchIsStale(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", relayBase())
	eng, logBuf := newTestEngine(t, ws)

	set := []RawChange{
		{ID: "r01", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"V1"}, NewBlock: []string{"V2"}}}},
		// 依赖 r01，但预期前置内容不是 r01 落地后的 "V2"。
		{ID: "r02", DependsOn: []string{"r01"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"V9"}, NewBlock: []string{"V3"}}}},
	}
	rep, err := eng.Apply(set)
	var rm *RelayMismatchError
	if !errors.As(err, &rm) {
		t.Fatalf("err = %v, want *RelayMismatchError", err)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("relay precondition mismatch must not be reported as conflict: %v", err)
	}
	if rm.ChangeID != "r02" || rm.Path != "f.txt" || rm.Anchor != 2 {
		t.Fatalf("mismatch = %+v, want change r02 at f.txt anchor 2", rm)
	}
	if len(rep.Outcomes) != 2 ||
		rep.Outcomes[0].Status != StatusApplied ||
		rep.Outcomes[1].ChangeID != "r02" || rep.Outcomes[1].Status != StatusStale {
		t.Fatalf("outcomes = %+v, want r01 applied + r02 stale", rep.Outcomes)
	}
	// 不得换到别的位置套用：r01 的结果保持原样，r02 未写入任何内容。
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, relayWant(2)) {
		t.Fatalf("file after stale = %v, want %v (r01 kept, r02 not applied)", got, relayWant(2))
	}
	log := logBuf.String()
	t.Logf("STALE log:\n%s", log)
	if !strings.Contains(log, "change=r02") || !strings.Contains(log, "decision=stale") ||
		!strings.Contains(log, "path=f.txt") {
		t.Fatalf("log must show stale decision with id and location:\n%s", log)
	}
}

// TestRelay_RehearseApplyIdentical 接力场景下预演与应用结论完全一致：
// 分别覆盖接力成功与前置对不上两种结局。
func TestRelay_RehearseApplyIdentical(t *testing.T) {
	cases := map[string][]RawChange{
		"success": relayChain(4),
		"stale": {
			{ID: "r01", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
				OldBlock: []string{"V1"}, NewBlock: []string{"V2"}}}},
			{ID: "r02", DependsOn: []string{"r01"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
				OldBlock: []string{"V9"}, NewBlock: []string{"V3"}}}},
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			ws1 := NewMemoryWorkspace()
			ws1.SeedFile("f.txt", relayBase())
			eng1, log1 := newTestEngine(t, ws1)
			rep1, err1 := eng1.Rehearse(set)
			t.Logf("REHEARSE log:\n%s", log1.String())

			ws2 := NewMemoryWorkspace()
			ws2.SeedFile("f.txt", relayBase())
			eng2, log2 := newTestEngine(t, ws2)
			rep2, err2 := eng2.Apply(set)
			t.Logf("APPLY log:\n%s", log2.String())

			if (err1 == nil) != (err2 == nil) {
				t.Fatalf("error presence differs: rehearse=%v apply=%v", err1, err2)
			}
			if !sameConclusions(rep1, rep2) {
				t.Fatalf("conclusions differ:\nrehearse=%+v\napply=%+v", rep1.Outcomes, rep2.Outcomes)
			}
			// 预演不得落盘。
			if got, _, _ := ws1.Read("f.txt"); !reflect.DeepEqual(got, relayBase()) {
				t.Fatalf("rehearse must not mutate workspace: %v", got)
			}
		})
	}
}

// TestRelay_InterruptedResume 接力链中途中断后：已落地部分被识别并跳过，
// 剩余部分从正确状态继续接力，不重复应用、不漏变更、不改坏已有结果。
func TestRelay_InterruptedResume(t *testing.T) {
	const n = 6
	interruptBefore := "r04"

	run := func(t *testing.T, submit func() []RawChange) {
		dir := t.TempDir()
		ws := NewMemoryWorkspace()
		ws.SeedFile("f.txt", relayBase())

		var firstLog bytes.Buffer
		eng1 := newPersistentEngine(t, ws, dir, func(id string) error {
			if id == interruptBefore {
				return errors.New("simulated CI process killed")
			}
			return nil
		}, &firstLog)
		rep1, err := eng1.Apply(relayChain(n))
		if err == nil || rep1.Completed {
			t.Fatalf("first run must be interrupted: err=%v completed=%v", err, rep1.Completed)
		}
		for _, o := range rep1.Outcomes {
			if o.Status != StatusApplied {
				t.Fatalf("first run outcome = %+v, want applied", o)
			}
		}
		got, _, _ := ws.Read("f.txt")
		if !reflect.DeepEqual(got, relayWant(4)) {
			t.Fatalf("after interruption = %v, want %v (r01..r03 landed)", got, relayWant(4))
		}
		t.Logf("FIRST RUN log:\n%s", firstLog.String())

		var secondLog bytes.Buffer
		eng2 := newPersistentEngine(t, ws, dir, nil, &secondLog)
		rep2, err := eng2.Apply(submit())
		if err != nil {
			t.Fatalf("resume run: %v", err)
		}
		if !rep2.Completed {
			t.Fatalf("resume run must complete: %+v", rep2.Outcomes)
		}
		got, _, _ = ws.Read("f.txt")
		if !reflect.DeepEqual(got, relayWant(n+1)) {
			t.Fatalf("after resume = %v, want %v", got, relayWant(n+1))
		}
		t.Logf("RESUME log:\n%s", secondLog.String())
	}

	t.Run("resubmit full batch", func(t *testing.T) {
		run(t, func() []RawChange { return relayChain(n) })
	})

	t.Run("submit only remaining", func(t *testing.T) {
		run(t, func() []RawChange { return relayChain(n)[3:] })
	})
}

// lastLines 返回字符串的最后 n 行（日志截取用）。
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
