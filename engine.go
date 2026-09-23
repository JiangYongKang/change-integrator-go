package integrator

import (
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"time"
)

// Options 是引擎选项。
type Options struct {
	// Logger 用于打印每条变更的判定依据、跳过原因与冲突位置。
	Logger *log.Logger
	// BeforeChange 在每条未跳过的变更应用前调用；返回非 nil 将中断本次运行
	// （测试用故障注入：模拟 CI 进程被 kill）。
	BeforeChange func(id string) error
}

// Engine 是变更集成引擎。
type Engine struct {
	ws          Workspace
	journalPath string
	opts        Options
}

// ErrFingerprintMismatch 表示同一 ID 的变更内容与上次成功应用时不一致。
var ErrFingerprintMismatch = errors.New("change fingerprint differs from previously applied record")

// ConflictError 表示运行中出现重叠冲突；引擎在首个冲突处停止。
type ConflictError struct {
	Conflict Conflict
}

func (e *ConflictError) Error() string {
	c := e.Conflict
	return fmt.Sprintf("conflict: change=%q overlaps with=%q at %s base-lines [%d,%d): %s",
		c.ChangeID, c.AgainstID, c.Path, c.BaseStart, c.BaseEnd, c.Detail)
}

// NewEngine 创建引擎。
func NewEngine(ws Workspace, journalPath string, opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	return &Engine{ws: ws, journalPath: journalPath, opts: opts}
}

// Rehearse 在不落盘的前提下预演变更集：工作区写入仅存在于内存覆盖层，
// 状态日志只读不写。对同一前置状态，结论与 Apply 完全一致。
func (e *Engine) Rehearse(raw []RawChange) (*Report, error) {
	return e.run(raw, ModeRehearse)
}

// Apply 实际应用变更集并持久化状态；重入时从状态日志处继续。
func (e *Engine) Apply(raw []RawChange) (*Report, error) {
	return e.run(raw, ModeApply)
}

