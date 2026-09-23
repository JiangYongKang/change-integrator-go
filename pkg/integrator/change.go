// Package integrator 实现面向持续集成的变更集成引擎：
// 对一组待入主干的变更进行归一、顺序判定、预演(dry-run)与应用(apply)，
// 并输出可复现、可解释的结论。
package integrator

// Mode 表示一次集成运行的模式。
type Mode string

const (
	// ModeDryRun 预演：只计算结论，不修改基准工作区，也不写入续跑状态。
	ModeDryRun Mode = "dry-run"
	// ModeApply 应用：按判定顺序真正落盘变更，并持久化续跑状态。
	ModeApply Mode = "apply"
)

// LineOp 是 HunkLine 的行级操作类型。
type LineOp string

const (
	OpContext LineOp = " " // 上下文行，必须与基准内容逐字一致
	OpAdd     LineOp = "+" // 新增行
	OpDel     LineOp = "-" // 删除行
)

// HunkLine 表示补丁块中的一行。
type HunkLine struct {
	Op   LineOp // 行操作：上下文 / 新增 / 删除
	Text string // 行内容，不含行尾换行
}

// Hunk 表示一个文件补丁块，统一以 unified-diff 风格表达。
type Hunk struct {
	OldStart int        // 旧文件起始行号（1 起）；新文件追加时为 0
	OldLines int        // 旧文件跨度（上下文+删除行计数）
	NewStart int        // 新文件起始行号（1 起）
	NewLines int        // 新文件跨度（上下文+新增行计数）
	Lines    []HunkLine // 块内的行序列
}

// FilePatch 表示对单个文件的一组有序补丁块。
type FilePatch struct {
	Path     string // 目标文件相对路径，统一以 "/" 分隔
	IsNew    bool   // 是否新建文件
	IsDelete bool   // 是否删除文件
	Hunks    []Hunk // 补丁块；新建文件时通常只有一个全新增块
}

// Change 是调用方提交的原始变更（提交或补丁归一后的输入模型）。
type Change struct {
	// ID 是变更的稳定唯一标识（如提交哈希）。空值由引擎按内容指纹生成。
	ID string
	// Depends 声明本变更显式依赖的变更 ID。
	Depends []string
	// Message 用于日志与报告展示的描述信息。
	Message string
	// Files 是该变更包含的全部文件补丁。
	Files []FilePatch
}

// NormalizedChange 是归一后的变更：附带内容指纹与排序键。
type NormalizedChange struct {
	Change
	// Fingerprint 为内容指纹：对 Files 与 Message 规范化后计算，与 ID 无关。
	Fingerprint string
	// OrderKey 为确定性排序键（归一为 ID），用于在依赖关系之外稳定破平。
	OrderKey string
}

// Status 是单个变更在一次运行中的结论。
type Status string

const (
	StatusApplied  Status = "applied"  // 成功应用
	StatusSkipped  Status = "skipped"  // 跳过：此前已成功应用
	StatusConflict Status = "conflict" // 冲突：未应用，且不影响已应用的变更
)

// ConflictLocation 给出冲突的具体位置与来源。
type ConflictLocation struct {
	Change       string `json:"change"`                 // 产生冲突的变更 ID
	Path         string `json:"path"`                   // 冲突文件路径
	HunkIdx      int    `json:"hunkIndex"`              // 变更内第几个补丁块（0 起）
	Line         int    `json:"line"`                   // 基准文件中的行号（1 起，0 表示无法定位到行）
	Detail       string `json:"detail"`                 // 人类可读的冲突原因
	SourceChange string `json:"sourceChange,omitempty"` // 导致冲突的已落盘变更 ID；空表示源于基准本身
}

// EntryResult 是单个变更的运行结论。
type EntryResult struct {
	ID        string             `json:"id"`
	Status    Status             `json:"status"`
	Reason    string             `json:"reason,omitempty"` // skipped/conflict 的可解释原因
	Conflicts []ConflictLocation `json:"conflicts,omitempty"`
}

// Report 是一次运行的完整结论。
type Report struct {
	Mode    Mode          `json:"mode"`
	Order   []string      `json:"order"` // 判定后的应用顺序（变更 ID）
	Results []EntryResult `json:"results"`
	// Rejected 非空表示变更集在归一/顺序判定阶段被整体拒绝，未进入应用。
	Rejected *RejectError `json:"rejected,omitempty"`
}

// RejectReason 是可区分的拒绝原因类别。
type RejectReason string

const (
	RejectDuplicate        RejectReason = "duplicate"          // 重复提交：同 ID 或同内容指纹
	RejectMissingDep       RejectReason = "missing-dependency" // 缺少前置依赖
	RejectOrderUndecidable RejectReason = "order-undecidable"  // 顺序不可确定（依赖成环/无依据并列重叠）
	RejectInvalid          RejectReason = "invalid-change"     // 变更自身非法
)

// RejectError 表示变更集在归一/顺序判定阶段被明确拒绝。
type RejectError struct {
	Reason  RejectReason `json:"reason"`
	Message string       `json:"message"`
	// Detail 承载与具体拒绝原因相关的标识，例如重复 ID、缺失依赖、成环节点。
	Detail []string `json:"detail,omitempty"`
}

func (e *RejectError) Error() string {
	return string(e.Reason) + ": " + e.Message
}
