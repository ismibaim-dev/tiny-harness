// Simulated tiny-harness backend: intercepts fetch('/api/...') and plays
// scripted agent runs over a small fake project. Nothing leaves the page.
(function () {
  const realFetch = window.fetch.bind(window);
  let FAST = false;
  const STOP = new Error('stopped');
  const hex = n => Array.from(crypto.getRandomValues(new Uint8Array(n)), b => b.toString(16).padStart(2, '0')).join('');
  const MAXCTX = 24000;

  const PROFILES = [
    { name: 'claude', type: 'anthropic', model: 'claude-sonnet-5-5', models: ['claude-sonnet-5-5', 'claude-opus-5-5', 'claude-haiku-5-5'] },
    { name: 'gemini', type: 'gemini', model: 'gemini-3.8-flash', models: ['gemini-3.8-flash', 'gemini-4-argon', 'gemini-3.6-flash'] },
    { name: 'deepseek', type: 'openai', model: 'deepseek-flash', models: ['deepseek-flash', 'deepseek-v4-pro'] },
    { name: 'glm', type: 'openai', model: 'glm-4.6', models: ['glm-4.6', 'glm-5'] },
  ];
  const LIVE = {
    claude: ['claude-haiku-5-5', 'claude-opus-5-5', 'claude-sonnet-5-5', 'claude-fable-5-1'],
    gemini: ['gemini-2.5-flash', 'gemini-2.5-pro', 'gemini-3-flash-preview', 'gemini-3.1-flash-lite', 'gemini-3.5-flash-lite', 'gemini-3.6-flash', 'gemini-3.8-flash', 'gemini-4-argon'],
    deepseek: ['deepseek-flash', 'deepseek-v4-pro'],
    glm: ['glm-4.5-air', 'glm-4.6', 'glm-5'],
  };

  const CONVERT_BUGGY = `"""Unit conversions for sensor readings."""


def c_to_f(c: float) -> float:
    return c * 9 / 5 + 23


def kmh_to_ms(kmh: float) -> float:
    return kmh / 3.6
`;
  const WS = {
    default: {
      path: 'C:\\Users\\you\\projects\\weather-api', bug: true, dirty: false, committed: false,
      files: {
        'AGENTS.md': 'Use pytest. Keep functions small and typed.\n',
        'README.md': '# weather-api\nSmall Flask service that serves readings from the rooftop sensor.\n\nRun: `python app.py`\nTest: `python -m pytest -q`\n',
        'app.py': `from flask import Flask, jsonify
from convert import c_to_f

app = Flask(__name__)


@app.get("/reading")
def reading():
    # TODO: cache the last reading instead of hitting the sensor each time
    c = read_sensor()
    return jsonify(celsius=c, fahrenheit=c_to_f(c))


def read_sensor() -> float:
    return 31.4  # stub until the sensor driver is wired in


if __name__ == "__main__":
    app.run(port=5000)
`,
        'convert.py': CONVERT_BUGGY,
        'requirements.txt': 'flask==3.0.3\npytest==8.3.2\n',
        'tests/test_convert.py': `from convert import c_to_f, kmh_to_ms


def test_freezing():
    assert c_to_f(0) == 32


def test_boiling():
    assert c_to_f(100) == 212


def test_wind():
    assert round(kmh_to_ms(36), 2) == 10.0
`,
      },
    },
    site: {
      path: 'C:\\Users\\you\\projects\\portfolio', bug: false, dirty: false, committed: false,
      files: {
        'README.md': '# portfolio\nStatic personal site. Deployed from main.\n',
        'index.html': '<!doctype html>\n<title>Portfolio</title>\n<link rel="stylesheet" href="style.css">\n<h1>Projects</h1>\n<!-- TODO: add the IoT dashboard project -->\n',
        'style.css': 'body { font-family: system-ui; max-width: 60ch; margin: auto; }\n',
      },
    },
  };

  const sessions = {};
  function newSession(id, profile, ws) {
    const s = {
      id: id || hex(4), title: '', profile: PROFILES.some(p => p.name === profile) ? profile : 'claude', model: '',
      mode: 'ask', workspace: WS[ws] ? ws : 'default', events: [], busy: false, live: '', lv: 0,
      usage: { in: 0, out: 0, cache_read: 0 }, ctx: 0, epoch: hex(4), pending: {}, turnAllow: false,
      stopped: false, waiters: [], updated: Date.now() / 1000, asks: [],
    };
    sessions[s.id] = s;
    return s;
  }
  const modelOf = s => s.model || PROFILES.find(p => p.name === s.profile).model;
  const ws = s => WS[s.workspace];

  // ------------------------------------------------------------- timeline
  function wake(s) { const w = s.waiters; s.waiters = []; w.forEach(f => f()); }
  function emit(s, e) { e.n = s.events.length + 1; s.events.push(e); s.updated = Date.now() / 1000; wake(s); }
  const sleep = ms => new Promise(r => setTimeout(r, FAST ? 0 : ms));
  async function pause(s, ms) { await sleep(ms); if (s.stopped) throw STOP; }

  async function say(s, text) {
    await think(s);
    if (!FAST) {
      const parts = text.match(/\S+\s*|\s+/g) || [text];
      for (let i = 0; i < parts.length; i += 2) {
        if (s.stopped) throw STOP;
        s.live += parts[i] + (parts[i + 1] || '');
        s.lv++; wake(s);
        await sleep(28);
      }
    }
    s.live = ''; s.lv++;
    emit(s, { type: 'assistant', text });
    s.usage.out += Math.round(text.length / 3.6);
  }

  async function think(s) {
    if (s.ctx > MAXCTX * 0.85) {
      emit(s, { type: 'status', text: 'compacting' });
      await pause(s, 900);
      emit(s, { type: 'compact', text: summarize(s) });
      s.ctx = 1600;
    }
    await pause(s, 380 + Math.random() * 350);
    s.usage.in += s.ctx + 2400;
    if (s.profile === 'claude') s.usage.cache_read += Math.round(s.ctx * 0.85);
    s.ctx += 900 + Math.round(Math.random() * 700);
  }

  function summarize(s) {
    const asks = s.asks.slice(-6).map(a => '- User asked: ' + a);
    return ['Goals and progress so far:', ...asks,
      ws(s).bug ? '- convert.py still has the +23 offset bug; 2 tests failing.' : '- Fixed c_to_f offset in convert.py (+23 → +32); all 3 tests pass.',
      ws(s).committed ? '- Fix committed on main.' : (ws(s).dirty ? '- convert.py change not committed yet.' : ''),
      '- Workspace: ' + ws(s).path].filter(Boolean).join('\n');
  }

  function needsApproval(s, kind, name, cmd) {
    if (kind === 'read') return false;
    if (s.mode === 'yolo' || s.turnAllow) return false;
    if (name === 'bash' && /^(git status|git log|git diff|pwd|date)( |$)/.test(cmd) && !/[;&|<>`$]/.test(cmd)) return false;
    if (s.mode === 'auto-edit' && (kind === 'edit' || kind === 'net')) return false;
    return true;
  }

  // Runs one tool call; returns {out} or {denied:true}.
  async function tool(s, name, kind, summary, run) {
    await think(s);
    const id = 't_' + hex(4);
    emit(s, { type: 'tool_call', id, name, text: summary });
    await pause(s, 200);
    if (needsApproval(s, kind, name, summary.split('\n')[0])) {
      const ok = await new Promise(res => { s.pending[id] = res; emit(s, { type: 'approval', id, name, text: summary }); });
      delete s.pending[id];
      emit(s, { type: 'approval_done', id, text: ok ? 'allowed' : 'denied' });
      if (s.stopped) { emit(s, { type: 'tool_result', id, text: 'cancelled by user', err: true }); throw STOP; }
      if (!ok) { emit(s, { type: 'tool_result', id, text: 'the user denied this action', err: true }); return { denied: true }; }
    }
    await pause(s, kind === 'exec' ? 900 : kind === 'net' ? 1100 : 250);
    const r = run();
    emit(s, { type: 'tool_result', id, text: r.out, err: !!r.err });
    s.ctx += Math.round(r.out.length / 4);
    return r;
  }

  // ------------------------------------------------------------- fake tools
  const numbered = t => t.replace(/\n$/, '').split('\n').map((l, i) => String(i + 1).padStart(6, ' ') + '\t' + l).join('\n') + '\n';
  function listDir(w) {
    const top = new Map();
    Object.entries(w.files).forEach(([p, c]) => {
      const i = p.indexOf('/');
      if (i > 0) top.set(p.slice(0, i) + '/', null); else top.set(p, c.length);
    });
    return [...top.keys()].sort().map(k => k.endsWith('/') ? k : `${k}  (${top.get(k)} B)`).join('\n');
  }
  function grep(w, re) {
    const out = [];
    Object.entries(w.files).forEach(([p, c]) => c.split('\n').forEach((l, i) => { if (re.test(l)) out.push(`${p}:${i + 1}:${l.trim()}`); }));
    return out.length ? out.join('\n') : '(no matches)';
  }
  function pytest(w) {
    if (!Object.keys(w.files).some(p => p.startsWith('tests/'))) return { out: '\nno tests ran in 0.01s\n[exit code 5]' };
    if (w.bug) return { out: `FF.                                                          [100%]
=========================== FAILURES ===========================
________________________ test_freezing _________________________
    def test_freezing():
>       assert c_to_f(0) == 32
E       assert 23.0 == 32
tests/test_convert.py:5: AssertionError
_________________________ test_boiling _________________________
    def test_boiling():
>       assert c_to_f(100) == 212
E       assert 203.0 == 212
tests/test_convert.py:9: AssertionError
=================== short test summary info ====================
FAILED tests/test_convert.py::test_freezing - assert 23.0 == 32
FAILED tests/test_convert.py::test_boiling - assert 203.0 == 212
2 failed, 1 passed in 0.04s
[exit code 1]` };
    return { out: '...                                                          [100%]\n3 passed in 0.03s' };
  }

  // ------------------------------------------------------------- scripts
  async function explore(s) {
    const w = ws(s);
    await say(s, "I'll look around the project first.");
    await tool(s, 'list_dir', 'read', '.', () => ({ out: listDir(w) }));
    await tool(s, 'read_file', 'read', 'README.md', () => ({ out: numbered(w.files['README.md']) }));
    await tool(s, 'grep', 'read', 'TODO  in  .', () => ({ out: grep(w, /TODO/) }));
    if (s.workspace === 'default') {
      await say(s, "This is **weather-api**, a small Flask service for the rooftop sensor.\n\n" +
        "- `app.py` serves `GET /reading` with Celsius and Fahrenheit\n- `convert.py` has the unit conversions\n" +
        "- `tests/` has 3 pytest tests for the converters\n- `AGENTS.md` asks for pytest and small typed functions\n\n" +
        "One TODO: `app.py` re-reads the sensor on every request instead of caching. Want me to run the tests?");
    } else {
      await say(s, "This is a small static **portfolio** site: `index.html` and `style.css`, no build step and no tests.\n\n" +
        "One TODO in `index.html`: add the IoT dashboard project.");
    }
  }

  async function fixTests(s) {
    const w = ws(s);
    await say(s, 'Running the test suite first.');
    let r = await tool(s, 'bash', 'exec', 'python -m pytest -q', () => pytest(w));
    if (r.denied) return say(s, "OK, I won't run anything. You can run `python -m pytest -q` yourself and paste the output here.");
    if (!w.bug) {
      return say(s, s.workspace === 'default' ? 'All 3 tests pass. Nothing to fix.' : "This workspace has no tests, so there's nothing to fix. Switch to the default workspace to see a failing test.");
    }
    await say(s, 'Two tests fail: `c_to_f(0)` gives 23 and `c_to_f(100)` gives 203. Both are off by 9, so the offset is wrong. Checking the converter.');
    await tool(s, 'read_file', 'read', 'convert.py', () => ({ out: numbered(w.files['convert.py']) }));
    r = await tool(s, 'edit_file', 'edit', 'convert.py\n- return c * 9 / 5 + 23\n+ return c * 9 / 5 + 32', () => {
      w.files['convert.py'] = w.files['convert.py'].replace('+ 23', '+ 32');
      w.bug = false; w.dirty = true;
      return { out: 'edited convert.py (1 replacement(s))' };
    });
    if (r.denied) return say(s, "Understood, I left `convert.py` alone. The fix is to change `+ 23` to `+ 32` on line 5.");
    r = await tool(s, 'bash', 'exec', 'python -m pytest -q', () => pytest(w));
    if (r.denied) return say(s, 'Edit is in place but I did not re-run the tests. `python -m pytest -q` should now show 3 passed.');
    await say(s, 'Fixed. `c_to_f` added 23 instead of 32.\n\n- `convert.py` line 5: `+ 23` → `+ 32`\n- Tests: 3 passed\n\nNot committed yet. Say "commit it" if you want me to.');
  }

  async function git(s) {
    const w = ws(s);
    await tool(s, 'bash', 'exec', 'git status', () => ({ out: w.dirty
      ? "On branch main\nYour branch is up to date with 'origin/main'.\n\nChanges not staged for commit:\n  modified:   convert.py\n"
      : "On branch main\nYour branch is " + (w.committed ? "ahead of 'origin/main' by 1 commit." : "up to date with 'origin/main'.") + "\n\nnothing to commit, working tree clean\n" }));
    await say(s, w.dirty ? 'One uncommitted change: `convert.py` (the temperature fix). Say "commit it" to commit.'
      : w.committed ? 'Working tree is clean and you are 1 commit ahead. Push when ready.' : 'Working tree is clean. `git status` is on the safe list, so it ran without asking.');
  }

  async function commit(s) {
    const w = ws(s);
    if (!w.dirty) return say(s, 'There is nothing to commit right now.');
    const r = await tool(s, 'bash', 'exec', 'git commit -am "Fix Celsius to Fahrenheit offset"', () => {
      w.dirty = false; w.committed = true;
      return { out: '[main 3f2a9c1] Fix Celsius to Fahrenheit offset\n 1 file changed, 1 insertion(+), 1 deletion(-)' };
    });
    if (r.denied) return say(s, 'Not committed.');
    await say(s, 'Committed as `3f2a9c1`. I did not push.');
  }

  async function web(s, text) {
    const url = (text.match(/https?:\/\/\S+/) || ['https://docs.python.org/3/library/unittest.html'])[0];
    const r = await tool(s, 'web_fetch', 'net', url, () => ({ out: 'HTTP 200  text/html; charset=utf-8\n\n' +
      'unittest — Unit testing framework\nSource code: Lib/unittest/__init__.py\n\nThe unittest unit testing framework was originally inspired by JUnit and has a similar flavor as major unit testing frameworks in other languages. It supports test automation, sharing of setup and shutdown code for tests, aggregation of tests into collections, and independence of the tests from the reporting framework.\n\nBasic example\n…\n[truncated; continue with offset=40000]' }));
    if (r.denied) return say(s, 'Skipped the fetch.');
    await say(s, 'Fetched it. Key points:\n\n- `unittest` is the standard-library test framework (JUnit style)\n- Tests are methods named `test_*` on a `TestCase` subclass\n- pytest runs `unittest` tests too, so this project could mix both\n\nIn **ask** mode, `web_fetch` needs approval because a page can contain instructions aimed at the model.');
  }

  async function howItWorks(s) {
    await say(s, 'Here is what happens when you send a message:\n\n' +
      '1. The phone posts your text (and any photos, already shrunk to about 300 KB) to the harness on your PC.\n' +
      '2. The harness sends the conversation and its tool list to the model you picked: Claude, Gemini, DeepSeek or any OpenAI-compatible API.\n' +
      '3. The reply streams back. If the model asks for a tool, the harness runs it on the PC: read files, grep, edit, run a shell command, fetch a page, or call an MCP server.\n' +
      '4. Anything that changes files or runs commands waits for your tap first, unless you chose auto-approve edits.\n' +
      '5. The tool output goes back to the model, and the loop repeats until the model answers without calling a tool.\n\n' +
      'The phone only ever long-polls one small JSON endpoint, so a flaky mobile connection just resumes where it left off. ' +
      'Chats are saved on the PC, and when the history gets big the harness asks the model for a summary and continues from that. ' +
      'The whole thing is one Go program of about 7 MB that idles at roughly 9 MB of RAM.\n\n' +
      'Try tapping **Stop** while a long answer like this one is streaming.');
  }

  async function image(s, imgs) {
    const info = await Promise.all(imgs.map(measure));
    const lines = info.map((m, i) => `- Photo ${i + 1}: ${m.w}×${m.h} JPEG, about ${m.kb} KB after the phone shrank it, mostly ${m.color}`);
    await say(s, 'Got ' + imgs.length + ' image' + (imgs.length > 1 ? 's' : '') + '. In this preview there is no real model to look at it, so here is what the page itself can measure:\n\n' +
      lines.join('\n') + '\n\nWith a real key, the harness sends the image to the model (Claude, Gemini and most OpenAI-compatible vision models accept it).');
  }
  function measure(img) {
    return new Promise(res => {
      const im = new Image();
      im.onload = () => {
        const c = document.createElement('canvas'); c.width = 24; c.height = 24;
        const x = c.getContext('2d'); x.drawImage(im, 0, 0, 24, 24);
        const d = x.getImageData(0, 0, 24, 24).data; let r = 0, g = 0, b = 0;
        for (let i = 0; i < d.length; i += 4) { r += d[i]; g += d[i + 1]; b += d[i + 2]; }
        const n = d.length / 4;
        res({ w: im.naturalWidth, h: im.naturalHeight, kb: Math.round(img.data.length * 0.75 / 1024), color: colorName(r / n, g / n, b / n) });
      };
      im.onerror = () => res({ w: '?', h: '?', kb: Math.round(img.data.length * 0.75 / 1024), color: 'unknown colours' });
      im.src = 'data:' + img.media + ';base64,' + img.data;
    });
  }
  function colorName(r, g, b) {
    const mx = Math.max(r, g, b), mn = Math.min(r, g, b), l = (mx + mn) / 2;
    if (mx - mn < 24) return l > 200 ? 'white or very light' : l < 55 ? 'black or very dark' : 'grey';
    let h;
    if (mx === r) h = ((g - b) / (mx - mn)) * 60; else if (mx === g) h = (2 + (b - r) / (mx - mn)) * 60; else h = (4 + (r - g) / (mx - mn)) * 60;
    h = (h + 360) % 360;
    const name = h < 15 ? 'red' : h < 45 ? 'orange or brown' : h < 70 ? 'yellow' : h < 160 ? 'green' : h < 200 ? 'teal' : h < 255 ? 'blue' : h < 290 ? 'purple' : h < 340 ? 'pink' : 'red';
    return (l < 80 ? 'dark ' : l > 190 ? 'light ' : '') + name;
  }

  async function fallback(s, text) {
    await say(s, `This preview runs a simulated model (pretending to be ${s.profile} · ${modelOf(s)}), so it only follows a few scripts. ` +
      'Try one of the suggestions under the chat: fix the failing test, look around the project, `git status`, read a docs page, attach a photo, or ask how the harness works.');
  }

  async function runTurn(s, text, imgs) {
    s.busy = true; s.stopped = false; s.turnAllow = false;
    emit(s, { type: 'status', text: 'busy' });
    const t = text.toLowerCase();
    try {
      if (imgs.length) await image(s, imgs);
      else if (/\bcommit\b/.test(t)) await commit(s);
      else if (/test|fix|bug|fail/.test(t)) await fixTests(s);
      else if (/https?:|docs|fetch|web/.test(t)) await web(s, text);
      else if (/\bgit\b/.test(t)) await git(s);
      else if (/how (does|do)|explain|work\??$|architecture/.test(t)) await howItWorks(s);
      else if (/what'?s in|project|folder|files|look around|overview|todo/.test(t)) await explore(s);
      else await fallback(s, text);
    } catch (e) {
      if (e !== STOP) throw e;
      const partial = s.live;
      s.live = ''; s.lv++;
      if (partial.trim()) emit(s, { type: 'assistant', text: partial + ' …' });
      emit(s, { type: 'error', text: 'stopped', err: true });
    } finally {
      s.busy = false; s.live = ''; s.lv++;
      emit(s, { type: 'status', text: 'idle' });
    }
  }

  // ------------------------------------------------------------- HTTP layer
  const json = (v, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } });
  const fail = (status, msg) => new Response(msg, { status });

  function snap(s, q) {
    let since = +q.get('since') || 0; const lv = +(q.get('lv') ?? -1);
    let reset = false;
    if (q.get('epoch') !== s.epoch || since < 0 || since > s.events.length) { since = 0; reset = true; }
    const events = s.events.slice(since);
    return {
      changed: reset || events.length > 0 || lv !== s.lv,
      body: { events, next: s.events.length, epoch: s.epoch, reset, busy: s.busy, title: s.title, profile: s.profile,
        model: modelOf(s), mode: s.mode, workspace: s.workspace, live: s.live, lv: s.lv, usage: { ...s.usage }, ctx: s.ctx },
    };
  }

  async function handle(method, url, body, signal) {
    const q = url.searchParams, p = url.pathname;
    if (p === '/api/info') return json({ profiles: PROFILES, default: 'claude', workspaces: Object.keys(WS),
      paths: Object.fromEntries(Object.entries(WS).map(([k, v]) => [k, v.path])), os: 'windows', tools: 9, mcp_tools: 2, version: '0.2.0', max_ctx: MAXCTX });
    if (p === '/api/sessions') return json(Object.values(sessions).filter(s => s.events.length)
      .sort((a, b) => b.updated - a.updated).map(s => ({ id: s.id, title: s.title, profile: s.profile, workspace: s.workspace, updated: s.updated })));
    if (p === '/api/models') { await sleep(500); return json(LIVE[q.get('profile')] || []); }
    if (p === '/api/session') { const s = sessions[body.id] || newSession('', body.profile, body.workspace); return json({ id: s.id }); }
    const s = sessions[(body && body.id) || q.get('id')];
    if (!s) return fail(404, 'no such session');
    switch (p) {
      case '/api/poll': {
        let r = snap(s, q);
        const wait = Math.min(+q.get('wait') || 0, 25);
        if (!r.changed && wait > 0) {
          await new Promise((res, rej) => {
            const t = setTimeout(res, wait * 1000);
            s.waiters.push(() => { clearTimeout(t); res(); });
            if (signal) signal.addEventListener('abort', () => { clearTimeout(t); rej(new DOMException('aborted', 'AbortError')); });
          });
          await sleep(40);
          r = snap(s, q);
        }
        return json(r.body);
      }
      case '/api/send': {
        if (s.busy) return fail(409, 'the agent is still working on the previous message');
        const text = (body.text || '').trim() || '(see image)';
        const imgs = body.images || [];
        if (!s.title) s.title = text.slice(0, 60);
        s.asks.push(text.slice(0, 80));
        emit(s, { type: 'user', text, imgs: imgs.length });
        runTurn(s, text, imgs);
        return json({ ok: true });
      }
      case '/api/approve': {
        const f = s.pending[body.call_id];
        if (!f) return fail(404, 'no such pending approval');
        if (body.allow && body.all) s.turnAllow = true;
        f(!!body.allow);
        return json({ ok: true });
      }
      case '/api/stop': {
        if (s.busy) { s.stopped = true; Object.values(s.pending).forEach(f => f(false)); wake(s); }
        return json({ ok: true });
      }
      case '/api/config': {
        if (s.busy && (body.profile || body.model != null || body.workspace)) return fail(409, 'wait until the agent is idle to switch model or workspace');
        if (body.mode && !['ask', 'auto-edit'].includes(body.mode)) return fail(400, 'mode must be ask or auto-edit');
        if (body.profile && body.profile !== s.profile) { s.profile = body.profile; s.model = ''; }
        if (body.model != null) s.model = body.model === PROFILES.find(x => x.name === s.profile).model ? '' : body.model.trim();
        if (body.mode) s.mode = body.mode;
        if (body.workspace && WS[body.workspace]) s.workspace = body.workspace;
        wake(s);
        return json({ ok: true });
      }
      case '/api/compact': {
        if (s.busy) return fail(409, 'the agent is still working on the previous message');
        if (s.events.filter(e => e.type === 'user').length < 1) return fail(409, 'nothing to compact yet');
        s.busy = true; emit(s, { type: 'status', text: 'busy' });
        (async () => {
          await sleep(1200);
          s.usage.in += s.ctx; s.usage.out += 180;
          emit(s, { type: 'compact', text: summarize(s) });
          s.ctx = 1600; s.busy = false;
          emit(s, { type: 'status', text: 'idle' });
        })();
        return json({ ok: true });
      }
      case '/api/delete': { s.stopped = true; delete sessions[s.id]; return json({ ok: true }); }
    }
    return fail(404, 'not found');
  }

  window.fetch = async (input, opts = {}) => {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (!url.pathname.startsWith('/api/')) return realFetch(input, opts);
    const auth = (opts.headers && (opts.headers.Authorization || opts.headers.authorization)) || '';
    if (!auth.replace('Bearer ', '').trim()) return fail(401, 'unauthorized');
    await ready;
    let body = null;
    try { body = opts.body ? JSON.parse(opts.body) : null; } catch (e) { return fail(400, 'bad request'); }
    return handle((opts.method || 'GET').toUpperCase(), url, body, opts.signal);
  };

  // Seed one finished chat so the first screen shows a real run.
  const ready = (async () => {
    FAST = true;
    const s = newSession('seed0001', 'claude', 'default');
    s.title = "What's in this project?";
    s.asks.push(s.title);
    emit(s, { type: 'user', text: s.title });
    await runTurn(s, s.title, []);
    s.events = s.events.filter(e => e.type !== 'status');
    s.events.forEach((e, i) => { e.n = i + 1; });
    FAST = false;
  })();
})();
