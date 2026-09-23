package integrator

import (
	"fmt"
	"sort"
)

// resolveOrder 对归一后的变更集做顺序判定。
//
// 判定规则（任一不满足即返回可区分类别的 *RejectError，绝不静默丢弃）：
//  1. 目标重复：同一 ID 出现多次，或不同 ID 但内容指纹相同 → duplicate；
//  2. 缺少前置：Depends 引用了变更集之外（或不存在）的 ID → missing-dependency；
//  3. 依赖成环：显式依赖图存在环 → order-undecidable；
//  4. 无依据并列：两个变更作用于同一文件，且二者之间没有任何（传递）依赖
//     关系给出先后依据 → order-undecidable。不按到达顺序硬套，也不做任意破平。
//
// 通过校验后，按显式依赖做拓扑排序；同层就绪节点以 OrderKey 字典序稳定破平
// （这些节点作用范围互不重叠，先后次序不影响应用结论）。
func resolveOrder(changes []*NormalizedChange) ([]*NormalizedChange, error) {
	byID := make(map[string]*NormalizedChange, len(changes))
	fpOwner := make(map[string]string, len(changes))
	for _, c := range changes {
		if other, ok := byID[c.ID]; ok {
			return nil, &RejectError{
				Reason:  RejectDuplicate,
				Message: fmt.Sprintf("变更目标重复：ID %s 出现多次（%s / %s）", c.ID, other.Message, c.Message),
				Detail:  []string{c.ID},
			}
		}
		byID[c.ID] = c
		if other, ok := fpOwner[c.Fingerprint]; ok {
			return nil, &RejectError{
				Reason: RejectDuplicate,
				Message: fmt.Sprintf("重复提交：变更 %s 与 %s 内容指纹相同（%s），即使标识不同也视为同一变更",
					other, c.ID, shortID(c.Fingerprint)),
				Detail: []string{other, c.ID, c.Fingerprint},
			}
		}
		fpOwner[c.Fingerprint] = c.ID
	}

	// 依赖完整性校验。
	for _, c := range changes {
		for _, d := range c.Depends {
			if _, ok := byID[d]; !ok {
				return nil, &RejectError{
					Reason:  RejectMissingDep,
					Message: fmt.Sprintf("变更 %s 缺少前置依赖：%s 不在本次变更集中", c.ID, d),
					Detail:  []string{c.ID, d},
				}
			}
		}
	}

	// 依赖成环检测。
	if cyc := findCycle(changes, byID); cyc != nil {
		return nil, &RejectError{
			Reason:  RejectOrderUndecidable,
			Message: fmt.Sprintf("依赖关系成环，顺序不可确定：%v", cyc),
			Detail:  cyc,
		}
	}

	// 传递可达性：reach[a][b] 表示 a 必须先于 b（存在显式依赖路径）。
	reach := transitiveReach(changes, byID)

	// 作用范围重叠但缺乏先后依据的判定。
	pathsByID := make(map[string]map[string]bool, len(changes))
	for _, c := range changes {
		set := make(map[string]bool, len(c.Files))
		for _, f := range c.Files {
			set[f.Path] = true
		}
		pathsByID[c.ID] = set
	}
	ids := make([]string, 0, len(changes))
	for _, c := range changes {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a, b := ids[i], ids[j]
			if reach[a][b] || reach[b][a] {
				continue // 依赖关系已给出先后依据
			}
			if p := sharedPath(pathsByID[a], pathsByID[b]); p != "" {
				return nil, &RejectError{
					Reason: RejectOrderUndecidable,
					Message: fmt.Sprintf(
						"顺序不可确定：变更 %s 与 %s 的作用范围在 %s 上重叠，但二者之间没有任何依赖关系，"+
							"拒绝按到达顺序猜测先后", a, b, p),
					Detail: []string{a, b, p},
				}
			}
		}
	}

	return topoSort(changes, byID), nil
}

func sharedPath(a, b map[string]bool) string {
	for p := range a {
		if b[p] {
			return p
		}
	}
	return ""
}

