package integrator

import "fmt"

// fileState 是应用过程中对单个文件的内存视图。
type fileState struct {
	path    string
	present bool
	lines   []string
	// owner[i] 标记 lines[i] 最近一次由哪个变更落盘（空串为基准原始行）。
	owner []string
}

// newFileState 从基准内容构造文件视图。
func newFileState(path string, base []string, present bool) *fileState {
	lines := append([]string(nil), base...)
	owner := make([]string, len(lines))
	return &fileState{path: path, present: present, lines: lines, owner: owner}
}

// applyFileChange 把单个文件补丁应用到文件视图上。
// 成功返回更新后的新视图（不就地修改，便于失败时保证已应用内容不被破坏）；
// 失败返回带定位信息与来源变更的 ConflictLocation。
func applyFileChange(fs *fileState, changeID string, fp FilePatch) (*fileState, *ConflictLocation) {
	if fp.IsDelete {
		if !fs.present {
			return nil, conflict(changeID, fp.Path, 0, 0, "删除目标文件不存在", "")
		}
		return &fileState{path: fs.path, present: false, lines: nil, owner: nil}, nil
	}

	if fp.IsNew {
		if fs.present {
			// 文件已存在：若由其它变更创建，指出来源。
			owners := distinctOwners(fs.owner)
			src := ""
			if len(owners) > 0 {
				src = owners[0]
			}
			return nil, conflict(changeID, fp.Path, 0, 0, "新建目标文件已存在", src)
		}
		lines, err := materializeNew(changeID, fp)
		if err != nil {
			return nil, err
		}
		return &fileState{path: fs.path, present: true, lines: lines, owner: filledOwners(len(lines), changeID)}, nil
	}

	if !fs.present {
		return nil, conflict(changeID, fp.Path, 0, 0, "目标文件不存在，且补丁未标记为新建", "")
	}

	// 原地修改：依次应用各 hunk，严格定位，禁止跨已消费区间回溯。
	result := append([]string(nil), fs.lines...)
	owner := append([]string(nil), fs.owner...)
	minSearch := 0
	for hi := range fp.Hunks {
		h := fp.Hunks[hi]
		pos, src, detail := locateHunk(result, owner, h, minSearch, changeID, fp.Path, hi)
		if pos < 0 {
			return nil, conflict(changeID, fp.Path, hi, srcLine(h), detail, src)
		}
		result = spliceHunk(result, &owner, pos, h, changeID)
		// 下一个 hunk 的匹配起点推进到本次产出之后，保证块间有序、不重叠。
		minSearch = pos + countOld(h)
	}
	return &fileState{path: fs.path, present: true, lines: result, owner: owner}, nil
}

// materializeNew 把新建文件的补丁块展开为完整行内容。
func materializeNew(changeID string, fp FilePatch) ([]string, *ConflictLocation) {
	var lines []string
	for hi := range fp.Hunks {
		for _, ln := range fp.Hunks[hi].Lines {
			if ln.Op != OpAdd {
				return nil, conflict(changeID, fp.Path, hi, 0, "新建文件补丁块只能包含新增行", "")
			}
			lines = append(lines, ln.Text)
		}
	}
	return lines, nil
}

