package store

import (
	"context"
	"fmt"
)

// PhaseApplier 是执行配置应用流程的端口。
//
// 由外壳注入实现：把本地 desired 内容转换为 Core 可接受的不可变快照，并按
// prepare、health-check、publish、drain 四阶段应用（FR-26）。Core 不接触本包的模型。
type PhaseApplier interface {
	// Apply 以 desired 内容为输入执行完整应用流程，返回各阶段结果。
	Apply(ctx context.Context, desiredContent string) ApplyOutcome
}

// ApplyOutcome 是一次应用流程的结果。
type ApplyOutcome struct {
	// Phases 是本轮各阶段的结果，顺序即执行顺序。
	Phases []PhaseOutcome
}

// PhaseOutcome 是单个阶段的结果。
type PhaseOutcome struct {
	Phase       string
	Succeeded   bool
	ErrorDetail string
}

// 阶段顺序常量，供应用流程实现组织结果。
var phaseOrder = []string{PhasePrepare, PhaseHealthCheck, PhasePublish, PhaseDrain}

// PhaseOrder 返回四阶段的固定顺序。
func PhaseOrder() []string {
	ordered := make([]string, len(phaseOrder))
	copy(ordered, phaseOrder)
	return ordered
}

// Recover 执行启动恢复：以本地 desired 为输入重新走完整应用流程。
//
// 硬边界：它不以落库的 active/last-good 记录充当运行态，也不把内存快照写回为 desired。
// active 在恢复起点视为空，只有 publish 成功才由 RecordApplyResult 推进落库记录。
func (s *Store) Recover(ctx context.Context, applier PhaseApplier) error {
	input, err := s.LoadRevisionForRecovery(ctx)
	if err != nil {
		return err
	}
	if !input.HasDesired {
		s.logger.Info("本地尚无已接收的配置版本，active 保持为空")
		return nil
	}
	if applier == nil {
		return fmt.Errorf("存在本地配置版本 %d，但未提供应用流程实现，拒绝恢复", input.DesiredRevision)
	}
	s.logger.Info("开始从本地 desired 恢复配置",
		"版本", input.DesiredRevision,
		"上次记录的active", input.RecordedActiveRevision,
		"上次记录的lastGood", input.RecordedLastGoodRevision)

	outcome := applier.Apply(ctx, input.DesiredContent)
	if len(outcome.Phases) == 0 {
		return fmt.Errorf("应用流程未返回任何阶段结果，恢复中止")
	}
	for _, phase := range outcome.Phases {
		err := s.Transaction(ctx, func(tx *Tx) error {
			return tx.RecordApplyResult(ApplyResultInput{
				Revision:    input.DesiredRevision,
				Phase:       phase.Phase,
				Succeeded:   phase.Succeeded,
				ErrorDetail: phase.ErrorDetail,
			})
		})
		if err != nil {
			return err
		}
	}

	state, err := s.LoadRevisionState(ctx)
	if err != nil {
		return err
	}
	s.logger.Info("配置恢复完成",
		"版本", input.DesiredRevision,
		"recordedActive", state.ActiveRevision,
		"recordedLastGood", state.LastGoodRevision)
	return nil
}

// LoadRevisionState 读取三个 revision 的当前落库取值。
func (s *Store) LoadRevisionState(ctx context.Context) (RevisionRecord, error) {
	var state RevisionRecord
	if err := s.View(ctx, func(tx *Tx) error {
		var err error
		state, err = tx.RevisionState()
		return err
	}); err != nil {
		return RevisionRecord{}, err
	}
	return state, nil
}
