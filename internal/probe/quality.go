package probe

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func reportBaselineID(spec Spec, baselines map[string]*Baseline) string {
	base := baselines[spec.Baseline]
	if spec.Mode == ModeFingerprint {
		family := map[string]*Baseline{}
		for model, sample := range baselines {
			if Family(model) == Family(spec.Baseline) {
				family[model] = sample
			}
		}
		raw, _ := json.Marshal(family)
		hash := sha256.Sum256(raw)
		return fmt.Sprintf("%s@%x", spec.Baseline, hash[:6])
	}
	if base == nil {
		return "protocol:" + spec.Baseline
	}
	raw, _ := json.Marshal(base)
	hash := sha256.Sum256(raw)
	return fmt.Sprintf("%s@%x", spec.Baseline, hash[:6])
}

func (p *plan) reportQuality(calls []Call, base *Baseline) *Report {
	by := map[string][]Call{}
	for _, call := range calls {
		by[call.Item] = append(by[call.Item], call)
	}
	r := &Report{Baseline: p.spec.Baseline, SuiteVersion: p.suite.Version, RuleVersion: qualityRuleVersion, Calls: calls,
		Blocks: []ReportBlock{{Key: capabilityBlock, Title: "能力"}, {Key: billingBlock, Title: "账"}, {Key: experienceBlock, Title: "体验与兼容"}}}
	if base == nil && p.protocol {
		r.Mode = "protocol"
	}
	add := func(item Item) {
		rule := qualityRules[item.Key]
		rule.ID, rule.Version = item.Key, qualityRuleVersion
		item.RuleID, item.Title = rule.ID, rule.Title
		for index := range r.Blocks {
			if block := &r.Blocks[index]; block.Key == rule.Block {
				block.Items = append(block.Items, item)
				break
			}
		}
		r.Rules = append(r.Rules, rule)
	}
	anchors := by["anchor"]
	if len(p.tokenNames) > 0 {
		add(p.judgeTokens(by["tokens"], base))
		anchors = by["tokens"][:min(3, len(by["tokens"]))]
	}
	for _, item := range p.qualityReasoning(by["reasoning"], base) {
		add(item)
	}
	add(p.qualityInstruction(by["instruction"]))
	answer, usage := p.qualityNeedle(by["needle"])
	add(answer)
	add(usage)
	add(qualityRequests(calls))
	add(qualitySessions(calls, anchors))
	r.Blocks[0].Verdict = qualityVerdict(r.Blocks[0].Items)
	r.Verdict = r.Blocks[0].Verdict
	add(qualityCache(calls, p.protocol))
	add(qualityInjection(anchors, p.spec))
	add(p.qualityTool(by["tool"]))
	if p.protocol {
		add(checkQualityAnswer("document", by["document"], "读出了 PDF 中的编号", "未找到 PDF 中的编号 "+p.docCode, func(c Call) bool {
			return strings.Contains(strings.ToUpper(c.Text), p.docCode)
		}))
		add(checkQualityAnswer("echo", by["echo"], "尖括号标签原样输出", "未原样输出 "+p.echoTag, func(c Call) bool {
			return strings.TrimSpace(c.Text) == p.echoTag
		}))
	} else {
		add(p.qualityStructured(by["structured"]))
	}
	add(p.qualityProtocol(calls))
	for _, item := range observe(qualityCompleted(calls), nil, nil, nil, nil) {
		if item.Key == "invisible" || item.Key == "search" {
			add(item)
		}
	}
	var speed Item
	r.Metrics, speed = qualityMetrics(calls, p.protocol)
	add(speed)
	return r
}

func qualityCallIssue(c Call) string {
	if c.Error != "" {
		return c.Error
	}
	if c.HTTPStatus >= http.StatusBadRequest {
		return fmt.Sprintf("HTTP %d", c.HTTPStatus)
	}
	switch c.FinishReason {
	case "incomplete", "length", "max_tokens", "model_context_window_exceeded", "content_filter", "refusal", "failed", "error", "in_progress", "queued", "cancelled":
		return c.FinishReason
	}
	switch c.Item {
	case "reasoning", "instruction", "needle", "document", "echo", "structured", "tool":
		if clean(c.Text) == "" && len(c.ToolCalls) == 0 {
			return "上游未返回作答内容"
		}
	case "anchor", "tokens", "chain":
		if !c.InputReported && clean(c.Text) == "" {
			return "上游未返回作答内容或输入用量"
		}
	}
	return ""
}

