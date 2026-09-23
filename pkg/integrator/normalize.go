package integrator

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// normalizeSet 把原始变更集归一为统一模型：
// 校验单条变更的合法性、规范化路径与行内容、补全 ID、计算内容指纹与排序键。
// 变更集层面的重复识别在 resolveOrder 中完成。
func normalizeSet(changes []Change) ([]*NormalizedChange, error) {
	if len(changes) == 0 {
		return nil, &RejectError{Reason: RejectInvalid, Message: "变更集为空"}
	}
	out := make([]*NormalizedChange, 0, len(changes))
	for i := range changes {
		n, err := normalizeOne(&changes[i])
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func normalizeOne(c *Change) (*NormalizedChange, error) {
	id := strings.TrimSpace(c.ID)
	if len(c.Files) == 0 {
		return nil, rejectInvalid(id, "变更不包含任何文件补丁")
	}

	seenPaths := make(map[string]bool, len(c.Files))
	files := make([]FilePatch, 0, len(c.Files))
	for fi := range c.Files {
		fp, err := normalizeFilePatch(&c.Files[fi], id)
		if err != nil {
			return nil, err
		}
		if seenPaths[fp.Path] {
			return nil, rejectInvalid(id, "同一变更内对目标文件重复打补丁: "+fp.Path)
		}
		seenPaths[fp.Path] = true
		files = append(files, fp)
	}

	fp := fingerprint(c.Message, files)
	if id == "" {
		// 调用方未提供稳定 ID：以内容指纹作为身份，天然可识别重复提交。
		id = "c" + fp[:12]
	}
	return &NormalizedChange{
		Change: Change{
			ID:      id,
			Depends: normalizeDeps(id, c.Depends),
			Message: c.Message,
			Files:   files,
		},
		Fingerprint: fp,
		OrderKey:    id,
	}, nil
}

func normalizeDeps(id string, deps []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || d == id || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func normalizeFilePatch(in *FilePatch, changeID string) (FilePatch, error) {
	p := path.Clean(strings.ReplaceAll(in.Path, "\\", "/"))
	if p == "." || p == "/" || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return FilePatch{}, rejectInvalid(changeID, "非法目标路径: "+in.Path)
	}
	if in.IsNew && in.IsDelete {
		return FilePatch{}, rejectInvalid(changeID, "文件补丁不能同时标记新建与删除: "+p)
	}
	hunks := make([]Hunk, 0, len(in.Hunks))
	for hi := range in.Hunks {
		h, err := normalizeHunk(&in.Hunks[hi], changeID, p, hi, in.IsNew)
		if err != nil {
			return FilePatch{}, err
		}
		hunks = append(hunks, h)
	}
	if !in.IsDelete && !in.IsNew && len(hunks) == 0 {
		return FilePatch{}, rejectInvalid(changeID, "文件补丁缺少补丁块: "+p)
	}
	return FilePatch{Path: p, IsNew: in.IsNew, IsDelete: in.IsDelete, Hunks: hunks}, nil
}

func normalizeHunk(in *Hunk, changeID, p string, hi int, isNew bool) (Hunk, error) {
	if in.OldStart < 0 || in.NewStart < 0 {
		return Hunk{}, rejectInvalid(changeID, "补丁块行号非法")
	}
	var oldN, newN int
	lines := make([]HunkLine, 0, len(in.Lines))
	for li, ln := range in.Lines {
		switch ln.Op {
		case OpContext, OpAdd, OpDel:
		default:
			return Hunk{}, rejectInvalid(changeID, "补丁块含未知行操作")
		}
		text := strings.TrimSuffix(ln.Text, "\r")
		if strings.ContainsAny(text, "\n\r") {
			return Hunk{}, rejectInvalid(changeID, "补丁行包含未剥离的换行符")
		}
		lines = append(lines, HunkLine{Op: ln.Op, Text: text})
		switch ln.Op {
		case OpContext, OpDel:
			oldN++
		}
		switch ln.Op {
		case OpContext, OpAdd:
			newN++
		}
		_ = li
	}
	if in.OldLines != oldN || in.NewLines != newN {
		return Hunk{}, rejectInvalid(changeID, "补丁块跨度与行内容不一致: "+p)
	}
	if isNew && oldN != 0 {
		return Hunk{}, rejectInvalid(changeID, "新建文件的补丁块不得含旧文件行: "+p)
	}
	start := in.NewStart
	if start == 0 {
		start = in.OldStart
	}
	return Hunk{OldStart: in.OldStart, OldLines: oldN, NewStart: start, NewLines: newN, Lines: lines}, nil
}

func rejectInvalid(id, msg string) error {
	return &RejectError{Reason: RejectInvalid, Message: msg, Detail: []string{id}}
}

// fingerprint 对消息与全部补丁内容做规范化哈希，与 ID 无关，
// 用于识别“同一变更换了标识再次提交”的重复提交。
func fingerprint(message string, files []FilePatch) string {
	h := sha256.New()
	var b strings.Builder
	b.WriteString("msg\x00")
	b.WriteString(message)
	b.WriteByte(0x0a)
	for _, f := range files {
		b.WriteString("file\x00")
		b.WriteString(f.Path)
		b.WriteByte(0)
		if f.IsNew {
			b.WriteByte('n')
		}
		if f.IsDelete {
			b.WriteByte('d')
		}
		b.WriteByte(0x0a)
		for _, hk := range f.Hunks {
			b.WriteString("hunk\x00")
			for _, ln := range hk.Lines {
				b.WriteString(string(ln.Op))
				b.WriteByte(0)
				b.WriteString(ln.Text)
				b.WriteByte(0x0a)
			}
		}
	}
	h.Write([]byte(b.String()))
	return hex.EncodeToString(h.Sum(nil))
}
