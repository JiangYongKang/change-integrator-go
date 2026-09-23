package integrator

import (
	"bytes"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T, ws Workspace) (*Engine, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	return NewEngine(ws, filepath.Join(t.TempDir(), "journal.json"), Options{Logger: logger}), &buf
}

func baseFile() []string { return []string{"L1", "L2", "L3", "L4", "L5"} }

// TestEngine_Conflict_OverlappingLines 同一基准行的两个改动必须冲突，
// 且给出冲突位置与来源；第一个变更已干净应用，不得回退。
func TestEngine_Conflict_OverlappingLines(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", baseFile())
	eng, logBuf := newTestEngine(t, ws)

	raw := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"L2", "L3"}, NewBlock: []string{"A2", "A3"}}}},
		{ID: "b", Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
			OldBlock: []string{"L3", "L4"}, NewBlock: []string{"B3", "B4"}}}},
	}
	rep, err := eng.Apply(raw)
	if err == nil {
		t.Fatalf("expected ConflictError, got nil; report=%+v", rep)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConflictError", err)
	}
	if ce.Conflict.ChangeID != "b" || ce.Conflict.AgainstID != "a" {
		t.Fatalf("conflict parties = %s vs %s, want b vs a", ce.Conflict.ChangeID, ce.Conflict.AgainstID)
	}
	if ce.Conflict.Path != "f.txt" || ce.Conflict.BaseStart != 2 || ce.Conflict.BaseEnd != 4 {
		t.Fatalf("conflict location = %s [%d,%d), want f.txt [2,4)",
			ce.Conflict.Path, ce.Conflict.BaseStart, ce.Conflict.BaseEnd)
	}
	if len(rep.Outcomes) != 2 ||
		rep.Outcomes[0].ChangeID != "a" || rep.Outcomes[0].Status != StatusApplied ||
		rep.Outcomes[1].ChangeID != "b" || rep.Outcomes[1].Status != StatusConflicted {
		t.Fatalf("outcomes = %+v", rep.Outcomes)
	}
	t.Logf("engine log:\n%s", logBuf.String())
	if !strings.Contains(logBuf.String(), "decision=conflicted") ||
		!strings.Contains(logBuf.String(), "against=a") ||
		!strings.Contains(logBuf.String(), "path=f.txt") {
		t.Fatalf("log must print change id, basis and conflict location:\n%s", logBuf.String())
	}
	// 已干净应用的 a 保留，不得回退。
	got, _, _ := ws.Read("f.txt")
	want := []string{"L1", "A2", "A3", "L4", "L5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("file after conflict = %v, want %v (a kept, no rollback)", got, want)
	}
}

// TestEngine_RehearseAndApply_IdenticalConclusions 预演与应用对同一变更集、
// 同一前置状态必须给出完全一致的结论。
func TestEngine_RehearseAndApply_IdenticalConclusions(t *testing.T) {
	mkSet := func() ([]RawChange, []string) {
		base := baseFile()
		return []RawChange{
			{ID: "a", DependsOn: nil, Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
				OldBlock: []string{"L1"}, NewBlock: []string{"A1"}}}},
			{ID: "b", DependsOn: []string{"a"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 4,
				OldBlock: []string{"L4", "L5"}, NewBlock: []string{"B4", "B5"}}}},
		}, base
	}

	// 预演
	ws1 := NewMemoryWorkspace()
	_, base := mkSet()
	ws1.SeedFile("f.txt", base)
	eng1, log1 := newTestEngine(t, ws1)
	set, _ := mkSet()
	rep1, err := eng1.Rehearse(set)
	if err != nil {
		t.Fatalf("rehearse: %v", err)
	}
	t.Logf("REHEARSE log:\n%s", log1.String())

	// 应用（独立的相同前置状态）
	ws2 := NewMemoryWorkspace()
	ws2.SeedFile("f.txt", append([]string(nil), base...))
	eng2, log2 := newTestEngine(t, ws2)
	rep2, err := eng2.Apply(set)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Logf("APPLY log:\n%s", log2.String())

	if !sameConclusions(rep1, rep2) {
		t.Fatalf("conclusions differ:\nrehearse=%+v\napply=%+v", rep1.Outcomes, rep2.Outcomes)
	}
	// 预演必须不落盘
	if got, _, _ := ws1.Read("f.txt"); !reflect.DeepEqual(got, baseFile()) {
		t.Fatalf("rehearse must not mutate workspace: %v", got)
	}
	// 应用必须落盘
	got, _, _ := ws2.Read("f.txt")
	want := []string{"A1", "L2", "L3", "B4", "B5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applied file = %v, want %v", got, want)
	}
}

