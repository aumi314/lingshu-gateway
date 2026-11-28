package probe

import (
	"fmt"
	"sort"
	"strings"
)

// qualityBillingThreshold 从规则清单取账块阈值。
func qualityBillingThreshold(id, key string) int64 {
	return qualityRules[id].Thresholds[key]
}

func qualityMedianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func qualityMedianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// qualityInjection 看同模板的短基准请求：上报输入比官方同模板基准多多少。
// 调用方已经把 by["tokens"] 截到前三条，或者只传 by["anchor"]；这里不再重复截断。
// 只报差额和证据，超过阈值作运营告警，不折算金额。
func qualityInjection(calls []Call, spec Spec) Item {
	it := Item{Key: "injection", Status: NotApplicable, Details: map[string]any{
		"template": spec.Template, "template_hash": spec.TemplateHash,
	}}
	if len(calls) == 0 {
		it.Summary = "不适用：本次没有注入基准请求"
		return it
	}

	valid := []int64{}
	excluded := []map[string]any{}
	for _, call := range calls {
		if call.Item != "tokens" && call.Item != "anchor" {
			continue
		}
		issue := qualityCallIssue(call)
		if issue == "" && call.InputReported && call.InputTokens > 0 {
			valid = append(valid, call.InputTokens)
			continue
		}
		excluded = append(excluded, map[string]any{
			"item": call.Item, "session": call.Session, "input": call.InputTokens,
			"issue": issue, "input_reported": call.InputReported,
		})
	}
	it.Details["anchor_calls"] = len(calls)
	it.Details["valid_samples"] = len(valid)
	if len(excluded) > 0 {
		it.Details["excluded"] = excluded
	}
	if len(valid) == 0 {
		it.Status = Unknown
		it.Summary = "不可核验：没有成功且上报输入的同模板基准请求，无法计算注入差额"
		return it
	}

	median := qualityMedianInt64(valid)
	it.Details["anchor_tokens"] = valid
	it.Details["median_input_tokens"] = median
	base, calibrated := templateBases[spec.TemplateHash]
	if !calibrated {
		it.Status = Unknown
		it.Summary = fmt.Sprintf("不可核验：模板 %s 的官方基准未标定；同模板基准输入中位数 %d token（%d 个有效样本），无法计算注入差额", spec.TemplateHash, median, len(valid))
		return it
	}
	it.Details["baseline_tokens"] = base
	it.Details["baseline_source"] = "docs/decisions.md · 渠道质量（完整伪装模板标定值）"
	extra := median - int64(base)
	it.Details["extra_tokens"] = extra
	threshold := qualityBillingThreshold("injection", "alert_extra_tokens")
	it.Details["alert_extra_tokens"] = threshold
	prefix := fmt.Sprintf("同模板基准输入中位数 %d token（%d 个有效样本，官方同模板基准 %d，依据 docs/decisions.md「渠道质量」的模板标定值）", median, len(valid), base)
	if extra > threshold {
		it.Status = Warn
		it.Summary = prefix + fmt.Sprintf("，比官方多 %d token，超过 %d token 运营告警线", extra, threshold)
		return it
	}
	it.Status = Pass
	if extra >= 0 {
		it.Summary = prefix + fmt.Sprintf("，比官方多 %d token，未超过 %d token 运营告警线", extra, threshold)
	} else {
		it.Summary = prefix + fmt.Sprintf("，比官方少 %d token，未超过 %d token 运营告警线", -extra, threshold)
	}
	return it
}

