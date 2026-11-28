package probe

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func qualityAnchorCall(tokens int64) Call {
	return Call{Item: "tokens", UsageReported: true, InputReported: true, Done: true, InputTokens: tokens}
}

func TestQualityInjectionThresholdBoundary(t *testing.T) {
	spec := Spec{Disguise: true, TemplateHash: "4468e298e601", Template: "codex 0.160.0"}
	base := int64(templateBases[spec.TemplateHash])
	for _, extra := range []int64{999, 1000, 1001} {
		item := qualityInjection([]Call{qualityAnchorCall(base + extra)}, spec)
		want := Warn
		if extra <= 1000 {
			want = Pass
		}
		if item.Status != want || !strings.Contains(item.Summary, fmt.Sprintf("%d token", extra)) {
			t.Errorf("extra=%d: status=%s summary=%s, want %s", extra, item.Status, item.Summary, want)
		}
		if item.Details["baseline_source"] != "docs/decisions.md · 渠道质量（完整伪装模板标定值）" || item.Details["valid_samples"] != 1 {
			t.Errorf("extra=%d: 明细缺基准来源或样本数: %+v", extra, item.Details)
		}
	}
}

func TestQualityInjectionUsesMedianAndEvidence(t *testing.T) {
	spec := Spec{Disguise: true, TemplateHash: "62ac38171048", Template: "claude-code 2.1.281"}
	base := int64(templateBases[spec.TemplateHash])
	calls := []Call{
		qualityAnchorCall(base + 500),
		qualityAnchorCall(base + 900),
		qualityAnchorCall(base + 700),
		qualityAnchorCall(base + 4000),
		{Item: "tokens", UsageReported: true, InputReported: true, Done: true, InputTokens: base + 8000, HTTPStatus: http.StatusPaymentRequired, Error: "HTTP 402"},
	}
	item := qualityInjection(calls, spec)
	if item.Status != Pass || item.Details["median_input_tokens"] != int64(base+900) {
		t.Fatalf("注入中位数或状态不对: %s %+v", item.Summary, item.Details)
	}
	if item.Details["anchor_calls"] != 5 || item.Details["valid_samples"] != 4 {
		t.Errorf("样本/证据计数不对: %+v", item.Details)
	}
	if !strings.Contains(item.Summary, "4 个有效样本") || !strings.Contains(item.Summary, "多 900 token") {
		t.Errorf("摘要没报样本数或差额: %s", item.Summary)
	}
}

func TestQualityInjectionUnknownAndNotApplicable(t *testing.T) {
	unknown := qualityInjection([]Call{{Item: "anchor", Error: "检测已取消"}}, Spec{Disguise: true, TemplateHash: "4468e298e601"})
	if unknown.Status != Unknown || !strings.Contains(unknown.Summary, "不可核验") {
		t.Errorf("无有效样本应为不可核验: %s %s", unknown.Status, unknown.Summary)
	}
	uncalibrated := qualityInjection([]Call{qualityAnchorCall(1000)}, Spec{Disguise: true, TemplateHash: "uncalibrated"})
	if uncalibrated.Status != Unknown || !strings.Contains(uncalibrated.Summary, "未标定") {
		t.Errorf("模板未标定应为不可核验: %s %s", uncalibrated.Status, uncalibrated.Summary)
	}
	notReported := qualityInjection([]Call{{Item: "tokens", UsageReported: true, InputTokens: 1000, Done: true}}, Spec{Disguise: true, TemplateHash: "4468e298e601"})
	if notReported.Status != Unknown {
		t.Errorf("输入字段缺失应为不可核验: %s %s", notReported.Status, notReported.Summary)
	}
	missing := qualityInjection(nil, Spec{})
	if missing.Status != NotApplicable || missing.Key != "injection" {
		t.Errorf("没有基准应为不适用: %s %s", missing.Status, missing.Summary)
	}
}

func qualityChainCall(input, cached int64) Call {
	return Call{Item: "chain", UsageReported: true, InputReported: true, CacheReported: true, Done: true, InputTokens: input, CachedTokens: cached}
}

func TestQualityCachePassesGPTChainAndBlock(t *testing.T) {
	calls := []Call{
		qualityChainCall(20000, 0),
		qualityChainCall(20600, 19840),
		qualityChainCall(21200, 20480),
		qualityChainCall(21800, 21120),
	}
	item := qualityCache(calls, false)
	if item.Status != Pass || item.Details["rates_suppressed"] == true {
		t.Fatalf("正常 GPT 链应通过: %s %+v", item.Summary, item.Details)
	}
	if len(item.Details["chain_rows"].([]map[string]any)) != 4 || item.Details["valid_pairs"] != 3 {
		t.Errorf("链明细缺行: %+v", item.Details)
	}
	if !strings.Contains(item.Summary, "前缀接续率") || !strings.Contains(item.Summary, "本轮缓存占比") {
		t.Errorf("摘要没分开写两种比例: %s", item.Summary)
	}
	if item.Details["gpt_block_tokens"] != int64(128) {
		t.Errorf("GPT 块大小不对: %+v", item.Details)
	}
}