// run 是两种模式共享的执行流程，保证结论一致的唯一来源。
func (e *Engine) run(raw []RawChange, mode Mode) (*Report, error) {
	// 1. 归一化。
	changes := make([]*Change, 0, len(raw))
	for _, r := range raw {
		c, err := Normalize(r)
		if err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}

	// 2. 载入跨运行状态（预演只读不写），随后去重 / 依赖 / 顺序判定；
	// 存在拒绝项时整个集合不执行。此前已应用的变更也算满足的前置。
	journal, err := LoadJournal(e.journalPath)
	if err != nil {
		return nil, err
	}
	appliedIDs := map[string]bool{}
	for id := range journal.Applied {
		appliedIDs[id] = true
	}
	plan, err := BuildPlanWithApplied(changes, appliedIDs)
	if err != nil {
		return nil, err
	}
	if len(plan.Rejections) > 0 {
		return &Report{Mode: mode, Order: plan.Order}, &RejectedError{Rejections: plan.Rejections}
	}

	// 4. 工作区：预演使用读穿透/写缓冲的覆盖层，应用使用真实工作区。
	ws := Workspace(e.ws)
	if mode == ModeRehearse {
		ws = &overlayWorkspace{base: e.ws, overlay: map[string][]string{}}
	}

	report := &Report{Mode: mode, Order: append([]string(nil), plan.Order...)}

	// 已在之前运行中应用的变更，其作用范围仍参与后续冲突判定。
	accum := newScopeAccumulator()
	var journalIDs []string
	for id := range journal.Applied {
		journalIDs = append(journalIDs, id)
	}
	sort.Strings(journalIDs)
	for _, id := range journalIDs {
		rec := journal.Applied[id]
		for _, sc := range rec.Scopes {
			accum.add(id, sc)
		}
	}

	// 5. 严格按计划顺序处理。
	for pos, id := range plan.Order {
		ch := plan.Changes[id]
		deps := append([]string(nil), ch.DependsOn...)
		sort.Strings(deps)

		// 5.1 已成功应用（本次重复提交 / 中断后续跑）：识别并跳过，不重复应用。
		if rec, ok := journal.Applied[id]; ok {
			if rec.Fingerprint != ch.Fingerprint {
				e.opts.Logger.Printf("change=%s deps=%v position=%d decision=error reason=fingerprint_mismatch",
					id, deps, pos)
				return report, fmt.Errorf("change %s: %w", id, ErrFingerprintMismatch)
			}
			report.Outcomes = append(report.Outcomes, Outcome{
				ChangeID: id,
				Status:   StatusSkipped,
				Reason:   "already applied in a previous run; recognized via journal and skipped",
			})
			e.opts.Logger.Printf("change=%s deps=%v position=%d decision=skipped basis=journal_record order=%d",
				id, deps, pos, rec.Order)
			continue
		}

		// 5.2 冲突判定：与所有更早的（含跳过的）已应用变更比较基准作用范围。
		if c := firstConflict(ch, accum); c != nil {
			report.Outcomes = append(report.Outcomes, Outcome{
				ChangeID: id,
				Status:   StatusConflicted,
				Conflict: c,
				Reason: fmt.Sprintf("overlaps change %q at %s base-lines [%d,%d)",
					c.AgainstID, c.Path, c.BaseStart, c.BaseEnd),
			})
			e.opts.Logger.Printf("change=%s deps=%v position=%d decision=conflicted against=%s path=%s base=[%d,%d)",
				id, deps, pos, c.AgainstID, c.Path, c.BaseStart, c.BaseEnd)
			return report, &ConflictError{Conflict: *c}
		}

		// 5.3 中断注入点（应用前）。
		if e.opts.BeforeChange != nil {
			if ierr := e.opts.BeforeChange(id); ierr != nil {
				e.opts.Logger.Printf("change=%s deps=%v position=%d decision=interrupted detail=%v",
					id, deps, pos, ierr)
				return report, ierr
			}
		}

		// 5.4 应用（预演落到覆盖层；应用落到真实工作区）。
		if err := applyChange(ws, ch); err != nil {
			// 崩溃窗口安全网：若工作区内容已呈现该变更的结果，则视为已应用并补记日志。
			if mode == ModeApply && detectAlreadyApplied(e.ws, ch) {
				rec := AppliedRecord{ID: id, Fingerprint: ch.Fingerprint,
					Order: journal.nextOrder(), AppliedAt: time.Now().UTC(), Scopes: ch.Scopes}
				journal.markApplied(rec)
				if serr := journal.save(e.journalPath); serr != nil {
					return report, serr
				}
				report.Outcomes = append(report.Outcomes, Outcome{
					ChangeID: id,
					Status:   StatusSkipped,
					Reason:   "workspace already contained the change; journal backfilled and skipped",
				})
				for _, sc := range ch.Scopes {
					accum.add(id, sc)
				}
				continue
			}
			return report, fmt.Errorf("apply change %s: %w", id, err)
		}

		// 5.5 记录结论。预演不持久化；应用在工作区写入成功后立即落状态日志。
		if mode == ModeApply {
			rec := AppliedRecord{ID: id, Fingerprint: ch.Fingerprint,
				Order: journal.nextOrder(), AppliedAt: time.Now().UTC(), Scopes: ch.Scopes}
			journal.markApplied(rec)
			if err := journal.save(e.journalPath); err != nil {
				return report, fmt.Errorf("persist journal after %s: %w", id, err)
			}
		}
		report.Outcomes = append(report.Outcomes, Outcome{
			ChangeID: id,
			Status:   StatusApplied,
			Reason:   fmt.Sprintf("applied in topological position %d; deps=%v", pos, deps),
		})
		for _, sc := range ch.Scopes {
			accum.add(id, sc)
		}
		e.opts.Logger.Printf("change=%s deps=%v position=%d decision=applied mode=%s", id, deps, pos, mode)
	}

	report.Completed = len(report.Outcomes) == len(plan.Order)
	return report, nil
}

// scopeEntry 是已占作用范围的内部记录。
type scopeEntry struct {
	changeID string
	scope    Scope
}

// scopeAccumulator 累加更早变更占用的基准作用范围。
type scopeAccumulator struct {
	entries []scopeEntry
}

func newScopeAccumulator() *scopeAccumulator { return &scopeAccumulator{} }

