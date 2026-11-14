#!/usr/bin/env python3
"""Build a self-contained, searchable Wiki. No scripts or assets from a CDN."""
from pathlib import Path, PurePosixPath
import html
import base64
import mimetypes
import posixpath
import re
import markdown

ROOT = Path(__file__).resolve().parents[1]
REPO_URL = 'https://github.com/chenlz22550/NetPulse'
PAGES = [
    ('index', '项目介绍', 'wiki/index.md'),
    ('getting-started', '安装与首次运行', 'wiki/getting-started.md'),
    ('configuration', '拓扑与多机房', 'wiki/configuration.md'),
    ('alerts', '监控与告警', 'wiki/alerts.md'),
    ('discovery', '发现与端口定位', 'wiki/discovery.md'),
    ('ai-mcp', 'AI 诊断与 MCP', 'wiki/ai-mcp.md'),
    ('security', '安全与故障排查', 'wiki/security.md'),
    ('development', '开发、测试与发布', 'wiki/development.md'),
    ('topology', '拓扑字段参考', 'docs/topology-fields.md'),
    ('plugins', '插件开发参考', 'docs/plugin-development.md'),
]
SOURCE_TO_SLUG = {src: slug for slug, _, src in PAGES}

CSS = '''
:root{color-scheme:light dark;--bg:#f5f3eb;--panel:#fffdf7;--ink:#222820;--muted:#5e695e;--line:#d6d9cd;--accent:#286046;--code:#e8ecdf}
@media(prefers-color-scheme:dark){:root:not([data-theme=light]){--bg:#141b17;--panel:#1a231d;--ink:#e4eadd;--muted:#a4b4a6;--line:#36463a;--accent:#9ed5ae;--code:#26362b}}
:root[data-theme=dark]{color-scheme:dark;--bg:#141b17;--panel:#1a231d;--ink:#e4eadd;--muted:#a4b4a6;--line:#36463a;--accent:#9ed5ae;--code:#26362b}
:root[data-theme=light]{color-scheme:light;--bg:#f5f3eb;--panel:#fffdf7;--ink:#222820;--muted:#5e695e;--line:#d6d9cd;--accent:#286046;--code:#e8ecdf}
*{box-sizing:border-box}html,body{max-width:100%;overflow-x:clip}body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.85 "PingFang SC","Microsoft YaHei",sans-serif}
a{color:var(--accent);text-underline-offset:4px}
header{padding:20px 5vw;display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;border-bottom:1px solid var(--line);gap:12px 16px}
.brand{font-weight:800;font-size:22px;text-decoration:none;letter-spacing:1px}.brand small{font:12px monospace;letter-spacing:3px;margin-left:10px;color:var(--muted)}
.links{display:flex;flex-wrap:wrap;align-items:center;gap:12px 18px;font-size:14px}
.shell{max-width:1400px;margin:auto;display:grid;grid-template-columns:250px minmax(0,1fr)}
aside{padding:32px 24px;position:sticky;top:0;align-self:start;max-height:100vh;overflow:auto;border-right:1px solid var(--line)}
.label{font:12px monospace;letter-spacing:3px;color:var(--muted);margin-bottom:20px}
.search-box{display:none}.js .search-box{display:block}
.sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
input{width:100%;min-width:0;padding:12px;margin-bottom:22px;border:1px solid var(--line);border-radius:0;background:var(--panel);color:var(--ink);font:inherit}
nav a{display:block;padding:7px 10px;margin:3px 0;font-size:14px;text-decoration:none;border-left:2px solid transparent}
nav a[aria-current=page]{border-color:var(--accent);background:var(--code);font-weight:bold}
main{min-width:0;padding:46px clamp(18px,5vw,72px);background:var(--panel);min-height:85vh}
section{max-width:830px;min-width:0}
section img{display:block;max-width:100%;height:auto;border:1px solid var(--line);margin:18px 0;cursor:zoom-in}
section a:has(>img){display:block}
.js section[data-page]{display:none}.js section[data-page].active{display:block}
h1{font-size:clamp(28px,5vw,48px);letter-spacing:-1px;line-height:1.3;margin:8px 0 32px;font-weight:700}
h2{font-size:23px;margin:38px 0 14px;padding-top:12px;border-top:1px solid var(--line)}
h3{font-size:18px}p,li{overflow-wrap:anywhere}
pre{max-width:100%;overflow-x:auto;padding:20px;background:var(--code);border-left:3px solid var(--accent);font:13px/1.8 ui-monospace,monospace}
code{background:var(--code);padding:2px 5px;font-size:.88em;overflow-wrap:anywhere}
pre code{padding:0;white-space:pre;overflow-wrap:normal}
.table-wrap{max-width:100%;overflow-x:auto;margin:16px 0}
table{border-collapse:collapse;width:100%;font-size:14px}
td,th{padding:10px 10px;border:1px solid var(--line);text-align:left;overflow-wrap:break-word;vertical-align:top}
th{background:var(--code)}
blockquote{border-left:3px solid var(--accent);padding-left:18px;margin-left:0;color:var(--muted)}
footer{padding:26px 5vw;border-top:1px solid var(--line);font-size:12px;color:var(--muted)}
[hidden]{display:none!important}
.result{display:block;text-decoration:none;border-bottom:1px solid var(--line);padding:18px 0}
.result strong{display:block;font-size:20px}.result span{color:var(--muted);font-size:14px;overflow-wrap:anywhere}
button{font:inherit;padding:5px 10px;border:1px solid var(--line);background:var(--panel);color:var(--ink);cursor:pointer}
#theme-btn{display:none}.js #theme-btn{display:inline-block}
:focus-visible{outline:2px solid var(--accent);outline-offset:3px}
.kicker{font:12px monospace;color:var(--muted);letter-spacing:3px;margin-bottom:24px}
.nojs-note{padding:12px 16px;margin-bottom:24px;border-left:3px solid var(--accent);background:var(--code);font-size:14px}
@media(max-width:700px){
.shell{grid-template-columns:minmax(0,1fr)}
aside{position:static;border-right:0;border-bottom:1px solid var(--line);padding:20px;max-height:none}
nav{display:flex;flex-wrap:wrap;gap:2px}nav a{font-size:12px;padding:5px 8px}
input{margin-bottom:10px}main{padding:28px 18px}header{padding:18px}
.brand small{display:block;margin:0}.label{margin-bottom:12px}
table{font-size:12px}td,th{padding:8px 6px}
}
'''

