package integrator

// Workspace 是引擎对基准工作区与续跑状态的抽象。
type Workspace interface {
	// ReadFile 读取基准文件；文件不存在时 (nil, false, nil)。
	ReadFile(path string) (lines []string, exists bool, err error)
	// WriteFile 落盘文件内容。
	WriteFile(path string, lines []string) error
	// RemoveFile 删除文件；文件不存在不报错。
	RemoveFile(path string) error
	// LoadState 读取续跑状态；状态文件不存在时 (nil, nil)。
	LoadState() (*State, error)
	// SaveState 写入续跑状态。
	SaveState(s *State) error
}

// State 是续跑状态：记录一次集成会话中已成功应用的变更，以及它们触及文件的
// “应用后镜像”。镜像使续跑无需对磁盘做启发式补丁重放即可精确重建现场
// （链式修改、崩溃窗口都能正确处理），并保留逐行来源用于冲突归因。
type State struct {
	// SetID 标识变更集（由变更集内容指纹派生），用于识别“同一变更集被再次提交”。
	SetID string `json:"setID"`
	// Applied 按实际应用顺序记录已成功应用的变更。
	Applied []AppliedRecord `json:"applied"`
	// Files 是已应用变更触及文件的当前（应用后）镜像；
	// 未出现在其中的文件被视为保持基准原样，续跑时直接从磁盘读取。
	Files map[string]FileImage `json:"files,omitempty"`
}

// FileImage 是单个文件在“已应用变更之后”的完整镜像。
type FileImage struct {
	// Present 为 false 表示该文件在应用后应不存在（被删除）。
	Present bool `json:"present"`
	// Lines 为应用后的完整行内容。
	Lines []string `json:"lines,omitempty"`
	// Owners[i] 标记 Lines[i] 最近由哪个变更落盘（空串为基准原始行）。
	Owners []string `json:"owners,omitempty"`
}

// AppliedRecord 记录一个已成功应用的变更。
type AppliedRecord struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Order       int    `json:"order"` // 在判定顺序中的位置
}

// HasID 报告状态中是否已存在某变更 ID。
func (s *State) HasID(id string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.Applied {
		if r.ID == id {
			return true
		}
	}
	return false
}

// LookupID 返回指定 ID 的已应用记录。
func (s *State) LookupID(id string) (AppliedRecord, bool) {
	if s == nil {
		return AppliedRecord{}, false
	}
	for _, r := range s.Applied {
		if r.ID == id {
			return r, true
		}
	}
	return AppliedRecord{}, false
}

// LookupFingerprint 返回指定内容指纹的已应用记录，
// 用于识别“同一内容换 ID 再次提交”的重复场景。
func (s *State) LookupFingerprint(fp string) (AppliedRecord, bool) {
	if s == nil {
		return AppliedRecord{}, false
	}
	for _, r := range s.Applied {
		if r.Fingerprint == fp {
			return r, true
		}
	}
	return AppliedRecord{}, false
}

func cloneState(s *State) *State {
	if s == nil {
		return nil
	}
	cp := &State{SetID: s.SetID, Applied: append([]AppliedRecord(nil), s.Applied...)}
	if s.Files != nil {
		cp.Files = make(map[string]FileImage, len(s.Files))
		for p, img := range s.Files {
			cp.Files[p] = FileImage{
				Present: img.Present,
				Lines:   append([]string(nil), img.Lines...),
				Owners:  append([]string(nil), img.Owners...),
			}
		}
	}
	return cp
}
