// Hermetic module lifecycle tests; browser and installed-host acceptance are separate.
import assert from 'node:assert/strict'
import test from 'node:test'
import { mount } from '../jevplugin/panel.mjs'

class Node {
  constructor(tag, ownerDocument) { this.tag = tag; this.ownerDocument = ownerDocument; this.children = []; this.events = new Map(); this.value = ''; this.textContent = '' }
  append(...nodes) { for (const node of nodes) { node.parent = this; this.children.push(node); if (this.tag === 'select' && this.children.length === 1) this.value = node.value } }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes) }
  setAttribute() {}
  addEventListener(type, fn) { this.events.set(type, fn) }
  removeEventListener(type, fn) { if (this.events.get(type) === fn) this.events.delete(type) }
  get childElementCount() { return this.children.length }
  get firstElementChild() { return this.children[0] }
  remove() { this.parent.children = this.parent.children.filter(node => node !== this) }
  async click() { await this.events.get('click')?.() }
}
const all = root => [root, ...root.children.flatMap(all)]
const find = (root, text) => all(root).find(node => node.textContent === text)
const settle = () => new Promise(resolve => setImmediate(resolve))
const setup = async (t, respond) => {
  const calls = []
  const doc = { createElement: tag => new Node(tag, doc) }
  const element = new Node('main', doc)
  const controller = new AbortController()
  t.mock.method(globalThis, 'fetch', async (path, options) => {
    calls.push({ path, options, body: options.body && JSON.parse(options.body) })
    if (path === '/jev/api/status') return { ok: true, json: async () => ({ configured: true, settings: { model: 'jev-1.13.0' } }) }
    return respond(path, options)
  })
  const cleanup = mount(element, { signal: controller.signal })
  t.after(cleanup)
  await settle()
  find(element, 'Task or followup').children[0].value = 'Fixture task'
  return { calls, element, controller, cleanup }
}

test('mount withdrawal is idempotent and does not cancel a durable task', async t => {
  const fixture = await setup(t, () => { throw Error('Unexpected host call') })
  assert.ok(find(fixture.element, 'Jev playground'))
  assert.equal(all(fixture.element).filter(n => n.tag === 'input' && n.type === 'password').length, 0)
  const button = find(fixture.element, 'Start isolated coding task')
  fixture.controller.abort(); fixture.cleanup()
  await button.click()
  assert.equal(fixture.element.childElementCount, 0)
  assert.equal(fixture.calls.length, 1)
  mount(fixture.element, { signal: fixture.controller.signal })()
  assert.equal(fixture.element.childElementCount, 0)
})

test('route preview uses only the real public host preview path', async t => {
  const fixture = await setup(t, () => ({ ok: true, json: async () => ({ job: { id: 'preview', state: 'completed', result: { status: 'pending', reason: 'Fixture: no eligible coding agent' } } }) }))
  await find(fixture.element, 'Preview route').click()
  const call = fixture.calls.at(-1)
  assert.equal(call.path, '/jev/api/coding/preview')
  assert.equal(call.body.preferences.routingPolicy, 'balanced')
  assert.equal(call.body.preferences.permissionMode, 'ask')
  assert.ok(!fixture.calls.some(call => /create|prompt|egress/.test(call.path)))
  assert.ok(all(fixture.element).some(node => node.textContent.includes('no eligible coding agent')))
})

test('mixed decision submits SDK primitives and renders result as text', async t => {
  const fixture = await setup(t, () => ({ ok: true, json: async () => ({ job: { id: 'job', state: 'completed', result: { model: 'jev-1.13.0', note: '<script>unsafe()</script>' } } }) }))
  await find(fixture.element, 'Run decision').click()
  const call = fixture.calls.at(-1)
  assert.equal(call.path, '/jev/api/decisions/start')
  assert.deepEqual(call.body.questions.map(q => q.kind), ['choice', 'score', 'noul'])
  assert.equal(call.body.model, 'jev-1.13.0')
  assert.ok(all(fixture.element).some(node => node.tag === 'pre' && node.textContent.includes('<script>')))
})

test('unavailable host is shown without a local successful result', async t => {
  const fixture = await setup(t, () => ({ ok: false, status: 503 }))
  await find(fixture.element, 'Preview route').click()
  assert.ok(all(fixture.element).some(node => node.textContent.includes('HTTP 503')))
  assert.ok(find(fixture.element, 'No route preview requested.'))
})

test('late response cannot revive an unmounted generation', async t => {
  let resolve
  const fixture = await setup(t, () => new Promise(done => { resolve = done }))
  const clicking = find(fixture.element, 'Preview route').click()
  fixture.controller.abort()
  resolve({ ok: true, json: async () => ({ job: { state: 'completed' } }) })
  await clicking
  assert.equal(fixture.element.childElementCount, 0)
})

test('coding followups and dock share one session; detach never ends it', async t => {
  const session = { id: 'session-one', state: 'active', activePromptId: 'prompt-one' }
  const fixture = await setup(t, path => ({ ok: true, json: async () => path.endsWith('/events')
    ? { events: [], lastSequence: '0', hasMore: false }
    : path.endsWith('/open-surface') ? { sessionId: session.id, surfaceId: 'dock-one', focused: true }
      : { session, promptId: 'prompt-one' } }))
  find(fixture.element, 'Gateway project ID').children[0].value = 'project-one'
  await find(fixture.element, 'Start isolated coding task').click()
  await find(fixture.element, 'Send followup').click()
  await find(fixture.element, 'Open same task in terminal dock').click()
  fixture.cleanup()
  await settle()
  const prompts = fixture.calls.filter(call => call.path.endsWith('/prompt'))
  assert.equal(prompts.length, 2)
  assert.ok(prompts.every(call => call.body.sessionId === 'session-one'))
  assert.equal(fixture.calls.filter(call => call.path.endsWith('/create')).length, 1)
  assert.equal(fixture.calls.find(call => call.path.endsWith('/open-surface')).body.sessionId, 'session-one')
  assert.ok(!fixture.calls.some(call => call.path.endsWith('/end')))
  assert.equal(fixture.element.childElementCount, 0)
})

test('empty coding task cannot create a session or worktree', async t => {
  const fixture = await setup(t, () => { throw Error('Unexpected host call') })
  find(fixture.element, 'Task or followup').children[0].value = ''
  await find(fixture.element, 'Start isolated coding task').click()
  assert.equal(fixture.calls.length, 1)
  assert.ok(all(fixture.element).some(node => node.textContent.includes('Enter a task')))
})