HEAD_JS = '''
(function(){
  document.documentElement.classList.add('js');
  try{var t=localStorage.getItem('netpulse-wiki-theme');if(t==='light'||t==='dark')document.documentElement.dataset.theme=t;}catch(e){}
})();
'''

JS = '''
const pages=[...document.querySelectorAll('section[data-page]')];
const pageIds=new Set(pages.map(p=>p.id));
const search=document.querySelector('#search');
const results=document.querySelector('#results');
const themeBtn=document.querySelector('#theme-btn');

function currentId(){
  const id=location.hash.slice(1)||'index';
  return pageIds.has(id)?id:'index';
}
function show(){
  const activeId=currentId();
  pages.forEach(p=>p.classList.toggle('active',p.id===activeId));
  results.hidden=true;
  if(search.value)search.value='';
  document.querySelectorAll('nav a').forEach(a=>{
    if(a.hash.slice(1)===activeId)a.setAttribute('aria-current','page');
    else a.removeAttribute('aria-current');
  });
  const active=pages.find(p=>p.id===activeId);
  document.title=(active?.dataset.title||'Wiki')+' · 网脉 NetPulse';
}
function runSearch(){
  const q=search.value.trim().toLowerCase();
  if(!q){show();return;}
  pages.forEach(p=>p.classList.remove('active'));
  results.hidden=false;
  results.replaceChildren();
  const title=document.createElement('h1');
  title.textContent='搜索结果';
  results.append(title);
  let count=0;
  for(const p of pages){
    const clone=p.cloneNode(true);
    clone.querySelector('.kicker')?.remove();
    const text=(p.dataset.title+' '+clone.textContent).replace(/\\s+/g,' ').trim();
    const i=text.toLowerCase().indexOf(q);
    if(i<0)continue;
    count++;
    const a=document.createElement('a');
    a.className='result';
    a.href='#'+p.id;
    const b=document.createElement('strong');
    b.textContent=p.dataset.title;
    const s=document.createElement('span');
    const start=Math.max(0,i-45);
    const end=Math.min(text.length,i+120);
    s.textContent=(start>0?'…':'')+text.slice(start,end)+(end<text.length?'…':'');
    a.append(b,s);
    results.append(a);
  }
  if(!count){
    const p=document.createElement('p');
    p.textContent='没有找到相关内容，试试“告警”“安装”或“端口”。';
    results.append(p);
  }
}

addEventListener('hashchange',()=>{show();document.querySelector('main').scrollIntoView({block:'start'});});
document.addEventListener('click',e=>{
  const a=e.target.closest('a[href^="#"]');
  if(!a)return;
  const target=a.getAttribute('href').slice(1)||'index';
  if(!pageIds.has(target))return;
  const cur=location.hash.slice(1)||'index';
  if(cur===target){
    e.preventDefault();
    show();
    document.querySelector('main').scrollIntoView({block:'start'});
  }
});
search.addEventListener('input',runSearch);
search.addEventListener('search',runSearch);
search.addEventListener('keydown',e=>{if(e.key==='Escape'){search.value='';show();search.blur();}});

if(themeBtn){
  const media=matchMedia('(prefers-color-scheme: dark)');
  function currentTheme(){
    return document.documentElement.dataset.theme||(media.matches?'dark':'light');
  }
  function syncThemeLabel(){
    themeBtn.textContent=currentTheme()==='dark'?'浅色模式':'深色模式';
  }
  themeBtn.addEventListener('click',()=>{
    const next=currentTheme()==='dark'?'light':'dark';
    document.documentElement.dataset.theme=next;
    try{localStorage.setItem('netpulse-wiki-theme',next);}catch(e){}
    syncThemeLabel();
  });
  media.addEventListener('change',syncThemeLabel);
  syncThemeLabel();
}
show();
'''


