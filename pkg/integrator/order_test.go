package integrator

import (
	"reflect"
	"testing"
)

// nc 构造一个最小可用的归一变更（单文件单新增块），便于顺序测试聚焦判定逻辑。
func nc(id string, paths []string, deps ...string) *NormalizedChange {
	files := make([]FilePatch, 0, len(paths))
	for _, p := range paths {
		files = append(files, FilePatch{Path: p, IsNew: true, Hunks: []Hunk{{
			NewStart: 1, NewLines: 1, Lines: []HunkLine{{OpAdd, "x-" + id}},
		}}})
	}
	return &NormalizedChange{
		Change:      Change{ID: id, Depends: deps, Files: files},
		Fingerprint: "fp-" + id,
		OrderKey:    id,
	}
}

func TestResolveOrder_DuplicateIDs(t *testing.T) {
	a, b := nc("a", []string{"x"}), nc("a", []string{"y"})
	b.Fingerprint = "fp-b"
	_, err := resolveOrder([]*NormalizedChange{a, b})
	if err == nil || rejectReason(err) != RejectDuplicate {
		t.Fatalf("期望 duplicate，得到 %v", err)
	}
}

func TestResolveOrder_DuplicateFingerprint(t *testing.T) {
	a, b := nc("a", []string{"x"}), nc("b", []string{"y"})
	b.Fingerprint = a.Fingerprint // 换了 ID 但内容相同
	_, err := resolveOrder([]*NormalizedChange{a, b})
	if err == nil || rejectReason(err) != RejectDuplicate {
		t.Fatalf("同指纹不同 ID 必须判 duplicate，得到 %v", err)
	}
}

func TestResolveOrder_MissingDependency(t *testing.T) {
	a := nc("a", []string{"x"}, "ghost")
	_, err := resolveOrder([]*NormalizedChange{a})
	if err == nil || rejectReason(err) != RejectMissingDep {
		t.Fatalf("期望 missing-dependency，得到 %v", err)
	}
}

func TestResolveOrder_Cycle(t *testing.T) {
	a := nc("a", []string{"a.txt"}, "b")
	b := nc("b", []string{"b.txt"}, "a")
	_, err := resolveOrder([]*NormalizedChange{a, b})
	if err == nil || rejectReason(err) != RejectOrderUndecidable {
		t.Fatalf("成环必须判 order-undecidable，得到 %v", err)
	}
}

func TestResolveOrder_UndecidableOverlap(t *testing.T) {
	// 两个变更改同一文件且无依赖：即便按到达顺序能套，也必须拒绝。
	a := nc("a", []string{"shared.txt"})
	b := nc("b", []string{"shared.txt"})
	_, err := resolveOrder([]*NormalizedChange{a, b})
	if err == nil || rejectReason(err) != RejectOrderUndecidable {
		t.Fatalf("无依据重叠必须判 order-undecidable，得到 %v", err)
	}
	// 反向提交，结论原因必须一致（稳定，不按到达顺序硬套）。
	_, err2 := resolveOrder([]*NormalizedChange{b, a})
	if rejectReason(err2) != RejectOrderUndecidable {
		t.Fatalf("反向提交应给出相同拒绝类别，得到 %v", err2)
	}
}

func TestResolveOrder_DependencyResolvesOverlap(t *testing.T) {
	// 重叠但有显式依赖给出先后依据：允许，顺序为 a -> b。
	a := nc("a", []string{"shared.txt"})
	b := nc("b", []string{"shared.txt"}, "a")
	got, err := resolveOrder([]*NormalizedChange{b, a}) // 故意乱序提交
	if err != nil {
		t.Fatalf("有依赖的重叠应允许: %v", err)
	}
	if ids := orderIDs(got); !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("顺序错误: %v", ids)
	}
}

func TestResolveOrder_TransitiveDependency(t *testing.T) {
	// a -> b -> c；a 与 c 重叠但存在传递依赖，顺序可确定。
	a := nc("a", []string{"s"})
	b := nc("b", []string{"other"}, "a")
	c := nc("c", []string{"s"}, "b")
	got, err := resolveOrder([]*NormalizedChange{c, b, a})
	if err != nil {
		t.Fatal(err)
	}
	if ids := orderIDs(got); !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("传递依赖排序错误: %v", ids)
	}
}

func TestResolveOrder_IndependentChangesStableByKey(t *testing.T) {
	// 互不重叠的独立变更没有先后依据冲突，按 OrderKey 稳定破平，
	// 且结论与提交顺序无关。
	c1 := nc("c", []string{"c.txt"})
	c2 := nc("a", []string{"a.txt"})
	c3 := nc("b", []string{"b.txt"})
	got, err := resolveOrder([]*NormalizedChange{c1, c2, c3})
	if err != nil {
		t.Fatal(err)
	}
	if ids := orderIDs(got); !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("期望按排序键稳定输出，得到 %v", ids)
	}
	got2, _ := resolveOrder([]*NormalizedChange{c3, c1, c2})
	if !reflect.DeepEqual(orderIDs(got), orderIDs(got2)) {
		t.Fatal("顺序必须与提交顺序无关")
	}
}

func orderIDs(cs []*NormalizedChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}
