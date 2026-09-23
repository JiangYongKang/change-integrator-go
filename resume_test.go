package integrator

import (
	"bytes"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"testing"
)

func newPersistentEngine(t *testing.T, ws Workspace, journalPath string, hook func(string) error, logBuf *bytes.Buffer) *Engine {
	t.Helper()
	return NewEngine(ws, filepath.Join(journalPath, "journal.json"),
		Options{Logger: log.New(logBuf, "", 0), BeforeChange: hook})
}

func resumeSet() []RawChange {
	return []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"L1"}, NewBlock: []string{"A1"}}}},
		{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
			OldBlock: []string{"L3"}, NewBlock: []string{"B3"}}}},
		{ID: "c", DependsOn: []string{"b"}, Files: []RawFileChange{{Path: "g.txt", OldStart: 1,
			OldBlock: []string{"G1"}, NewBlock: []string{"C1"}}}},
	}
}

// TestResume_InterruptedRunContinuesFromRightPosition 首次运行在 b 之前中断；
// 再次提交时 a 被识别并跳过，b、c 从正确位置继续，无重复应用。
func TestResume_InterruptedRunContinuesFromRightPosition(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3", "L4"})
	ws.SeedFile("g.txt", []string{"G1", "G2"})

	var firstLog bytes.Buffer
	eng1 := newPersistentEngine(t, ws, dir, func(id string) error {
		if id == "b" {
			return errors.New("simulated CI process killed")
		}
		return nil
	}, &firstLog)

	rep1, err := eng1.Apply(resumeSet())
	if err == nil {
		t.Fatalf("interrupted run must surface the interruption error")
	}
	if rep1.Completed {
		t.Fatalf("interrupted report must not be completed")
	}
	statuses := map[string]Status{}
	for _, o := range rep1.Outcomes {
		statuses[o.ChangeID] = o.Status
	}
	if statuses["a"] != StatusApplied || statuses["b"] != "" || statuses["c"] != "" {
		t.Fatalf("first run outcomes = %+v, want only a applied", statuses)
	}
	t.Logf("FIRST RUN log:\n%s", firstLog.String())

	// 中断后工作区只包含 a 的结果
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, []string{"A1", "L2", "L3", "L4"}) {
		t.Fatalf("after interruption f.txt = %v", got)
	}

	// 第二次运行：同一变更集再次提交，无故障注入
	var secondLog bytes.Buffer
	eng2 := newPersistentEngine(t, ws, dir, nil, &secondLog)
	rep2, err := eng2.Apply(resumeSet())
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	t.Logf("RESUME RUN log:\n%s", secondLog.String())
	if len(rep2.Outcomes) != 3 {
		t.Fatalf("resume outcomes = %d, want 3", len(rep2.Outcomes))
	}
	want := []struct {
		id     string
		status Status
	}{
		{"a", StatusSkipped},
		{"b", StatusApplied},
		{"c", StatusApplied},
	}
	for i, w := range want {
		if rep2.Outcomes[i].ChangeID != w.id || rep2.Outcomes[i].Status != w.status {
			t.Fatalf("resume outcome[%d] = %+v, want %s/%s",
				i, rep2.Outcomes[i], w.id, w.status)
		}
	}
	// 日志必须打印跳过判定依据
	if !containsAll(secondLog.String(), "change=a", "decision=skipped", "basis=journal_record") {
		t.Fatalf("resume log missing skip basis:\n%s", secondLog.String())
	}
	got, _, _ = ws.Read("f.txt")
	if !reflect.DeepEqual(got, []string{"A1", "L2", "B3", "L4"}) {
		t.Fatalf("after resume f.txt = %v", got)
	}
	gotG, _, _ := ws.Read("g.txt")
	if !reflect.DeepEqual(gotG, []string{"C1", "G2"}) {
		t.Fatalf("after resume g.txt = %v", gotG)
	}
}

// TestResume_RepeatedSubmissionSkipsAll 全部成功后再次提交同一变更集：
// 全部识别为已应用并跳过，工作区字节级不变。
func TestResume_RepeatedSubmissionSkipsAll(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3", "L4"})
	ws.SeedFile("g.txt", []string{"G1", "G2"})

	var logBuf bytes.Buffer
	eng := newPersistentEngine(t, ws, dir, nil, &logBuf)
	rep1, err := eng.Apply(resumeSet())
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	snapshot := ws.Snapshot()

	rep2, err := eng.Apply(resumeSet())
	if err != nil {
		t.Fatalf("resubmission: %v", err)
	}
	for _, o := range rep2.Outcomes {
		if o.Status != StatusSkipped {
			t.Fatalf("change %s status = %s, want skipped", o.ChangeID, o.Status)
		}
	}
	if !reflect.DeepEqual(ws.Snapshot(), snapshot) {
		t.Fatalf("workspace mutated on resubmission")
	}
	if !rep1.Completed || !rep2.Completed {
		t.Fatalf("both runs should be completed")
	}
	t.Logf("RESUBMIT log:\n%s", logBuf.String())
}