func qualityCompleted(calls []Call) []Call {
	var completed []Call
	for _, call := range calls {
		if qualityCallIssue(call) == "" {
			completed = append(completed, call)
		}
	}
	return completed
}

func qualityRequests(calls []Call) Item {
	complete, failed, blocked, quota, cancelled := 0, 0, 0, 0, 0
	for _, call := range calls {
		msg := strings.ToLower(qualityCallIssue(call))
		switch {
		case msg == "":
			complete++
		case quotaError(call, msg):
			quota++
		case policyBlocked(call):
			blocked++
		case msg == "检测已取消" || msg == "超时或被取消" || msg == "cancelled" || strings.Contains(msg, "context canceled"):
			cancelled++
		default:
			failed++
		}
	}
	return Item{Key: "requests", Status: Info, Summary: fmt.Sprintf("测成 %d，失败 %d，拦截 %d，额度错误 %d，取消 %d", complete, failed, blocked, quota, cancelled),
		Details: map[string]any{"completed": complete, "failed": failed, "blocked": blocked, "quota": quota, "cancelled": cancelled}}
}

func qualitySessions(calls, anchors []Call) Item {
	type session struct {
		Session      int            `json:"session"`
		Completed    int            `json:"completed"`
		Models       map[string]int `json:"models"`
		IDPrefixes   map[string]int `json:"id_prefixes"`
		ServiceTiers map[string]int `json:"service_tiers"`
		AnchorInputs []int64        `json:"anchor_input_tokens,omitempty"`
	}
	bySession := map[int]*session{}
	var rows []*session
	for _, call := range calls {
		if call.Error == "检测已取消" {
			continue
		}
		row := bySession[call.Session]
		if row == nil {
			row = &session{Session: call.Session, Models: map[string]int{}, IDPrefixes: map[string]int{}, ServiceTiers: map[string]int{}}
			bySession[call.Session] = row
			rows = append(rows, row)
		}
		if qualityCallIssue(call) == "" {
			row.Completed++
			row.Models[orDash(call.Model)]++
			row.IDPrefixes[idPrefix(call.ID)]++
			row.ServiceTiers[orDash(call.ServiceTier)]++
		}
	}
	for _, call := range anchors {
		if row := bySession[call.Session]; row != nil && qualityCallIssue(call) == "" && call.InputReported {
			row.AnchorInputs = append(row.AnchorInputs, call.InputTokens)
		}
	}
	completed := 0
	for _, row := range rows {
		if row.Completed > 0 {
			completed++
		}
	}
	return Item{Key: "sessions", Status: Info, Summary: fmt.Sprintf("实发覆盖 %d 个会话，%d 个取得完整响应；明细展示各会话的响应字段和基准输入", len(rows), completed), Details: map[string]any{"sessions": rows}}
}

func unavailableQualityCall(calls []Call) string {
	if len(calls) == 0 {
		return "没有样本"
	}
	c := calls[0]
	switch {
	case policyBlocked(c):
		return "请求被拦截（HTTP 403）"
	case quotaError(c, strings.ToLower(qualityCallIssue(c))):
		return "请求遇到额度错误"
	case gatewayError(c):
		return fmt.Sprintf("重试后网关仍超时（HTTP %d）", c.HTTPStatus)
	default:
		return qualityCallIssue(c)
	}
}

func checkQualityAnswer(key string, calls []Call, passed, failed string, valid func(Call) bool) Item {
	it := Item{Key: key}
	if reason := unavailableQualityCall(calls); reason != "" {
		it.Status, it.Summary = Unknown, "没测成："+reason
	} else if c := calls[0]; valid(c) {
		it.Status, it.Summary = Pass, passed
	} else {
		it.Status, it.Summary = Fail, failed
		it.Details = map[string]any{"answer": truncate(clean(c.Text), 200), "tool_calls": c.ToolCalls}
	}
	return it
}