func (a *scopeAccumulator) add(changeID string, sc Scope) {
	a.entries = append(a.entries, scopeEntry{changeID: changeID, scope: sc})
}

// firstConflict 返回 ch 与已占范围的首个冲突（按来源 ID 与位置确定，保证稳定）。
func firstConflict(ch *Change, accum *scopeAccumulator) *Conflict {
	type hit struct {
		against string
		sc      Scope
	}
	var hits []hit
	for _, mine := range ch.Scopes {
		for _, e := range accum.entries {
			if scopesOverlap(mine, e.scope) {
				hits = append(hits, hit{against: e.changeID, sc: mine})
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].against != hits[j].against {
			return hits[i].against < hits[j].against
		}
		if hits[i].sc.Path != hits[j].sc.Path {
			return hits[i].sc.Path < hits[j].sc.Path
		}
		return hits[i].sc.Start < hits[j].sc.Start
	})
	h := hits[0]
	detail := "both changes modify the same base lines"
	if h.sc.Create {
		detail = "both changes create the same target file"
	}
	return &Conflict{
		ChangeID:  ch.ID,
		AgainstID: h.against,
		Path:      h.sc.Path,
		BaseStart: h.sc.Start,
		BaseEnd:   h.sc.End,
		Detail:    detail,
	}
}

// scopesOverlap 判定两个基准作用范围是否冲突：
//   - 同路径的文件创建与任何改动（含另一次创建）冲突；
//   - 普通行区间按半开区间相交判定（端点相接不算冲突，插入点相同算冲突）。
func scopesOverlap(a, b Scope) bool {
	if a.Path != b.Path {
		return false
	}
	if a.Create || b.Create {
		return true
	}
	// 两个零长度插入落在同一锚点时结果顺序敏感，视为冲突。
	if a.Start == a.End && b.Start == b.End {
		return a.Start == b.Start
	}
	return a.Start < b.End && b.Start < a.End
}

// overlayWorkspace 在读穿透底层工作区的基础上缓冲写入，预演专用。
type overlayWorkspace struct {
	base    Workspace
	overlay map[string][]string
}

func (o *overlayWorkspace) Read(path string) ([]string, bool, error) {
	if ls, ok := o.overlay[path]; ok {
		return append([]string(nil), ls...), true, nil
	}
	return o.base.Read(path)
}

func (o *overlayWorkspace) Write(path string, lines []string) error {
	o.overlay[path] = append([]string(nil), lines...)
	return nil
}

// detectAlreadyApplied 是“工作区已写但日志未落盘”崩溃窗口下的内容级识别：
// 逐文件比对当前内容是否正是该变更应用后的结果。
func detectAlreadyApplied(ws Workspace, ch *Change) bool {
	byPath := map[string][]FileChange{}
	for _, f := range ch.Files {
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		hunks := byPath[path]
		cur, exists, err := ws.Read(path)
		if err != nil || !exists {
			return false
		}
		if hunks[0].Create {
			var want []string
			for _, h := range hunks {
				want = append(want, h.NewBlock...)
			}
			if !equalLines(cur, want) {
				return false
			}
			continue
		}
		// 每个 hunk 的新块必须能在文件中唯一或在锚点定位（锚点优先，否则唯一搜索）。
		for _, h := range hunks {
			if _, perr := locateNewBlock(cur, h); perr != nil {
				return false
			}
		}
	}
	return true
}

// locateNewBlock 在已应用后的文件内容中定位 hunk 的新块。
func locateNewBlock(cur []string, h FileChange) (int, error) {
	anchor := h.OldStart - 1
	if anchor >= 0 && anchor+len(h.NewBlock) <= len(cur) &&
		equalLines(cur[anchor:anchor+len(h.NewBlock)], h.NewBlock) {
		return anchor, nil
	}
	var found []int
	for i := 0; i+len(h.NewBlock) <= len(cur); i++ {
		if equalLines(cur[i:i+len(h.NewBlock)], h.NewBlock) {
			found = append(found, i)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return 0, ErrContextMismatch
	}
	return 0, fmt.Errorf("%w: new block matches %d locations", ErrContextMismatch, len(found))
}
