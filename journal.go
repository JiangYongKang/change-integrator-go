package integrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// AppliedRecord 记录一条已应用变更。
type AppliedRecord struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Order       int       `json:"order"`
	AppliedAt   time.Time `json:"applied_at"`
	Scopes      []Scope   `json:"scopes"`
	// DependsOn 记录该变更声明的前置，使跨运行的接力祖先链在中断续跑、
	// 只提交剩余变更时仍可被识别（旧版日志无此字段时按无已知前置处理）。
	DependsOn []string `json:"depends_on,omitempty"`
}

// journalFile 是状态日志的 JSON 存储结构。
type journalFile struct {
	Version int             `json:"version"`
	Applied []AppliedRecord `json:"applied"`
}

// Journal 是跨运行持久化的状态日志。
type Journal struct {
	Applied map[string]AppliedRecord
}

// LoadJournal 从路径读取状态日志（文件不存在时返回空日志）。
func LoadJournal(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Journal{Applied: map[string]AppliedRecord{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load journal: %w", err)
	}
	var f journalFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("load journal: corrupt journal: %w", err)
	}
	j := &Journal{Applied: map[string]AppliedRecord{}}
	for _, r := range f.Applied {
		j.Applied[r.ID] = r
	}
	return j, nil
}

// save 将状态日志原子写入（先写临时文件再 rename），避免中断留下半截日志。
func (j *Journal) save(path string) error {
	if path == "" {
		return fmt.Errorf("journal path is empty")
	}
	f := journalFile{Version: 1}
	for _, r := range j.Applied {
		f.Applied = append(f.Applied, r)
	}
	sort.Slice(f.Applied, func(a, b int) bool { return f.Applied[a].Order < f.Applied[b].Order })
	data, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (j *Journal) markApplied(rec AppliedRecord) {
	j.Applied[rec.ID] = rec
}

func (j *Journal) isApplied(id string) bool {
	_, ok := j.Applied[id]
	return ok
}

// nextOrder 返回下一条已应用记录的顺序号。
func (j *Journal) nextOrder() int {
	n := 0
	for _, r := range j.Applied {
		if r.Order >= n {
			n = r.Order + 1
		}
	}
	return n
}
