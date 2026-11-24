// 渠道页「连接状态」列回归。使用安装了 playwright 的 Node 环境运行：node web/tests/channels.browser.cjs。
const { chromium } = require('playwright')
const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
// 端口用 2 万段：本机 Windows 动态端口范围是 1024–15000，镜像网络下 51xx 会被随机占用
const path = require('node:path')

;(async () => {
  const server = spawn(process.execPath, [path.join(__dirname, '../node_modules/vite/bin/vite.js'), '--host', '127.0.0.1', '--port', '25190', '--strictPort'], { cwd: path.join(__dirname, '..'), stdio: 'ignore' })
  const browser = await chromium.launch({ headless: true })
  try {
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch('http://127.0.0.1:25190')).ok) break } catch {}
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    const oneKey = [{ id: 90, masked: 'sk-z…zzzz', enabled: true, note: '', status: { state: 'ok' } }]
    const base = { template: 'standard', note: '', keys: oneKey, api_key_masked: 'sk-…', priority: 0, enabled: true, base_url_chat: 'https://up.example/v1', base_url_messages: '', base_url_responses: '', base_url_systemone: '', proxy_url: '', group_ids: [1], models: [{ model_id: 1, model_name: 'gpt-6-sol', upstream_name: '' }], balance: [], balance_checked_at: null, balance_error: '' }
    const stat = { success: 0, failed: 0, first_token_ms: null, tokens_per_sec: null, cache_read_tokens: 0, input_tokens: 0, last_success_at: null, last_failure_at: null }
    const channels = [
      // 健康：97 / 100 → 97%，首字 2.35s → 2.4s（保留一位），缓存 870 / 1000
      { ...base, id: 1, name: 'healthy', template: 'custom', note: '质量好，0.15 倍', keys: [
        { id: 11, masked: 'sk-a…aaaa', enabled: true, note: '主号', status: { state: 'ok' }, stats: { success: 12, failed: 0 } },
        { id: 12, masked: 'sk-b…bbbb', enabled: true, note: '', status: { state: 'cooling', until: '2026-11-07T12:00:00Z', reason: 'HTTP 429 rate limited' }, stats: { success: 3, failed: 2 } },
        { id: 13, masked: 'sk-c…cccc', enabled: true, note: '', status: { state: 'invalid', until: '2026-11-07T12:30:00Z', reason: 'HTTP 401 invalid key' } },
      ], stats: { ...stat, success: 97, failed: 3, first_token_ms: 2350, tokens_per_sec: 45, cache_read_tokens: 870, input_tokens: 1000, last_success_at: '2026-11-06T07:00:00Z', last_failure_at: '2026-11-06T06:00:00Z' },
        models: [
          { model_id: 1, model_name: 'gpt-6-sol', upstream_name: '' },
          { model_id: 2, model_name: 'gpt-6.1-terra', upstream_name: 'openai/gpt-6.1-terra' },
          { model_id: 4, model_name: 'foo-1.2-3', upstream_name: 'manual/legacy' },
        ] },
      // 199 / 200 也只显示 99%，不四舍五入成 100%
      { ...base, id: 2, name: 'almost', stats: { ...stat, success: 199, failed: 1 } },
      { ...base, id: 3, name: 'broken', cooling: true, stats: { ...stat, success: 1, failed: 9 } },
      { ...base, id: 4, name: 'idle', stats: null },
      { ...base, id: 5, name: 'empty', models: [], stats: null },
    ]
    const siteModels = [
      { id: 1, name: 'gpt-6-sol', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
      { id: 2, name: 'gpt-6.1-terra', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
      { id: 3, name: 'claude-fable-5-2', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
      { id: 4, name: 'foo-1.2-3', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
      { id: 5, name: 'foo-1-2.3', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
      { id: 6, name: 'provider/gpt-6-sol', input_price: '1', output_price: '2', cache_read_price: '0', cache_write_price: '0', price_rules: [], fast_multiplier: null },
    ]
    const fetchedModels = ['openai/gpt-6.1-terra', 'anthropic/claude-fable-5.2', 'provider/unknown']
    const orphanedUpstream = 'orphaned-upstream'
    let saved = null
    const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
    await page.route(/\/src\/pages\/admin\/Channels\.tsx(?:\?|$)/, async (route) => {
      const response = await route.fetch()
      const source = await response.text()
      const options = /connectedModels\.map\(\(name\) => \(\{\s*value: name,\s*label: name\s*\}\)\)/g
      const withOrphanedOption = source.replace(options, `$&.concat({ value: '${orphanedUpstream}', label: '${orphanedUpstream}' })`)
      assert.notEqual(withOrphanedOption, source)
      return route.fulfill({ response, body: withOrphanedOption })
    })
    await page.route('**/api/**', (route) => {
      const p = new URL(route.request().url()).pathname
      const json = (b) => route.fulfill({ json: b })
      if (p === '/api/me') return json({ user: { id: 1, username: 'admin', role: 'admin', balance: '5', enabled: true, user_group_ids: [1], preferences: { language: 'zh' }, created_at: '2026-10-25' }, cny_per_usd: '6.7' })
      if (p === '/api/admin/channels/1' && route.request().method() === 'PUT') { saved = route.request().postDataJSON(); return json({ id: 1, keys_deduped: 1 }) }
      if (p === '/api/admin/channels') return json(channels)
      if (p === '/api/admin/groups') return json([{ id: 1, name: 'default', ratio: '1', enabled: true, is_public: true, user_group_ids: [], description: '' }])
      if (p === '/api/admin/models') return json(siteModels)
      if (p === '/api/admin/channels/fetch-models') return json(fetchedModels)
      return json([])
    })
    await page.goto('http://127.0.0.1:25190/console/admin/channels')
    const row = (name) => page.locator('tbody tr').filter({ has: page.getByText(name, { exact: true }) })
    await row('healthy').waitFor()

    // 不再有模型列，改为连接状态
    assert.equal(await page.getByRole('columnheader', { name: '模型' }).count(), 0)
    await page.getByRole('columnheader', { name: '连接状态' }).waitFor()
    await row('healthy').getByText('成功率 97% · 100 次', { exact: true }).waitFor()
    await row('healthy').getByText('首字 2.4s · 45 tok/s · 缓存 87%', { exact: true }).waitFor()
    await row('almost').getByText('成功率 99% · 200 次', { exact: true }).waitFor()
    await row('broken').getByText('成功率 10% · 10 次', { exact: true }).waitFor()
    await row('broken').getByText('冷却中', { exact: true }).waitFor()
    assert.equal(await row('broken').locator('.ant-badge-status-error').count(), 1)
    assert.equal(await row('healthy').locator('.ant-badge-status-success').count(), 1)
    await row('idle').getByText('24 小时无调用', { exact: true }).waitFor()
    await row('empty').getByText('未配置模型', { exact: true }).waitFor()
    // 悬停看次数和最近成功 / 失败时间
    await row('healthy').getByText('成功率 97% · 100 次', { exact: true }).hover()
    await page.getByText('最近 24 小时：成功 97 次，失败 3 次', { exact: true }).waitFor()
    // 渠道备注显示在名称下方；多 key 的渠道标出 key 数，有失效的标红
    await row('healthy').getByText('质量好，0.15 倍', { exact: true }).waitFor()
    assert.equal(await row('healthy').locator('.ant-tag-red').filter({ hasText: '3 把 key' }).count(), 1)
    assert.equal(await row('idle').getByText(/把 key/).count(), 0)
    // 编辑抽屉：每把 key 的掩码、状态、24 小时次数。行里没有编辑按钮，点行里的空白单元格（ID 列）打开抽屉
    assert.equal(await row('healthy').getByRole('button', { name: /编\s*辑/ }).count(), 0)
    assert.equal(await row('healthy').getAttribute('aria-label'), '编辑渠道 · healthy')
    await row('healthy').locator('td').nth(1).click()
    await page.getByText('编辑渠道 · healthy', { exact: true }).waitFor()
    const drawer = page.locator('.ant-drawer-section')
    await drawer.getByText('sk-a…aaaa', { exact: true }).waitFor()
    await drawer.getByText('正常', { exact: true }).waitFor()
    await drawer.getByText(/^冷却中（到 /).waitFor()
    await drawer.getByText(/^失效（/).waitFor()
    await drawer.getByText('12 / 0').waitFor()
    // 接入模型按原名和映射反推，不再猜测厂商前缀/版本号，也不显示“站内已有”。
    const connected = drawer.getByTestId('connected-models')
    const modelNames = () => connected.locator('.ant-tag .mono').evaluateAll((spans) => spans.map((span) => span.textContent.trim()))
    const mappingSection = drawer.getByTestId('model-mapping')
    const mappingRows = () => mappingSection.locator('.ant-table-tbody').last().locator('tr')
    const addManualModel = async (name) => {
      await connected.getByRole('button', { name: /手动添加/ }).click()
      const input = connected.getByRole('textbox')
      await input.fill(name)
      await input.press('Enter')
    }
    const chooseMapping = async (row, index, name) => {
      await row.getByRole('combobox').nth(index).click()
      await page.locator('.ant-select-dropdown:visible').locator('.ant-select-item-option-content').getByText(name, { exact: true }).click()
    }
    const addMapping = async (upstream, site) => {
      await mappingSection.getByRole('button', { name: /添加映射/ }).click()
      const row = mappingRows().last()
      await chooseMapping(row, 0, upstream)
      await chooseMapping(row, 1, site)
      return row
    }
    await connected.getByText('接入的模型（3）', { exact: true }).waitFor()
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'openai/gpt-6.1-terra', 'manual/legacy'])
    const mappedTag = connected.locator('.ant-tag').filter({ hasText: 'openai/gpt-6.1-terra' })
    await mappedTag.waitFor()
    assert.equal(await mappedTag.getAttribute('class').then((value) => value.includes('ant-tag-red')), false)
    await mappingSection.getByText('需要时才加：客户端请求站内模型，发给上游的是左边的名字', { exact: true }).waitFor()
    await mappingRows().filter({ hasText: 'gpt-6.1-terra' }).getByText('openai/gpt-6.1-terra', { exact: true }).waitFor()
    await mappingRows().filter({ hasText: 'foo-1.2-3' }).getByText('manual/legacy', { exact: true }).waitFor()
    assert.equal(await mappingRows().count(), 2)
    assert.equal(await drawer.getByText('站内已有', { exact: true }).count(), 0)
    // 手动添加：重复名字不重复加；站内无同名又没映射的标签是红色，补映射后恢复。
    await connected.getByRole('button', { name: /手动添加/ }).click()
    const addInput = connected.getByRole('textbox')
    await addInput.fill('discarded/manual')
    await addInput.press('Escape')
    await addInput.waitFor({ state: 'hidden' })
    assert.equal((await modelNames()).includes('discarded/manual'), false)
    await addManualModel('  manual/legacy  ')
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'openai/gpt-6.1-terra', 'manual/legacy'])
    await addManualModel('provider/foo-1-2.3')
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'openai/gpt-6.1-terra', 'manual/legacy', 'provider/foo-1-2.3'])
    const warningTag = connected.locator('.ant-tag-red').filter({ hasText: 'provider/foo-1-2.3' })
    await warningTag.waitFor()
    await connected.getByText(/红色的 1 个模型站内没有/).waitFor()
    assert.equal(await connected.getByRole('link', { name: '「定价」页配置价格' }).getAttribute('href'), '/console/admin/pricing')
    // 自动映射：去厂商前缀、版本号点与连字符等同，唯一候选才填；拿不准的点名提示
    await addManualModel('anthropic/claude-fable-5.2')
    await addManualModel('zz/nothing-1')
    await mappingSection.getByRole('button', { name: '自动映射' }).click()
    await page.getByText(/已自动映射 \d+ 个；.*zz\/nothing-1/).waitFor()
    await mappingRows().filter({ hasText: 'anthropic/claude-fable-5.2' }).getByText('claude-fable-5-2', { exact: true }).waitFor()
    await connected.locator('.ant-tag').filter({ hasText: 'zz/nothing-1' }).locator('.ant-tag-close-icon').click()
    await connected.locator('.ant-tag').filter({ hasText: 'anthropic/claude-fable-5.2' }).locator('.ant-tag-close-icon').click()
    await addMapping('provider/foo-1-2.3', 'foo-1-2.3')
    await warningTag.waitFor({ state: 'hidden' })
    assert.equal(await mappingSection.getByRole('button', { name: '自动映射' }).count(), 0)
    // 拉取弹窗：已接入预勾选；“上游不存在”默认勾选，搜索隐藏的勾选保留，取消不改草稿。
    await drawer.locator('button').filter({ hasText: '从上游拉取模型' }).click()
    const picker = page.getByRole('dialog').filter({ hasText: '上游有 3 个模型' })
    const checkedCount = () => picker.locator('input[type="checkbox"]:checked').count()
    await picker.getByRole('searchbox').waitFor()
    assert.equal(await picker.getByTestId('picked-models').count(), 0)
    assert.equal(await checkedCount(), 4)
    assert.equal(await picker.getByRole('checkbox', { name: /openai\/gpt-6.1-terra/ }).isChecked(), true)
    assert.equal(await picker.getByRole('checkbox', { name: /anthropic\/claude-fable-5.2/ }).isChecked(), false)
    assert.equal(await picker.getByRole('checkbox', { name: /provider\/unknown/ }).isChecked(), false)
    const missing = picker.getByTestId('missing-upstream-models')
    await missing.getByText('上游不存在', { exact: true }).waitFor()
    for (const name of ['gpt-6-sol', 'manual/legacy', 'provider/foo-1-2.3']) await missing.getByText(name, { exact: false }).waitFor()
    assert.equal(await missing.locator('input[type="checkbox"]:checked').count(), 3)
    assert.equal(await picker.getByText('站内已有', { exact: true }).count(), 0)
    await missing.getByRole('checkbox', { name: /manual\/legacy/ }).uncheck()
    assert.equal(await checkedCount(), 3)
    await picker.getByRole('searchbox').fill('anthropic')
    assert.equal(await picker.getByRole('checkbox', { name: /openai\/gpt-6.1-terra/ }).isVisible(), false)
    assert.equal(await missing.getByRole('checkbox', { name: 'gpt-6-sol', exact: true }).isVisible(), false)
    await picker.getByRole('checkbox', { name: /anthropic\/claude-fable-5.2/ }).check()
    await picker.getByRole('searchbox').fill('')
    await picker.getByRole('button', { name: /确\s*定/ }).click()
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'openai/gpt-6.1-terra', 'provider/foo-1-2.3', 'anthropic/claude-fable-5.2'])
    assert.equal(await mappingRows().count(), 2)
    assert.equal(await mappingSection.getByText('manual/legacy', { exact: true }).count(), 0)
    await picker.waitFor({ state: 'hidden' })
    await drawer.locator('button').filter({ hasText: '从上游拉取模型' }).click()
    await picker.getByRole('checkbox', { name: /openai\/gpt-6.1-terra/ }).uncheck()
    await picker.getByRole('checkbox', { name: /provider\/unknown/ }).check()
    await picker.getByRole('button', { name: /取\s*消/ }).click()
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'openai/gpt-6.1-terra', 'provider/foo-1-2.3', 'anthropic/claude-fable-5.2'])
    assert.equal(await mappingRows().count(), 2)
    // 全部取消勾选后确认：接入与映射一起移除，空态文案出现。
    await picker.waitFor({ state: 'hidden' })
    await drawer.locator('button').filter({ hasText: '从上游拉取模型' }).click()
    await picker.getByRole('checkbox', { name: /provider\/foo-1-2.3/ }).waitFor() // 弹窗渲染完再数，否则一开始数到 0 直接跳过
    while (await checkedCount() > 0) await picker.locator('input[type="checkbox"]:checked').first().uncheck()
    assert.equal(await checkedCount(), 0)
    await picker.getByRole('button', { name: /确\s*定/ }).click()
    await connected.getByText('还没有接入模型，点「从上游拉取模型」勾选', { exact: true }).waitFor()
    assert.equal(await mappingRows().count(), 0)
    assert.equal(await mappingSection.getByRole('button', { name: '自动映射' }).count(), 0)
    await addManualModel('gpt-6-sol')
    await addManualModel('manual/legacy')
    await drawer.locator('button').filter({ hasText: '从上游拉取模型' }).click()
    await picker.getByRole('checkbox', { name: 'provider/unknown', exact: true }).check()
    await picker.getByRole('checkbox', { name: 'anthropic/claude-fable-5.2', exact: true }).check()
    await picker.getByRole('button', { name: /确\s*定/ }).click()
    assert.deepEqual(await modelNames(), ['gpt-6-sol', 'manual/legacy', 'anthropic/claude-fable-5.2', 'provider/unknown'])
    await addMapping('manual/legacy', 'foo-1.2-3')
    await addMapping('anthropic/claude-fable-5.2', 'claude-fable-5-2')
    const save = drawer.locator('.ant-drawer-extra').getByRole('button', { name: /保\s*存/ })
    const expectSaveBlocked = async (message) => {
      await save.click()
      await page.getByText(message, { exact: true }).waitFor()
      assert.equal(saved, null)
    }
    await mappingSection.getByRole('button', { name: /添加映射/ }).click()
    const draftMapping = mappingRows().last()
    await expectSaveBlocked('第 3 条映射没选上游模型')
    await chooseMapping(draftMapping, 0, 'manual/legacy')
    await expectSaveBlocked('映射里的 manual/legacy 没选站内模型')
    await chooseMapping(draftMapping, 1, 'gpt-6.1-terra')
    await expectSaveBlocked('上游模型 manual/legacy 有重复映射')
    await chooseMapping(draftMapping, 0, orphanedUpstream)
    await expectSaveBlocked(`映射里的 ${orphanedUpstream} 还没接入，先在上面接入`)
    await draftMapping.getByRole('button', { name: '删除映射' }).click()
    // 追加：粘贴里的重复当场去掉并提示
    await drawer.getByRole('button', { name: /添加 key/ }).click()
    const dlg = page.getByRole('dialog').filter({ hasText: '追加' })
    await dlg.locator('textarea').fill('sk-new-key-12345678\n\nsk-new-key-12345678')
    await dlg.getByRole('button', { name: /确\s*定/ }).click()
    await page.getByText('将追加 1 把 key（粘贴里去掉 1 把重复），保存后生效', { exact: true }).waitFor()
    await drawer.getByText('待保存', { exact: true }).waitFor()
    // 覆盖：只留这次粘贴的
    await drawer.getByRole('button', { name: /添加 key/ }).click()
    await dlg.getByText('覆盖', { exact: true }).click()
    await dlg.getByText(/覆盖会替换现有的全部 key/).waitFor()
    await dlg.locator('textarea').fill('sk-only-one-12345678\nsk-a-same-as-old-123')
    await dlg.getByRole('button', { name: /确\s*定/ }).click()
    await drawer.getByText('sk-o…5678', { exact: true }).waitFor()
    await drawer.getByText('sk-a…-123', { exact: true }).waitFor()
    for (const old of ['sk-a…aaaa', 'sk-b…bbbb', 'sk-c…cccc', 'sk-n…5678']) assert.equal(await drawer.getByText(old, { exact: true }).count(), 0)
    // 无同名的接入模型会先拦截；补上映射后保存，payload 按接入模型逐行写出。
    await expectSaveBlocked('这些模型站内没有：provider/unknown。在下方加映射，或去「定价」页配置价格')
    await addMapping('provider/unknown', 'gpt-6.1-terra')
    // 保存：新 key 带完整内容；服务端去掉的重复数提示出来
    await save.click()
    for (let i = 0; i < 100 && !saved; i++) await page.waitForTimeout(30)
    assert.deepEqual(saved.keys, [{ api_key: 'sk-only-one-12345678', enabled: true, note: '' }, { api_key: 'sk-a-same-as-old-123', enabled: true, note: '' }])
    assert.equal(saved.note, '质量好，0.15 倍')
    assert.equal(saved.api_key, undefined)
    assert.deepEqual(saved.models, [
      { model_id: 1, model_name: 'gpt-6-sol', upstream_name: 'gpt-6-sol' },
      { model_id: 4, model_name: 'foo-1.2-3', upstream_name: 'manual/legacy' },
      { model_id: 3, model_name: 'claude-fable-5-2', upstream_name: 'anthropic/claude-fable-5.2' },
      { model_id: 2, model_name: 'gpt-6.1-terra', upstream_name: 'provider/unknown' },
    ])
    await page.getByText('渠道已保存，自动去掉了 1 把重复的 key', { exact: true }).waitFor()
    // 行里的操作各自独立，不打开编辑：删除（取消确认）、启用开关、拖动把手、额度
    const drawerTitle = page.getByText('编辑渠道 · healthy', { exact: true })
    await row('healthy').getByRole('button', { name: /删\s*除/ }).click()
    await page.locator('.ant-popconfirm').getByRole('button', { name: /取\s*消/ }).click()
    assert.equal(await drawerTitle.isVisible(), false)
    await row('almost').getByRole('switch').click()
    assert.equal(await drawerTitle.isVisible(), false)
    await row('healthy').locator('button[title*="拖动排序"]').click()
    assert.equal(await drawerTitle.isVisible(), false)
    await row('healthy').locator('[data-row-action]').click()
    assert.equal(await drawerTitle.isVisible(), false)
    // 拖动把手真的能改顺序，拖的过程中不会打开编辑
    const order = () => page.locator('tbody tr').evaluateAll((rows) => rows.map((r) => r.cells[1]?.textContent?.trim()).filter(Boolean))
    const before = await order()
    const handle = row('healthy').locator('button[title*="拖动排序"]')
    const box = await handle.boundingBox()
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2 + 60, { steps: 12 })
    await page.mouse.up()
    await page.waitForTimeout(400)
    assert.notDeepEqual(await order(), before)
    assert.equal(await drawerTitle.isVisible(), false)
    console.log('PASS: channel note under the name, key count tag colored by key health, per-key status and 24h counts in the drawer, connected models and mappings reverse from saved rows, manual add dedups, missing upstream models default checked, picker has no summary bar, cancel and uncheck-all behave, warning tags and orphan mapping block save, append dedups pasted keys, replace keeps only pasted keys, save payload carries new keys and model rows, server dedup count shown; channel list shows connection status instead of models: success rate floors, colors, TTFT/speed/cache line, cooling tag, idle and no-model states, tooltip details')
  } finally {
    await browser.close()
    server.kill()
  }
})().catch((error) => { console.error(error); process.exitCode = 1 })
