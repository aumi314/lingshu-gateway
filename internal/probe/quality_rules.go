package probe

const (
	reportFormatVersion = 3
	qualityRuleVersion  = "quality-v3.1"
	capabilityBlock     = "capability"
	billingBlock        = "billing"
	experienceBlock     = "experience"
	decidable           = "decidable"
	observation         = "observation"
)

type ReportBlock struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Verdict string `json:"verdict,omitempty"`
	Items   []Item `json:"items"`
}

type ReportRule struct {
	ID         string           `json:"id"`
	Block      string           `json:"block"`
	Title      string           `json:"title"`
	AppliesTo  string           `json:"applies_to"`
	Thresholds map[string]int64 `json:"thresholds,omitempty"`
	Evidence   string           `json:"evidence"`
	Source     string           `json:"source"`
	State      string           `json:"state"`
	Version    string           `json:"version"`
	Required   bool             `json:"required,omitempty"`
}

var qualityRules = map[string]ReportRule{
	"tokens": {Block: capabilityBlock, Title: "分词指纹", AppliesTo: "有分词对照的 GPT / Responses 与 Claude / Messages",
		Thresholds: map[string]int64{"tolerance_tokens": 1, "coverage_numerator": 2, "coverage_denominator": 3, "match_numerator": 5, "match_denominator": 6, "mismatch_cutoff_pct": 50, "fixed_offset_min_samples": 2},
		Evidence:   "比较探针与同轮基准请求的输入差值，固定偏差单列", Source: "internal/probe/data/baselines/ · token_deltas", State: decidable, Required: true},
	"reasoning_coverage": {Block: capabilityBlock, Title: "推理样本覆盖", AppliesTo: "GPT / Responses medium；Claude / Messages high",
		Thresholds: map[string]int64{"gpt_min_valid": 7, "gpt_min_recurrence": 4, "claude_min_valid": 3, "claude_min_recurrence": 2},
		Evidence:   "按实发强度统计完整作答的样本及递推覆盖", Source: "docs/decisions.md · 推理判定", State: decidable, Required: true},
	"reasoning_usage": {Block: capabilityBlock, Title: "推理用量", AppliesTo: "GPT / Responses medium；Claude / Messages high",
		Thresholds: map[string]int64{"min_reasoning_tokens": 1}, Evidence: "完整作答却上报零推理为失败；未上报用量单列", Source: "docs/decisions.md · 推理判定", State: decidable, Required: true},
	"reasoning_signature": {Block: capabilityBlock, Title: "Thinking 签名", AppliesTo: "Claude / Messages high",
		Thresholds: map[string]int64{"min_signed_blocks": 1}, Evidence: "完整作答需带非空 thinking 签名", Source: "docs/decisions.md · 推理判定", State: decidable, Required: true},
	"reasoning_answers": {Block: capabilityBlock, Title: "推理答题观察", AppliesTo: "GPT / Responses medium；Claude / Messages high",
		Evidence: "对照当场生成的标准答案，按题型展示有效分母", Source: "internal/probe/data/suite.json · math", State: observation},
	"instruction": {Block: capabilityBlock, Title: "指令层级", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "分别核对指定词和结尾标记", Source: "docs/decisions.md · 指令层级", State: decidable, Required: true},
	"needle_answer": {Block: capabilityBlock, Title: "长上下文 · 编号找回", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "核对本次题面开头、中间及末尾的随机编号", Source: "internal/probe/data/suite.json · needle", State: decidable, Required: true},
	"needle_usage": {Block: capabilityBlock, Title: "长上下文 · 输入用量", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Thresholds: map[string]int64{"min_input_tokens": needleMinTokens}, Evidence: "约 10 万 token 的长上下文请求的上报输入下限", Source: "docs/decisions.md · 大海捞针", State: decidable, Required: true},
	"requests": {Block: capabilityBlock, Title: "请求覆盖", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "区分成功、请求失败、拦截、额度错误和取消", Source: "本次检测调用记录", State: observation},
	"sessions": {Block: capabilityBlock, Title: "会话间观察", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "展示实测会话、成功响应的模型、编号格式和 service_tier 分布", Source: "本次检测调用记录", State: observation},
	"cache": {Block: billingBlock, Title: "缓存上报与接续", AppliesTo: "GPT / Responses、Claude / Messages 缓存链；仅 GPT 使用缓存块对照",
		Thresholds: map[string]int64{"gpt_block_tokens": 128, "max_prefix_pct": 102, "min_prefix_pct": 97, "high_min_samples": 2, "high_odd_pct": 50, "low_min_samples": 4, "low_odd_pct": 20},
		Evidence:   "核验缓存块和链内输入/缓存关系，异常时保留原始报数", Source: "docs/research/model-probe.md · 三家 GPT 渠道实测；docs/decisions.md · 缓存上报", State: observation},
	"injection": {Block: billingBlock, Title: "基准输入差额", AppliesTo: "已有标定的同一完整客户端模板",
		Thresholds: map[string]int64{"alert_extra_tokens": 1000}, Evidence: "短基准请求上报输入与同模板对照的差值", Source: "docs/decisions.md · 渠道质量", State: decidable},
	"structured": {Block: experienceBlock, Title: "结构化输出", AppliesTo: "GPT / Responses",
		Evidence: "核对 JSON 对象、字段类型和随机值", Source: "本次检测 JSON Schema", State: decidable},
	"tool": {Block: experienceBlock, Title: "工具调用", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "核对随机工具名、参数格式和规范化城市名", Source: "本次检测工具参数定义", State: decidable},
	"document": {Block: experienceBlock, Title: "文档读取", AppliesTo: "Claude / Messages",
		Evidence: "核对 PDF 内的随机编号", Source: "本次检测 PDF", State: decidable},
	"echo": {Block: experienceBlock, Title: "原样输出", AppliesTo: "Claude / Messages",
		Evidence: "核对本次请求的尖括号随机标签", Source: "本次检测题面", State: decidable},
	"protocol_format": {Block: experienceBlock, Title: "协议字段", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "核对响应 ID、流结束状态及用量；Messages 另核对 model、事件和用量字段", Source: "本次检测协议响应", State: decidable},
	"invisible": {Block: experienceBlock, Title: "不可见字符", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "统计可见正文中出现的零宽字符", Source: "本次检测调用记录", State: observation},
	"search": {Block: experienceBlock, Title: "搜索痕迹", AppliesTo: "GPT / Responses 与 Claude / Messages",
		Evidence: "检查回答中的搜索来源标记", Source: "本次检测调用记录", State: observation},
	"speed": {Block: experienceBlock, Title: "短题测速", AppliesTo: "两种协议的基准短题、指令题、结构化输出和原样输出",
		Thresholds: map[string]int64{"min_output_tokens": 20, "min_visible_events": 2, "min_stream_ms": 300},
		Evidence:   "以首个可见正文事件计首字，使用可见正文流时间窗估算输出速度", Source: "本次检测可见正文流事件", State: observation},
}

func qualityVerdict(items []Item) string {
	missing := false
	required := false
	for _, item := range items {
		rule := qualityRules[item.RuleID]
		if rule.Block != capabilityBlock || rule.State != decidable {
			continue
		}
		if item.Status == Fail {
			return "不通过"
		}
		if rule.Required && item.Status != NotApplicable {
			required = true
			missing = missing || item.Status != Pass
		}
	}
	if missing || !required {
		return "无法判定"
	}
	return "通过"
}