// TestResume_FingerprintMismatchRejected 同一 ID 但内容被篡改时明确报错，不得静默按新版本应用。
func TestResume_FingerprintMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2"})

	var logBuf bytes.Buffer
	eng := newPersistentEngine(t, ws, dir, nil, &logBuf)
	set1 := []RawChange{{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
		OldBlock: []string{"L1"}, NewBlock: []string{"A1"}}}}}
	if _, err := eng.Apply(set1); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	tampered := []RawChange{{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
		OldBlock: []string{"L1"}, NewBlock: []string{"DIFFERENT"}}}}}
	if _, err := eng.Apply(tampered); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
}

// TestResume_RemainingConflictStillDetected 中断后续跑时，剩余变更与已应用内容
// 的冲突仍必须被检出，且已应用变更保持跳过。
func TestResume_RemainingConflictStillDetected(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3", "L4"})

	var firstLog bytes.Buffer
	eng1 := newPersistentEngine(t, ws, dir, func(id string) error {
		if id == "b" {
			return errors.New("kill")
		}
		return nil
	}, &firstLog)
	set := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"L1", "L2"}, NewBlock: []string{"A1", "A2"}}}},
		{ID: "b", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"L2", "L3"}, NewBlock: []string{"B2", "B3"}}}},
	}
	if _, err := eng1.Apply(set); err == nil {
		t.Fatalf("expected interruption")
	}

	var secondLog bytes.Buffer
	eng2 := newPersistentEngine(t, ws, dir, nil, &secondLog)
	rep2, err := eng2.Apply(set)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError after resume", err)
	}
	if rep2.Outcomes[0].ChangeID != "a" || rep2.Outcomes[0].Status != StatusSkipped {
		t.Fatalf("a must be skipped on resume: %+v", rep2.Outcomes[0])
	}
	if rep2.Outcomes[1].Status != StatusConflicted ||
		ce.Conflict.ChangeID != "b" || ce.Conflict.AgainstID != "a" {
		t.Fatalf("b must conflict against a after resume: %+v", rep2.Outcomes)
	}
	t.Logf("RESUME-CONFLICT log:\n%s", secondLog.String())
}

// TestResume_RehearseRepeatedStable 预演对带历史状态的同一变更集重复运行，
// 结论必须稳定（预演不写状态，故每次全部 applied）。
func TestResume_RehearseRepeatedStable(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3"})

	var logBuf bytes.Buffer
	eng := newPersistentEngine(t, ws, dir, nil, &logBuf)
	set := resumeSet()[0:1]
	set[0].Files = []RawFileChange{{Path: "f.txt", OldStart: 1,
		OldBlock: []string{"L1"}, NewBlock: []string{"A1"}}}
	set[0].DependsOn = nil
	r1, err := eng.Rehearse(set)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eng.Rehearse(set)
	if err != nil {
		t.Fatal(err)
	}
	if !sameConclusions(r1, r2) {
		t.Fatalf("rehearsal conclusions not stable:\n%+v\n%+v", r1.Outcomes, r2.Outcomes)
	}
	// 预演不得创建状态文件
	if j, err := LoadJournal(filepath.Join(dir, "journal.json")); err != nil || len(j.Applied) != 0 {
		t.Fatalf("rehearse must not persist journal: %v %+v", err, j)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestResume_PartialSubmissionWithAppliedPrereq 首次只应用 a；之后只提交 b，
// 其前置 a 已在此前运行中应用，应当允许（前置视为已满足），而不是误报缺前置。
func TestResume_PartialSubmissionWithAppliedPrereq(t *testing.T) {
	dir := t.TempDir()
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3"})

	var logBuf bytes.Buffer
	eng := newPersistentEngine(t, ws, dir, nil, &logBuf)
	first := []RawChange{{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
		OldBlock: []string{"L1"}, NewBlock: []string{"A1"}}}}}
	if _, err := eng.Apply(first); err != nil {
		t.Fatalf("apply a: %v", err)
	}

	second := []RawChange{{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
		OldBlock: []string{"L3"}, NewBlock: []string{"B3"}}}}}
	rep, err := eng.Apply(second)
	if err != nil {
		t.Fatalf("partial submission with applied prereq must succeed: %v", err)
	}
	if rep.Outcomes[0].ChangeID != "b" || rep.Outcomes[0].Status != StatusApplied {
		t.Fatalf("outcome = %+v, want b applied", rep.Outcomes[0])
	}
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, []string{"A1", "L2", "B3"}) {
		t.Fatalf("file = %v", got)
	}
	t.Logf("PARTIAL RESUBMIT log:\n%s", logBuf.String())

	// 前置既不在集合中也不在日志中时，仍必须被拒绝。
	third := []RawChange{{ID: "c", DependsOn: []string{"never-seen"}, Files: []RawFileChange{{Path: "f.txt",
		OldStart: 1, OldBlock: []string{"A1"}, NewBlock: []string{"C1"}}}}}
	_, err = eng.Apply(third)
	var re *RejectedError
	if !errors.As(err, &re) || re.Rejections[0].Reason != RejectMissingDependency {
		t.Fatalf("err = %v, want missing_dependency for never-seen prereq", err)
	}
}
