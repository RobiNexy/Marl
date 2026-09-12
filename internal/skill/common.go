package skill

// 路径技能共享的 Resolver 桥与错误分流。

import (
	"errors"
	"fmt"

	"marl/internal/ns"

	"marl/internal/types"
)

// resolvedPath 是 Resolver 检查通过后的路径事实（types.PathResolution 的
// 技能内部别名，统一消费写法）。
type resolvedPath = types.PathResolution

// 授权/注册的错误哨兵（主循环机械翻译错误码的依据——Part 9.4"如实回填"）。
var (
	// ErrNotAllowed：技能不在许可白名单（Authorizer 返回）。
	ErrNotAllowed = errors.New("skill not allowed")
	// ErrNotRegistered：技能未注册（Registry.Get 返回）。
	ErrNotRegistered = errors.New("skill not registered")
)

// ResultFromAuthorizeError 把授权/注册阶段的错误翻译成模型可见的业务失败。
// 错误码集合（SKILL_NOT_ALLOWED / 未知技能→UNKNOWN_SKILL 系字符串）与
// 接口注释的约定一致；实现侧只认哨兵，不解析文本。
func ResultFromAuthorizeError(err error) *SkillResult {
	switch {
	case errors.Is(err, ErrNotAllowed):
		return &SkillResult{OK: false, ErrorType: ErrSkillNotAllowed, Message: err.Error()}
	case errors.Is(err, ErrNotRegistered):
		return &SkillResult{OK: false, ErrorType: "UNKNOWN_SKILL", Message: err.Error()}
	default:
		return &SkillResult{OK: false, ErrorType: ErrNamespaceExceeded, Message: err.Error()}
	}
}

// errBusiness 是技能层把"业务失败"从"基础设施故障"里分流的标记
// （Skill.Execute 契约：两类失败分属 SkillResult 与 error 通道）。技能
// 内部把"结论是不行"打成 errBusiness，businessOf 统一翻译成
// SkillResult{OK:false}；反方向的错误保持 error != nil 上抛。
type errBusiness struct {
	code string
	msg  string
}

func (e errBusiness) Error() string { return e.msg }

// resolvePath 是路径类技能与命名空间沙箱（Part 5.4）的唯一桥：
// 所有 syscall 前的地址必须从这里来。Resolve 失败在此统一翻译成错误码。
func resolvePath(env *SkillEnv, path string, need types.PathMode) (resolvedPath, error) {
	if env == nil || env.Resolver == nil {
		return resolvedPath{}, errBusiness{code: ErrNotFound,
			msg: "no resolver attached (framework bug: SkillEnv.Resolver must not be nil)"}
	}
	res, err := env.Resolver.Resolve(env.Namespace, path, need)
	if err != nil {
		return resolvedPath{}, translateResolveErr(err)
	}
	return res, nil
}

// translateResolveErr 把 Resolver 的错误哨兵翻译成错误码（错误码表）：
//
//	ns.ErrUnavailable → ENOENT        （hidden 的对外伪装，Part 5.1——
//	                                   越界与不存在共用同一错误码，不暴露挂载表）
//	ns.ErrReadonly    → PATH_READONLY
//	其余              → NAMESPACE_EXCEEDED（兜底，原始文本保留在 message）
func translateResolveErr(err error) error {
	switch {
	case errors.Is(err, ns.ErrUnavailable):
		return errBusiness{code: ErrNotFound, msg: fmt.Sprintf("path not available (ENOENT): %v", err)}
	case errors.Is(err, ns.ErrReadonly):
		return errBusiness{code: ErrPathReadonly, msg: err.Error()}
	default:
		return errBusiness{code: ErrNamespaceExceeded, msg: err.Error()}
	}
}

// businessOf 把错误分流到 Skill.Execute 的两种失败形态：
// errBusiness → 业务失败（SkillResult，模型可见）；其它 → 原样透传
// （基础设施故障，上层处置）。
func businessOf(err error) (*SkillResult, error) {
	var eb errBusiness
	if errors.As(err, &eb) {
		return &SkillResult{OK: false, ErrorType: eb.code, Message: eb.msg}, nil
	}
	return nil, err
}