func (p *plan) qualityInstruction(calls []Call) Item {
	word, marker := calls[:min(1, len(calls))], calls[min(1, len(calls)):]
	wordCheck := checkQualityAnswer("instruction", word, "按系统提示只回复指定词", "未按系统提示只回复 "+p.onlyWord, func(c Call) bool {
		return strings.EqualFold(strings.Trim(clean(c.Text), ".!。 "), p.onlyWord)
	})
	markerCheck := checkQualityAnswer("instruction", marker, "按系统提示加上结尾标记", "未按系统提示加上结尾标记", func(c Call) bool {
		return strings.HasSuffix(clean(c.Text), p.marker)
	})
	status := Pass
	if wordCheck.Status == Fail || markerCheck.Status == Fail {
		status = Fail
	} else if wordCheck.Status == Unknown || markerCheck.Status == Unknown {
		status = Unknown
	}
	return Item{Key: "instruction", Status: status, Summary: "指定词：" + wordCheck.Summary + "；结尾标记：" + markerCheck.Summary}
}

func (p *plan) qualityNeedle(calls []Call) (Item, Item) {
	answer := Item{Key: "needle_answer", Status: Unknown}
	usage := Item{Key: "needle_usage", Status: Unknown}
	if issue := unavailableQualityCall(calls); issue != "" {
		answer.Summary, usage.Summary = "没测成："+issue, "没测成："+issue
		return answer, usage
	}
	c := calls[0]
	var missing []string
	for index, code := range p.needleCodes {
		if !strings.Contains(strings.ToUpper(c.Text), code) {
			missing = append(missing, fmt.Sprintf("第 %d 个（约 %d%% 处）", index+1, int(needleAt[index]*100)))
		}
	}
	answer.Details = map[string]any{"codes": p.needleCodes, "answer": truncate(clean(c.Text), 200)}
	if len(missing) > 0 {
		answer.Status, answer.Summary = Fail, "漏答编号："+strings.Join(missing, "、")
	} else {
		answer.Status, answer.Summary = Pass, "开头、中间、末尾的三个编号均已找回"
	}
	threshold := qualityRules["needle_usage"].Thresholds["min_input_tokens"]
	if !c.InputReported {
		usage.Summary = "上游未上报输入用量"
	} else {
		usage.Details = map[string]any{"input_tokens": c.InputTokens}
		if c.InputTokens < threshold {
			usage.Status, usage.Summary = Fail, fmt.Sprintf("输入报数 %d token，低于 %d token 下限", c.InputTokens, threshold)
		} else {
			usage.Status, usage.Summary = Pass, fmt.Sprintf("输入报数 %d token，达到 %d token 下限", c.InputTokens, threshold)
		}
	}
	return answer, usage
}

func (p *plan) qualityStructured(calls []Call) Item {
	return checkQualityAnswer("structured", calls, "JSON 格式、字段类型和校验值符合要求", "返回内容未匹配 JSON 结构或校验值", func(c Call) bool {
		var fields map[string]json.RawMessage
		var nonce string
		var value int
		return json.Unmarshal([]byte(clean(c.Text)), &fields) == nil && len(fields) == 2 &&
			json.Unmarshal(fields["nonce"], &nonce) == nil && nonce == p.nonce &&
			json.Unmarshal(fields["value"], &value) == nil && value == p.nonceValue
	})
}

var weatherCountries = map[string]string{
	"Reykjavik": "Iceland", "Montevideo": "Uruguay", "Ljubljana": "Slovenia", "Windhoek": "Namibia",
	"Valparaiso": "Chile", "Hobart": "Australia", "Tromso": "Norway", "Cusco": "Peru",
}

func validWeatherTool(call ToolCall, name, city string) bool {
	var args map[string]json.RawMessage
	var reportedCity string
	if call.Name != name || json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return false
	}
	if json.Unmarshal(args["city"], &reportedCity) != nil || reportedCity == "" {
		return false
	}
	normalize := func(text string) string { return Fold(strings.ToLower(strings.Join(strings.Fields(text), " "))) }
	gotCity, country, hasCountry := strings.Cut(strings.ReplaceAll(reportedCity, "，", ","), ",")
	expectedCountry := weatherCountries[city]
	return normalize(gotCity) == normalize(city) && (!hasCountry || expectedCountry != "" && normalize(country) == normalize(expectedCountry))
}

