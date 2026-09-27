// Portable SDK-mounted module. No Gateway renderer, router or store imports.
const text = inline => ({ format: 'text', content: { inline } })
const terminalStates = new Set(['completed', 'failed', 'cancelled', 'expired'])
const example = () => ({
  state: text('A customer cannot sign in and needs help today.'),
  questions: [
    { id: 'team', kind: 'choice', instructions: text('Choose the responsible team.'), choice: { options: [{ id: 'support', description: text('Technical help') }, { id: 'billing', description: null }] } },
    { id: 'urgency', kind: 'score', instructions: text('Rate urgency.'), score: { levels: [text('Routine'), text('Urgent')] } },
    { id: 'needs_help', kind: 'noul', instructions: text('Does the customer need assistance?'), noul: {} },
  ],
})

export function mount(element, host) {
  if (host.signal.aborted) return () => {}
  const doc = element.ownerDocument
  const lifetime = new AbortController()
  const signal = lifetime.signal
  let disposed = false
  const cleanups = []
  const el = (tag, value, className) => {
    const node = doc.createElement(tag)
    if (value != null) node.textContent = value
    if (className) node.className = className
    return node
  }
  const button = (label, action) => {
    const node = el('button', label)
    node.type = 'button'
    const listener = async () => {
      node.disabled = true
      notice.textContent = 'Host operations can incur usage. Inspect the current operation result below.'
      try { await action() } catch (error) { if (!signal.aborted) notice.textContent = error.message }
      finally { if (!signal.aborted) node.disabled = false }
    }
    node.addEventListener('click', listener)
    cleanups.push(() => node.removeEventListener('click', listener))
    return node
  }
  const field = (label, tag = 'input', value = '') => {
    const node = el(tag)
    node.value = value
    if (tag === 'input') node.type = 'text'
    const wrap = el('label', label, 'jev-field')
    wrap.append(node)
    return { node, wrap }
  }
  const select = (label, choices) => {
    const f = field(label, 'select')
    for (const [value, title] of choices) { const option = el('option', title); option.value = value; f.node.append(option) }
    return f
  }
  const section = (title, description) => {
    const node = el('section', null, 'jev-section')
    node.append(el('h2', title), el('p', description, 'jev-muted'))
    return node
  }
  const row = (...children) => { const node = el('div', null, 'jev-row'); node.append(...children); return node }
  const json = value => JSON.stringify(value, null, 2)
  const id = () => crypto.randomUUID()
  const wait = ms => new Promise((resolve, reject) => {
    const abort = () => { clearTimeout(timer); reject(new DOMException('Unmounted', 'AbortError')) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve() }, ms)
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
  const api = async (path, body) => {
    if (signal.aborted) throw new DOMException('Unmounted', 'AbortError')
    const response = await fetch(`/jev/api/${path}`, {
      method: body === undefined ? 'GET' : 'POST', credentials: 'same-origin', signal,
      headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!response.ok) throw Error(`Host operation unavailable or denied (HTTP ${response.status}). Check Gateway readiness and permissions.`)
    const result = await response.json()
    if (signal.aborted) throw new DOMException('Unmounted', 'AbortError')
    return result
  }
  const readText = async content => {
    if (content?.inline !== undefined) return content.inline
    if (!content?.handleId) return ''
    let offset = 0
    const chunks = []
    while (true) {
      const chunk = await api('payload/read', { handleId: content.handleId, offset: String(offset), limit: 24576 })
      const bytes = Uint8Array.from(atob(chunk.data), c => c.charCodeAt(0))
      if (bytes.length > 24576 || offset + bytes.length > 8388608 || BigInt(chunk.nextOffset) !== BigInt(offset + bytes.length)) throw Error('Invalid payload progress from host.')
      chunks.push(bytes); offset += bytes.length
      if (chunk.eof) break
      if (!bytes.length) throw Error('Host payload made no progress.')
    }
    const result = new Uint8Array(offset)
    let start = 0
    for (const chunk of chunks) { result.set(chunk, start); start += chunk.length }
    return new TextDecoder('utf-8', { fatal: true }).decode(result)
  }

  const root = el('div', null, 'jev-playground')
  const stylesheet = el('link'); stylesheet.rel = 'stylesheet'; stylesheet.href = '/jev/panel.css'
  const notice = el('p', 'Reading host configuration…', 'jev-notice')
  notice.setAttribute('role', 'status')
  root.append(el('h1', 'Jev playground'), el('p', 'Typed AI decisions and host-owned coding tasks. Jev chooses; coding agents do the work.'), notice)
  let model = 'jev-1.13.0'
  const refresh = async () => {
    const status = await api('status')
    model = status.settings.model
    notice.textContent = status.configured ? `API key configured in Gateway · ${model}. Requests can incur Typesafe usage.` : 'Set the write-only API key in Gateway Settings → Jev. This panel never receives the key.'
  }
  root.append(button('Refresh configuration', refresh))

  const decisions = section('Decision primitives', 'Edit the shared SDK state and questions. Choice selects an option, Score uses an ordered rubric, and Noul returns a probability. Results include actual model and accounted usage.')
  const input = field('Mixed batch JSON', 'textarea', json(example()))
  input.node.rows = 15
  input.node.spellcheck = false
  input.node.className = 'jev-machine'
  const result = el('pre', 'No decision submitted.', 'jev-result jev-machine')
  let decisionJob = null
  let decisionBusy = false
  const runDecision = async () => {
    if (decisionBusy) return
    const body = JSON.parse(input.node.value)
    decisionBusy = true
    try {
      decisionJob = (await api('decisions/start', { ...body, providerId: 'jev', model, purpose: 'evaluation', idempotencyKey: id() })).job
      while (!signal.aborted) {
        result.textContent = json(decisionJob)
        if (terminalStates.has(decisionJob.state)) break
        await wait(250)
        decisionJob = (await api('decisions/read', { jobId: decisionJob.id })).job
      }
      if (decisionJob.resultHandleId) result.textContent = await readText({ handleId: decisionJob.resultHandleId })
    } finally { decisionBusy = false }
  }
  decisions.append(input.wrap, row(button('Run decision', runDecision), button('Cancel decision', async () => {
    if (decisionJob && !terminalStates.has(decisionJob.state)) decisionJob = (await api('decisions/cancel', { jobId: decisionJob.id })).job
  })), result)

  const coding = section('Route preview and coding sessions', 'Choose an existing Gateway project and committed checkout reference. Preview uses the real host router and may incur Jev usage, but creates no process or worktree. Starting a task creates an isolated worktree; followups keep the same agent.')
  const project = field('Gateway project ID')
  const checkout = field('Committed checkout reference', 'input', 'HEAD')
  const policy = select('Routing policy', [['balanced', 'Balanced'], ['economy', 'Economy'], ['fastest', 'Fastest'], ['maximum-quality', 'Maximum quality']])
  const permission = select('Permission mode', [['ask', 'Ask'], ['auto-edit', 'Auto-edit'], ['full-access', 'Full access']])
  const request = field('Task or followup', 'textarea')
  request.node.rows = 4
  const sessionID = field('Durable session ID (attach to an existing task)')
  const sessionInfo = el('pre', 'No coding session attached.', 'jev-result')
  const previewInfo = el('pre', 'No route preview requested.', 'jev-result')
  const transcript = el('div', null, 'jev-transcript')
  transcript.setAttribute('role', 'log'); transcript.setAttribute('aria-label', 'Coding session events'); transcript.setAttribute('aria-live', 'off')
  const approvals = el('div', null, 'jev-approvals')
  let session = null
  let sessionEpoch = 0
  let previewJob = null
  let previewBusy = false
  let sequence = 0n
  const preferences = () => ({ routingPolicy: policy.node.value, permissionMode: permission.node.value })
  const intent = () => ({ projectId: project.node.value.trim(), checkoutRef: checkout.node.value.trim(), preferences: preferences(), idempotencyKey: id() })
  const promptContent = () => {
    const inline = request.node.value
    if (!inline.trim() || new TextEncoder().encode(inline).length > 32768) throw Error('Enter a task or followup of at most 32 KiB before continuing.')
    return { inline }
  }
  const renderSession = next => {
    session = next; sessionID.node.value = next.id; sessionInfo.textContent = json(next)
  }
  const showEvent = async (event, epoch) => {
    const item = el('article', null, 'jev-event')
    item.append(el('strong', `${event.sequence} · ${event.kind}`))
    const value = event.message?.content || event.tool?.content
    const content = value ? await readText(value) : json(event)
    const bounded = content.length > 16384 ? content.slice(0, 16384) + '\n[Display truncated; complete event remains in host history.]' : content
    item.append(el('pre', bounded, event.tool ? 'jev-machine' : ''))
    if (signal.aborted || epoch !== sessionEpoch) return
    if (event.approval) {
      const approval = event.approval
      const card = el('div', null, 'jev-approval')
      card.append(el('p', approval.title || 'Agent requests permission'))
      for (const option of approval.options || []) card.append(button(option.name, async () => {
        const sid = event.sessionId
        if (session?.id !== sid || epoch !== sessionEpoch) return
        renderSession((await api('coding/approve', { sessionId: sid, approvalId: approval.id, optionId: option.id })).session)
        card.remove()
      }))
      approvals.append(card)
    }
    transcript.append(item)
    while (transcript.childElementCount > 300) transcript.firstElementChild.remove()
  }
  const observe = async epoch => {
    try {
      while (!signal.aborted && epoch === sessionEpoch && session) {
        const sid = session.id
        const history = await api('coding/events', { sessionId: sid, afterSequence: String(sequence), limit: 50 })
        if (epoch !== sessionEpoch) return
        for (const event of history.events) {
          const next = BigInt(event.sequence)
          if (next <= sequence) continue
          if (event.sessionId !== sid) throw Error('Host returned an unrelated session event.')
          if (sequence && next !== sequence + 1n) throw Error('Session history has a gap. Attach again to reload it.')
          await showEvent(event, epoch)
          if (epoch !== sessionEpoch || signal.aborted) return
          sequence = next
        }
        const current = await api('coding/read', { sessionId: sid })
        if (epoch !== sessionEpoch) return
        renderSession(current.session)
        if (current.session.state === 'ended' || current.session.state === 'completed') return
        if (!history.hasMore) await wait(700)
      }
    } catch (error) { if (!signal.aborted && epoch === sessionEpoch) notice.textContent = error.message }
  }
  const attach = next => {
    sessionEpoch++; sequence = 0n; transcript.replaceChildren(); approvals.replaceChildren(); renderSession(next)
    void observe(sessionEpoch)
  }
  const preview = async () => {
    if (previewBusy) return
    previewBusy = true
    try {
      previewJob = (await api('coding/preview', { ...intent(), content: promptContent() })).job
      while (!signal.aborted) {
        previewInfo.textContent = json(previewJob)
        if (terminalStates.has(previewJob.state)) break
        await wait(250)
        previewJob = (await api('coding/preview-read', { jobId: previewJob.id })).job
      }
    } finally { previewBusy = false }
  }
  coding.append(row(project.wrap, checkout.wrap), row(policy.wrap, permission.wrap), request.wrap,
    row(button('Preview route', preview), button('Cancel preview', async () => {
      if (previewJob && !terminalStates.has(previewJob.state)) previewJob = (await api('coding/preview-cancel', { jobId: previewJob.id })).job
    }), button('Discover coding agents', async () => { previewInfo.textContent = json(await api('coding/catalog', {})) })), previewInfo,
    row(button('Start isolated coding task', async () => {
      if (session && !['ended', 'completed'].includes(session.state)) throw Error('End or complete the attached task before starting another.')
      const content = promptContent()
      const created = await api('coding/create', intent())
      attach(created.session)
      renderSession((await api('coding/prompt', { sessionId: created.session.id, content, idempotencyKey: id() })).session)
    }), button('Send followup', async () => {
      if (!session) throw Error('Start or attach a session first.')
      renderSession((await api('coding/prompt', { sessionId: session.id, content: promptContent(), idempotencyKey: id() })).session)
    }), button('Stop prompt', async () => {
      if (session?.activePromptId) renderSession((await api('coding/stop', { sessionId: session.id, promptId: session.activePromptId })).session)
    })), sessionID.wrap,
    row(button('Attach session', async () => { attach((await api('coding/read', { sessionId: sessionID.node.value.trim() })).session) }),
      button('Open same task in terminal dock', async () => {
        if (!session) throw Error('Start or attach a session first.')
        await api('coding/open-surface', { sessionId: session.id })
        notice.textContent = 'The host opened the same task in the terminal dock. Closing that docked tab ends its session.'
      }), button('Complete task', async () => {
        if (session) renderSession((await api('coding/complete', { sessionId: session.id })).session)
      }), button('End session', async () => {
        if (session) renderSession((await api('coding/end', { sessionId: session.id })).session)
      })),
    el('p', 'End session stops its agent connection. Navigating away from this playground only detaches the view; it does not end the task.', 'jev-muted'), sessionInfo, approvals, transcript)
  root.append(decisions, coding)
  element.replaceChildren(stylesheet, root)
  const cleanup = () => {
    if (disposed) return
    disposed = true; sessionEpoch++; lifetime.abort()
    host.signal.removeEventListener('abort', cleanup)
    for (const unsubscribe of cleanups) unsubscribe()
    element.replaceChildren()
    // Never cancel/end a durable task as a side effect of presentation teardown.
  }
  host.signal.addEventListener('abort', cleanup, { once: true })
  if (host.signal.aborted) cleanup()
  else void refresh().catch(error => { if (!signal.aborted) notice.textContent = error.message })
  return cleanup
}