func TestQualityCacheSuppressesSuspectRates(t *testing.T) {
	calls := []Call{
		qualityChainCall(20000, 0),
		qualityChainCall(20600, 20000),
		qualityChainCall(21000, 19840), // 97% 接续线以下，且命中数不是 128 的整数倍
		qualityChainCall(21600, 24000), // 超过 102% 接续线
	}
	item := qualityCache(calls, false)
	if item.Status != Warn || item.Details["rates_suppressed"] != true {
		t.Fatalf("有疑点必须压掉派生率: %s %+v", item.Summary, item.Details)
	}
	if _, ok := item.Details["median_prefix_pct"]; ok {
		t.Errorf("有疑点时不该给可靠命中率: %+v", item.Details)
	}
	if len(item.Details["chain_rows"].([]map[string]any)) != 4 {
		t.Errorf("疑点链也必须保留原始行: %+v", item.Details)
	}
	if !strings.Contains(item.Summary, "明细仅保留原始输入和缓存数") {
		t.Errorf("摘要没说明派生率不可用: %s", item.Summary)
	}
}

func TestQualityCacheClaudeIgnoresGPTBlockAlignment(t *testing.T) {
	calls := []Call{
		qualityChainCall(20000, 0),
		qualityChainCall(20600, 19773), // 不是 128 整数倍，Claude 不应因此告警
		qualityChainCall(21200, 20346),
		qualityChainCall(21800, 20911),
	}
	item := qualityCache(calls, true)
	if item.Status != Pass {
		t.Fatalf("Claude 不应套 GPT 对齐规则: %s %+v", item.Summary, item.Details)
	}
	if _, ok := item.Details["gpt_block_tokens"]; ok {
		t.Errorf("Claude 报告不该有 GPT 块大小阈值: %+v", item.Details)
	}
}

func TestQualityCacheFailedCallIsNotEvidence(t *testing.T) {
	calls := []Call{
		qualityChainCall(20000, 0),
		{Item: "chain", UsageReported: true, InputReported: true, CacheReported: true, Done: true, InputTokens: 20600, CachedTokens: 19840, FinishReason: "incomplete"},
		qualityChainCall(21200, 20480),
		qualityChainCall(21800, 21120),
	}
	item := qualityCache(calls, false)
	if item.Status != Unknown || item.Details["valid_pairs"] != 1 {
		t.Errorf("链内对照缺测应记不可核验: %s %+v", item.Summary, item.Details)
	}
	omitted := []Call{
		qualityChainCall(20000, 0),
		{Item: "chain", UsageReported: true, InputReported: true, Done: true, InputTokens: 20600, CachedTokens: 19840},
		qualityChainCall(21200, 20480),
		qualityChainCall(21800, 21120),
	}
	if got := qualityCache(omitted, false); got.Status != Unknown || got.Details["valid_pairs"] != 2 {
		t.Errorf("缓存字段缺失应记不可核验: %s %+v", got.Summary, got.Details)
	}
	invalid := []Call{
		qualityChainCall(20000, 0),
		qualityChainCall(20600, 21000),
		qualityChainCall(21200, 20480),
		qualityChainCall(21800, 21120),
	}
	if got := qualityCache(invalid, false); got.Status != Warn || got.Details["rates_suppressed"] != true {
		t.Errorf("缓存数大于当前输入应为有疑点: %s %+v", got.Summary, got.Details)
	}
}

func TestQualityCacheLowContinuationIsNotSuspect(t *testing.T) {
	calls := []Call{
		qualityChainCall(20000, 0),
		qualityChainCall(20600, 19000),
		qualityChainCall(21200, 19600),
		qualityChainCall(21800, 20200),
	}
	item := qualityCache(calls, true)
	if item.Status != Pass || item.Details["rates_suppressed"] == true {
		t.Fatalf("接续率低本身不是报数疑点: %s %+v", item.Summary, item.Details)
	}
	if !strings.Contains(item.Summary, "前缀接续率") || !strings.Contains(item.Summary, "本轮缓存占比") || !strings.Contains(item.Summary, "低于 97%") {
		t.Errorf("摘要应保留两个比例并说明接续率低: %s", item.Summary)
	}
	if missing := qualityCache(nil, false); missing.Status != NotApplicable || missing.Key != "cache" {
		t.Errorf("没有缓存请求应为不适用: %s %s", missing.Status, missing.Summary)
	}
}
