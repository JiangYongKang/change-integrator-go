package integrator

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDirWorkspace_ReadWriteAndApply(t *testing.T) {
	root := t.TempDir()
	ws := NewDirWorkspace(root)
	base := []string{"alpha", "beta", "gamma", "delta"}
	if err := ws.Write("sub/dir/f.txt", base); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, exists, err := ws.Read("sub/dir/f.txt")
	if err != nil || !exists {
		t.Fatalf("read: exists=%v err=%v", exists, err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("roundtrip = %v, want %v", got, base)
	}
	// 落盘内容确实带结尾换行
	data, err := os.ReadFile(filepath.Join(root, "sub/dir/f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha\nbeta\ngamma\ndelta\n" {
		t.Fatalf("on-disk content = %q", string(data))
	}

	ch, err := Normalize(RawChange{ID: "c1", Files: []RawFileChange{
		{Path: "sub/dir/f.txt", OldStart: 2, OldBlock: []string{"beta"}, NewBlock: []string{"BETA"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyChange(ws, ch); err != nil {
		t.Fatalf("applyChange: %v", err)
	}
	got, _, _ = ws.Read("sub/dir/f.txt")
	if !reflect.DeepEqual(got, []string{"alpha", "BETA", "gamma", "delta"}) {
		t.Fatalf("after apply = %v", got)
	}
}

func TestApplyChange_MultipleHunksSameFile(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3", "L4", "L5"})
	ch, _ := Normalize(RawChange{ID: "multi", Files: []RawFileChange{
		{Path: "f.txt", OldStart: 1, OldBlock: []string{"L1"}, NewBlock: []string{"N1"}},
		{Path: "f.txt", OldStart: 5, OldBlock: []string{"L5"}, NewBlock: []string{"N5", "N6"}},
	}})
	if err := applyChange(ws, ch); err != nil {
		t.Fatal(err)
	}
	got, _, _ := ws.Read("f.txt")
	want := []string{"N1", "L2", "L3", "L4", "N5", "N6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-hunk = %v, want %v", got, want)
	}
}

func TestApplyChange_AnchorFallbackUniqueSearch(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"L1", "L2", "L3", "L4"})
	// 锚点声称第 1 行，实际旧块唯一出现在第 3 行：应接受回退定位。
	ch, _ := Normalize(RawChange{ID: "shifted", Files: []RawFileChange{{
		Path: "f.txt", OldStart: 1, OldBlock: []string{"L3"}, NewBlock: []string{"X3"}}}})
	if err := applyChange(ws, ch); err != nil {
		t.Fatalf("unique fallback should succeed: %v", err)
	}
	got, _, _ := ws.Read("f.txt")
	if !reflect.DeepEqual(got, []string{"L1", "L2", "X3", "L4"}) {
		t.Fatalf("after fallback = %v", got)
	}
}

func TestApplyChange_AmbiguousMatchRejected(t *testing.T) {
	ws := NewMemoryWorkspace()
	ws.SeedFile("f.txt", []string{"dup", "x", "dup"})
	ch, _ := Normalize(RawChange{ID: "amb", Files: []RawFileChange{{
		Path: "f.txt", OldStart: 2, OldBlock: []string{"dup"}, NewBlock: []string{"z"}}}})
	err := applyChange(ws, ch)
	if !errors.Is(err, ErrContextMismatch) {
		t.Fatalf("err = %v, want ErrContextMismatch (ambiguous)", err)
	}
}

func TestDirWorkspace_PathEscapeRejected(t *testing.T) {
	ws := NewDirWorkspace(t.TempDir())
	if err := ws.Write("../escape.txt", []string{"x"}); err == nil {
		t.Fatalf("path traversal write must be rejected")
	}
	if _, _, err := ws.Read("/etc/passwd"); err == nil {
		t.Fatalf("absolute path read must be rejected")
	}
}

func TestNormalize_Validation(t *testing.T) {
	if _, err := Normalize(RawChange{ID: ""}); err == nil {
		t.Fatal("empty id must fail")
	}
	if _, err := Normalize(RawChange{ID: "x", Files: []RawFileChange{{Path: "f", OldStart: 0,
		OldBlock: []string{"a"}, NewBlock: []string{"b"}}}}); err == nil {
		t.Fatal("non-positive old start must fail")
	}
	if _, err := Normalize(RawChange{ID: "x", Files: []RawFileChange{{Path: "f", Create: true,
		OldStart: 1, NewBlock: []string{"b"}}}}); err == nil {
		t.Fatal("create with old start must fail")
	}
}
