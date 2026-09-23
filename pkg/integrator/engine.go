package integrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
)

// Logger 用于输出可解释的判定日志。
type Logger interface {
	Printf(format string, args ...any)
}

// discardLogger 在调用方未提供 logger 时丢弃日志。
type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// nopLogger 在测试中可替换为带缓冲的记录器。
var _ Logger = discardLogger{}

// Engine 是变更集成引擎。
type Engine struct {
	ws     Workspace
	logger Logger
}

// New 创建引擎。logger 为 nil 时丢弃日志。
func New(ws Workspace, logger Logger) *Engine {
	if logger == nil {
		logger = discardLogger{}
	}
	return &Engine{ws: ws, logger: logger}
}

// Integrate 对变更集执行预演或应用。
//
// 两种模式共用同一条执行路径（同一顺序、同一匹配/冲突判定、同一状态识别），
// 因此对同一基准与同一续跑状态，两种模式产出完全一致的 applied/skipped/
// conflict 集合；唯一差异是 apply 会真正写文件并持久化状态，dry-run 不会。
//
// 归一/顺序判定失败时返回 *RejectError，且不改动任何文件。
func (e *Engine) Integrate(mode Mode, changes []Change) (*Report, error) {
	if mode != ModeDryRun && mode != ModeApply {
		return nil, fmt.Errorf("未知运行模式: %q", mode)
	}

	normalized, err := normalizeSet(changes)
	if err != nil {
		return rejectReport(mode, err), err
	}
	ordered, err := resolveOrder(normalized)
	if err != nil {
		return rejectReport(mode, err), err
	}

	orderIDs := make([]string, len(ordered))
	for i, c := range ordered {
		orderIDs[i] = c.ID
	}
	setID := computeSetID(ordered)
	e.logger.Printf("[%s] 变更集 %s：归一 %d 项，判定顺序 %v", mode, setID[:12], len(ordered), orderIDs)

	// 载入续跑状态。只有属于同一变更集的状态才被承认，避免跨集误跳。
	loaded, err := e.ws.LoadState()
	if err != nil {
		return nil, err
	}
	state := &State{SetID: setID, Files: map[string]FileImage{}}
	files := map[string]*fileState{}
	if loaded != nil && loaded.SetID == setID {
		state = cloneState(loaded)
		if state.Files == nil {
			state.Files = map[string]FileImage{}
		}
		if len(state.Applied) > 0 {
			e.logger.Printf("[%s] 识别到同一变更集的续跑状态：已应用 %d 项 %v",
				mode, len(state.Applied), appliedIDList(state))
			// 直接以状态中保存的“应用后镜像”重建内存视图，无需对磁盘做
			// 启发式补丁重放；镜像含逐行来源，冲突归因不受影响。
			loadImagesIntoView(files, state)
			// apply 模式下对磁盘做一次性幂等对账：补上“状态已写、文件未刷”
			// 崩溃窗口造成的差异；dry-run 绝不写盘。
			if mode == ModeApply {
				if err := e.reconcileAll(files, state); err != nil {
					return nil, err
				}
			}
		}
	} else if loaded != nil {
		e.logger.Printf("[%s] 现有状态属于其他变更集（%s），忽略并从空状态开始",
			mode, shortID(loaded.SetID))
	}

	report := &Report{Mode: mode, Order: orderIDs}

	for idx, c := range ordered {
		// 续跑/重复提交：按 ID 或内容指纹识别已成功应用的变更并跳过，
		// 不重复应用、不静默丢失。
		if rec, already := state.LookupID(c.ID); already {
			entry := EntryResult{ID: c.ID, Status: StatusSkipped,
				Reason: fmt.Sprintf("变更已在上次运行中成功应用（顺序位置 %d），跳过", rec.Order)}
			report.Results = append(report.Results, entry)
			e.logger.Printf("[%s] 跳过变更 %s：ID 命中已应用记录（判定依据：续跑状态位置 %d）",
				mode, c.ID, rec.Order)
			continue
		}
		if rec, ok := state.LookupFingerprint(c.Fingerprint); ok {
			entry := EntryResult{ID: c.ID, Status: StatusSkipped,
				Reason: fmt.Sprintf("变更内容与已应用的 %s 指纹相同（%s），按重复提交跳过", rec.ID, shortID(c.Fingerprint))}
			report.Results = append(report.Results, entry)
			e.logger.Printf("[%s] 跳过变更 %s：内容指纹与已应用的 %s 相同（判定依据：指纹 %s）",
				mode, c.ID, rec.ID, shortID(c.Fingerprint))
			continue
		}

		e.logger.Printf("[%s] 开始应用变更 %s（顺序位置 %d，依赖 %v）",
			mode, c.ID, idx, c.Depends)

		// 在当前累积视图上依次应用该变更的全部文件补丁。
		var changeConflicts []ConflictLocation
		for fi := range c.Files {
			fp := c.Files[fi]
			fs, err := e.loadFileView(files, fp.Path)
			if err != nil {
				return nil, err
			}
			next, loc := applyFileChange(fs, c.ID, fp)
			if loc != nil {
				changeConflicts = append(changeConflicts, *loc)
				e.logger.Printf("[%s] 冲突：变更 %s @ %s（块 %d，行 %d）来源=%s 原因=%s",
					mode, loc.Change, loc.Path, loc.HunkIdx, loc.Line,
					emptyAs(loc.SourceChange, "<基准>"), loc.Detail)
				break
			}
			files[fp.Path] = next
		}

		if len(changeConflicts) > 0 {
			// 冲突：本变更未应用，此前已干净应用的变更保持落盘，不回退；
			// 同时停止后续变更（顺序上它们在本变更之后），保证结论稳定。
			report.Results = append(report.Results, EntryResult{
				ID:        c.ID,
				Status:    StatusConflict,
				Reason:    "变更存在冲突，未应用；后续变更保持未处理",
				Conflicts: changeConflicts,
			})
			e.logger.Printf("[%s] 变更 %s 判定为冲突，停止于顺序位置 %d；已应用变更不回退",
				mode, c.ID, idx)
			break
		}

		// 全部文件补丁干净通过：先持久化“已应用”状态，再真正落盘内容。
		// 状态先行保证：即使进程在落盘过程中被中断，续跑时该变更也会被
		// 识别为已应用而跳过，绝不重复应用；反之（先落盘后存状态）会留下
		// “已落盘但未登记”的窗口，可能导致重复应用。
		if mode == ModeApply {
			record := AppliedRecord{ID: c.ID, Fingerprint: c.Fingerprint, Order: idx}
			state.Applied = append(state.Applied, record)
			// 把本变更产生的“应用后镜像”写入状态，连同“已应用”记录一起
			// 先行持久化；随后才真正落盘文件（状态先行，杜绝重复应用）。
			storeChangeImages(state, c, files)
			if err := e.ws.SaveState(state); err != nil {
				state.Applied = state.Applied[:len(state.Applied)-1]
				return nil, fmt.Errorf("变更 %s 的续跑状态持久化失败，本次应用中止: %w", c.ID, err)
			}
			writeErr := e.writeChangeFiles(c, files)
			if writeErr != nil {
				return nil, fmt.Errorf("变更 %s 已登记但内容落盘失败，续跑时将安全跳过: %w", c.ID, writeErr)
			}
		}
		verb := "已应用"
		if mode == ModeDryRun {
			verb = "预演通过（记为 applied，不写盘）"
		}
		report.Results = append(report.Results, EntryResult{ID: c.ID, Status: StatusApplied})
		e.logger.Printf("[%s] 变更 %s %s（顺序位置 %d）", mode, c.ID, verb, idx)
	}

	return report, nil
}

