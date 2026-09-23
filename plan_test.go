package integrator

import (
	"strings"
	"testing"
)

func mustNormalize(t *testing.T, raw RawChange) *Change {
	t.Helper()
	c, err := Normalize(raw)
	if err != nil {
		t.Fatalf("normalize %s: %v", raw.ID, err)
	}
	return c
}

func TestBuildPlan_TopologicalOrder(t *testing.T) {
	changes := []*Change{
		mustNormalize(t, RawChange{ID: "c", DependsOn: []string{"b"},
			Files: []RawFileChange{{Path: "f.txt", OldStart: 1, OldBlock: []string{"x"}, NewBlock: []string{"c"}}}}),
		mustNormalize(t, RawChange{ID: "a",
			Files: []RawFileChange{{Path: "g.txt", OldStart: 1, OldBlock: []string{"y"}, NewBlock: []string{"a"}}}}),
		mustNormalize(t, RawChange{ID: "b", DependsOn: []string{"a"},
			Files: []RawFileChange{{Path: "h.txt", OldStart: 1, OldBlock: []string{"z"}, NewBlock: []string{"b"}}}}),
	}
	plan, err := BuildPlan(changes)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Rejections) != 0 {
		t.Fatalf("unexpected rejections: %+v", plan.Rejections)
	}
	got := strings.Join(plan.Order, ",")
	if got != "a,b,c" {
		t.Fatalf("order = %s, want a,b,c (dependencies first, id tie-break)", got)
	}
}

func TestBuildPlan_MissingDependency(t *testing.T) {
	changes := []*Change{
		mustNormalize(t, RawChange{ID: "a", DependsOn: []string{"ghost"},
			Files: []RawFileChange{{Path: "f.txt", OldStart: 1, OldBlock: []string{"x"}, NewBlock: []string{"a"}}}}),
	}
	plan, _ := BuildPlan(changes)
	if len(plan.Rejections) != 1 {
		t.Fatalf("rejections = %+v, want 1", plan.Rejections)
	}
	r := plan.Rejections[0]
	if r.Reason != RejectMissingDependency || r.ChangeID != "a" || r.RelatedID != "ghost" {
		t.Fatalf("rejection = %+v", r)
	}
	t.Logf("change=a decision=rejected reason=%s related=%s detail=%q", r.Reason, r.RelatedID, r.Detail)
}

func TestBuildPlan_CycleUndecidable(t *testing.T) {
	changes := []*Change{
		mustNormalize(t, RawChange{ID: "a", DependsOn: []string{"b"},
			Files: []RawFileChange{{Path: "a.txt", OldStart: 1, OldBlock: []string{"1"}, NewBlock: []string{"a"}}}}),
		mustNormalize(t, RawChange{ID: "b", DependsOn: []string{"a"},
			Files: []RawFileChange{{Path: "b.txt", OldStart: 1, OldBlock: []string{"1"}, NewBlock: []string{"b"}}}}),
	}
	plan, _ := BuildPlan(changes)
	seen := map[string]bool{}
	for _, r := range plan.Rejections {
		if r.Reason != RejectOrderUndecidable {
			t.Fatalf("reason = %s, want %s", r.Reason, RejectOrderUndecidable)
		}
		seen[r.ChangeID] = true
		t.Logf("change=%s decision=rejected reason=%s detail=%q", r.ChangeID, r.Reason, r.Detail)
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("both cycle members must be rejected: %+v", plan.Rejections)
	}
}

func TestBuildPlan_DuplicateID(t *testing.T) {
	changes := []*Change{
		mustNormalize(t, RawChange{ID: "a",
			Files: []RawFileChange{{Path: "f.txt", OldStart: 1, OldBlock: []string{"1"}, NewBlock: []string{"x"}}}}),
		mustNormalize(t, RawChange{ID: "a",
			Files: []RawFileChange{{Path: "g.txt", OldStart: 1, OldBlock: []string{"1"}, NewBlock: []string{"y"}}}}),
	}
	plan, _ := BuildPlan(changes)
	var found bool
	for _, r := range plan.Rejections {
		if r.Reason == RejectDuplicateID && r.ChangeID == "a" {
			found = true
			t.Logf("change=a decision=rejected reason=%s detail=%q", r.Reason, r.Detail)
		}
	}
	if !found {
		t.Fatalf("duplicate id rejection missing: %+v", plan.Rejections)
	}
}

func TestBuildPlan_DuplicateContentDifferentID(t *testing.T) {
	fc := []RawFileChange{{Path: "f.txt", OldStart: 1, OldBlock: []string{"1"}, NewBlock: []string{"x"}}}
	changes := []*Change{
		mustNormalize(t, RawChange{ID: "a", Files: fc}),
		mustNormalize(t, RawChange{ID: "b", Files: fc}),
	}
	plan, _ := BuildPlan(changes)
	var found bool
	for _, r := range plan.Rejections {
		if r.Reason == RejectDuplicateChange && r.ChangeID == "b" && r.RelatedID == "a" {
			found = true
			t.Logf("change=b decision=rejected reason=%s related=a detail=%q", r.Reason, r.Detail)
		}
	}
	if !found {
		t.Fatalf("duplicate content rejection missing: %+v", plan.Rejections)
	}
}