func (p *plan) qualityTool(calls []Call) Item {
	return checkQualityAnswer("tool", calls, "工具名和城市参数符合要求", "工具名或城市参数未匹配预期城市 "+p.city, func(c Call) bool {
		return len(c.ToolCalls) == 1 && validWeatherTool(c.ToolCalls[0], p.toolName, p.city)
	})
}

func (p *plan) qualityProtocol(calls []Call) Item {
	completed := qualityCompleted(calls)
	if len(completed) == 0 {
		return Item{Key: "protocol_format", Status: Unknown, Summary: "没有可核对的完整响应"}
	}
	if p.protocol {
		return p.judgeMessagesProtocol(completed)
	}
	missingID, missingUsage, incomplete := 0, 0, 0
	for _, call := range completed {
		if call.ID == "" {
			missingID++
		}
		if !call.UsageReported {
			missingUsage++
		}
		if !call.Done || call.FinishReason == "" {
			incomplete++
		}
	}
	if missingID+missingUsage+incomplete > 0 {
		return Item{Key: "protocol_format", Status: Warn, Summary: fmt.Sprintf("%d 个响应缺编号，%d 个缺用量，%d 个缺正常结束标记（共 %d 个）", missingID, missingUsage, incomplete, len(completed))}
	}
	return Item{Key: "protocol_format", Status: Pass, Summary: fmt.Sprintf("%d 个响应带编号、用量及正常结束标记", len(completed))}
}

func qualityMetrics(calls []Call, messages bool) (map[string]any, Item) {
	var input, output, cached, streamOut, streamMS int64
	var first []int
	short, speedSamples, failures, tokensSeen := 0, 0, 0, 0
	thresholds := qualityRules["speed"].Thresholds
	for _, call := range calls {
		if call.Item == "tokens" {
			tokensSeen++
		}
		if qualityCallIssue(call) != "" {
			failures++
			continue
		}
		input, output, cached = input+call.InputTokens, output+call.OutputTokens, cached+call.CachedTokens
		switch call.Item {
		case "tokens":
			if tokensSeen > 3 {
				continue
			}
		case "anchor", "instruction", "structured", "echo":
		default:
			continue
		}
		short++
		if call.VisibleFirstMS <= 0 {
			continue
		}
		first = append(first, call.VisibleFirstMS)
		visible := call.OutputTokens - call.ReasoningTokens
		elapsed := int64(call.VisibleLastMS - call.VisibleFirstMS)
		if call.UsageReported && (call.ReasoningReported || messages && call.SignedThinking == 0) && visible >= thresholds["min_output_tokens"] &&
			int64(call.VisibleEvents) >= thresholds["min_visible_events"] && elapsed >= thresholds["min_stream_ms"] {
			streamOut += visible
			streamMS += elapsed
			speedSamples++
		}
	}
	m := map[string]any{"requests": len(calls), "errors": failures, "input_tokens": input, "output_tokens": output, "cached_tokens": cached,
		"ttft_samples": len(first), "output_tps_samples": speedSamples, "eligible_short_samples": short}
	var ttftReason, tpsReason string
	if len(first) > 0 {
		m["first_token_ms"] = median(first)
	} else if short == 0 {
		ttftReason = "无已完成的白名单短题"
	} else {
		ttftReason = "白名单短题没有可见正文流事件"
	}
	if streamMS > 0 {
		m["tokens_per_sec"] = float64(int(float64(streamOut)/float64(streamMS)*10000)) / 10
	} else if ttftReason != "" {
		tpsReason = ttftReason
	} else {
		tpsReason = fmt.Sprintf("没有上报可核验用量且达到 %d 正文 token、%d 个可见事件和 %d ms 正文时间窗的短题", thresholds["min_output_tokens"], thresholds["min_visible_events"], thresholds["min_stream_ms"])
	}
	if ttftReason != "" {
		m["ttft_missing_reason"] = ttftReason
	}
	if tpsReason != "" {
		m["output_tps_missing_reason"] = tpsReason
	}
	return m, Item{Key: "speed", Status: Info, Summary: fmt.Sprintf("白名单短题 %d 个；首字 %d 个样本，输出速度 %d 个样本", short, len(first), speedSamples)}
}
