package integrator

import (
	"sort"
	"strings"
)

// MemWorkspace 是基于内存 map 的 Workspace 实现，用于测试与预演。
type MemWorkspace struct {
	Files map[string][]string
	state *State
	// Written/Removed 记录落盘轨迹，便于测试断言“未重复写入”等行为。
	Writes  []string
	Removes []string
}

// NewMemWorkspace 创建内存工作区，base 为基准文件内容快照（按 "\n" 切行）。
func NewMemWorkspace(base map[string]string) *MemWorkspace {
	files := make(map[string][]string, len(base))
	paths := make([]string, 0, len(base))
	for p := range base {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		files[p] = splitContent(base[p])
	}
	return &MemWorkspace{Files: files}
}

// Snapshot 返回当前文件内容的不可变拷贝（以 "\n" 连接），便于断言。
func (w *MemWorkspace) Snapshot() map[string]string {
	out := make(map[string]string, len(w.Files))
	for p, lines := range w.Files {
		out[p] = strings.Join(lines, "\n")
	}
	return out
}

func (w *MemWorkspace) ReadFile(path string) ([]string, bool, error) {
	lines, ok := w.Files[path]
	if !ok {
		return nil, false, nil
	}
	return append([]string(nil), lines...), true, nil
}

func (w *MemWorkspace) WriteFile(path string, lines []string) error {
	w.Files[path] = append([]string(nil), lines...)
	w.Writes = append(w.Writes, path)
	return nil
}

func (w *MemWorkspace) RemoveFile(path string) error {
	delete(w.Files, path)
	w.Removes = append(w.Removes, path)
	return nil
}

func (w *MemWorkspace) LoadState() (*State, error) {
	if w.state == nil {
		return nil, nil
	}
	return cloneState(w.state), nil
}

func (w *MemWorkspace) SaveState(s *State) error {
	w.state = cloneState(s)
	return nil
}

// PutState 直接注入续跑状态，用于模拟“上次运行中断后”的现场。
func (w *MemWorkspace) PutState(s *State) {
	w.state = cloneState(s)
}

func splitContent(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
