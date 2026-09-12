package spawner

// 子命名空间的构建与子集校验（Part 9.3 / 9.2）。
//
// 权限单调递减不变量：子.writable ⊆ 父.writable、子.readable ⊆ 父.readable、
// 父.hidden ⊆ 子.hidden（最后一条在"默认 hidden"模型下由前两条自动成立，
// 见 types.Namespace.Subset 的口径注释）。

import (
	"fmt"

	"marl/internal/proto"
	"marl/internal/types"
)

// buildNamespace 从请求与父命名空间构建子命名空间。
//
// 规则（Part 9.2 / 9.3）：
//   - 父的 read 挂载 → 子继承为 read（ReadablePaths 默认继承父的 read 面）；
//   - 父的 write 挂载 → 子默认**降级为 read**（宽读窄写的延续：父的可写
//     面对子默认只是可读），除非被 WritablePaths 覆盖；
//   - WritablePaths 的每一条必须是父可写范围的子集（提权在裁决层拒绝）；
//   - ReadablePaths 的每一条必须落在父可读 ∪ 可写范围内（显式请求的
//     读面不能凭空出现）。
//
// 返回的命名空间还要过 Subset 终检（Adjudicate 里做）——本函数的规则是
// 构建语义，Subset 是不变量，两层防线。
//
// 失败：带具体路径的错误（Part 9.4：告诉 LLM 正确的范围是什么）。
func buildNamespace(req *proto.SpawnRequest, parent *types.Namespace) (*types.Namespace, error) {
	if parent == nil {
		// 无父命名空间（测试/最小装配）：请求即授权（没有基准就没有
		// 子集语义）——调用方（Adjudicate）在此情形跳过 Subset 终检。
		ns := &types.Namespace{AgentID: "pending"}
		for _, w := range req.WritablePaths {
			ns.Mounts = append(ns.Mounts, types.Mount{Pattern: w, Mode: types.PathWrite})
		}
		for _, r := range req.ReadablePaths {
			ns.Mounts = append(ns.Mounts, types.Mount{Pattern: r, Mode: types.PathRead})
		}
		return ns, nil
	}
	ns := &types.Namespace{AgentID: "pending"}
	// 1. 继承父挂载：write 降级为 read。
	for _, m := range parent.Mounts {
		mode := m.Mode
		if mode == types.PathWrite {
			mode = types.PathRead
		}
		ns.Mounts = append(ns.Mounts, types.Mount{Pattern: m.Pattern, Mode: mode})
	}
	// 2. 显式可写请求：必须是父可写范围的子集。
	for _, w := range req.WritablePaths {
		if err := checkCovered(parent, w, types.PathWrite, "可写"); err != nil {
			return nil, err
		}
		ns.Mounts = append(ns.Mounts, types.Mount{Pattern: w, Mode: types.PathWrite})
	}
	// 3. 显式可读请求：必须落在父可读 ∪ 可写范围内。
	for _, r := range req.ReadablePaths {
		if err := checkCovered(parent, r, types.PathRead, "可读"); err != nil {
			return nil, err
		}
		// 已由继承覆盖（父 read/write 均已继承为 read）——重复挂载无害
		//（bestMount 取最具体），这里不追加，保持挂载表干净。
	}
	return ns, nil
}

// checkCovered 校验 pattern 请求被父命名空间中 mode ≥ need 的挂载覆盖。
//
// 失败：错误文本带"你只能分配……"的指引（Part 9.4 第二条的措辞纪律）。
func checkCovered(parent *types.Namespace, pattern string, need types.PathMode, action string) error {
	for _, m := range parent.Mounts {
		if modeRankAtLeast(m.Mode, need) && types.PatternCovers(m.Pattern, pattern) {
			return nil
		}
	}
	return fmt.Errorf("请求的%s路径 %s 超出你的范围。你只能分配这些范围内的路径：%v",
		action, pattern, grantedOf(parent, need))
}

// modeRankAtLeast 报告 m 的能力序是否 ≥ need。
func modeRankAtLeast(m, need types.PathMode) bool {
	return modeRankOf(m) >= modeRankOf(need)
}

func modeRankOf(m types.PathMode) int {
	switch m {
	case types.PathWrite:
		return 2
	case types.PathRead:
		return 1
	default:
		return 0
	}
}

// grantedOf 列出父命名空间中满足 need 的挂载模式（错误提示用）。
func grantedOf(parent *types.Namespace, need types.PathMode) []string {
	out := []string{}
	for _, m := range parent.Mounts {
		if modeRankAtLeast(m.Mode, need) && m.Mode != types.PathHidden {
			out = append(out, m.Pattern)
		}
	}
	if len(out) == 0 {
		out = append(out, "（无）")
	}
	return out
}
