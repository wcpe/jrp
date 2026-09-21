package core

import "errors"

// Stage 是配置应用状态机的阶段标记。
//
// 它既是失败定位（首个失败阶段），也是成功程度的表达（Applied 表示切换已生效，
// Drained 表示旧资源也排空完毕）。用具名枚举而不是裸字符串：宿主与外壳都要据此
// 分支，拼写漂移会让分支静默走错。
type Stage string

const (
	// StageValidate 是快照结构校验与 revision 比较阶段。
	StageValidate Stage = "validate"
	// StagePrepare 是构造新资源的阶段；此阶段不影响 active。
	StagePrepare Stage = "prepare"
	// StageHealthCheck 是验证新资源可用性的阶段。
	StageHealthCheck Stage = "health-check"
	// StageApplied 表示切换已生效，但 drain 未完成或无需 drain。
	//
	// 旧资源排空超上限时也停在这里：publish 已成功、切换不可撤销，把阶段退回
	// 会误导宿主以为变更没生效。
	StageApplied Stage = "applied"
	// StageDrained 表示切换已生效且旧资源按上限排空完毕。
	StageDrained Stage = "drained"
)

// ErrApplyInProgress 表示同一 Engine 上已有一次 Apply 正在进行。
//
// 并发调用直接返回它，不排队也不静默合并：排队与 latest-wins 属宿主的控制面
// 策略（ADR-0005），Core 代它决定会让上层无法表达自己的取舍。
var ErrApplyInProgress = errors.New("已有配置应用正在进行")

// Binding 是宿主注入的网络资源。
//
// 当前版本不接受宿主注入资源：访客入口一律由 Core 按快照自建（规格 §2）。本类型
// 与 Deployment.Bindings 保留的唯一目的是让"传了注入资源"这一意图可被**明确拒绝**
// 而不是静默忽略——静默忽略会让宿主误以为注入生效，而实际的资源归属、复用与关闭
// 承诺全都落空。两个 Engine 都在 validate 阶段以 `Stage: Validate` 拒绝非空绑定。
type Binding struct {
	// ID 是宿主为同一逻辑资源指定的稳定标识。
	ID string
	// Resource 是宿主注入的资源。
	Resource any
}

// ApplyError 携带失败的阶段，宿主用 errors.As 取出后按阶段分支。
//
// 读阶段而不解析错误文本：文本随措辞变化，阶段是稳定契约（规格 §3.7）。
type ApplyError struct {
	// Stage 是首个失败的阶段。
	Stage Stage
	err   error
}

// NewApplyError 构造带阶段的失败。
func NewApplyError(stage Stage, err error) *ApplyError {
	return &ApplyError{Stage: stage, err: err}
}

// Error 返回含阶段的可读表达；原因由下层提供，本身不含凭据。
func (applyError *ApplyError) Error() string {
	return "配置应用在 " + string(applyError.Stage) + " 阶段失败：" + applyError.err.Error()
}

// Unwrap 返回失败原因，使 errors.Is 可判定底层哨兵（例如 ErrConfigInvalid）。
func (applyError *ApplyError) Unwrap() error {
	return applyError.err
}

// ApplyResult 是一次 Apply 的结果。
//
// 变更计数用于让宿主判断"是否真的切了东西"：revision 递增但 Changed 与 Drained
// 都为 0，说明内容等价、只是版本号推进了。
type ApplyResult struct {
	// Revision 是本次应用的 revision。
	Revision uint64
	// Previous 是切换前的 active revision；首次应用为 0。
	Previous uint64
	// Stage 是终止阶段。
	Stage Stage
	// Changed 是本次新增或变化的资源数。
	Changed int
	// Drained 是本次排空的资源数。
	Drained int
	// DrainIncomplete 表示排空超上限、剩余连接被强制释放。
	//
	// 单独成字段而不是塞进 Stage：publish 已成功这一事实必须能被宿主区分出来，
	// 否则宿主的"应用成功"分支会被排空异常整个吃掉。
	DrainIncomplete bool
}

// Published 判断切换是否已经生效。
//
// 排空异常不影响该结论：切换不可撤销，排空问题由宿主另行告警（规格 §3.6）。
func (result ApplyResult) Published() bool {
	return result.Stage == StageApplied || result.Stage == StageDrained
}

// SnapshotRevisionUnknown 是尚未建立 active 时的 revision 取值。
//
// 用 0 而不是哨兵类型：revision 由宿主分配并单调递增，宿主从 1 开始即可，
// 0 自然成为"还没有 active"的无歧义表达。
const SnapshotRevisionUnknown uint64 = 0