// locateHunk 在 lines[minSearch:] 中查找与 hunk 的旧侧（上下文+删除行）
// 逐字一致的位置。
//
// 定位规则：优先使用 hunk.OldStart-1 给出的声明位置；若该处不匹配，再在
// 允许的搜索区间内扫描，但只接受“唯一匹配”；扫描得到多个候选即视为定位
// 歧义并判冲突——绝不猜测。
//
// 返回值 pos 为匹配起始下标（插入型 hunk 为插入点下标）；src 为导致冲突的
// 来源变更 ID（空串表示基准内容本身）。
func locateHunk(lines, owners []string, h Hunk, minSearch int, changeID, path string, hi int) (pos int, src string, detail string) {
	old := oldSide(h)
	if len(old) == 0 {
		// 纯插入块：以声明的新文件行号为插入点（在该行之前插入）。
		p := h.NewStart - 1
		if p < 0 {
			p = 0
		}
		if p > len(lines) {
			p = len(lines)
		}
		return p, "", ""
	}

	// 1) 声明位置精确匹配（允许此前插入造成的漂移，但不跨消费区间）。
	if cand := h.OldStart - 1; cand >= minSearch && cand+len(old) <= len(lines) {
		if matchAt(lines, cand, old) {
			return cand, "", ""
		}
	}

	// 2) 全允许区间扫描，收集候选。
	var hits []int
	for c := minSearch; c+len(old) <= len(lines); c++ {
		if matchAt(lines, c, old) {
			hits = append(hits, c)
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], "", ""
	case len(hits) > 1:
		return -1, "", fmt.Sprintf("补丁块在基准中存在多处相同匹配（候选行 %v），定位有歧义，拒绝猜测应用",
			offsetToOneBased(hits))
	}

	// 3) 无匹配：找出声明位置处第一个不一致的行，并给出其来源变更。
	expectIdx := h.OldStart - 1
	if expectIdx < minSearch {
		expectIdx = minSearch
	}
	if expectIdx > len(lines) {
		expectIdx = len(lines)
	}
	for k, want := range old {
		idx := expectIdx + k
		if idx >= len(lines) {
			return -1, "", fmt.Sprintf("补丁块期望第 %d 行存在内容，但文件仅 %d 行（可能被前置变更截断）",
				idx+1, len(lines))
		}
		if lines[idx] != want {
			s := owners[idx]
			why := "基准内容与补丁上下文不一致"
			if s != "" && s != changeID {
				why = "上下文行已被变更 " + s + " 修改"
			}
			return -1, s, fmt.Sprintf("%s：期望 %q，实际 %q（文件第 %d 行）", why, want, lines[idx], idx+1)
		}
	}
	return -1, "", "补丁块无法定位"
}

// spliceHunk 用 hunk 的新侧替换 lines[pos:pos+oldCount]，并同步维护 owner。
func spliceHunk(lines []string, owner *[]string, pos int, h Hunk, changeID string) []string {
	oldCount := countOld(h)
	var newLines []string
	var newOwner []string
	oldCursor := pos // 上下文/删除行在旧视图中的对应下标
	for _, ln := range h.Lines {
		switch ln.Op {
		case OpDel:
			oldCursor++
		case OpContext:
			// 上下文行保留原内容与原来源；只有新增/替换才归属本变更。
			newLines = append(newLines, ln.Text)
			newOwner = append(newOwner, ownerAt(*owner, oldCursor))
			oldCursor++
		case OpAdd:
			newLines = append(newLines, ln.Text)
			newOwner = append(newOwner, changeID)
		}
	}
	tail := append([]string(nil), lines[pos+oldCount:]...)
	tailOwner := append([]string(nil), (*owner)[pos+oldCount:]...)
	out := append(append([]string(nil), lines[:pos]...), newLines...)
	*owner = append(append([]string(nil), (*owner)[:pos]...), newOwner...)
	out = append(out, tail...)
	*owner = append(*owner, tailOwner...)
	return out
}

func ownerAt(owner []string, idx int) string {
	if idx >= 0 && idx < len(owner) {
		return owner[idx]
	}
	return ""
}

func oldSide(h Hunk) []string {
	var out []string
	for _, ln := range h.Lines {
		if ln.Op == OpContext || ln.Op == OpDel {
			out = append(out, ln.Text)
		}
	}
	return out
}

func countOld(h Hunk) int {
	n := 0
	for _, ln := range h.Lines {
		if ln.Op == OpContext || ln.Op == OpDel {
			n++
		}
	}
	return n
}

func matchAt(lines []string, pos int, want []string) bool {
	if pos+len(want) > len(lines) {
		return false
	}
	for i, w := range want {
		if lines[pos+i] != w {
			return false
		}
	}
	return true
}

func filledOwners(n int, id string) []string {
	o := make([]string, n)
	for i := range o {
		o[i] = id
	}
	return o
}

func distinctOwners(owner []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range owner {
		if o != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

func offsetToOneBased(idxs []int) []int {
	out := make([]int, len(idxs))
	for i, v := range idxs {
		out[i] = v + 1
	}
	return out
}

func srcLine(h Hunk) int {
	if h.OldStart > 0 {
		return h.OldStart
	}
	return h.NewStart
}

func conflict(changeID, path string, hunkIdx, line int, detail, source string) *ConflictLocation {
	return &ConflictLocation{
		Change:       changeID,
		Path:         path,
		HunkIdx:      hunkIdx,
		Line:         line,
		Detail:       detail,
		SourceChange: source,
	}
}
