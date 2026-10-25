import re, sys

src = open('/home/claude/tinyharness/web/index.html').read()
sim = open('/home/claude/preview/sim.js').read()

def once(s, old, new):
    n = s.count(old)
    if n != 1:
        sys.exit(f'expected 1 match, got {n}: {old[:70]!r}')
    return s.replace(old, new)

css = re.search(r'<style>(.*?)</style>', src, re.S).group(1)
body = re.search(r'<body>(.*?)<script>', src, re.S).group(1)
js = re.search(r'<script>(.*?)</script>', src, re.S).group(1)

# --- theme tokens: light on :root, dark guarded both ways (artifact contract)
css = once(css,
  ':root{--bg:#111418;--fg:#e6e8eb;--mut:#8b93a1;--card:#1a1f26;--line:#2a313b;--acc:#6aa9ff;--ok:#3ecf8e;--bad:#ff6b6b;--warn:#f5b54a;--user:#243248;--code:#0c0f13;--onacc:#0b1220}\n'
  '@media (prefers-color-scheme:light){:root{--bg:#f6f7f9;--fg:#15181d;--mut:#667085;--card:#fff;--line:#dde1e7;--acc:#1f6feb;--ok:#138a5a;--bad:#d0342c;--warn:#b7791f;--user:#dbe8ff;--code:#eef0f3;--onacc:#fff}}',
  ':root{--bg:#f6f7f9;--fg:#15181d;--mut:#667085;--card:#fff;--line:#dde1e7;--acc:#1f6feb;--ok:#138a5a;--bad:#d0342c;--warn:#b7791f;--user:#dbe8ff;--code:#eef0f3;--demo:#fff6e0;--demofg:#7a5410;--onacc:#fff}\n'
  '@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){--bg:#111418;--fg:#e6e8eb;--mut:#8b93a1;--card:#1a1f26;--line:#2a313b;--acc:#6aa9ff;--ok:#3ecf8e;--bad:#ff6b6b;--warn:#f5b54a;--user:#243248;--code:#0c0f13;--demo:#2a2312;--demofg:#f2cf86;--onacc:#0b1220;color-scheme:dark}}\n'
  ':root[data-theme="dark"]{--bg:#111418;--fg:#e6e8eb;--mut:#8b93a1;--card:#1a1f26;--line:#2a313b;--acc:#6aa9ff;--ok:#3ecf8e;--bad:#ff6b6b;--warn:#f5b54a;--user:#243248;--code:#0c0f13;--demo:#2a2312;--demofg:#f2cf86;--onacc:#0b1220;color-scheme:dark}')
css = once(css, 'body{display:flex;flex-direction:column;height:100dvh}', 'body{display:flex;flex-direction:column;height:100%}')
gut = 'padding-left:max(16px,env(safe-area-inset-left,0px));padding-right:max(16px,env(safe-area-inset-right,0px))'
css = once(css, 'header{display:flex;gap:8px;align-items:center;padding:calc(8px + env(safe-area-inset-top)) 12px 8px;',
                f'header{{display:flex;gap:8px;align-items:center;padding-block:8px;{gut};')
css = once(css, '#log{flex:1;overflow-y:auto;padding:12px;', f'#log{{flex:1;overflow-y:auto;padding-block:12px;{gut};')
css = once(css, 'form{display:flex;flex-direction:column;gap:6px;padding:8px 12px calc(8px + env(safe-area-inset-bottom));',
                f'form{{display:flex;flex-direction:column;gap:6px;padding-block:8px;{gut};')
css = once(css, '.drawer{position:absolute;left:0;top:0;bottom:0;width:min(86vw,360px);background:var(--card);padding:calc(12px + env(safe-area-inset-top)) 12px 12px;',
                '.drawer{position:absolute;left:0;top:0;bottom:0;width:min(86vw,360px);background:var(--card);padding:calc(12px + env(safe-area-inset-top,0px)) 16px 12px max(16px,env(safe-area-inset-left,0px));')
css += f'''
#demo{{background:var(--demo);color:var(--demofg);font-size:13px;line-height:1.35;padding-block:7px;{gut};border-bottom:1px solid var(--line)}}
#chips{{display:flex;gap:6px;overflow-x:auto;scrollbar-width:none;padding-bottom:2px}}
#chips::-webkit-scrollbar{{display:none}}
#chips button{{flex:none;font-size:13px;padding:6px 11px;border-radius:999px;background:var(--bg);color:var(--fg)}}
button:focus-visible,select:focus-visible,textarea:focus-visible,input:focus-visible{{outline:2px solid var(--acc);outline-offset:1px}}
@media (prefers-reduced-motion:reduce){{.dot.busy{{animation:none}}}}
'''

# --- body: banner + suggestion chips
body = once(body, '<div id="log" class="hide"></div>',
  '<div id="demo" class="hide">Preview with a simulated model. Nothing runs on a PC and no API key is used. Tap a suggestion below to try it.</div>\n<div id="log" class="hide"></div>')
body = once(body, '<div id="thumbs"></div>',
  '<div id="chips">'
  '<button type="button" data-t="Fix the failing test">Fix the failing test</button>'
  '<button type="button" data-t="git status">git status</button>'
  '<button type="button" data-t="commit it">Commit it</button>'
  '<button type="button" data-photo="1">Attach a photo</button>'
  '<button type="button" data-t="Read https://docs.python.org/3/library/unittest.html">Read a docs page</button>'
  '<button type="button" data-t="How does this harness work?">How does it work?</button>'
  '<button type="button" data-t="What\'s in this project?">What\'s in this project?</button>'
  '</div>\n<div id="thumbs"></div>')
body = once(body, "Paste the access token printed by the server (or in <code>~/.tiny-harness/token</code>).",
  "Paste the access token printed by the server (or in <code>~/.tiny-harness/token</code>). In this preview any text works.")

# --- script tweaks for the preview
js = once(js, "let token=ls.get('th_token'), sid=ls.get('th_sid');", "let token='demo', sid='seed0001';")
js = once(js, "['#hdr','#log','#form'].forEach(s=>$(s).classList.add('hide'));", "['#hdr','#demo','#log','#form'].forEach(s=>$(s).classList.add('hide'));")
js = once(js, "['#hdr','#log','#form'].forEach(s=>$(s).classList.remove('hide')); }", "['#hdr','#demo','#log','#form'].forEach(s=>$(s).classList.remove('hide')); }")
# confirm() is blocked in the artifact viewer: delete needs a second tap instead
js = once(js, "const del=el('button',null,'✕'); del.onclick=async()=>{ if(!confirm('Delete this chat?')) return;",
              "const del=el('button',null,'✕'); del.onclick=async()=>{ if(del.textContent!=='Delete?'){ del.textContent='Delete?'; return; }")
js = once(js, "\nstart();\n", """
document.querySelectorAll('#chips button').forEach(b=>b.onclick=()=>{
  if(busy) return;
  if(b.dataset.photo){ $('#attach').click(); return; }
  ta.value=b.dataset.t; grow(); $('#form').requestSubmit();
});
start();
""")

out = ('<title>Tiny Harness</title>\n<meta name="claude:viewport-fit-sides" content="cover">\n'
       f'<style>{css}</style>\n{body}<script>\n{sim}\n</script>\n<script>{js}</script>\n')
open('/home/claude/preview/tiny-harness.html', 'w').write(out)
print('ok', len(out), 'bytes')
