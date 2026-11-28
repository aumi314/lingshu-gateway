// 模型检测页回归。使用安装了 playwright 的 Node 环境运行：node web/tests/probes.browser.cjs。
const { chromium } = require('playwright')
const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
// 端口用 2 万段：本机 Windows 动态端口范围是 1024–15000，镜像网络下 51xx 会被随机占用
const path = require('node:path')

;(async () => {
  const server = spawn(process.execPath, [path.join(__dirname, '../node_modules/vite/bin/vite.js'), '--host', '127.0.0.1', '--port', '25191', '--strictPort'], { cwd: path.join(__dirname, '..'), stdio: 'ignore' })
  const browser = await chromium.launch({ headless: true })
  try {
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch('http://127.0.0.1:25191')).ok) break } catch {}
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    const channel = { id: 24, name: 'relay-fixture', template: 'standard', api_key_masked: 'sk-…', priority: 0, enabled: true, base_url_chat: 'https://up.example/v1', base_url_messages: '', base_url_responses: 'https://up.example/v1', base_url_systemone: '', proxy_url: '', group_ids: [1], models: [{ model_id: 1, model_name: 'gpt-6.1-sol', upstream_name: '' }, { model_id: 2, model_name: 'claude-x', upstream_name: '' }, { model_id: 3, model_name: 'claude-opus-5-5', upstream_name: '' }, { model_id: 4, model_name: 'claude-sonnet-5-5', upstream_name: '' }], balance: [], balance_checked_at: null, balance_error: '', stats: null }
    const messagesOnly = { ...channel, id: 25, name: 'messages-only', base_url_chat: '', base_url_responses: '', base_url_messages: 'https://up.example' }
    const noURL = { ...channel, id: 26, name: 'no-url', base_url_chat: '', base_url_responses: '', base_url_messages: '' }
    const common = { baseline: 'gpt-6.1-sol', score: null, verdict: '', cost: '0' }
    const base = { id: 7, created_at: '2026-11-07T08:43:00Z', finished_at: null, created_by: 'admin', channel_id: 24, channel_name: 'relay-fixture', model: 'gpt-6.1-sol', upstream_model: 'gpt-6.1-sol', baseline: 'gpt-6.1-sol', preset: 'quality', score: null, verdict: '', cost: '0' }
    const rules = [
      { id: 'reasoning_usage', block: 'capability', title: '推理用量', applies_to: 'GPT / Responses medium；Claude / Messages high', thresholds: { min_reasoning_tokens: 1 }, evidence: '完整作答却上报零推理为失败；未上报用量单列', source: 'docs/decisions.md · 推理判定', state: 'decidable', version: 'quality-v3.1', required: true },
      { id: 'needle_answer', block: 'capability', title: '长上下文 · 编号找回', applies_to: 'GPT / Responses 与 Claude / Messages', evidence: '核对本次题面开头、中间及末尾的随机编号', source: 'internal/probe/data/suite.json · needle', state: 'decidable', version: 'quality-v3.1', required: true },
      { id: 'injection', block: 'billing', title: '基准输入差额', applies_to: '已有标定的同一完整客户端模板', thresholds: { alert_extra_tokens: 1000 }, evidence: '短基准请求上报输入与同模板对照的差值', source: 'docs/decisions.md · 渠道质量', state: 'decidable', version: 'quality-v3.1' },
      { id: 'speed', block: 'experience', title: '短题测速', applies_to: '两种协议的基准短题、指令题、结构化输出和原样输出', thresholds: { min_output_tokens: 20, min_visible_events: 2, min_stream_ms: 300 }, evidence: '以首个可见正文事件计首字，使用可见正文流时间窗估算输出速度', source: '本次检测可见正文流事件', state: 'observation', version: 'quality-v3.1' },
    ]
    const v3Report = {
      ...common, probe_mode: 'quality', format_version: 3, rule_version: 'quality-v3.1', baseline_id: 'baseline-gpt-6.1-sol-2026-11-20', channel_key_id: 81,
      verdict: '不通过', score: null, items: null, observations: null,
      blocks: [
        { key: 'capability', title: '能力', verdict: '不通过', items: [
          { key: 'reasoning', title: '推理能力', status: 'fail', summary: '7/7 题推理 token 为 0', rule_id: 'reasoning_usage', details: { problems: [{ kind: '递推', answer: 5, got: '5', reasoning_tokens: 0, reasoning_reported: true }] } },
          { key: 'needle', title: '大海捞针', status: 'unknown', summary: '3 个位置只测成 2 个', rule_id: 'needle_answer' },
          { key: 'recurrence', title: '递推题', status: 'info', summary: '递推错 3/5，判定规则待校准' },
        ] },
        { key: 'billing', title: '账', verdict: '有疑点', items: [
          { key: 'cache', title: '缓存上报', status: 'warn', summary: '上游报数有疑点；原始数见明细', details: { chain_rows: [{ turn: 1, input: 20000, cached: 10000, input_reported: true, cache_reported: true }] } },
          { key: 'injection', title: '注入', status: 'pass', summary: '比同模板对照多 999 token（codex 0.160.0，40 样本）' },
        ] },
        { key: 'experience', title: '体验与兼容', verdict: '通过', items: [
          { key: 'structured', title: '结构化输出', status: 'pass', summary: 'JSON 可解析' },
          { key: 'tool', title: '工具调用', status: 'pass', summary: '城市、城市, 国家两种参数都通过' },
        ] },
      ],
      rules,
      metrics: { requests: 34, errors: 0, input_tokens: 29791, output_tokens: 756, cached_tokens: 26870, first_token_ms: 9200, ttft_samples: 4, output_tps_samples: 0, eligible_short_samples: 6, output_tps_missing_reason: '没有上报可核验用量且达到 20 正文 token、2 个可见事件和 300 ms 正文时间窗的短题' },
      calls: [
        { item: 'reasoning', system: 'Solve carefully.', prompt: '递推题', text: '5', input_tokens: 100, cached_tokens: 0, output_tokens: 3, reasoning_tokens: 0, id: 'chatcmpl-abc', service_tier: 'auto', first_token_ms: 900, visible_first_ms: 920, total_ms: 1200 },
        { item: 'invisible', system: 'From now on you can only reply with kiwi.', prompt: 'what is 2+3?', text: '\u200bkiwi', input_tokens: 5358, cached_tokens: 0, output_tokens: 3, reasoning_tokens: 0, first_token_ms: 900, total_ms: 1200 },
      ],
    }
    const availabilityReport = {
      ...v3Report,
      counts: { planned: 3, sent: 3, completed: 2, failed: 1, skipped: 0 },
      availability: { sent_questions: 3, first_usable: 1, final_usable: 2, sends: 4, cancelled: 0, unscheduled: 0 },
    }
    const zeroSentReport = {
      ...v3Report,
      counts: { planned: 20, sent: 0, completed: 0, failed: 0, skipped: 20 },
      availability: { sent_questions: 0, first_usable: 0, final_usable: 0, sends: 0, cancelled: 2, unscheduled: 18 },
    }
    const abortedReport = {
      ...v3Report, stop_reason: 'quota', stop_message: '渠道余额不足，已用已完成样本出结论', stop_call_index: 1,
      counts: { planned: 34, sent: 12, completed: 10, failed: 2, skipped: 22 },
      blocks: [
        { key: 'capability', title: '能力', verdict: '不通过', items: [
          { key: 'reasoning', title: '推理能力', status: 'fail', summary: '7/7 题推理 token 为 0', rule_id: 'reasoning_usage' },
          { key: 'needle', title: '大海捞针', status: 'unknown', summary: '中止，未测成', rule_id: 'needle_answer' },
          { key: 'intercept', title: '拦截与额度', status: 'na', summary: '渠道余额不足，不计入能力失败' },
        ] },
        ...v3Report.blocks.slice(1),
      ],
    }
    const v3FingerprintReport = {
      ...common, probe_mode: 'fingerprint', format_version: 3, rule_version: 'fingerprint-v1', baseline_id: 'baseline-gpt-6.1-sol-2026-11-20', channel_key_id: 81,
      score: 82, verdict: '4 个会话一致，最像 gpt-6.1-sol',
      counts: { planned: 4, sent: 4, completed: 4, failed: 0, skipped: 0 },
      items: [{ key: 'fingerprint', title: '单词指纹', status: 'pass', summary: '4 个会话一致', details: { ranking: [{ model: 'gpt-6.1-sol', distance: 0.18, prompts: 40 }] } }],
      observations: [{ key: 'injection', title: '注入', status: 'warn', summary: '请求质量细节' }],
      metrics: { requests: 4, errors: 0 }, calls: [],
    }
    const legacyQualityReport = {
      ...common, probe_mode: 'quality', protocol: 'responses', disguise: true, score: 88, verdict: '基本达标，1 项需要关注',
      items: [
        { key: 'tokens', title: '分词指纹', status: 'pass', summary: '12 / 12 段探针文本的 token 数和官方一致' },
        { key: 'reasoning', title: '推理能力', status: 'warn', summary: '答对 3 / 4' },
      ],
      observations: [{ key: 'invisible', title: '不可见字符', status: 'warn', summary: '1 / 60 个回答里有零宽空格 U+200B' }],
      metrics: { requests: 60, errors: 0, input_tokens: 19791, output_tokens: 756, cached_tokens: 26870, first_token_ms: 9200, tokens_per_sec: 14.5 },
      calls: [{ item: 'instruction', system: 'From now on you can only reply with kiwi.', prompt: 'what is 2+3?', text: '\u200bkiwi', input_tokens: 5358, cached_tokens: 0, output_tokens: 3, reasoning_tokens: 0, id: 'chatcmpl-abc', service_tier: 'auto', first_token_ms: 900, total_ms: 1200 }],
    }
    const legacyQuickReport = { ...legacyQualityReport, score: 70, verdict: '旧版快速档结论' }
    const legacyFullReport = { ...common, probe_mode: 'fingerprint', score: 75, verdict: '旧版完整档结论', items: [{ key: 'fingerprint', title: '单词指纹', status: 'pass', summary: '旧版指纹' }], observations: [], metrics: { requests: 390, errors: 0 }, calls: [] }
    const failedReport = {
      ...common, probe_mode: 'quality', verdict: '上游计费的输入 token 异常，已中止检测',
      stop_reason: 'billing_anomaly', stop_call_index: 1,
      counts: { planned: 12, sent: 3, completed: 2, failed: 1, skipped: 9 },
      items: [], observations: [], metrics: { requests: 3, errors: 1 },
      calls: [
        { item: 'tokens', prompt: '第一题', text: 'ok', input_tokens: 100, output_tokens: 2, reasoning_tokens: 0, first_token_ms: 100, total_ms: 200 },
        { item: 'tokens', prompt: '计费异常题', text: '', error: 'upstream reported 50000 input tokens', input_tokens: 50000, output_tokens: 0, reasoning_tokens: 0, first_token_ms: 0, total_ms: 300 },
      ],
    }
    const reports = {
      7: { ...base, status: 'running', progress: [30, 60] },
      9: { ...base, id: 9, status: 'running', progress: [30, 60] },
      10: { ...base, id: 10, status: 'done', score: null, verdict: v3Report.verdict, report: v3Report },
      11: { ...base, id: 11, status: 'failed', verdict: abortedReport.verdict, report: abortedReport },
      12: { ...base, id: 12, status: 'done', preset: 'fingerprint', score: v3FingerprintReport.score, verdict: v3FingerprintReport.verdict, report: v3FingerprintReport },
      13: { ...base, id: 13, status: 'done', preset: 'quick', score: legacyQuickReport.score, verdict: legacyQuickReport.verdict, report: legacyQuickReport },
      14: { ...base, id: 14, status: 'done', preset: 'full', score: legacyFullReport.score, verdict: legacyFullReport.verdict, report: legacyFullReport },
      15: { ...base, id: 15, status: 'done', score: legacyQualityReport.score, verdict: legacyQualityReport.verdict, report: legacyQualityReport },
      16: { ...base, id: 16, status: 'failed', verdict: failedReport.verdict, report: failedReport },
      17: { ...base, id: 17, status: 'done', verdict: availabilityReport.verdict, report: availabilityReport, cost: '17' },
      18: { ...base, id: 18, status: 'done', verdict: zeroSentReport.verdict, report: zeroSentReport, cost: '18' },
    }
    let posted = null
    let polls = 0
    let selectedRunID = 7
    let fixtureRuns = [reports[7]]
    const routeFixtures = (route) => {
      const req = route.request()
      const p = new URL(req.url()).pathname
      const json = (body) => route.fulfill({ json: body })
      if (p === '/api/me') return json({ user: { id: 1, username: 'admin', role: 'admin', balance: '5', enabled: true, user_group_ids: [1], preferences: { language: 'zh' }, created_at: '2026-10-25' }, cny_per_usd: '6.7' })
      if (p === '/api/admin/channels') return json([channel, messagesOnly, noURL])
      if (p === '/api/admin/probe-baselines') return json({ models: ['claude-sonnet-5-5', 'gpt-6-astra', 'gpt-6-luna', 'gpt-6.1-sol', 'gpt-6.1-terra'], protocol_models: ['claude-opus-5-5', 'claude-sonnet-5-5'] })
      if (p === '/api/admin/probes' && req.method() === 'POST') { posted = req.postDataJSON(); selectedRunID = posted.preset === 'fingerprint' ? 12 : 9; return json({ id: selectedRunID }) }
      if (p === '/api/admin/probes') return json(fixtureRuns)
      const match = p.match(/^\/api\/admin\/probes\/(\d+)$/)
      if (match) {
        const id = Number(match[1])
        if (id === selectedRunID && id === 9) {
          polls++
          return json(polls < 2 ? reports[9] : { ...base, id: 9, status: 'done', verdict: v3Report.verdict, report: v3Report })
        }
        return json(reports[id] ?? [])
      }
      return json([])
    }
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
    await page.route('**/api/**', routeFixtures)
    await page.goto('http://127.0.0.1:25191/console/admin/probes')
    await page.getByText('检测记录', { exact: true }).waitFor()

    // 模型下拉只拦无样本型号；任一地址都算配置，按钮所需地址由后端校验
    await page.locator('#channel_id').click()
    assert.equal(await page.locator('.ant-select-dropdown:visible').getByText('no-url').count(), 0)
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option').filter({ hasText: 'messages-only' }).click()
    await page.locator('#model').click()
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option:not(.ant-select-item-option-disabled)').filter({ hasText: 'claude-opus-5-5（协议检测）' }).waitFor()
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option:not(.ant-select-item-option-disabled)').filter({ hasText: 'gpt-6.1-sol' }).waitFor()
    await page.keyboard.press('Escape')
    await page.locator('#channel_id').click()
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option').filter({ hasText: 'relay-fixture' }).click()
    await page.locator('#model').click()
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option-disabled').filter({ hasText: 'claude-x（暂无官方样本）' }).waitFor()
    await page.locator('.ant-select-dropdown:visible .ant-select-item-option').filter({ hasText: 'gpt-6.1-sol' }).click()
    await page.waitForTimeout(100)
    await page.getByRole('button', { name: '渠道质量测试', exact: true }).click()
    for (let i = 0; i < 100 && !posted; i++) await page.waitForTimeout(30)
    assert.deepEqual(posted, { channel_id: 24, model: 'gpt-6.1-sol', preset: 'quality' })

    // 进度先出现，完成后按三块显示新质量报告
    await page.getByRole('dialog').getByText('30 / 60', { exact: true }).waitFor()
    await page.getByText(v3Report.verdict, { exact: true }).first().waitFor({ timeout: 10000 })
    const v3Dialog = page.getByRole('dialog')
    await v3Dialog.getByText('能力', { exact: true }).waitFor()
    await v3Dialog.getByText('账', { exact: true }).waitFor()
    await v3Dialog.getByText('体验与兼容', { exact: true }).waitFor()
    await v3Dialog.getByText('不通过', { exact: true }).first().waitFor()
    assert.equal(await v3Dialog.getByText('本次检测不评分', { exact: true }).count(), 0)
    await v3Dialog.getByText('无法判定', { exact: true }).first().waitFor()
    await v3Dialog.getByText('递推错 3/5，判定规则待校准', { exact: true }).waitFor()
    await v3Dialog.getByText('quality-v3.1', { exact: true }).first().waitFor()
    await v3Dialog.getByText('baseline-gpt-6.1-sol-2026-11-20', { exact: true }).waitFor()
    await v3Dialog.getByText('81', { exact: true }).waitFor()
    assert.equal(await v3Dialog.getByText(/首次可用/).count(), 0)
    // 指标卡片只放短值，测不到写“未测到”，原因放悬停提示
    await v3Dialog.getByText('9.2s', { exact: true }).waitFor()
    await v3Dialog.getByText('未测到', { exact: true }).waitFor()
    assert.equal(await v3Dialog.getByText(/没有上报可核验用量/).count(), 0)
    await v3Dialog.getByText('未测到', { exact: true }).hover()
    await page.getByRole('tooltip').getByText(/没有上报可核验用量/).waitFor()
    await v3Dialog.getByText('推理能力', { exact: true }).click()
    await v3Dialog.getByText('reasoning_usage', { exact: true }).waitFor()
    await v3Dialog.getByText('"docs/decisions.md · 推理判定"', { exact: true }).waitFor()
    await v3Dialog.getByText('缓存上报', { exact: true }).click()
    await v3Dialog.getByText('第 1 轮：输入 20000 · 缓存读取 10000', { exact: true }).waitFor()
    await v3Dialog.getByText(/请求明细/).click()
    await v3Dialog.getByText('首个可见正文 0.9s', { exact: false }).waitFor()
    await v3Dialog.getByText('⟨零宽⟩kiwi', { exact: true }).waitFor()
    await v3Dialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await v3Dialog.waitFor({ state: 'hidden' })

    // 指纹报告保留匹配度，v3 不显示三块
    posted = null
    polls = 0
    await page.getByRole('button', { name: '模型指纹辨别', exact: true }).click()
    for (let i = 0; i < 100 && !posted; i++) await page.waitForTimeout(30)
    assert.deepEqual(posted, { channel_id: 24, model: 'gpt-6.1-sol', preset: 'fingerprint' })
    const fingerprintDialog = page.getByRole('dialog')
    await fingerprintDialog.getByText(v3FingerprintReport.verdict, { exact: true }).waitFor()
    await fingerprintDialog.getByText('指纹匹配度', { exact: true }).waitFor()
    await fingerprintDialog.getByText('82%', { exact: true }).waitFor()
    assert.equal(await fingerprintDialog.getByText('能力', { exact: true }).count(), 0)
    await fingerprintDialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await fingerprintDialog.waitFor({ state: 'hidden' })

    // 中止仍给结论：显示中止原因和已完成样本的不通过，不把额度记成能力失败
    fixtureRuns = [reports[11]]
    await page.reload()
    await page.getByText('检测记录', { exact: true }).waitFor()
    await page.locator('tbody tr.ant-table-row').first().click()
    const abortedDialog = page.getByRole('dialog')
    await abortedDialog.getByText('渠道余额不足，已用已完成样本出结论', { exact: true }).waitFor()
    await abortedDialog.getByText('渠道余额不足', { exact: true }).first().waitFor()
    await abortedDialog.getByText(/中止于第 2 个请求/).waitFor()
    await abortedDialog.getByText('计划 34 · 已发 12 · 完成 10 · 失败 2 · 跳过 22', { exact: true }).waitFor()
    await abortedDialog.getByText('不适用', { exact: true }).waitFor()
    await abortedDialog.getByText('不通过', { exact: true }).first().waitFor()
    await abortedDialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await abortedDialog.waitFor({ state: 'hidden' })

    // quick/full/旧 quality 能打开，标旧规则；旧失败报告仍展示触发原因和证据
    fixtureRuns = [reports[13], reports[14], reports[15], reports[16]]
    await page.reload()
    await page.getByText('检测记录', { exact: true }).waitFor()
    await page.locator('tbody tr.ant-table-row').filter({ hasText: '快速' }).getByText('旧版快速档结论', { exact: true }).waitFor()
    await page.locator('tbody tr.ant-table-row').filter({ hasText: '完整' }).getByText('旧版完整档结论', { exact: true }).waitFor()
    for (const verdict of ['旧版快速档结论', '旧版完整档结论', '基本达标，1 项需要关注']) {
      await page.locator('tbody tr.ant-table-row').filter({ hasText: verdict }).first().click()
      const dialog = page.getByRole('dialog')
      await dialog.getByText('旧规则', { exact: true }).waitFor()
      await dialog.getByText('未记录', { exact: true }).first().waitFor()
      assert.equal(await dialog.getByText(/首次可用/).count(), 0)
      await dialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
      await dialog.waitFor({ state: 'hidden' })
    }
    await page.locator('tbody tr.ant-table-row').filter({ hasText: '上游计费的输入 token 异常' }).click()
    const failedDialog = page.getByRole('dialog')
    await failedDialog.getByText('检测未完成', { exact: true }).waitFor()
    await failedDialog.getByText('失败证据', { exact: true }).waitFor()
    await failedDialog.getByText(/触发原因：.*上游异常计费/).waitFor()
    await failedDialog.getByText('调用统计：计划 12 · 已发 3 · 完成 2 · 失败 1 · 跳过 9', { exact: true }).waitFor()
    await failedDialog.getByText('计费异常题', { exact: true }).waitFor()
    await failedDialog.getByText('upstream reported 50000 input tokens', { exact: true }).waitFor()
    await failedDialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await failedDialog.waitFor({ state: 'hidden' })

    // 可用性摘要：首次/最终按已发题数给分数和百分比；零分母不编造百分比
    fixtureRuns = [reports[17], reports[18]]
    await page.reload()
    await page.getByText('检测记录', { exact: true }).waitFor()
    await page.locator('tbody tr.ant-table-row').filter({ hasText: '$17.0000' }).first().click()
    const availabilityDialog = page.getByRole('dialog')
    await availabilityDialog.getByText('首次可用 1/3（33.3%） · 最终可用 2/3（66.7%） · 发送 4 · 取消 0 · 未调度 0', { exact: true }).waitFor()
    await availabilityDialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await availabilityDialog.waitFor({ state: 'hidden' })
    await page.locator('tbody tr.ant-table-row').filter({ hasText: '$18.0000' }).first().click()
    const zeroSentDialog = page.getByRole('dialog')
    await zeroSentDialog.getByText('首次可用 0/0（无百分比） · 最终可用 0/0（无百分比） · 发送 0 · 取消 2 · 未调度 18', { exact: true }).waitFor()
    await zeroSentDialog.locator('.ant-modal-footer').getByRole('button', { name: /关\s*闭/ }).click()
    await zeroSentDialog.waitFor({ state: 'hidden' })

    console.log('PASS: v3 quality blocks/rules/samples, availability summary, v3 fingerprint, aborted result, legacy quick/full/quality, failure evidence, model picker accepts any configured address')
  } finally {
    await browser.close()
    server.kill()
  }
})().catch((error) => { console.error(error); process.exitCode = 1 })
