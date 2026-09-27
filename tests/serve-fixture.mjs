// Local presentation fixture only. No credentials, live host calls or agent launch.
import { readFile } from 'node:fs/promises'
import { createServer } from 'node:http'
const root = new URL('../', import.meta.url)
const module = await readFile(new URL('jevplugin/panel.mjs', root))
const styles = await readFile(new URL('jevplugin/panel.css', root))
const tokens = process.env.GATEWAY_DESIGN_TOKENS_FILE ? await readFile(process.env.GATEWAY_DESIGN_TOKENS_FILE) : ''
const session = { id: 'fixture-session', taskId: 'fixture-task', state: 'active', activePromptId: 'fixture-prompt', projectId: 'fixture-project' }
const server = createServer(async (request, response) => {
  response.setHeader('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'")
  if (request.url === '/jev/panel.mjs') { response.setHeader('Content-Type', 'text/javascript'); response.end(module); return }
  if (request.url === '/jev/panel.css') { response.setHeader('Content-Type', 'text/css'); response.end(styles); return }
  if (request.url === '/tokens.css') { response.setHeader('Content-Type', 'text/css'); response.end(tokens); return }
  if (request.url === '/fixture.mjs') {
    response.setHeader('Content-Type', 'text/javascript')
    response.end(`import {mount} from '/jev/panel.mjs'; const controller = new AbortController(); const cleanup=mount(document.querySelector('main'), {signal:controller.signal}); document.querySelector('#detach').addEventListener('click',()=>{controller.abort();cleanup()});`)
    return
  }
  if (request.url.startsWith('/jev/api/')) {
    let body = ''; for await (const chunk of request) body += chunk
    const input = body ? JSON.parse(body) : {}
    response.setHeader('Content-Type', 'application/json')
    const path = request.url.slice('/jev/api/'.length)
    const result = path === 'status' ? { configured: true, settings: { model: 'jev-1.13.0' } }
      : path === 'decisions/start' ? { job: { id: 'fixture-decision', state: 'completed', result: { fixture: true, model: input.model, answers: input.questions.map(q => ({ id: q.id, kind: q.kind })), usage: 'Not measured; fixture only' } } }
      : path === 'coding/preview' ? { job: { id: 'fixture-preview', state: 'completed', result: { status: 'pending', reason: 'Fixture only: no eligible coding agent', costKnown: false, latencyKnown: false } } }
      : path === 'coding/catalog' ? { providers: [], nextCursor: '' }
      : path === 'coding/events' ? { events: [], lastSequence: '0', hasMore: false }
      : path === 'coding/open-surface' ? { sessionId: session.id, surfaceId: 'fixture-surface', focused: true }
      : { session: { ...session, state: path === 'coding/end' ? 'ended' : session.state }, promptId: 'fixture-prompt' }
    response.end(JSON.stringify(result)); return
  }
  response.setHeader('Content-Type', 'text/html')
  response.end('<!doctype html><html data-bd-product="gateway" data-bd-theme="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Jev presentation fixture</title><link rel="stylesheet" href="/tokens.css"><script type="module" src="/fixture.mjs"></script></head><body><aside>FIXTURE ONLY — no Gateway authorization, Typesafe usage or coding execution. <button id="detach">Detach fixture</button></aside><main></main></body></html>')
})
server.listen(0, '127.0.0.1', () => console.log(`Jev presentation fixture: http://127.0.0.1:${server.address().port}`))