// TestEngine_AdjacentRangesNoConflict 端点相接的两个改动不冲突。
func TestEngine_AdjacentRangesNoConflict(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", baseFile())
	eng, _ := newTestEngine(t, ws)
	raw := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
			OldBlock: []string{"L2"}, NewBlock: []string{"A2"}}}},
		{ID: "b", Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
			OldBlock: []string{"L3"}, NewBlock: []string{"B3"}}}},
	}
	rep, err := eng.Apply(raw)
	if err != nil {
		t.Fatalf("adjacent ranges must not conflict: %v", err)
	}
	for _, o := range rep.Outcomes {
		if o.Status != StatusApplied {
			t.Fatalf("outcome %s = %s, want applied", o.ChangeID, o.Status)
		}
	}
}

// TestEngine_SameInsertionPointConflicts 零长度纯插入插入点相同算冲突。
func TestEngine_SameInsertionPointConflicts(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", baseFile())
	eng, logBuf := newTestEngine(t, ws)
	raw := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
			OldBlock: nil, NewBlock: []string{"INS_A"}}}},
		{ID: "b", Files: []RawFileChange{{Path: "f.txt", OldStart: 3,
			OldBlock: nil, NewBlock: []string{"INS_B"}}}},
	}
	_, err := eng.Apply(raw)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	t.Logf("engine log:\n%s", logBuf.String())
}

// TestEngine_CreateSameFileConflicts 两个变更创建同一文件必须冲突。
func TestEngine_CreateSameFileConflicts(t *testing.T) {
	ws := NewMemoryWorkspace()
	eng, _ := newTestEngine(t, ws)
	raw := []RawChange{
		{ID: "a", Files: []RawFileChange{{Path: "new.txt", Create: true, NewBlock: []string{"A"}}}},
		{ID: "b", Files: []RawFileChange{{Path: "new.txt", Create: true, NewBlock: []string{"B"}}}},
	}
	_, err := eng.Apply(raw)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if ce.Conflict.AgainstID != "a" || ce.Conflict.ChangeID != "b" {
		t.Fatalf("conflict = %+v", ce.Conflict)
	}
}

// TestEngine_ContextMismatch 旧块在锚点和全文件都找不到时，返回可区分错误。
func TestEngine_ContextMismatch(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", baseFile())
	eng, _ := newTestEngine(t, ws)
	raw := []RawChange{{ID: "a", Files: []RawFileChange{{Path: "f.txt", OldStart: 2,
		OldBlock: []string{"WRONG"}, NewBlock: []string{"X"}}}}}
	if _, err := eng.Apply(raw); !errors.Is(err, ErrContextMismatch) {
		t.Fatalf("err = %v, want ErrContextMismatch", err)
	}
}

// TestEngine_RejectedSetDoesNotExecute 计划被拒绝时不得改动工作区。
func TestEngine_RejectedSetDoesNotExecute(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", baseFile())
	eng, _ := newTestEngine(t, ws)
	raw := []RawChange{
		{ID: "a", DependsOn: []string{"missing"}, Files: []RawFileChange{{Path: "f.txt", OldStart: 1,
			OldBlock: []string{"L1"}, NewBlock: []string{"X"}}}},
	}
	_, err := eng.Apply(raw)
	var re *RejectedError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want RejectedError", err)
	}
	if re.Rejections[0].Reason != RejectMissingDependency {
		t.Fatalf("reason = %s", re.Rejections[0].Reason)
	}
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, baseFile()) {
		t.Fatalf("rejected set must not mutate workspace: %v", got)
	}
}

func sameConclusions(a, b *Report) bool {
	if !reflect.DeepEqual(a.Order, b.Order) {
		return false
	}
	key := func(o Outcome) string { return string(o.Status) }
	if len(a.Outcomes) != len(b.Outcomes) {
		return false
	}
	for i := range a.Outcomes {
		if a.Outcomes[i].ChangeID != b.Outcomes[i].ChangeID ||
			key(a.Outcomes[i]) != key(b.Outcomes[i]) {
			return false
		}
		if (a.Outcomes[i].Conflict == nil) != (b.Outcomes[i].Conflict == nil) {
			return false
		}
	}
	return true
}
