package integrator

import (
	"reflect"
	"testing"
)

func ctx(s string) HunkLine { return HunkLine{OpContext, s} }
func add(s string) HunkLine { return HunkLine{OpAdd, s} }
func del(s string) HunkLine { return HunkLine{OpDel, s} }

func TestApplyFileChange_SimpleReplace(t *testing.T) {
	fs := newFileState("f", []string{"a", "b", "c"}, true)
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 2, OldLines: 1, NewStart: 2, NewLines: 1,
		Lines: []HunkLine{del("b"), add("B")},
	}}}
	got, loc := applyFileChange(fs, "C1", fp)
	if loc != nil {
		t.Fatalf("不应冲突: %+v", loc)
	}
	if want := []string{"a", "B", "c"}; !reflect.DeepEqual(got.lines, want) {
		t.Fatalf("结果错误: %v", got.lines)
	}
	if got.owner[1] != "C1" {
		t.Fatalf("被替换行应归属于 C1，owner=%v", got.owner)
	}
	if got.owner[0] != "" || got.owner[2] != "" {
		t.Fatalf("上下文行来源应保留为基准，owner=%v", got.owner)
	}
}

func TestApplyFileChange_InsertionHunk(t *testing.T) {
	fs := newFileState("f", []string{"a", "b"}, true)
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 1, OldLines: 0, NewStart: 1, NewLines: 1,
		Lines: []HunkLine{add("new")},
	}}}
	got, loc := applyFileChange(fs, "C1", fp)
	if loc != nil {
		t.Fatalf("插入块不应冲突: %+v", loc)
	}
	if want := []string{"new", "a", "b"}; !reflect.DeepEqual(got.lines, want) {
		t.Fatalf("插入结果错误: %v", got.lines)
	}
}

func TestApplyFileChange_DeclaredPositionDrift(t *testing.T) {
	// 前置变更已在开头插入一行；声明 OldStart=2 实际匹配在第 3 行，
	// 且匹配唯一，应允许漂移到唯一位置。
	fs := newFileState("f", []string{"pre", "a", "b", "c"}, true)
	fs.owner[0] = "PRE"
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 2, OldLines: 2, NewStart: 2, NewLines: 2,
		Lines: []HunkLine{ctx("a"), del("b"), add("B"), ctx("c")},
	}}}
	// 上述 hunk 旧侧为 a,b,c 三行（oldN=3），重新构造正确跨度。
	fp.Hunks[0].OldLines = 3
	fp.Hunks[0].NewLines = 3
	got, loc := applyFileChange(fs, "C1", fp)
	if loc != nil {
		t.Fatalf("唯一匹配应漂移成功: %+v", loc)
	}
	if want := []string{"pre", "a", "B", "c"}; !reflect.DeepEqual(got.lines, want) {
		t.Fatalf("漂移结果错误: %v", got.lines)
	}
}

func TestApplyFileChange_AmbiguousMatchRejected(t *testing.T) {
	// 文件中存在两处相同的两行上下文，且声明位置两处均不匹配：必须判歧义冲突，不猜测。
	fs := newFileState("f", []string{"x", "x", "x", "tail"}, true)
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 4, OldLines: 2, NewStart: 4, NewLines: 3,
		Lines: []HunkLine{ctx("x"), ctx("x"), add("y")},
	}}}
	_, loc := applyFileChange(fs, "C1", fp)
	if loc == nil {
		t.Fatal("多候选匹配必须冲突")
	}
	if loc.Detail == "" {
		t.Fatal("冲突必须给出人类可读原因")
	}
}

func TestApplyFileChange_ConflictSourceFromPriorChange(t *testing.T) {
	// C0 已把第 1 行改成 "changed"；C1 的上下文期望 "orig"，
	// 冲突必须指出来源变更 C0 与具体行号。
	fs := newFileState("f", []string{"changed", "b"}, true)
	fs.owner[0] = "C0"
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2,
		Lines: []HunkLine{ctx("orig"), ctx("b"), add("z")},
	}}}
	fp.Hunks[0].NewLines = 3
	_, loc := applyFileChange(fs, "C1", fp)
	if loc == nil {
		t.Fatal("上下文不一致必须冲突")
	}
	if loc.SourceChange != "C0" {
		t.Fatalf("冲突来源应为 C0，得到 %q（%s）", loc.SourceChange, loc.Detail)
	}
	if loc.Line != 1 {
		t.Fatalf("冲突行号应为 1，得到 %d", loc.Line)
	}
}

func TestApplyFileChange_NewFileAlreadyExistsFromOtherChange(t *testing.T) {
	fs := newFileState("f", []string{"data"}, true)
	for i := range fs.owner {
		fs.owner[i] = "C0"
	}
	fp := FilePatch{Path: "f", IsNew: true, Hunks: []Hunk{{
		NewStart: 1, NewLines: 1, Lines: []HunkLine{add("data")},
	}}}
	_, loc := applyFileChange(fs, "C1", fp)
	if loc == nil || loc.SourceChange != "C0" {
		t.Fatalf("新建已存在文件应冲突并指出来源 C0，得到 %+v", loc)
	}
}

func TestApplyFileChange_DeleteMissingFile(t *testing.T) {
	fs := newFileState("f", nil, false)
	fp := FilePatch{Path: "f", IsDelete: true}
	_, loc := applyFileChange(fs, "C1", fp)
	if loc == nil {
		t.Fatal("删除不存在文件必须冲突")
	}
}

func TestApplyFileChange_FailedApplyDoesNotMutateInput(t *testing.T) {
	fs := newFileState("f", []string{"a", "b"}, true)
	before := append([]string(nil), fs.lines...)
	fp := FilePatch{Path: "f", Hunks: []Hunk{{
		OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 2,
		Lines: []HunkLine{ctx("a"), ctx("ZZZ")},
	}}}
	_, loc := applyFileChange(fs, "C1", fp)
	if loc == nil {
		t.Fatal("应冲突")
	}
	if !reflect.DeepEqual(fs.lines, before) {
		t.Fatal("失败的应用不得就地修改输入视图")
	}
}
