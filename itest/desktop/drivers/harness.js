// The harness every desktop-smoke driver runs on.
//
// The runner concatenates this file with a scenario driver and hands the result to the app
// via TACHI_DEMO_JS, which feeds it to the webview once React has mounted. So a driver
// works on the real DOM with the real event handlers — which is the whole point of driving
// the app instead of the code.
//
// Reporting: smoke.check(label, ok, detail) records an assertion and smoke.finish() posts
// the lot to the runner's loopback sink, where it becomes the run's exit code. The same
// lines are drawn as a banner, so a screenshot explains itself without the runner's output.
//
// Writing a driver: wait for what you need (never fixed sleeps — they are how a smoke test
// becomes flaky), assert, finish. If a wait fails, stop early: `finish()` still reports the
// failure, and the later assertions would only cascade.
const smoke = (() => {
  const SINK = '__SINK__'
  const SCENARIO = '__SCENARIO__'
  // The runner waits longer than this; the watchdog exists so a stuck driver REPORTS
  // (with whatever it managed to assert) instead of leaving a bare timeout behind.
  const BUDGET = 60000

  const lines = []
  const missing = []
  const started = Date.now()
  let error = ''
  let finished = false
  let done = false

  function banner() {
    let el = document.getElementById('smoke-result')
    if (!el) {
      el = document.createElement('div')
      el.id = 'smoke-result'
      el.style.cssText =
        'position:fixed;left:0;right:0;bottom:0;z-index:99999;background:#111;color:#0f0;' +
        'font:12px ui-monospace,SFMono-Regular,Menlo,monospace;padding:5px 9px;white-space:pre-wrap;' +
        'max-height:42%;overflow:auto;border-top:1px solid #333'
      document.body.appendChild(el)
    }
    const bad = lines.filter((l) => !l.ok).length
    const head = (bad === 0 ? '✓ PASS ' : '✗ FAIL ') + SCENARIO + '  (' + lines.length + ' 项)'
    el.textContent = [head].concat(lines.map((l) => (l.ok ? '✓ ' : '✗ ') + l.label + (l.detail ? '  — ' + l.detail : ''))).join('\n')
  }

  function record(label, ok, detail) {
    const line = { label: label, ok: !!ok, detail: detail == null ? '' : String(detail) }
    lines.push(line)
    banner()
    // Stream every line out as it happens: a driver that hangs (or a webview that dies)
    // still shows the runner how far it got, which is the whole diagnosis.
    try {
      fetch(SINK + '/line', { method: 'POST', body: JSON.stringify(line) }).catch(() => {})
    } catch (e) {}
    return !!ok
  }

  // consoleLines: everything the page logged, forwarded so a failure that never reached an
  // assertion is still visible in the run's output.
  const consoleLines = []
  const origError = console.error
  console.error = function () {
    consoleLines.push(Array.prototype.map.call(arguments, String).join(' '))
    origError.apply(console, arguments)
  }

  function post(body) {
    // The console rides in the SAME body: /result is what unblocks the runner, so a second
    // POST with the console lines would race it — and lose (see Result.Console on the Go side).
    body.console = consoleLines
    try {
      fetch(SINK + '/result', { method: 'POST', body: JSON.stringify(body) }).catch(() => {})
    } catch (e) {
      /* the runner will time out; the banner is still on screen */
    }
  }

  function finish() {
    if (finished) return
    finished = true
    done = true
    record('driver 跑到结尾', true, '')
    // The DOM as the verdict found it: the artifact that works everywhere (see the sink).
    try {
      fetch(SINK + '/dom', { method: 'POST', body: document.documentElement.outerHTML }).catch(() => {})
    } catch (e) {}
    post({ scenario: SCENARIO, lines: lines, error: error, done: true, ms: Date.now() - started, missing: missing })
  }

  function fail(label, detail) {
    record(label, false, detail)
    finish()
  }

  // ── DOM helpers ───────────────────────────────────────────────────────────
  const q = (sel) => document.querySelector(sel)
  const qa = (sel) => Array.prototype.slice.call(document.querySelectorAll(sel))
  const text = (sel) => {
    const el = typeof sel === 'string' ? q(sel) : sel
    return el ? el.textContent.replace(/\s+/g, ' ').trim() : ''
  }
  const allText = (sel) => qa(sel).map((e) => e.textContent.replace(/\s+/g, ' ').trim())

  // React inputs ignore `el.value = …`: go through the native setter so onChange fires.
  function type(sel, value) {
    const el = typeof sel === 'string' ? q(sel) : sel
    if (!el) return false
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(el, value)
    el.dispatchEvent(new Event('input', { bubbles: true }))
    return true
  }

  function pick(sel, value) {
    const el = typeof sel === 'string' ? q(sel) : sel
    if (!el) return false
    Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value').set.call(el, value)
    el.dispatchEvent(new Event('change', { bubbles: true }))
    return true
  }

  function click(sel) {
    const el = typeof sel === 'string' ? q(sel) : sel
    if (!el) return false
    if (el.scrollIntoView) el.scrollIntoView({ block: 'center' })
    el.click()
    return true
  }

  function key(sel, k, opts) {
    const el = (typeof sel === 'string' ? q(sel) : sel) || document.activeElement || document.body
    el.dispatchEvent(new KeyboardEvent('keydown', Object.assign({ key: k, bubbles: true, cancelable: true }, opts || {})))
    return el
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

  // waitFor polls until fn() is truthy. On timeout it records a failure and returns null —
  // drivers check for that and stop, so one missing element does not produce twenty
  // misleading follow-up failures.
  async function waitFor(fn, label, timeout) {
    const budget = timeout || 15000
    // Record that we are waiting: a trace of "what was it waiting for" is the first thing
    // a timed-out run needs, and it costs one line.
    record('等待：' + label, true, budget + 'ms')
    const deadline = Date.now() + budget
    while (Date.now() < deadline) {
      try {
        const v = typeof fn === 'function' ? fn() : q(fn)
        if (v) return v
      } catch (e) {
        /* keep polling */
      }
      await sleep(150)
    }
    missing.push(label)
    record(label, false, '等了 ' + budget + 'ms 没出现')
    return null
  }

  // waitText waits until an element matching sel contains sub.
  const waitText = (sel, sub, timeout) =>
    waitFor(
      () => {
        const hit = qa(sel).filter((e) => e.textContent.indexOf(sub) >= 0)
        return hit.length ? hit[hit.length - 1] : null
      },
      '出现「' + sub + '」',
      timeout
    )

  // waitGone waits until nothing matches sel (e.g. the stop button clearing).
  const waitGone = (sel, label, timeout) => waitFor(() => !q(sel), label || '元素消失 ' + sel, timeout)

  // ── Failure paths ─────────────────────────────────────────────────────────
  window.addEventListener('error', (e) => {
    error = e.message || String(e.type)
  })
  window.addEventListener('unhandledrejection', (e) => {
    error = 'unhandled rejection: ' + ((e.reason && e.reason.message) || String(e.reason))
  })
  setTimeout(() => {
    if (finished) return
    record('driver 在预算内完成', false, BUDGET + 'ms 后仍在运行（watchdog）')
    finished = true // report what we have, marked as not-done
    post({ scenario: SCENARIO, lines: lines, error: error, done: false, ms: Date.now() - started, missing: missing })
  }, BUDGET)

  // Announce the load: if the runner never sees even this, the driver never ran (a bad
  // TACHI_DEMO_JS path, a stale instance) rather than failed.
  record('driver 载入', true, SCENARIO)

  return {
    check: record,
    log: (label, detail) => record(label, true, detail),
    eq: (label, got, want) => record(label, got === want, 'got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)),
    fail,
    finish,
    sleep,
    waitFor,
    waitText,
    waitGone,
    q,
    qa,
    text,
    allText,
    type,
    pick,
    click,
    key,
  }
})()