// loadImagesIntoView 用续跑状态中保存的“应用后镜像”直接重建内存文件视图，
// 不做任何补丁重放。镜像由引擎自身在应用时写入，因而与应用后现场逐字一致，
// 并携带逐行来源（owner）以支持冲突归因。
func loadImagesIntoView(cache map[string]*fileState, state *State) {
	paths := make([]string, 0, len(state.Files))
	for p := range state.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		img := state.Files[p]
		cache[p] = &fileState{
			path:    p,
			present: img.Present,
			lines:   append([]string(nil), img.Lines...),
			owner:   append([]string(nil), img.Owners...),
		}
	}
}

// reconcileAll 在 apply 模式续跑开始时对镜像覆盖的文件做一次性磁盘对账：
// 磁盘内容与镜像一致则不写；不一致（源于“状态已写、文件未刷”的崩溃窗口，
// 或文件被删除未完成）则幂等补刷到镜像内容。该过程不重跑任何变更，
// 也不修改续跑状态；dry-run 不调用本函数。
func (e *Engine) reconcileAll(cache map[string]*fileState, state *State) error {
	paths := make([]string, 0, len(state.Files))
	for p := range state.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		view := cache[p]
		disk, exists, err := e.ws.ReadFile(p)
		if err != nil {
			return err
		}
		switch {
		case view.present && (!exists || !equalLines(disk, view.lines)):
			e.logger.Printf("[apply] 续跑对账：补刷 %s（上次可能在落盘前中断）", p)
			if err := e.ws.WriteFile(p, view.lines); err != nil {
				return err
			}
		case !view.present && exists:
			e.logger.Printf("[apply] 续跑对账：补删除 %s（上次可能在落盘前中断）", p)
			if err := e.ws.RemoveFile(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// storeChangeImages 把一个已应用变更触及文件的当前视图保存进续跑状态镜像，
// 与“已应用”记录在同一次状态持久化中原子落盘。
func storeChangeImages(state *State, c *NormalizedChange, cache map[string]*fileState) {
	if state.Files == nil {
		state.Files = map[string]FileImage{}
	}
	for _, fp := range c.Files {
		view := cache[fp.Path]
		state.Files[fp.Path] = FileImage{
			Present: view.present,
			Lines:   append([]string(nil), view.lines...),
			Owners:  append([]string(nil), view.owner...),
		}
	}
}

// writeChangeFiles 把一个已通过校验的变更的全部文件视图落盘。
func (e *Engine) writeChangeFiles(c *NormalizedChange, cache map[string]*fileState) error {
	for _, fp := range c.Files {
		view := cache[fp.Path]
		if view.present {
			if err := e.ws.WriteFile(fp.Path, view.lines); err != nil {
				return err
			}
		} else {
			if err := e.ws.RemoveFile(fp.Path); err != nil {
				return err
			}
		}
	}
	return nil
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

// loadFileView 懒加载基准文件到内存视图；已缓存的视图包含此前变更的累积效果。
func (e *Engine) loadFileView(cache map[string]*fileState, path string) (*fileState, error) {
	if fs, ok := cache[path]; ok {
		return fs, nil
	}
	lines, exists, err := e.ws.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fs := newFileState(path, lines, exists)
	cache[path] = fs
	return fs, nil
}

// Plan 是预演模式的便捷入口。
func (e *Engine) Plan(changes []Change) (*Report, error) { return e.Integrate(ModeDryRun, changes) }

// Apply 是应用模式的便捷入口。
func (e *Engine) Apply(changes []Change) (*Report, error) { return e.Integrate(ModeApply, changes) }

// computeSetID 由归一后变更集的（ID, 指纹）序列派生变更集标识，
// 与提交顺序无关地唯一标识“同一变更集”。
func computeSetID(ordered []*NormalizedChange) string {
	type pair struct{ id, fp string }
	pairs := make([]pair, 0, len(ordered))
	for _, c := range ordered {
		pairs = append(pairs, pair{c.ID, c.Fingerprint})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	h := sha256.New()
	for _, p := range pairs {
		io.WriteString(h, p.id)
		h.Write([]byte{0})
		io.WriteString(h, p.fp)
		h.Write([]byte{0x0a})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rejectReport(mode Mode, err error) *Report {
	r := &Report{Mode: mode}
	if re, ok := err.(*RejectError); ok {
		r.Rejected = re
	}
	return r
}

func appliedIDList(s *State) []string {
	out := make([]string, 0, len(s.Applied))
	for _, r := range s.Applied {
		out = append(out, r.ID)
	}
	return out
}

func shortID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// AppliedIDs 返回结论中所有成功应用的变更 ID。
func (r *Report) AppliedIDs() []string { return r.idsByStatus(StatusApplied) }

// SkippedIDs 返回被跳过的变更 ID。
func (r *Report) SkippedIDs() []string { return r.idsByStatus(StatusSkipped) }

// ConflictIDs 返回冲突的变更 ID。
func (r *Report) ConflictIDs() []string { return r.idsByStatus(StatusConflict) }

func (r *Report) idsByStatus(st Status) []string {
	var out []string
	for _, x := range r.Results {
		if x.Status == st {
			out = append(out, x.ID)
		}
	}
	return out
}