def resolve_link(source: str, m: re.Match) -> str:
    target = html.unescape(m.group(1))
    if target.startswith(('http:', 'https:', 'mailto:', '#')):
        return m.group(0)
    base, sep, frag = target.partition('#')
    path_part, qsep, query = base.partition('?')
    joined = (PurePosixPath(source).parent / path_part).as_posix()
    norm = posixpath.normpath(joined)
    if norm in SOURCE_TO_SLUG:
        return f'href="#{SOURCE_TO_SLUG[norm]}"'
    if norm == '.' or norm.startswith('..'):
        return m.group(0)
    kind = 'tree' if (ROOT / norm).is_dir() else 'blob'
    suffix = (qsep + query if qsep else '') + (sep + frag if sep else '')
    return f'href="{REPO_URL}/{kind}/main/{html.escape(norm, quote=True)}{html.escape(suffix, quote=True)}"'


def embed_image(source: str, match: re.Match) -> str:
    """Inline repository images so the built Wiki also works offline."""
    target = html.unescape(match.group(1))
    if target.startswith(('data:', 'http:', 'https:')):
        return match.group(0)
    path = (ROOT / PurePosixPath(source).parent / target).resolve()
    if not path.is_relative_to(ROOT):
        raise ValueError(f'Image is outside the repository: {target}')
    mime = mimetypes.guess_type(path.name)[0]
    if mime not in ('image/webp', 'image/png', 'image/jpeg', 'image/gif'):
        raise ValueError(f'Unsupported image format: {target}')
    encoded = base64.b64encode(path.read_bytes()).decode('ascii')
    return f'src="data:{mime};base64,{encoded}"'


nav = []
sections = []
for slug, title, source in PAGES:
    text = (ROOT / source).read_text(encoding='utf-8')
    body = markdown.markdown(text, extensions=['tables', 'fenced_code', 'sane_lists'])
    body = re.sub(r'(<img[^>]+src="([^"]+)"[^>]*>)', r'<a href="\2" target="_blank" rel="noopener" aria-label="查看原图">\1</a>', body)
    body = re.sub(r'src="([^"]+)"', lambda m, s=source: embed_image(s, m), body)
    body = re.sub(r'href="([^"]+)"', lambda m, s=source: resolve_link(s, m), body)
    body = body.replace('<table>', '<div class="table-wrap"><table>').replace('</table>', '</table></div>')
    nav.append(f'<a href="#{slug}">{html.escape(title)}</a>')
    sections.append(
        f'<section data-page id="{slug}" data-title="{html.escape(title)}">'
        f'<div class="kicker">NETPULSE / KNOWLEDGE BASE</div>{body}</section>'
    )

page = (
    '<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">'
    '<meta name="viewport" content="width=device-width,initial-scale=1">'
    '<meta name="description" content="网脉 NetPulse 官方 Wiki：安装配置、网络监控、端口定位、AI 诊断及插件开发。">'
    '<title>网脉 NetPulse · Wiki</title>'
    f'<script>{HEAD_JS}</script><style>{CSS}</style></head><body>'
    '<header><a class="brand" href="#index">网脉 <small>NETPULSE / WIKI</small></a>'
    '<div class="links">'
    f'<a href="{REPO_URL}">GitHub ↗</a>'
    f'<a href="{REPO_URL}/releases">下载发行版 ↗</a>'
    '<button id="theme-btn" type="button" aria-label="切换明暗主题">深色模式</button>'
    '</div></header>'
    '<div class="shell"><aside><div class="label">网络运维 / 使用手册</div>'
    '<div class="search-box"><label for="search" class="sr-only">搜索 Wiki</label>'
    '<input id="search" type="search" placeholder="搜索 Wiki…" aria-label="搜索 Wiki"></div>'
    f'<nav aria-label="Wiki 目录">{"".join(nav)}</nav></aside>'
    f'<main><noscript><div class="nojs-note">当前未启用 JavaScript，无法使用交互搜索；以下已展开全部正文，点击目录可跳转。</div></noscript>'
    f'{"".join(sections)}<div id="results" aria-live="polite" hidden></div></main></div>'
    '<footer>网脉 NetPulse · 看清网络连接，及时发现异常。 / MIT · 离线可读，无第三方追踪</footer>'
    f'<script>{JS}</script></body></html>'
)

out = ROOT / 'dist/wiki'
out.mkdir(parents=True, exist_ok=True)
(out / 'index.html').write_text(page, encoding='utf-8')
(out / '.nojekyll').write_text('', encoding='utf-8')
print(f'Wiki built: {len(PAGES)} pages → dist/wiki/index.html')
