package integrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// RawChange 表示从提交或补丁归一化之前的原始变更输入。
type RawChange struct {
	ID        string
	Summary   string
	DependsOn []string
	Files     []RawFileChange
}

// RawFileChange 表示对单个文件的一次块级改动。
type RawFileChange struct {
	Path     string
	OldStart int      // 基准文件中的 1-based 锚点行号；Create 时必须为 0
	OldBlock []string // 被替换的旧行（空表示纯插入）
	NewBlock []string // 替换后的新行
	Create   bool     // 目标文件在基准上是否不存在
}

// Change 是归一化后的统一变更模型。
type Change struct {
	ID          string
	Summary     string
	DependsOn   []string
	Fingerprint string
	Files       []FileChange
	Scopes      []Scope
}

// FileChange 是归一化后的单文件改动。
type FileChange struct {
	Path     string
	OldStart int
	OldBlock []string
	NewBlock []string
	Create   bool
}

// Scope 表示变更在基准上的作用范围。
// 行区间采用基准文件的 0-based 半开区间 [Start, End)；
// Create=true 时区间为 [0,0)，重叠判定按整文件创建处理。
type Scope struct {
	Path   string
	Start  int
	End    int
	Create bool
}

// Normalize 把一条原始变更归一化为统一模型。
func Normalize(raw RawChange) (*Change, error) {
	if strings.TrimSpace(raw.ID) == "" {
		return nil, fmt.Errorf("normalize: change id must not be empty")
	}
	ch := &Change{
		ID:        raw.ID,
		Summary:   raw.Summary,
		DependsOn: append([]string(nil), raw.DependsOn...),
	}
	for _, f := range raw.Files {
		if strings.TrimSpace(f.Path) == "" {
			return nil, fmt.Errorf("normalize %s: file path must not be empty", raw.ID)
		}
		if f.Create {
			if f.OldStart != 0 || len(f.OldBlock) != 0 {
				return nil, fmt.Errorf("normalize %s: created file %s must not carry old lines or a start line",
					raw.ID, f.Path)
			}
		} else {
			if f.OldStart < 1 {
				return nil, fmt.Errorf("normalize %s: file %s old start must be 1-based and positive",
					raw.ID, f.Path)
			}
		}
		fc := FileChange{
			Path:     f.Path,
			OldStart: f.OldStart,
			OldBlock: append([]string(nil), f.OldBlock...),
			NewBlock: append([]string(nil), f.NewBlock...),
			Create:   f.Create,
		}
		ch.Files = append(ch.Files, fc)
	}
	// 同一变更内多个 hunk 针对同一文件时，记录全部 hunk 的区间。
	ch.Scopes = scopesOf(ch.Files)
	ch.Fingerprint = fingerprintOf(ch)
	return ch, nil
}

// scopesOf 由文件改动列表推导基准作用范围。
func scopesOf(files []FileChange) []Scope {
	var out []Scope
	for _, f := range files {
		var s Scope
		if f.Create {
			s = Scope{Path: f.Path, Start: 0, End: 0, Create: true}
		} else {
			s = Scope{
				Path:  f.Path,
				Start: f.OldStart - 1,
				End:   f.OldStart - 1 + len(f.OldBlock),
			}
		}
		out = append(out, s)
	}
	return out
}

// fingerprintOf 计算变更内容指纹：仅基于补丁内容，与 ID/摘要/依赖无关。
// 两条不同 ID 但补丁内容完全一致的变更得到相同指纹，据此识别重复提交。
func fingerprintOf(ch *Change) string {
	h := sha256.New()
	add := func(s string) {
		fmt.Fprintf(h, "%d:%s\n", len(s), s)
	}
	for _, f := range ch.Files {
		add(f.Path)
		if f.Create {
			add("create")
		} else {
			add("modify")
		}
		add(fmt.Sprintf("%d", f.OldStart))
		add(fmt.Sprintf("%d", len(f.OldBlock)))
		for _, l := range f.OldBlock {
			add(l)
		}
		add(fmt.Sprintf("%d", len(f.NewBlock)))
		for _, l := range f.NewBlock {
			add(l)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
