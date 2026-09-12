package spawner

// 深度闸（Part 9.7 闸 1，13.11 阶段 9 的显式归位）。之前内联在
// Adjudicate 里；阶段 9 起三层拓扑成为常规工作面，深度裁决独立成文件——
// 它是唯一需要在"到顶拒绝"与"权限配置"两处一致维护的拓扑规则。

import (
	"fmt"

	"marl/internal/proto"
)

// depthGate 是裁前的深度/权限裁决。
//
// 输入：请求者的深度与深度权限谓词（Config.CanSpawnAtDepth——阶段 7 起
// 它一般由 Profile 的 CanSpawn 按深度配置物化）。
// 输出：childDepth（批准时的子深度）与拒绝裁决（nil = 通过本闸）。
//
// 检查序即失败归因优先级（Part 9.2）：
//  1. 深度权限（不是深度问题而是角色问题 → PROFILE_NOT_PERMITTED）
//  2. 深度上限（角色允许但到顶 → MAX_DEPTH_REACHED）
func depthGate(canSpawn func(depth int) bool, requesterDepth, maxDepth int) (childDepth int, deny *proto.SpawnDecision) {
	if !canSpawn(requesterDepth) {
		return 0, rejectDecision(proto.SpawnErrNotPermitted,
			"你的角色不允许 fork 子 Agent；请用 file_write / file_edit 直接完成任务")
	}
	childDepth = requesterDepth + 1
	if childDepth > maxDepth {
		return 0, rejectDecision(proto.SpawnErrMaxDepth,
			"已达最大深度 %d，不能再 fork。请直接执行任务", maxDepth)
	}
	return childDepth, nil
}

// rejectDecision 是 depthGate 专属的拒绝构造（与包级 reject 同源；独立
// 命名只为避免 depthGate 的局部命名冲突）。
func rejectDecision(code proto.SpawnErrorCode, format string, args ...any) *proto.SpawnDecision {
	return &proto.SpawnDecision{
		Status: proto.SpawnRejected,
		Code:   code,
		Reason: fmt.Sprintf(format, args...),
	}
}
