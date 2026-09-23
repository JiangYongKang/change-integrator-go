package integrator

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DiskWorkspace 是基于磁盘目录的 Workspace 实现，用于真实 apply。
type DiskWorkspace struct {
	root      string
	statePath string
}

// NewDiskWorkspace 以 root 为基准根目录创建磁盘工作区。
func NewDiskWorkspace(root string) *DiskWorkspace {
	return &DiskWorkspace{root: root, statePath: filepath.Join(root, ".integrator-state.json")}
}

// SetStatePath 自定义状态文件位置（默认 <root>/.integrator-state.json）。
func (w *DiskWorkspace) SetStatePath(p string) *DiskWorkspace {
	w.statePath = p
	return w
}

func (w *DiskWorkspace) resolve(path string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(path))
	full := filepath.Join(w.root, clean)
	rel, err := filepath.Rel(w.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", &RejectError{Reason: RejectInvalid, Message: "路径越出基准根目录: " + path}
	}
	return full, nil
}

func (w *DiskWorkspace) ReadFile(path string) ([]string, bool, error) {
	full, err := w.resolve(path)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return splitContent(string(data)), true, nil
}

func (w *DiskWorkspace) WriteFile(path string, lines []string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (w *DiskWorkspace) RemoveFile(path string) error {
	full, err := w.resolve(path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (w *DiskWorkspace) LoadState() (*State, error) {
	data, err := os.ReadFile(w.statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (w *DiskWorkspace) SaveState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.statePath), 0o755); err != nil {
		return err
	}
	tmp := w.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.statePath)
}