// findCycle 使用三色 DFS 找出依赖环上的节点；无环返回 nil。
func findCycle(changes []*NormalizedChange, byID map[string]*NormalizedChange) []string {
	const white, gray, black = 0, 1, 2
	color := make(map[string]int, len(changes))
	stack := []string{}

	var visit func(id string) []string
	visit = func(id string) []string {
		color[id] = gray
		stack = append(stack, id)
		deps := append([]string(nil), byID[id].Depends...)
		sort.Strings(deps)
		for _, d := range deps {
			switch color[d] {
			case white:
				if cyc := visit(d); cyc != nil {
					return cyc
				}
			case gray:
				for k, x := range stack {
					if x == d {
						cyc := append([]string(nil), stack[k:]...)
						return append(cyc, d)
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}

	ids := make([]string, 0, len(changes))
	for _, c := range changes {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			if cyc := visit(id); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// transitiveReach 计算依赖边的传递闭包。
// 边 dep -> change 表示 dep 必须先于 change。
func transitiveReach(changes []*NormalizedChange, byID map[string]*NormalizedChange) map[string]map[string]bool {
	reach := make(map[string]map[string]bool, len(changes))
	for _, c := range changes {
		reach[c.ID] = map[string]bool{}
	}
	var walk func(from, cur string)
	walk = func(from, cur string) {
		for _, d := range byID[cur].Depends {
			if !reach[from][d] {
				reach[from][d] = true
				walk(from, d)
			}
		}
	}
	// reach[x] 记录 x 传递依赖的所有节点，即这些节点必须先于 x。
	// 因此 a 先于 b 等价于 reach[b][a]。为便于调用方直接查询，
	// 这里转换为 reach[a][b] == "a 先于 b"。
	deps := make(map[string]map[string]bool, len(changes))
	for _, c := range changes {
		deps[c.ID] = map[string]bool{}
	}
	for _, c := range changes {
		walk(c.ID, c.ID)
	}
	for _, c := range changes {
		for d := range reach[c.ID] {
			deps[d][c.ID] = true
		}
	}
	return deps
}

// topoSort 以 Kahn 算法做确定性拓扑排序：
// 每轮在零依赖（依赖已出队）的就绪节点中取 OrderKey 最小者。
func topoSort(changes []*NormalizedChange, byID map[string]*NormalizedChange) []*NormalizedChange {
	indeg := make(map[string]int, len(changes))
	successors := make(map[string][]string, len(changes))
	for _, c := range changes {
		indeg[c.ID] = len(c.Depends)
		for _, d := range c.Depends {
			successors[d] = append(successors[d], c.ID)
		}
	}
	ready := newIDHeap()
	for _, c := range changes {
		if indeg[c.ID] == 0 {
			ready.push(c.ID)
		}
	}
	out := make([]*NormalizedChange, 0, len(changes))
	for ready.len() > 0 {
		id := ready.pop()
		out = append(out, byID[id])
		succ := append([]string(nil), successors[id]...)
		sort.Strings(succ)
		for _, s := range succ {
			indeg[s]--
			if indeg[s] == 0 {
				ready.push(s)
			}
		}
	}
	return out
}

// idHeap 是按字典序取最小的就绪节点集合。
type idHeap struct{ items []string }

func newIDHeap() *idHeap { return &idHeap{} }

func (h *idHeap) len() int { return len(h.items) }

func (h *idHeap) push(id string) {
	h.items = append(h.items, id)
	for i := len(h.items) - 1; i > 0; {
		parent := (i - 1) / 2
		if h.items[parent] <= h.items[i] {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}

func (h *idHeap) pop() string {
	n := len(h.items)
	root := h.items[0]
	h.items[0] = h.items[n-1]
	h.items = h.items[:n-1]
	for i := 0; ; {
		l, r, best := 2*i+1, 2*i+2, i
		if l < len(h.items) && h.items[l] < h.items[best] {
			best = l
		}
		if r < len(h.items) && h.items[r] < h.items[best] {
			best = r
		}
		if best == i {
			break
		}
		h.items[i], h.items[best] = h.items[best], h.items[i]
		i = best
	}
	return root
}
