package integrator

// Mode 表示运行模式。
type Mode string

const (
	ModeRehearse Mode = "rehearse" // 预演：不落盘、不写状态
	ModeApply    Mode = "apply"    // 应用：写入工作区与状态日志
)

// Status 表示单条变更的结论状态。
type Status string

const (
	StatusApplied    Status = "applied"
	StatusSkipped    Status = "skipped"
	StatusConflicted Status = "conflicted"
	// StatusStale 接力式变更声明了依赖，但其预期前置内容与前序变更
	// 实际落地后的内容对不上（前置状态不匹配），与“两方抢同一处”的冲突可区分。
	StatusStale Status = "stale"
)

// Conflict 描述一个冲突的具体位置与来源。
type Conflict struct {
	ChangeID  string
	AgainstID string
	Path      string
	BaseStart int
	BaseEnd   int
	Detail    string
}

// Outcome 是单条变更的结论。
type Outcome struct {
	ChangeID string
	Status   Status
	Reason   string
	Conflict *Conflict
}

// Report 是一次运行对整个变更集的结论。
type Report struct {
	Mode      Mode
	Order     []string
	Outcomes  []Outcome
	Completed bool
}
