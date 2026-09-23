package integrator

import (
	"fmt"
	"sort"
	"strings"
)

// RejectReason 是变更集在归一/排序阶段被拒绝的可区分原因。
type RejectReason string

const (
	// RejectMissingDependency 变更声明的前置不在本次变更集中。
	RejectMissingDependency RejectReason = "missing_dependency"
	// RejectOrderUndecidable 依赖关系成环，不存在满足依赖的全序。
	RejectOrderUndecidable RejectReason = "order_undecidable"
	// RejectDuplicateID 同一变更 ID 在同一变更集中出现多次。
	RejectDuplicateID RejectReason = "duplicate_id"
	// RejectDuplicateChange 不同 ID 的变更携带完全相同的补丁内容（指纹相同）。
	RejectDuplicateChange RejectReason = "duplicate_change"
)

// Rejection 描述一条被拒绝的变更及原因。
type Rejection struct {
	Reason    RejectReason
	ChangeID  string
	RelatedID string // 相关对象：缺失的前置 / 先到的同 ID / 同指纹变更
	Detail    string
}

// Plan 是通过校验后的可执行计划。
type Plan struct {
	Order      []string
	Changes    map[string]*Change
	Rejections []Rejection
}

// RejectedError 表示变更集未通过归一/顺序校验，整个集合被拒绝执行。
type RejectedError struct {
	Rejections []Rejection
}

func (e *RejectedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "change set rejected (%d reason(s))", len(e.Rejections))
	for _, r := range e.Rejections {
		fmt.Fprintf(&b, "\n  - [%s] change=%q", r.Reason, r.ChangeID)
		if r.RelatedID != "" {
			fmt.Fprintf(&b, " related=%q", r.RelatedID)
		}
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
	}
	return b.String()
}

// BuildPlan 对归一化变更做去重、依赖校验与拓扑排序。
// 只要存在任何拒绝项，调用方必须中止执行（见 RejectedError）。
func BuildPlan(changes []*Change) (*Plan, error) {
	return BuildPlanWithApplied(changes, nil)
}

// BuildPlanWithApplied 与 BuildPlan 相同，但额外把 alreadyApplied 中的 ID
// 视为已满足的前置（来自此前运行的状态日志），支持只提交剩余变更的续跑场景。
func BuildPlanWithApplied(changes []*Change, alreadyApplied map[string]bool) (*Plan, error) {
	p := &Plan{Changes: map[string]*Change{}}

	// 1. 重复 ID 与重复内容（指纹）校验。
	firstByID := map[string]int{}
	firstByFP := map[string]string{}
	for i, c := range changes {
		if j, ok := firstByID[c.ID]; ok {
			p.Rejections = append(p.Rejections, Rejection{
				Reason:    RejectDuplicateID,
				ChangeID:  c.ID,
				RelatedID: changes[j].ID,
				Detail:    fmt.Sprintf("change id appears more than once (occurrences #%d and #%d)", j+1, i+1),
			})
			continue
		}
		firstByID[c.ID] = i
		if other, ok := firstByFP[c.Fingerprint]; ok {
			p.Rejections = append(p.Rejections, Rejection{
				Reason:    RejectDuplicateChange,
				ChangeID:  c.ID,
				RelatedID: other,
				Detail:    "identical patch content (same fingerprint); resubmission must reuse the original change id",
			})
			continue
		}
		firstByFP[c.Fingerprint] = c.ID
		p.Changes[c.ID] = c
	}
	if len(p.Rejections) > 0 {
		return p, nil
	}

	// 2. 前置依赖存在性校验（自依赖也会在此显式标注为缺少前置）。
	deps := map[string][]string{}
	for id, c := range p.Changes {
		seen := map[string]bool{}
		for _, d := range c.DependsOn {
			if seen[d] {
				continue
			}
			seen[d] = true
			deps[id] = append(deps[id], d)
			if d == id {
				p.Rejections = append(p.Rejections, Rejection{
					Reason:    RejectMissingDependency,
					ChangeID:  id,
					RelatedID: d,
					Detail:    "change declares itself as a prerequisite",
				})
				continue
			}
			if _, ok := p.Changes[d]; !ok && !alreadyApplied[d] {
				p.Rejections = append(p.Rejections, Rejection{
					Reason:    RejectMissingDependency,
					ChangeID:  id,
					RelatedID: d,
					Detail:    "prerequisite change is neither part of this change set nor previously applied",
				})
			}
		}
	}
	if len(p.Rejections) > 0 {
		return p, nil
	}

	// 3. Kahn 拓扑排序；可用节点按 ID 字典序选取，使无依赖歧义的顺序确定。
	indeg := map[string]int{}
	dependents := map[string][]string{}
	var ids []string
	for id := range p.Changes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, d := range deps[id] {
			// 仅本次变更集内的前置参与建图；此前已应用的前置没有节点，不计入入度。
			if _, inPlan := p.Changes[d]; !inPlan {
				continue
			}
			indeg[id]++
			dependents[d] = append(dependents[d], id)
		}
	}
	ready := make([]string, 0)
	for _, id := range ids {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	for len(ready) > 0 {
		sort.Strings(ready)
		id := ready[0]
		ready = ready[1:]
		p.Order = append(p.Order, id)
		followers := append([]string(nil), dependents[id]...)
		sort.Strings(followers)
		for _, f := range followers {
			indeg[f]--
			if indeg[f] == 0 {
				ready = append(ready, f)
			}
		}
	}

	// 4. 未排出的节点全部处于依赖环中：顺序不可判定。
	if len(p.Order) != len(p.Changes) {
		var stuck []string
		for id, n := range indeg {
			if n > 0 {
				stuck = append(stuck, id)
			}
		}
		sort.Strings(stuck)
		for _, id := range stuck {
			p.Rejections = append(p.Rejections, Rejection{
				Reason:   RejectOrderUndecidable,
				ChangeID: id,
				Detail:   fmt.Sprintf("dependency cycle among: %s", strings.Join(stuck, ", ")),
			})
		}
		return p, nil
	}
	return p, nil
}