// qualityCache 看缓存报数能不能直接用：GPT 看 128 token 块对齐，所有协议都看同一会话四轮链的接续。
// 链内一旦有疑点，只留原始输入 / 缓存数，不再把派生命中率当可靠结果。
func qualityCache(calls []Call, isMessages bool) Item {
	it := Item{Key: "cache", Status: NotApplicable, Details: map[string]any{"is_messages": isMessages}}
	if len(calls) == 0 {
		it.Summary = "不适用：本次没有缓存相关请求"
		return it
	}
	block := qualityBillingThreshold("cache", "gpt_block_tokens")
	maxPct := qualityBillingThreshold("cache", "max_prefix_pct")
	minPct := qualityBillingThreshold("cache", "min_prefix_pct")
	highMin := qualityBillingThreshold("cache", "high_min_samples")
	highOddPct := qualityBillingThreshold("cache", "high_odd_pct")
	lowMin := qualityBillingThreshold("cache", "low_min_samples")
	lowOddPct := qualityBillingThreshold("cache", "low_odd_pct")
	it.Details["max_prefix_pct"] = maxPct
	it.Details["min_prefix_pct"] = minPct
	if !isMessages {
		it.Details["gpt_block_tokens"] = block
	}

	chain := []Call{}
	chainRows := []map[string]any{}
	for _, call := range calls {
		if call.Item != "chain" {
			continue
		}
		chain = append(chain, call)
		row := map[string]any{
			"turn": len(chain), "input": call.InputTokens, "cached": call.CachedTokens,
			"input_reported": call.InputReported, "cache_reported": call.CacheReported, "issue": qualityCallIssue(call),
		}
		if call.HTTPStatus != 0 {
			row["http_status"] = call.HTTPStatus
		}
		if call.FinishReason != "" {
			row["finish_reason"] = call.FinishReason
		}
		chainRows = append(chainRows, row)
	}
	it.Details["chain_rows"] = chainRows

	cached, odd := 0, 0
	for _, call := range calls {
		if qualityCallIssue(call) != "" || !call.CacheReported || call.CachedTokens <= 0 {
			continue
		}
		cached++
		if !isMessages && call.CachedTokens%block != 0 {
			odd++
		}
	}
	it.Details["cached_calls"] = cached
	if !isMessages {
		it.Details["odd_block_calls"] = odd
	}
	cached64, odd64 := int64(cached), int64(odd)
	plannedPairs := len(chain) - 1
	if plannedPairs < 0 {
		plannedPairs = 0
	}
	if len(chain) == 0 && cached == 0 {
		it.Summary = "不适用：本次没有缓存链或缓存命中样本"
		return it
	}

	signals := []string{}
	lowContinuation := 0
	prefixRates, inputShares := []float64{}, []float64{}
	pairRows := []map[string]any{}
	validPairs := 0
	for index := 1; index < len(chain); index++ {
		previous, current := chain[index-1], chain[index]
		if qualityCallIssue(previous) != "" || qualityCallIssue(current) != "" || !previous.InputReported || !current.InputReported || !current.CacheReported {
			continue
		}
		if previous.InputTokens <= 0 || current.InputTokens <= 0 || current.CachedTokens < 0 || current.CachedTokens > current.InputTokens {
			signals = append(signals, fmt.Sprintf("第 %d 轮缓存报数不符合输入关系", index+1))
			continue
		}
		validPairs++
		prefix := float64(current.CachedTokens) / float64(previous.InputTokens) * 100
		share := float64(current.CachedTokens) / float64(current.InputTokens) * 100
		pairRows = append(pairRows, map[string]any{
			"turn": index + 1, "prefix_pct": prefix, "input_share_pct": share,
			"previous_input": previous.InputTokens, "input": current.InputTokens, "cached": current.CachedTokens,
		})
		prefixRates = append(prefixRates, prefix)
		inputShares = append(inputShares, share)
		if prefix > float64(maxPct) {
			signals = append(signals, fmt.Sprintf("第 %d 轮缓存读 %d 超过上一轮输入的 %d%%", index+1, current.CachedTokens, maxPct))
		}
		if prefix < float64(minPct) {
			lowContinuation++
		}
		if current.InputTokens <= previous.InputTokens {
			signals = append(signals, fmt.Sprintf("第 %d 轮输入 token 没有随对话增长", index+1))
		}
	}
	it.Details["valid_pairs"] = validPairs
	if !isMessages {
		chainOdd := 0
		for _, row := range pairRows {
			cachedTokens, _ := row["cached"].(int64)
			if cachedTokens > 0 && cachedTokens%block != 0 {
				chainOdd++
			}
		}
		if chainOdd > 0 && len(chain) >= 2 {
			signals = append(signals, fmt.Sprintf("缓存链里有 %d 轮命中数不是 %d 的整数倍", chainOdd, block))
		}
		if cached64 >= highMin && odd64*100 >= cached64*highOddPct {
			signals = append(signals, fmt.Sprintf("%d / %d 个命中缓存的请求不是 %d 的整数倍（至少 %d%%）", odd, cached, block, highOddPct))
		} else if cached64 >= lowMin && odd64*100 >= cached64*lowOddPct {
			signals = append(signals, fmt.Sprintf("%d / %d 个命中缓存的请求不是 %d 的整数倍（至少 %d%%）", odd, cached, block, lowOddPct))
		}
	}

	if len(signals) > 0 {
		it.Status = Warn
		it.Details["suspect_signals"] = signals
		it.Details["rates_suppressed"] = true
		it.Summary = "有疑点：" + strings.Join(signals, "；") + "；明细仅保留原始输入和缓存数"
		return it
	}
	if validPairs < plannedPairs || (validPairs == 0 && (isMessages || cached == 0 || odd > 0)) {
		it.Status = Unknown
		it.Summary = fmt.Sprintf("不可核验：计划 %d 组链内对照，只有 %d 组能核对；明细保留原始输入和缓存数", plannedPairs, validPairs)
		return it
	}

	it.Status = Pass
	it.Details["prefix_rows"] = pairRows
	if validPairs > 0 {
		it.Details["median_prefix_pct"] = qualityMedianFloat(prefixRates)
		it.Details["median_input_share_pct"] = qualityMedianFloat(inputShares)
	}
	parts := []string{"上游报数未见异常"}
	if validPairs > 0 {
		parts = append(parts, fmt.Sprintf("四轮缓存链前缀接续率中位数 %.1f%%，本轮缓存占比中位数 %.1f%%", qualityMedianFloat(prefixRates), qualityMedianFloat(inputShares)))
		if lowContinuation > 0 {
			parts = append(parts, fmt.Sprintf("%d 组前缀接续率低于 %d%%，本轮缓存占比保留原始报数", lowContinuation, minPct))
		}
	}
	if !isMessages && cached > 0 {
		parts = append(parts, fmt.Sprintf("命中缓存的请求中 %d/%d 个报数为 %d 的整数倍", cached-odd, cached, block))
	}
	it.Summary = strings.Join(parts, "；")
	return it
}
