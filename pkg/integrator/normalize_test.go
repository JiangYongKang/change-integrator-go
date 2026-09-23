package integrator

import (
	"strings"
	"testing"
)

func TestNormalize_AssignsIDFromFingerprint(t *testing.T) {
	c := Change{
		Files: []FilePatch{{
			Path: "a.txt",
			Hunks: []Hunk{{
				NewStart: 1, NewLines: 1,
				Lines: []HunkLine{{OpAdd, "hello"}},
			}},
		}},
	}
	got, err := normalizeSet([]Change{c})
	if err != nil {
		t.Fatalf("归一失败: %v", err)
	}
	if got[0].ID == "" || !strings.HasPrefix(got[0].ID, "c") {
		t.Fatalf("期望按指纹生成 c 前缀 ID，得到 %q", got[0].ID)
	}
	if got[0].OrderKey != got[0].ID {
		t.Fatalf("排序键应为 ID")
	}
	// 同样内容再次归一，指纹与生成 ID 必须稳定。
	got2, _ := normalizeSet([]Change{c})
	if got2[0].Fingerprint != got[0].Fingerprint || got2[0].ID != got[0].ID {
		t.Fatalf("同内容归一结果不稳定: %+v vs %+v", got[0], got2[0])
	}
}

func TestNormalize_PathCanonicalization(t *testing.T) {
	got, err := normalizeSet([]Change{{
		ID: "x",
		Files: []FilePatch{{
			Path: "dir\\sub/../a.txt",
			Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
				Lines: []HunkLine{{OpContext, "x"}}}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Files[0].Path != "dir/a.txt" {
		t.Fatalf("路径未规范化: %q", got[0].Files[0].Path)
	}
}

func TestNormalize_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		change  Change
		wantMsg string
	}{
		{
			name:    "空变更集",
			change:  Change{},
			wantMsg: "",
		},
	}
	_ = cases

	if _, err := normalizeSet(nil); err == nil {
		t.Fatal("空变更集必须被拒绝")
	} else if rejectReason(err) != RejectInvalid {
		t.Fatalf("空变更集拒绝原因错误: %v", err)
	}

	bad := []Change{
		{ID: "no-files"}, // 无补丁
		{ID: "bad-path", Files: []FilePatch{{Path: "../secret", Hunks: []Hunk{{OldStart: 1}}}}},
		{ID: "bad-count", Files: []FilePatch{{Path: "a", Hunks: []Hunk{{
			OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 1,
			Lines: []HunkLine{{OpContext, "x"}}}}}}},
		{ID: "new-with-old", Files: []FilePatch{{Path: "a", IsNew: true, Hunks: []Hunk{{
			NewStart: 1, OldLines: 1, Lines: []HunkLine{{OpDel, "x"}}}}}}},
		{ID: "dup-path", Files: []FilePatch{
			{Path: "a", Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
				Lines: []HunkLine{{OpContext, "x"}}}}},
			{Path: "a", Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
				Lines: []HunkLine{{OpContext, "y"}}}}},
		}},
	}
	for _, c := range bad {
		if _, err := normalizeSet([]Change{c}); err == nil {
			t.Fatalf("非法变更 %s 必须被拒绝", c.ID)
		} else if rejectReason(err) != RejectInvalid {
			t.Fatalf("变更 %s 拒绝类别错误: %v", c.ID, err)
		}
	}
}

func rejectReason(err error) RejectReason {
	if re, ok := err.(*RejectError); ok {
		return re.Reason
	}
	return ""
}
