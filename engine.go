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

// RelayMismatchError 表示声明了依赖关系的接力变更，其预期前置内容
// 与实际应用后的内容对不上（与 ConflictError 的“两方抢同一处”可区分）。
type RelayMismatchError struct {
	ChangeID string
	Path     string
	Anchor   int      // 预期位置的 1-based 锚点行号
	Expected []string // 预期前置内容（该 hunk 的旧块）
	Actual   []string // 实际该位置的内容
}

func (e *RelayMismatchError) Error() string {
	return fmt.Sprintf("relay mismatch: change=%q at %s anchor line %d: expected %q, actual %q",
		e.ChangeID, e.Path, e.Anchor, e.Expected, e.Actual)
}

// Unwrap 使 errors.Is(err, ErrRelayPrecondition) 成立。
func (e *RelayMismatchError) Unwrap() error { return ErrRelayPrecondition }

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

		// 5.2 冲突判定与接力识别：与所有更早的（含跳过的）已应用变更比较基准作用范围。
		// 重叠对象若在当前变更的传递依赖闭包中，属于“接力式演进”，不判冲突，
		// 改由应用原语严格校验前置内容；否则仍是“两方抢同一处”的冲突。
		closure := depClosure(ch, plan.Changes)
		c, relay := firstConflict(ch, accum, closure)
		if c != nil {
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
		if relay {
			e.opts.Logger.Printf("change=%s deps=%v position=%d decision=relay basis=overlaps_only_own_dependencies",
				id, deps, pos)
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
		// 接力变更走严格应用：锚点处必须精确匹配预期前置内容，不得换位置套用。
		var aerr error
		if relay {
			aerr = applyChangeStrict(ws, ch)
		} else {
			aerr = applyChange(ws, ch)
		}
		if aerr != nil {
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
			// 接力前置状态对不上：给出与冲突可区分的失败结论。
			if relay && errors.Is(aerr, ErrRelayPrecondition) {
				rm := relayMismatchDetails(ws, ch)
				report.Outcomes = append(report.Outcomes, Outcome{
					ChangeID: id,
					Status:   StatusMismatch,
					Reason: fmt.Sprintf("relay precondition mismatch at %s anchor line %d: expected %q, actual %q",
						rm.Path, rm.Anchor, rm.Expected, rm.Actual),
				})
				e.opts.Logger.Printf("change=%s deps=%v position=%d decision=mismatch path=%s anchor=%d expected=%q actual=%q",
					id, deps, pos, rm.Path, rm.Anchor, rm.Expected, rm.Actual)
				return report, rm
			}
			return report, fmt.Errorf("apply change %s: %w", id, aerr)
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
		if relay {
			e.opts.Logger.Printf("change=%s deps=%v position=%d decision=applied mode=%s relay=true basis=precondition_verified_at_anchor",
				id, deps, pos, mode)
		} else {
			e.opts.Logger.Printf("change=%s deps=%v position=%d decision=applied mode=%s", id, deps, pos, mode)
		}
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

// depClosure 返回 ch 的传递依赖闭包。不在本次变更集中、但已在此前运行中
// 应用的前置（仅作为叶子 ID 出现）也计入闭包，以支持只提交剩余变更的续跑。
func depClosure(ch *Change, changes map[string]*Change) map[string]bool {
	closure := map[string]bool{}
	stack := append([]string(nil), ch.DependsOn...)
	for len(stack) > 0 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if closure[d] {
			continue
		}
		closure[d] = true
		if dep, ok := changes[d]; ok {
			stack = append(stack, dep.DependsOn...)
		}
	}
	return closure
}

// firstConflict 返回 ch 与已占范围的首个冲突（按来源 ID 与位置确定，保证稳定）。
// 重叠对象若全部落在 closure（ch 的传递依赖闭包）中，则不视为冲突：
// 返回 relay=true，表示这是接力式演进，由应用原语严格校验前置内容。
func firstConflict(ch *Change, accum *scopeAccumulator, closure map[string]bool) (*Conflict, bool) {
	type hit struct {
		against string
		sc      Scope
	}
	var hits []hit
	relay := false
	for _, mine := range ch.Scopes {
		for _, e := range accum.entries {
			if !scopesOverlap(mine, e.scope) {
				continue
			}
			if closure[e.changeID] {
				relay = true
				continue
			}
			hits = append(hits, hit{against: e.changeID, sc: mine})
		}
	}
	if len(hits) == 0 {
		return nil, relay
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
	}, relay
}

// relayMismatchDetails 在严格应用失败后定位第一个与预期前置内容不符的 hunk，
// 构造包含具体位置、预期与实际内容的 RelayMismatchError。
func relayMismatchDetails(ws Workspace, ch *Change) *RelayMismatchError {
	for _, f := range ch.Files {
		cur, exists, err := ws.Read(f.Path)
		if f.Create {
			if err == nil && exists {
				return &RelayMismatchError{ChangeID: ch.ID, Path: f.Path, Anchor: 0,
					Expected: nil, Actual: cur}
			}
			continue
		}
		if err != nil || !exists {
			return &RelayMismatchError{ChangeID: ch.ID, Path: f.Path, Anchor: f.OldStart,
				Expected: f.OldBlock, Actual: nil}
		}
		anchor := f.OldStart - 1
		if anchor >= 0 && anchor+len(f.OldBlock) <= len(cur) &&
			equalLines(cur[anchor:anchor+len(f.OldBlock)], f.OldBlock) {
			continue
		}
		var actual []string
		if anchor >= 0 && anchor < len(cur) {
			end := anchor + len(f.OldBlock)
			if end > len(cur) {
				end = len(cur)
			}
			actual = append([]string(nil), cur[anchor:end]...)
		}
		return &RelayMismatchError{ChangeID: ch.ID, Path: f.Path, Anchor: f.OldStart,
			Expected: f.OldBlock, Actual: actual}
	}
	// 严格应用已失败，理论上必有不符的 hunk；此处仅为兜底。
	return &RelayMismatchError{ChangeID: ch.ID}
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
