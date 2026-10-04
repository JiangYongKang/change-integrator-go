package integrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace 是变更所作用的文件工作区抽象。
type Workspace interface {
	Read(path string) (lines []string, exists bool, err error)
	Write(path string, lines []string) error
}

// 上下文无法匹配（锚点行处既不是旧块，文件内也找不到唯一匹配位置）。
var ErrContextMismatch = errors.New("context mismatch: old block not found at anchor or anywhere uniquely")

// ErrRelayPrecondition 表示接力变更的预期前置内容（旧块）在锚点处与实际内容对不上。
// 接力变更不允许换到别的位置套用，因此锚点不符即失败，不做全文件搜索。
var ErrRelayPrecondition = errors.New("relay precondition mismatch: expected predecessor content not found at anchor")

// DirWorkspace 是落盘工作区。
type DirWorkspace struct {
	root string
}

// MemoryWorkspace 是内存工作区（用于预演与测试）。
type MemoryWorkspace struct {
	files map[string][]string
}

// NewDirWorkspace 创建落盘工作区。
func NewDirWorkspace(root string) *DirWorkspace {
	return &DirWorkspace{root: root}
}

// NewMemoryWorkspace 创建空的内存工作区。
func NewMemoryWorkspace() *MemoryWorkspace {
	return &MemoryWorkspace{files: map[string][]string{}}
}

// SeedFile 向内存工作区预置一个基准文件。
func (w *MemoryWorkspace) SeedFile(path string, lines []string) {
	w.files[path] = append([]string(nil), lines...)
}

// Snapshot 返回内存工作区全部文件的副本（测试断言用）。
func (w *MemoryWorkspace) Snapshot() map[string][]string {
	out := make(map[string][]string, len(w.files))
	for p, ls := range w.files {
		out[p] = append([]string(nil), ls...)
	}
	return out
}

func safePath(root, path string) (string, error) {
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(clean) {
		return "", fmt.Errorf("illegal file path %q", path)
	}
	full := filepath.Join(root, clean)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("file path %q escapes workspace root", path)
	}
	return full, nil
}

func (w *DirWorkspace) Read(path string) ([]string, bool, error) {
	full, err := safePath(w.root, path)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return splitLines(string(data)), true, nil
}

func (w *DirWorkspace) Write(path string, lines []string) error {
	full, err := safePath(w.root, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(joinLines(lines)), 0o644)
}

func (w *MemoryWorkspace) Read(path string) ([]string, bool, error) {
	ls, ok := w.files[path]
	if !ok {
		return nil, false, nil
	}
	return append([]string(nil), ls...), true, nil
}

func (w *MemoryWorkspace) Write(path string, lines []string) error {
	w.files[path] = append([]string(nil), lines...)
	return nil
}

// splitLines 按行切分文件内容：结果不含换行符；
// 文件结尾的单个换行使最后一行成为普通行，无结尾换行也不产生空行。
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// applyChange 在工作区上尝试应用一条变更（不检查重叠，重叠由引擎层判定）。
// 每个 hunk 先在基准内容上定位（锚点优先，锚点不符再全文件唯一搜索），
// 随后按位置从后往前替换，避免行号位移影响尚未处理的 hunk。
func applyChange(ws Workspace, ch *Change) error {
	return applyChangeMode(ws, ch, false)
}

// applyChangeStrict 是接力变更专用的应用原语：hunk 必须在声明的锚点处
// 精确匹配预期前置内容，锚点不符即返回 ErrRelayPrecondition，
// 不做全文件搜索——接力变更不得静默换到别的位置套用。
func applyChangeStrict(ws Workspace, ch *Change) error {
	return applyChangeMode(ws, ch, true)
}

func applyChangeMode(ws Workspace, ch *Change, strict bool) error {
	byPath := map[string][]FileChange{}
	var order []string
	for _, f := range ch.Files {
		if _, ok := byPath[f.Path]; !ok {
			order = append(order, f.Path)
		}
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	for _, path := range order {
		hunks := byPath[path]
		if hunks[0].Create {
			exists, err := fileExists(ws, path)
			if err != nil {
				return err
			}
			if exists {
				if strict {
					return fmt.Errorf("change %s file %s: %w: create expects absent file but it exists",
						ch.ID, path, ErrRelayPrecondition)
				}
				return fmt.Errorf("create %s: target file already exists", path)
			}
			var out []string
			for _, h := range hunks {
				out = append(out, h.NewBlock...)
			}
			if err := ws.Write(path, out); err != nil {
				return fmt.Errorf("create %s: %w", path, err)
			}
			continue
		}
		base, exists, err := ws.Read(path)
		if err != nil {
			return err
		}
		if !exists {
			if strict {
				return fmt.Errorf("change %s file %s: %w: modify expects existing file",
					ch.ID, path, ErrRelayPrecondition)
			}
			return fmt.Errorf("modify %s: target file does not exist", path)
		}
		var ps []placedHunk
		for _, h := range hunks {
			var start int
			var perr error
			if strict {
				start, perr = locateHunkStrict(base, h)
			} else {
				start, perr = locateHunk(base, h)
			}
			if perr != nil {
				return fmt.Errorf("change %s file %s: %w", ch.ID, path, perr)
			}
			ps = append(ps, placedHunk{start: start, h: h})
		}
		// 从后往前应用，保证前面 hunk 的索引不被位移影响。
		sortPlacedDesc(ps)
		out := append([]string(nil), base...)
		for _, p := range ps {
			end := p.start + len(p.h.OldBlock)
			out = append(out[:p.start], append(append([]string(nil), p.h.NewBlock...), out[end:]...)...)
		}
		if err := ws.Write(path, out); err != nil {
			return fmt.Errorf("modify %s: %w", path, err)
		}
	}
	return nil
}

func fileExists(ws Workspace, path string) (bool, error) {
	_, exists, err := ws.Read(path)
	return exists, err
}

// locateHunk 在基准内容上定位 hunk：锚点位置优先；
// 锚点不匹配时允许全文件搜索，但匹配位置必须唯一，否则拒绝（拒绝猜测）。
func locateHunk(base []string, h FileChange) (int, error) {
	anchor := h.OldStart - 1
	if anchor >= 0 && anchor+len(h.OldBlock) <= len(base) &&
		equalLines(base[anchor:anchor+len(h.OldBlock)], h.OldBlock) {
		return anchor, nil
	}
	var found []int
	for i := 0; i+len(h.OldBlock) <= len(base); i++ {
		if equalLines(base[i:i+len(h.OldBlock)], h.OldBlock) {
			found = append(found, i)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return 0, ErrContextMismatch
	}
	return 0, fmt.Errorf("%w: old block matches %d locations", ErrContextMismatch, len(found))
}

// locateHunkStrict 是接力变更的定位：只允许锚点处精确匹配，
// 不匹配即返回 ErrRelayPrecondition，绝不退化为全文件搜索（拒绝换位置套用）。
func locateHunkStrict(base []string, h FileChange) (int, error) {
	anchor := h.OldStart - 1
	if anchor >= 0 && anchor+len(h.OldBlock) <= len(base) &&
		equalLines(base[anchor:anchor+len(h.OldBlock)], h.OldBlock) {
		return anchor, nil
	}
	return 0, fmt.Errorf("%w: anchor line %d", ErrRelayPrecondition, h.OldStart)
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// placedHunk 表示一个已定位的 hunk。
type placedHunk struct {
	start int
	h     FileChange
}

func sortPlacedDesc(ps []placedHunk) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j-1].start < ps[j].start; j-- {
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
}
