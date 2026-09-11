package types

// AttachmentKind 是附件种类（Part 10.14）。
// v1 只完整支持 AttachImage；AttachPDF / AttachFile 进枚举但 v1 不实现。
//
// 零值契约：零值 AttachmentKind("") 非法，且**不得**被当作 AttachImage——
// "未指定类型"与"这是图片"是两个不同的声明，猜错会让二进制内容以图片的
// 方式送进模型，产生难以归因的解析失败。
//
// 未实现种类（v1 的 PDF/File）不等于"未识别值"：它们是已知但尚不支持，
// 必须返回明确的"暂不支持"错误，而不是静默丢弃附件——静默丢弃会让模型在
// 完全不知情的情况下基于残缺信息作答（它以为自己看到了那张图）。
type AttachmentKind string

const (
	AttachImage AttachmentKind = "image"
	AttachPDF   AttachmentKind = "pdf"  // v1 不实现
	AttachFile  AttachmentKind = "file" // v1 不实现
)

// Valid 报告 k 是否为三个已定义种类之一。零值返回 false。
func (k AttachmentKind) Valid() bool {
	switch k {
	case AttachImage, AttachPDF, AttachFile:
		return true
	}
	return false
}

// Supported 报告 k 在当前版本是否已实现。已知但未实现的种类必须由调用方
// 显式报错（见本类型注释），而不是靠"忘了 switch 分支"来兜底。
func (k AttachmentKind) Supported() bool { return k == AttachImage }

// AttachmentSource 描述 Attachment.Data 的解释方式（Part 10.3 / 10.14）。
//
// 零值契约（安全相关）：零值 AttachmentSource("") 非法，且**绝不允许**被
// 默认解释为 SourceBase64。原因是两个方向的错误代价并不对称：
//
//	把路径当 base64 —— 请求体里出现一段看似随机的字节，外部 API 报"无效图片"，
//	                   泄露面仅限于路径字符串本身；
//	把 base64 当路径 —— 框架拿这串字节去读运行主机上的文件，可能读到任意路径
//	                   （越界读取），并把文件内容带进日志与模型上下文。
//
// 因此所有消费点必须显式 switch，未识别值一律判错（fail fast），禁止写成
// "default 分支 = base64"。
type AttachmentSource string

const (
	// SourceBase64 表示 Data 携带 base64 原文。注意：base64 原文不存 SQLite，
	// 只在请求组装时短暂存在于内存。
	SourceBase64 AttachmentSource = "base64"
	SourceURL    AttachmentSource = "url"
	SourceFile   AttachmentSource = "file_path"
)

// Valid 报告 s 是否为三个已定义来源之一。零值返回 false。
func (s AttachmentSource) Valid() bool {
	switch s {
	case SourceBase64, SourceURL, SourceFile:
		return true
	}
	return false
}

// Attachment 是一个消息附件（v1 仅图片）。
//
// 存储规则（关键）：图片 base64 原文不存 SQLite，落到文件（.marl/attachments/，
// 不进版本控制）；Data 按 Source 解释为文件路径或 URL。Normalizer 发请求时才
// 读文件、按协议翻译。
//
// 不变量：
//   - Kind / Source 必须 Valid，零值非法；
//   - MimeType 非空——它是模型端解释字节的依据，不允许"猜"；
//   - Source == SourceFile 时 Data 是**路径字节**而非内容，消费点必须先经
//     命名空间 Resolver 校验该路径再读文件。附件是第二条读文件通道，
//     绕过 Resolver 就等于绕过整个命名空间沙箱（Part 5.4）；
//   - Detail 仅对 AttachImage 有意义，其它种类必须为空（否则配置会被静默忽略）。
//
// 零值契约：Attachment{} 非法（无类型、无 MIME、无来源）。
type Attachment struct {
	Kind     AttachmentKind
	MimeType string
	Source   AttachmentSource
	Data     []byte // 按 Source 解释；SourceFile 时为路径字节
	Detail   string // vision_detail 档位（low / high / auto）
}

// Validate 报告该附件是否可安全进入请求组装。
//
// 失败：Kind / Source 非法；MimeType 为空；非图片种类却带 Detail。
// 注意：Validate 只查自洽性，不查可读性——文件是否存在、路径是否在命名空间内
// 属消费点（Normalizer）的职责，因为那需要 Resolver 与文件系统。
//
// 并发：纯函数。
func (a Attachment) Validate() error {
	panic("TODO(phase 0): placeholder")
}
