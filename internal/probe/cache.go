package probe

import (
	"fmt"
	"math"
	"strings"
)

// 多轮缓存（docs/decisions.md「模型检测」）：同一会话连着聊几轮，看每轮的缓存有没有接上上一轮的全部内容。
// 和生产里的多轮对话同一个口径；中转少报缓存（比如为了利润把命中压到九成）在这里直接看得出来，缓存读比全价输入便宜一个数量级，少报就是多收钱。

const (
	chainTurns       = 4     // 第一轮写缓存，后三轮测接续
	chainFirstTokens = 20000 // 第一轮的填充文本，约 2 万 token
	chainNextTokens  = 600   // 之后每轮新加的内容
)

// cacheChain 在会话 0 上出一组多轮对话：每轮带上前面所有轮次（模型的回答用标准答案代替，缓存只看前缀，不需要真实回复）。
func (p *plan) cacheChain(tokensPerSentence int) {
	var history []Turn
	for k := range chainTurns {
		n := chainNextTokens
		if k == 0 {
			n = chainFirstTokens
		}
		a, b := 10+p.rng.IntN(90), 10+p.rng.IntN(90)
		prompt := strings.Join(p.sentences(n/tokensPerSentence), " ") + fmt.Sprintf("\n\nIgnore the text above. What is %d+%d? Answer with the number only.", a, b)
		p.add(Request{Item: "chain", Prompt: prompt, History: append([]Turn(nil), history...), MaxTokens: 16})
		history = append(history, Turn{User: prompt, Assistant: fmt.Sprint(a + b)})
	}
}

// withChain 把多轮缓存的结论放在“缓存计数”后面（没有就放最后）。
func withChain(obs []Item, cs []Call) []Item {
	it, ok := judgeChain(cs)
	if !ok {
		return obs
	}
	at := len(obs)
	for i, o := range obs {
		if o.Key == "cache" {
			at = i + 1
		}
	}
	return append(obs[:at], append([]Item{it}, obs[at:]...)...)
}

// judgeChain：每轮命中的缓存 ÷ 上一轮的全部输入。上游原样报时接近 100%（OpenAI 按 128 token 一块命中，只差最后不满一块的部分）。
func judgeChain(cs []Call) (Item, bool) {
	if len(cs) == 0 {
		return Item{}, false
	}
	it := Item{Key: "cache_chain", Title: "多轮缓存"}
	var prev, hit, input int64
	turns := []map[string]any{}
	for k := 1; k < len(cs); k++ {
		a, b := cs[k-1], cs[k]
		if a.Error != "" || b.Error != "" || !a.UsageReported || !b.UsageReported {
			continue
		}
		prev, hit, input = prev+a.InputTokens, hit+b.CachedTokens, input+b.InputTokens
		turns = append(turns, map[string]any{"turn": k + 1, "previous_input": a.InputTokens, "input": b.InputTokens, "cached": b.CachedTokens})
	}
	if prev == 0 {
		it.Status, it.Summary = Info, "多轮对话没测成（请求失败或上游没报用量）"
		return it, true
	}
	rate, share := float64(hit)/float64(prev), float64(hit)/float64(input)
	it.Details = map[string]any{"turns": turns, "hit_rate": rate, "cached_share": share}
	pct := func(x float64) int { return int(math.Round(x * 100)) }
	// 缓存占比向下取整，和调用日志一致：没全中时不显示成 100%
	tail := fmt.Sprintf("（每轮输入里缓存占 %d%%，和调用日志的缓存率同一口径）", int(math.Floor(share*100)))
	switch {
	case rate > 1.02:
		it.Status, it.Summary = Warn, fmt.Sprintf("缓存数比上一轮的全部内容还多（%d%%），是中转自己算的，不是上游原样", pct(rate))+tail
	case rate >= 0.97:
		it.Status, it.Summary = Pass, fmt.Sprintf("连着聊 %d 轮，每轮都接上了上一轮的缓存：上一轮内容命中 %d%%", len(turns)+1, pct(rate))+tail
	case rate >= 0.5:
		it.Status, it.Summary = Warn, fmt.Sprintf("上一轮内容只命中 %d%%：多轮对话里本该命中缓存的输入，约 %d%% 按全价计费", pct(rate), 100-pct(rate))+tail
	default:
		it.Status, it.Summary = Warn, fmt.Sprintf("多轮对话基本没接上缓存：上一轮内容只命中 %d%%，大部分输入按全价计费", pct(rate))+tail
	}
	return it, true
}
