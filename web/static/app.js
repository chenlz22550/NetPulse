/* netPing 管理页面主程序：路由、数据同步、各页面渲染。 */
(function () {
  'use strict';
  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  const esc = Charts.esc;
  const fmtMs = Charts.fmtMs;

  const STATE_TXT = { ok: '正常', slow: '延迟高', loss: '丢包', down: '离线', unknown: '等待', paused: '暂停', disabled: '未监控' };
  const EVENT_TXT = {
    down: ['离线', 'down'], still_down: ['仍离线', 'down'], recover: ['恢复', 'ok'], loss: ['丢包', 'loss'],
    loss_recover: ['丢包恢复', 'ok'], slow: ['延迟高', 'slow'], slow_recover: ['延迟恢复', 'ok'], test: ['测试', 'unknown'],
  };
  const PALETTE = ['#2e7d32', '#6a3d9a', '#1565c0', '#e08a00', '#00897b', '#c2185b', '#8d5524', '#5d7a1f', '#9e9e9e',
    '#ad1457', '#37474f', '#7cb342', '#d84315', '#283593', '#00838f', '#6d4c41', '#b0a000', '#4e342e'];

  const S = {
    topo: null, rev: 0, status: {}, summary: null, paused: false, meta: null, changedOnDisk: false,
    sel: null, search: '', filter: 'all', vendor: null, vendorColors: {}, connected: false,
    listSort: { key: 'ip', dir: 1 }, listFilter: '', listState: '',
  };
  let view = null;        // 当前拓扑视图
  let page = null;        // 当前页面 { name, update() }

  // ---------------------------------------------------------------------------
  // 通用
  async function api(method, url, body, opts) {
    opts = opts || {};
    const init = { method, headers: {} };
    if (body !== undefined) {
      if (opts.text) { init.body = body; init.headers['Content-Type'] = 'text/plain; charset=utf-8'; }
      else { init.body = JSON.stringify(body); init.headers['Content-Type'] = 'application/json'; }
    }
    const r = await fetch(url, init);
    const ct = r.headers.get('Content-Type') || '';
    const data = ct.includes('json') ? await r.json() : await r.text();
    if (r.status === 401 && data && data.login_required) {
      toLogin();
      throw new Error('登录已过期，请重新登录');
    }
    if (!r.ok) {
      const e = new Error((data && data.error) || ('HTTP ' + r.status));
      e.problems = data && data.problems;
      throw e;
    }
    return data;
  }
  // 登录过期或密码被修改：跳到登录页，登录后回到当前页面
  function toLogin() { location.replace('/login.html' + location.hash); }
  let toastTimer = 0;
  // action: { label, fn } 在提示中显示一个按钮（如“撤销”），提示停留更久
  function toast(msg, isErr, action) {
    let el = $('#toast');
    if (!el) { el = document.createElement('div'); el.id = 'toast'; document.body.appendChild(el); }
    el.className = 'toast' + (isErr ? ' err' : '');
    el.textContent = msg;
    if (action) {
      const b = document.createElement('button');
      b.className = 'toast-act';
      b.textContent = action.label;
      b.onclick = () => { el.classList.add('hidden'); action.fn(); };
      el.appendChild(b);
    }
    el.classList.remove('hidden');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.add('hidden'), action ? 10000 : isErr ? 8000 : 2500);
  }
  async function copyText(text) {
    try { await navigator.clipboard.writeText(text); toast('已复制'); }
    catch (e) {
      const ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); toast('已复制'); } catch (_) { toast('复制失败，请手动选择复制', true); }
      ta.remove();
    }
  }
  function errMsg(e) { return e.message + (e.problems ? '\n- ' + e.problems.join('\n- ') : ''); }
  function fmtAgo(ms) {
    if (!ms) return '-';
    let s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 60) return s + '秒';
    if (s < 3600) return Math.floor(s / 60) + '分' + (s % 60) + '秒';
    if (s < 86400) return Math.floor(s / 3600) + '小时' + Math.floor(s % 3600 / 60) + '分';
    return Math.floor(s / 86400) + '天' + Math.floor(s % 86400 / 3600) + '小时';
  }
  function pct(v) { return v == null ? '-' : (v >= 10 || v === 0 ? Math.round(v) : v.toFixed(1)) + '%'; }
  function lossCls(v, warn) { return v >= 100 ? 'v-down' : v >= (warn || 20) ? 'v-warn' : v > 0 ? 'v-warn' : ''; }
  const WD = ['', '一', '二', '三', '四', '五', '六', '日'];
  function describeSchedule(sch) {
    if (!sch || !sch.enabled) return '全天候监控';
    return (sch.windows || []).map(w => {
      const d = w.days && w.days.length && w.days.length < 7 ? '周' + w.days.map(x => WD[x]).join('、') : '每天';
      return `${d} ${w.start}-${w.end}`;
    }).join('；');
  }
  function stateBadge(st) { const s = st || 'disabled'; return `<span class="badge ${s}">${STATE_TXT[s] || s}</span>`; }

  function swName(id) { const s = S.topo && S.topo.switches.find(x => x.id === id); return s ? (s.name || s.id) : id; }
  function attachLabel(n) {
    if (!n.attach || !n.attach.switch) return '未定位';
    return swName(n.attach.switch) + (n.attach.port ? ' ' + n.attach.port : '');
  }
  function findTarget(id) {
    if (!S.topo) return null;
    const n = S.topo.nodes.find(x => x.id === id);
    if (n) return { kind: 'node', obj: n };
    const s = S.topo.switches.find(x => x.id === id);
    if (s) return { kind: 'switch', obj: s };
    return null;
  }
  function computeVendorColors() {
    const cnt = {};
    S.topo.nodes.forEach(n => { const v = n.vendor || '未知'; cnt[v] = (cnt[v] || 0) + 1; });
    const vendors = Object.keys(cnt).sort((a, b) => cnt[b] - cnt[a] || a.localeCompare(b));
    S.vendorColors = {};
    vendors.forEach((v, i) => { S.vendorColors[v] = PALETTE[i % PALETTE.length]; });
    S.vendorCounts = cnt;
    S.vendorList = vendors;
  }
  const vendorColor = v => S.vendorColors[v || '未知'] || '#999';

  // 端口表达式展开：Gi0/1-24, 1-26, Te0/25-30
  function expandPorts(text) {
    const out = [];
    text.split(/[\s,，;；]+/).filter(Boolean).forEach(tok => {
      const m = tok.match(/^(.*?)(\d+)-(\d+)$/);
      if (m && +m[3] >= +m[2] && +m[3] - +m[2] < 512) {
        for (let i = +m[2]; i <= +m[3]; i++) out.push(m[1] + i);
      } else out.push(tok);
    });
    return Array.from(new Set(out));
  }

  // ---------------------------------------------------------------------------
  // 数据同步
  async function loadTopology() {
    const d = await api('GET', 'api/topology');
    S.topo = d.topology;
    S.rev = d.rev;
    S.changedOnDisk = d.changed_on_disk;
    computeVendorColors();
    const site = S.topo.site || {};
    $('#siteTitle').innerHTML = `${esc(site.name || 'netPing')}<span class="slash">/</span>网络拓扑`;
    $('#siteSub').textContent = [site.subnet, site.gateway ? '网关 ' + site.gateway : '', `${S.topo.switches.length} 交换机 · ${S.topo.nodes.length} 节点`].filter(Boolean).join(' · ');
    document.title = (site.name || 'netPing') + ' · netPing';
    if (S.sel && !findSel(S.sel)) S.sel = null;
    if (page && page.onTopology) page.onTopology();
  }
  function findSel(sel) {
    if (!sel || !S.topo) return null;
    if (sel.kind === 'node') return S.topo.nodes.find(x => x.id === sel.id);
    if (sel.kind === 'switch') return S.topo.switches.find(x => x.id === sel.id);
    if (sel.kind === 'link') return S.topo.links.find(x => x.id === sel.id);
    if (sel.kind.startsWith('new')) return {};
    return null;
  }

  function connectStream() {
    const es = new EventSource('api/stream');
    es.onmessage = ev => {
      const d = JSON.parse(ev.data);
      S.connected = true;
      S.status = d.statuses || {};
      S.summary = d.summary;
      S.paused = d.paused;
      S.manualPaused = d.manual_paused;
      S.pauseReason = d.pause_reason;
      S.schedule = d.schedule;
      S.changedOnDisk = d.changed_on_disk;
      updateKPIs();
      if (d.rev !== S.rev && !(view && view.isDragging())) {
        S.rev = d.rev;
        loadTopology().catch(e => toast(errMsg(e), true));
      }
      if (page && page.update) page.update();
    };
    es.onerror = () => {
      S.connected = false; updateKPIs();
      fetch('api/auth/state', { cache: 'no-store' }).then(r => r.json()).then(st => { if (!st.logged_in) toLogin(); }).catch(() => {});
    };
  }

  function updateKPIs() {
    const sm = S.summary;
    if (!S.connected) { $('#kRun').textContent = '断开'; $('#kRun').className = 'v down'; return; }
    if (!sm) return;
    const b = sm.by_state || {};
    $('#kOk').textContent = b.ok || 0;
    $('#kWarn').textContent = (b.slow || 0) + (b.loss || 0);
    $('#kDown').textContent = b.down || 0;
    $('#kGw').textContent = sm.gateway_rtt != null ? fmtMs(sm.gateway_rtt) : (sm.gateway_ip ? '离线' : '-');
    const txt = { manual: '已暂停', schedule: '非监控时段' }[S.pauseReason] || '运行中';
    $('#kRun').textContent = txt;
    $('#kRun').className = 'v ' + (S.paused ? 'warn' : 'ok');
    $('#kPause').title = (S.schedule ? '监控时段：' + S.schedule + '\n' : '') + '点击手动暂停/恢复监控';
  }

  $('#kPause').addEventListener('click', async () => {
    const to = !S.manualPaused;
    if (to && !confirm('手动暂停所有 ping 监控？暂停期间不会产生告警。')) return;
    if (!to && S.pauseReason === 'schedule') toast('当前不在监控时段，需在「设置 → 监控时段」中调整');
    try { await api('POST', 'api/monitor/pause', { paused: to }); S.manualPaused = to; }
    catch (e) { toast(errMsg(e), true); }
  });

  // ---------------------------------------------------------------------------
  // 路由
  function route() {
    const h = location.hash.replace(/^#\/?/, '') || 'topo';
    const [name, arg] = h.split('/');
    $$('nav.tabs a').forEach(a => a.classList.toggle('active', a.dataset.tab === (name === 'node' ? 'list' : name)));
    const app = $('#app');
    if (page && page.destroy) page.destroy();
    view = null;
    page = null;
    app.innerHTML = '';
    switch (name) {
      case 'list': page = listPage(app); break;
      case 'node': page = nodePage(app, decodeURIComponent(arg || '')); break;
      case 'alerts': page = alertsPage(app); break;
      case 'settings': page = settingsPage(app); break;
      case 'mcp': page = mcpPage(app); break;
      case 'discovery': page = discoveryPage(app); break;
      case 'locate': page = locatePage(app); break;
      case 'guide': page = guidePage(app); break;
      case 'ai': page = aiPage(app, decodeURIComponent(arg || '')); break;
      default: page = topoPage(app);
    }
  }
  window.addEventListener('hashchange', route);

  // ---------------------------------------------------------------------------
  // 教程页：静态使用指南
  function guidePage(app) {
    const go = (tab, text) => `<a href="#/${tab}" class="g-go">${text} →</a>`;
    const kbd = k => `<kbd>${k}</kbd>`;
    const SECTIONS = [
      ['start', '快速上手', `
        <ol class="g-steps">
          <li><b>运行程序。</b>Windows：把 <code>netPing.exe</code> 放到固定目录（如 <code>D:\\netPing\\</code>）双击运行，不需要管理员权限；
            macOS：把 <code>netPing.app</code> 拖到「应用程序」后打开，首次提示“无法验证开发者”时右键 →「打开」。
            程序常驻在任务栏托盘 / 菜单栏，图标颜色：<span class="g-dot ok"></span>正常 <span class="g-dot warn"></span>丢包或延迟高 <span class="g-dot down"></span>有设备离线 <span class="g-dot idle"></span>暂停。</li>
          <li><b>设置登录密码。</b>第一次打开管理页面时，在<b>运行 netPing 的电脑上</b>设置密码（至少 6 位）。之后每次打开都要输入密码，登录后这个浏览器 30 天内免登录。</li>
          <li><b>核对机房和设备。</b>在「设置 → 机房信息」填写网段和默认网关；到「拓扑」页检查交换机、端口和设备是否和实际一致。设备多时可以用「扫描发现」自动添加。</li>
          <li><b>配置告警。</b>在「设置」里填写企业微信群机器人的 Webhook 并发送测试消息；再到「监控列表」给需要告警的设备打勾。<b>设备默认只监控、不告警。</b></li>
          <li><b>按需开启更多功能：</b>「端口定位」自动查出设备插在哪个交换机口；「MCP」让 AI 助手用自然语言查询和维护。</li>
        </ol>
        <div class="g-note">其他电脑访问：浏览器打开 <code>http://服务器IP:8090/</code>。打不开时检查防火墙是否放行 8090 端口，以及 config.yaml 中 <code>listen</code> 是否为 <code>0.0.0.0:8090</code>。</div>`],

      ['topo', '看懂拓扑图', `
        <h4>交换机与网口</h4>
        <ul>
          <li>交换机竖着画，网口按实物排列：<b>每排左边奇数口、右边偶数口</b>，每 8 个口一组，光口（SFP）单独放在下面。</li>
          <li>网口指示灯：<span class="g-dot ok"></span>设备在线 <span class="g-dot warn"></span>丢包 / 延迟高 <span class="g-dot down"></span>离线，级联口用主题色表示。鼠标停在网口上可看到端口名和下面挂了哪些设备。</li>
          <li>交换机底部显示已用口数，如 <code>17/30 口</code>。</li>
        </ul>
        <h4>设备卡片</h4>
        <ul>
          <li>卡片显示 IP、厂商（或名称）、MAC 和实时延迟。<b>红框</b>为离线，<b>橙框</b>为丢包或延迟高，左侧色条代表厂商（右侧「厂商图例」可点击筛选）。</li>
          <li>卡片上方的标签：默认网关、关键、存疑（斜纹）、DHCP、自动发现、跳板机、<span class="g-tag">告警</span>（已开启企业微信告警）。</li>
          <li><b>虚线框</b>表示一个端口后面有多台设备（口后还有傻瓜交换机、AP 等）；左下的虚线区域是“未定位”的设备，还不知道接在哪个口。</li>
        </ul>
        <h4>连线</h4>
        <ul>
          <li>每个网口从卡扣朝向的一侧出线：和卡片同侧的口直接连过去；另一侧的口会绕过交换机底部再连到卡片。</li>
          <li>交换机之间的级联链路是粗虚线，点一下可以编辑两端端口和备注。</li>
          <li>手动拖动过的卡片用细虚线连接。</li>
        </ul>
        <h4>操作</h4>
        <ul>
          <li>拖动空白处平移，滚轮缩放，拖动卡片或交换机标题调整位置（自动保存）。快捷键：${kbd('/')} 检索、${kbd('Esc')} 清除、${kbd('F')} 适配窗口。</li>
          <li><b>双击</b>卡片查看这台设备的延迟曲线；单击后在右侧「详情」里编辑。</li>
          <li><b>快速接线</b>：先点交换机上的网口，再点设备卡片，设备就接到这个口并排到它旁边（可撤销）；按住 ${kbd('Shift')} 点卡片可以连续把多台接到同一个口，${kbd('Esc')} 取消。</li>
          <li>工具栏「+ 交换机 / + 节点 / + 链路」新增；「自动排布」把拖乱的卡片恢复为按端口排列，未定位的设备按 IP 整齐排在下方，10 秒内可点「撤销」。</li>
          <li>交换机的编辑表单里可以设置端口列表、终端卡片显示在左侧还是右侧、每个端口的卡片列数、端口物理位置对照。</li>
          <li>右侧「检索 / 筛选」可按 IP、MAC、厂商、端口、名称搜索，或筛选异常、已开告警、多 MAC 端口等。</li>
        </ul>
        ${go('topo', '打开拓扑')}`],

      ['list', '监控列表与延迟曲线', `
        <ul>
          <li>「监控列表」列出所有地址的当前延迟、1 分钟平均、抖动、1 分钟 / 5 分钟 / 1 小时丢包率和最近 60 次的迷你曲线，点表头排序，可按状态和关键字筛选。</li>
          <li>「告警」列勾选即开启这台设备的企业微信告警；先筛选再点「筛选结果开启告警」可批量开启。</li>
          <li>点设备进入详情：延迟曲线（平均值、最小~最大区间、阈值线）和丢包柱状图，范围从 15 分钟到 30 天。</li>
          <li>历史数据按分钟汇总保存，默认保留 30 天（「设置 → 默认 Ping 参数」可改）。</li>
        </ul>
        ${go('list', '打开监控列表')}`],

      ['alert', '企业微信告警', `
        <ol class="g-steps">
          <li>在企业微信群里「添加群机器人」，复制 Webhook 地址。</li>
          <li>填到「设置 → 企业微信机器人告警」，勾选「启用告警推送」，保存后点「发送测试消息」，确认群里收到。</li>
          <li>给需要告警的设备开启告警：「监控列表」勾选，或在拓扑页设备表单中勾选「推送企业微信告警」。</li>
        </ol>
        <table class="g-table">
          <tr><th>规则</th><th>默认</th></tr>
          <tr><td>离线</td><td>连续 5 次 ping 不通；连续 3 次成功算恢复，恢复消息附带离线时长</td></tr>
          <tr><td>丢包</td><td>1 分钟丢包率 ≥ 20% 并持续 2 分钟</td></tr>
          <tr><td>延迟高</td><td>1 分钟平均延迟 ≥ 100ms 并持续 3 分钟</td></tr>
          <tr><td>重复提醒</td><td>故障未恢复时每 30 分钟提醒一次</td></tr>
          <tr><td>合并</td><td>10 秒内的事件合成一条；同一交换机下多台同时离线合并为“交换机故障”</td></tr>
        </table>
        <div class="g-note">这些数值都可以在「设置」里改，单台设备的延迟 / 丢包阈值可以在它的表单里单独设置。服务器不能直接上网时填写 HTTP 代理；老系统证书过旧导致发送失败时，可临时勾选「跳过 HTTPS 证书校验」。「告警」页可以查看历史告警和发送结果。</div>
        ${go('settings', '去设置告警')}`],

      ['scan', '扫描发现：自动添加设备、跟踪 IP 变化', `
        <ul>
          <li><b>扫描任务</b>：每个任务包含一个或多个网段和扫描间隔，按时自动扫描。默认有一个“机房网段”任务。也可以「临时扫描」任意网段，不保存为任务。</li>
          <li><b>自动添加</b>：扫到拓扑里没有的设备会加为“未定位”节点（带「自动发现」标签）；关掉自动添加时，它们列在「未登记设备」里，可以逐个或一键添加。任务里还可以给新设备加标签、同时开启告警。</li>
          <li><b>按 MAC 跟踪 IP</b>：DHCP 设备换了 IP 时，自动把这台设备的 IP 改成新地址，历史曲线、告警开关和接线位置都保留。设备的「IP 分配方式」设为“静态锁定”时只提示、不自动改。</li>
          <li><b>防误报</b>：可跟踪的设备突然 ping 不通时会立即重新扫描，找到新 IP 就更新，不会误报离线。</li>
          <li><b>主机名</b>：每次扫描都会查询在线设备的主机名（Windows 计算机名、苹果 / Linux / 打印机等的 .local 名、DNS 名），记录在设备的「主机名」里；没有填写名称的设备在拓扑图上显示主机名，也可以按主机名检索。手机等设备通常不公布主机名。</li>
          <li>扫描时读取电脑的 ARP 表获得 MAC，所以<b>只能看到和运行 netPing 的电脑在同一网段（VLAN）的设备 MAC</b>；其他网段只能发现在线。</li>
        </ul>
        <div class="g-note">macOS 上第一次扫描时，如果系统询问“允许 netPing 查找本地网络上的设备”，请选择<b>允许</b>，否则读不到 MAC。</div>
        ${go('discovery', '打开扫描发现')}`],

      ['locate', '端口定位：查出设备插在哪个口', `
        <ul>
          <li>通过 SNMP（v2c 只读）读取交换机的 MAC 地址表，按设备 MAC 找到它实际插在哪台交换机的哪个端口。</li>
          <li>交换机上开启只读团体名即可，如锐捷：<code>snmp-server community 团体名 ro</code>。在「端口定位」页给每台交换机填写读取地址（管理 IP）和团体名，点「测试」确认能读到。</li>
          <li>“未定位”的设备查到端口后自动挂上去；已定位但端口变了的设备只列入「待确认」，点「采纳」后才修改。</li>
          <li>拓扑里登记的交换机间链路端口会当作级联口，不算设备位置。<b>请把交换机之间的连线登记完整</b>，否则级联口可能被误判。</li>
          <li>交换机表单里可填写「端口位置对照」（如 <code>12: A03 机柜 配线架 2-12</code>），定位时自动填入设备的物理位置。</li>
          <li>默认每 15 分钟定位一次；扫描发现添加新设备后也会立即定位。</li>
        </ul>
        ${go('locate', '打开端口定位')}`],

      ['schedule', '监控时段与暂停', `
        <ul>
          <li>「设置 → 监控时段」可以只在指定时间监控，如工作日 08:00–18:00。时段外自动暂停 ping、告警和定时扫描，托盘图标变灰。</li>
          <li>结束时间早于开始时间表示跨过午夜，如 22:00 至 06:00。</li>
          <li>顶栏右上角「监控」按钮、托盘菜单都可以手动暂停 / 恢复，和监控时段互不影响。</li>
        </ul>`],

      ['yaml', '设置与拓扑文件', `
        <ul>
          <li>所有交换机、设备、链路保存在 <code>topology.yaml</code>，文件开头有全部字段说明。「设置」页可以在线编辑、导入、下载这个文件，保存前会自动校验。</li>
          <li>用文本编辑器或 AI 改了文件后，在「设置」里点「从磁盘重新加载」生效。</li>
          <li>程序配置在 <code>config.yaml</code>：监听地址、告警、扫描、定位、MCP 等。网页上能改的项目改了立即生效；监听地址需要重启程序。</li>
          <li>数据目录：Windows 为 exe 所在目录，macOS 为 <code>~/Library/Application Support/netPing</code>，可从托盘菜单「打开数据目录」进入。<code>data/</code> 是历史数据和告警记录，<code>logs/</code> 是日志。</li>
          <li><b>迁移到新机房</b>：把新机房交换机的 MAC 表和 ARP 表交给 AI，按 <code>docs/topology-fields.md</code> 里的提示词生成新的 topology.yaml；接入 MCP 后也可以直接让 AI 生成并写入。</li>
        </ul>
        ${go('settings', '打开设置')}`],

      ['aidiag', 'AI 诊断：让大模型帮你看问题', `
        <ul>
          <li>「AI 诊断」页用大模型（默认 DeepSeek 的 deepseek-flash）结合 netPing 的实时数据和历史曲线诊断网络问题，给出<b>结论、依据和排查步骤</b>。</li>
          <li>第一次使用：在页面右侧填写 API Key（在 DeepSeek 开放平台创建），点「测试连接」确认正常。服务器不能直接上网时，可填写 HTTP 代理（留空则使用告警设置里的代理）。</li>
          <li>点「一键诊断」「最近 1 小时」「分析告警」等快捷按钮，或直接描述现象，如“3 楼打印机经常连不上”。可以接着追问。</li>
          <li>在拓扑页的设备详情、设备曲线页点「AI 诊断」，会直接诊断这一台设备。</li>
          <li>AI 会按需查询设备列表、历史曲线、告警记录、扫描与端口定位结果，回答上方会显示它查了什么。AI <b>只能查询，不能修改</b>任何配置。</li>
          <li>「深度思考」更慢、更贵，复杂问题时再开。每次诊断下方显示用时、查询次数和 token 用量。</li>
        </ul>
        <div class="g-note">诊断时会把相关设备的 IP、MAC、名称和延迟丢包数据发送给模型服务商。API Key 保存在 config.yaml，网页和 MCP 都不会返回它。</div>
        ${go('ai', '打开 AI 诊断')}`],

      ['mcp', 'AI 接入（MCP）', `
        <ul>
          <li>netPing 内置 MCP 接口，可以接入 Claude Code、Cursor、WorkBuddy、VS Code、Claude Desktop、Cherry Studio 等 AI 客户端，用自然语言查延迟丢包、分析历史、维护设备和告警。</li>
          <li>接入步骤：在「MCP」页点「生成」访问令牌并保存 → 选择你用的客户端，复制配置片段 → 粘贴到客户端的 MCP 配置里。点「测试连接」可确认接口可用。</li>
          <li>只想让 AI 查询、不允许修改时，勾选「只读模式」。AI 的每次修改都会记录在日志里。</li>
          <li>页面下方有示例问法，比如“现在机房有哪些设备离线或丢包？”、“扫描一下网段，把新发现的打印机放到接入交换机 Port 7”。</li>
        </ul>
        ${go('mcp', '打开 MCP')}`],

      ['account', '登录与安全', `
        <ul>
          <li>打开管理页面需要输入密码，登录后这个浏览器 30 天内免登录；netPing 重启不影响。</li>
          <li>「设置 → 登录密码」可以修改密码或退出登录。修改密码后，其他已登录的浏览器都需要重新登录。</li>
          <li>同一来源连续输错 5 次密码会被暂时锁定（1 分钟起，最长 15 分钟）。</li>
          <li><b>忘记密码</b>：删除 config.yaml 中 <code>auth</code> 下的 <code>password_hash</code> 和 <code>password</code> 两行，重启 netPing，再在运行 netPing 的电脑上打开网页重新设置。</li>
        </ul>`],

      ['faq', '常见问题', `
        <dl class="g-faq">
          <dt>所有设备都显示离线？</dt>
          <dd>先在运行 netPing 的电脑上用命令行 <code>ping</code> 一个设备 IP，确认网络能通。电脑不在机房网段、或设备防火墙禁 ping 时都会显示离线。</dd>
          <dt>群里收不到告警？</dt>
          <dd>先点「发送测试消息」看报错；确认设备已开启告警（卡片上有「告警」标签）；检查是否处于暂停或非监控时段。服务器不能上网时需要配置代理。</dd>
          <dt>扫描发现读不到 MAC、不能跟踪 IP？</dt>
          <dd>只有和运行 netPing 的电脑在同一网段的设备才能读到 MAC。macOS 还需要在「系统设置 → 隐私与安全性 → 本地网络」里允许 netPing。</dd>
          <dt>卡片位置乱了？</dt>
          <dd>点拓扑页的「自动排布」，按端口重新排列；不满意 10 秒内点「撤销」。</dd>
          <dt>端口定位把设备放到了级联口上？</dt>
          <dd>到拓扑页用「+ 链路」把交换机之间的连线补全，再到「端口定位」重新定位。</dd>
          <dt>升级后页面显示不正常？</dt>
          <dd>按 ${kbd('Ctrl')}+${kbd('F5')}（Mac 上 ${kbd('Cmd')}+${kbd('Shift')}+${kbd('R')}）强制刷新。</dd>
        </dl>`],
    ];
    app.innerHTML = `<div class="page pad"><div class="guide">
      <nav class="g-toc"><div class="g-toc-h">使用指南</div>${SECTIONS.map(([id, t], i) => `<a data-g="${id}"><span class="no">${String(i + 1).padStart(2, '0')}</span>${t.split('：')[0]}</a>`).join('')}</nav>
      <div class="g-body">${SECTIONS.map(([id, t, body], i) => `<section class="panel" id="g-${id}"><div class="ph"><span class="no">${i + 1}</span>${t}</div><div class="pb">${body}</div></section>`).join('')}</div>
    </div></div>`;
    // 目录点击滚动到对应章节（页面路由使用 #，不能直接用锚点）
    $$('.g-toc a', app).forEach(a => a.onclick = () => {
      const el = $('#g-' + a.dataset.g);
      if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
    return {};
  }

  // ---------------------------------------------------------------------------
  // AI 诊断页
  // 轻量 Markdown 渲染：先转义再处理标题、列表、粗体、行内代码、代码块、表格
  function mdLite(src) {
    const inline = t => esc(t)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
    const lines = String(src || '').replace(/\r/g, '').split('\n');
    let html = '', list = null, i = 0;
    const closeList = () => { if (list) { html += `</${list}>`; list = null; } };
    while (i < lines.length) {
      const l = lines[i];
      if (/^```/.test(l)) {
        closeList();
        const buf = [];
        i++;
        while (i < lines.length && !/^```/.test(lines[i])) buf.push(lines[i++]);
        i++;
        html += `<pre class="ai-code">${esc(buf.join('\n'))}</pre>`;
        continue;
      }
      if (/^\s*\|.*\|\s*$/.test(l) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
        closeList();
        const row = r => r.trim().replace(/^\||\|$/g, '').split('|').map(c => c.trim());
        let t = '<table class="g-table"><tr>' + row(l).map(c => `<th>${inline(c)}</th>`).join('') + '</tr>';
        i += 2;
        while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) t += '<tr>' + row(lines[i++]).map(c => `<td>${inline(c)}</td>`).join('') + '</tr>';
        html += t + '</table>';
        continue;
      }
      let m;
      if ((m = l.match(/^(#{1,4})\s+(.*)$/))) { closeList(); html += `<h4>${inline(m[2])}</h4>`; }
      else if ((m = l.match(/^\s*[-*•]\s+(.*)$/))) { if (list !== 'ul') { closeList(); html += '<ul>'; list = 'ul'; } html += `<li>${inline(m[1])}</li>`; }
      else if ((m = l.match(/^\s*\d+[.、)]\s+(.*)$/))) { if (list !== 'ol') { closeList(); html += '<ol>'; list = 'ol'; } html += `<li>${inline(m[1])}</li>`; }
      else if (!l.trim()) { closeList(); }
      else { closeList(); html += `<p>${inline(l)}</p>`; }
      i++;
    }
    closeList();
    return html;
  }

  function aiPage(app, focusID) {
    const store = () => sessionStorage.setItem('np_ai', JSON.stringify(chat.slice(-40)));
    let chat = [];
    try { chat = JSON.parse(sessionStorage.getItem('np_ai') || '[]'); } catch (e) { chat = []; }
    let settings = null, busy = null;
    const QUICK = [
      ['一键诊断', '帮我全面诊断一下当前网络：有没有离线、丢包、延迟高的设备？原因可能是什么？需要我做什么？'],
      ['最近 1 小时', '最近 1 小时哪些设备最不稳定？按严重程度排序，给出可能原因。'],
      ['网关与核心', '网关和核心交换机现在是否正常？对比最近 24 小时有没有变差。'],
      ['分析告警', '分析最近的告警记录：有没有反复离线的设备或同时发生的故障？可能的根因是什么？'],
      ['找公共故障点', '现在异常的设备是不是同一台交换机、同一个端口或同一条上联下面的？帮我找出最可能的公共故障点。'],
      ['掉线规律', '最近 7 天哪些设备最不稳定？有没有每天固定时段掉线、或者多台设备同时掉线的情况？'],
      ['夜间巡检', '按夜间巡检的口径总结一下网络健康状况，列出需要明天处理的事项。'],
    ];
    app.innerHTML = `<div class="page pad"><div class="ai">
      <div class="panel ai-main"><div class="ph">AI 诊断<span class="muted" style="font-weight:normal" id="aiModel"></span>
        <span class="r btns" style="margin:0"><button class="btn small" id="aiClear">清空对话</button></span></div>
        <div class="ai-log" id="aiLog"></div>
        <div class="ai-quick" id="aiQuick">${QUICK.map(([t], i) => `<button class="chip" data-q="${i}">${t}</button>`).join('')}</div>
        <form class="ai-input" id="aiForm">
          <textarea name="q" rows="2" placeholder="描述你遇到的问题，如：3 楼打印机经常连不上；或者问：接入交换机 12 口下面的设备为什么丢包？（Enter 发送，Shift+Enter 换行）"></textarea>
          <button class="btn primary" type="submit" id="aiSend">发送</button>
        </form>
      </div>
      <div class="ai-side">
        <div class="panel"><div class="ph">模型设置<span class="r" id="aiState"></span></div><div class="pb"><form id="aiSet"></form></div></div>
        <div class="panel"><div class="ph">说明</div><div class="pb"><ul class="help" style="margin:0;padding-left:18px">
          <li>AI 会先看当前概况，再按需查询设备列表、历史曲线、告警记录、扫描与端口定位结果，最后给出结论和排查步骤。</li>
          <li>AI 只能<b>查询</b>和<b>现场探测</b>（ping、TCP 端口、ARP、路由追踪、DNS、通过 SNMP 读交换机端口状态），不能修改任何配置。</li>
          <li>开启「回答前自检」后，AI 写完回答会对照数据逐条核对，缺证据时再实测；核对通过显示 ✓，发现问题会直接给出修正后的回答（可展开查看初稿）。</li>
          <li>诊断时会把相关设备的 IP、MAC、名称、延迟丢包数据发送给模型服务商（默认 DeepSeek）。</li>
          <li>在拓扑页或设备详情页点「AI 诊断」可以直接诊断某一台设备。</li>
        </ul></div></div>
      </div>
    </div></div>`;

    const log = $('#aiLog');
    const msgHTML = (m, i) => {
      if (m.role === 'user') return `<div class="ai-msg user" data-i="${i}"><div class="ai-bubble">${esc(m.content).replace(/\n/g, '<br>')}</div></div>`;
      const tools = (m.tools || []).map(t => `<span class="ai-tool${t.phase === 'review' ? ' review' : ''}" title="${esc(t.args || '')}">${t.phase === 'review' ? '核实' : '查询'} · ${esc(t.title || t.name)}${t.args && t.args !== '{}' ? ' <span class="muted">' + esc(t.args.slice(0, 60)) + '</span>' : ''}</span>`).join('');
      const ck = m.check;
      let check = '';
      if (ck && ck.result === 'reviewing') check = '';
      else if (ck && ck.result === 'pass') check = `<div class="ai-check ok">✓ 已自检${ck.calls ? `（追加核实 ${ck.calls} 次）` : ''}${ck.text ? '：' + esc(ck.text) : ''}</div>`;
      else if (ck && ck.result === 'revised') check = `<div class="ai-check fix">✎ 自检发现问题，已修正回答${ck.calls ? `（追加核实 ${ck.calls} 次）` : ''}</div>`;
      else if (ck && (ck.result === 'failed' || ck.result === 'skipped') && ck.text) check = `<div class="ai-check warn">${esc(ck.text)}</div>`;
      const draft = m.draft && ck && ck.result === 'revised' ? `<details class="ai-draft"><summary>查看修正前的初稿</summary><div class="ai-md">${mdLite(m.draft)}</div></details>` : '';
      const meta = m.meta ? `<div class="ai-meta">${esc(m.meta)}</div>` : '';
      const status = m.status ? `<div class="ai-status"><span class="ai-spin"></span>${esc(m.status)}</div>` : '';
      const err = m.error ? `<div class="ai-err">${esc(m.error)}</div>` : '';
      return `<div class="ai-msg bot" data-i="${i}">${tools ? `<div class="ai-tools">${tools}</div>` : ''}<div class="ai-md">${mdLite(m.content)}</div>${check}${draft}${status}${err}${meta}</div>`;
    };
    const renderLog = () => {
      log.innerHTML = chat.length ? chat.map(msgHTML).join('') :
        `<div class="ai-empty"><b>有什么网络问题？</b><br>点下面的快捷按钮，或直接描述现象。AI 会结合 netPing 的实时数据和历史曲线给出判断。</div>`;
      log.scrollTop = log.scrollHeight;
    };
    const renderOne = i => {
      const el = $(`.ai-msg[data-i="${i}"]`, log);
      const stick = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
      if (el) el.outerHTML = msgHTML(chat[i], i); else renderLog();
      if (stick) log.scrollTop = log.scrollHeight;
    };
    const setBusy = b => {
      busy = b;
      $('#aiSend').textContent = b ? '停止' : '发送';
      $('#aiSend').classList.toggle('danger', !!b);
      $$('#aiQuick .chip').forEach(x => { x.disabled = !!b; });
    };

    async function ask(question, focus) {
      if (busy) return;
      if (!settings || !settings.ready) { toast('请先在右侧设置 API Key', true); $('#aiKeyUnlock, #aiSet [name=api_key]')?.focus(); return; }
      const history = chat.filter(m => !m.error && m.content).map(m => ({ role: m.role, content: m.content }));
      chat.push({ role: 'user', content: question });
      const bot = { role: 'assistant', content: '', tools: [], status: '正在连接…' };
      chat.push(bot);
      const bi = chat.length - 1;
      renderLog();
      const ctl = new AbortController();
      setBusy(ctl);
      let pendingBreak = false;
      try {
        const r = await fetch('api/ai/diagnose', {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, signal: ctl.signal,
          body: JSON.stringify({ messages: history.concat([{ role: 'user', content: question }]), focus: focus || '' }),
        });
        if (!r.ok) {
          const d = await r.json().catch(() => ({}));
          if (r.status === 401 && d.login_required) { toLogin(); return; }
          throw new Error(d.error || 'HTTP ' + r.status);
        }
        const rd = r.body.getReader(), dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { value, done } = await rd.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let k;
          while ((k = buf.indexOf('\n')) >= 0) {
            const line = buf.slice(0, k).trim();
            buf = buf.slice(k + 1);
            if (!line) continue;
            let ev;
            try { ev = JSON.parse(line); } catch (e) { continue; }
            if (ev.t === 'status') bot.status = ev.text;
            else if (ev.t === 'delta') {
              if (pendingBreak && bot.content && !bot.content.endsWith('\n')) bot.content += '\n\n';
              pendingBreak = false;
              bot.content += ev.text; bot.status = '';
            } else if (ev.t === 'tool') { bot.tools.push(ev); bot.status = '正在查询 ' + (ev.title || ev.name) + '…'; }
            else if (ev.t === 'break') pendingBreak = true;
            else if (ev.t === 'review') { bot.check = { result: 'reviewing' }; bot.status = '正在自检…'; }
            else if (ev.t === 'revise') { bot.draft = bot.content; bot.content = ''; pendingBreak = false; bot.status = '正在修正回答…'; }
            else if (ev.t === 'review_end') {
              bot.check = { result: ev.result, text: ev.text || '', calls: ev.calls || 0 };
              if (ev.restore && bot.draft) { bot.content = bot.draft; bot.draft = ''; }
              bot.status = '';
            }
            else if (ev.t === 'error') { bot.error = ev.error; bot.status = ''; }
            else if (ev.t === 'done') {
              bot.status = '';
              const u = ev.usage || {};
              const rv = { pass: ' · 自检通过', revised: ' · 自检已修正', failed: ' · 自检未完成' }[ev.review] || '';
              bot.meta = `${ev.model} · ${(ev.ms / 1000).toFixed(1)} 秒 · 查询 ${ev.tools} 次 · ${u.total_tokens || 0} tokens${rv}`;
            }
            renderOne(bi);
          }
        }
        if (bot.check && bot.check.result === 'reviewing') bot.check = null;
        if (!bot.content && bot.draft) { bot.content = bot.draft; bot.draft = ''; }
        if (!bot.content && !bot.error) bot.error = '模型没有返回内容，请重试。';
      } catch (e) {
        if (bot.check && bot.check.result === 'reviewing') bot.check = null;
        if (!bot.content && bot.draft) { bot.content = bot.draft; bot.draft = ''; }
        if (e.name === 'AbortError') bot.error = bot.content ? '' : '已停止';
        else bot.error = errMsg(e);
        if (bot.content && e.name === 'AbortError') bot.meta = '已停止';
      } finally {
        bot.status = '';
        setBusy(null);
        renderOne(bi);
        store();
      }
    }

    const form = $('#aiForm');
    form.onsubmit = ev => {
      ev.preventDefault();
      if (busy) { busy.abort(); return; }
      const q = form.elements.q.value.trim();
      if (!q) return;
      form.elements.q.value = '';
      ask(q);
    };
    form.elements.q.addEventListener('keydown', ev => {
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) { ev.preventDefault(); form.requestSubmit(); }
    });
    $$('#aiQuick .chip').forEach(b => b.onclick = () => ask(QUICK[+b.dataset.q][1]));
    $('#aiClear').onclick = () => { if (busy) busy.abort(); chat = []; store(); renderLog(); };

    function drawSettings() {
      const a = settings;
      const PROTOCOLS = [
        ['openai', 'OpenAI Chat Completions', 'https://api.deepseek.com', 'deepseek-flash', '大多数模型与中转站（/chat/completions）'],
        ['openai-responses', 'OpenAI Responses', 'https://api.openai.com/v1', 'gpt-5', 'OpenAI 新版 Responses 接口（/responses）'],
        ['anthropic', 'Anthropic (Claude)', 'https://api.anthropic.com', 'claude-sonnet-4-5', 'Claude 原生 Messages 接口（/v1/messages）'],
        ['gemini', 'Google Gemini', 'https://generativelanguage.googleapis.com', 'gemini-2.5-flash', 'Gemini 原生接口（/v1beta/models/...）'],
      ];
      const curProto = a.protocol || 'openai';
      $('#aiState').innerHTML = a.ready ? '<span class="badge ok">可用</span>' : a.api_key_set ? '<span class="badge unknown">已停用</span>' : '<span class="badge slow">未设置 Key</span>';
      $('#aiModel').textContent = a.ready ? '  ' + a.model + (a.thinking ? ' · 深度思考' : '') : '';
      $('#aiSet').innerHTML = `
        <div class="checks" style="margin-bottom:8px"><label><input type="checkbox" name="enabled" ${a.enabled ? 'checked' : ''}> 启用 AI 诊断</label>
          <label title="更慢、更贵，复杂问题时可开启"><input type="checkbox" name="thinking" ${a.thinking ? 'checked' : ''}> 深度思考</label>
          <label title="写完回答后逐条核对数据，必要时实测（ping、端口、交换机端口状态），发现问题自动修正；会多一次模型调用"><input type="checkbox" name="self_check" ${a.self_check ? 'checked' : ''}> 回答前自检</label></div>
        <label class="f"><span>API Key${a.api_key_set ? `（已设置 ${esc(a.api_key_hint)}，留空不修改）` : ''}</span>
          <button class="btn ai-key-unlock" type="button" id="aiKeyUnlock">${a.api_key_set ? '点击修改 API Key' : '点击填写 API Key'}</button></label>
        <label class="f"><span>协议格式</span><select name="protocol">${PROTOCOLS.map(([v, t]) => `<option value="${v}"${v === curProto ? ' selected' : ''}>${t}</option>`).join('')}</select></label>
        <div class="row2">
          <label class="f"><span>模型</span><input type="text" name="model" value="${esc(a.model)}"></label>
          <label class="f"><span>超时（秒）</span><input type="number" name="timeout_sec" min="10" max="600" value="${a.timeout_sec}"></label>
        </div>
        <label class="f"><span>接口地址</span><input type="text" name="base_url" value="${esc(a.base_url)}"></label>
        <label class="f"><span>HTTP 代理（留空时使用告警设置里的代理）</span><input type="text" name="proxy_url" value="${esc(a.proxy_url || '')}" placeholder="http://10.0.0.1:3128"></label>
        <div class="btns"><button class="btn primary" type="submit">保存</button><button class="btn" type="button" id="aiTest">测试连接</button>
          ${a.api_key_set ? '<button class="btn danger" type="button" id="aiClrKey">清除 Key</button>' : ''}</div>
        <div class="help" id="aiTestMsg">支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Claude、Google Gemini 协议，默认 DeepSeek（deepseek-flash）。Key 保存在 config.yaml，网页和 MCP 都不会把它返回。</div>`;
      const f = $('#aiSet');
      const protoSel = f.elements.protocol;
      const defaultURLs = new Set(PROTOCOLS.map(p => p[2]));
      const defaultModels = new Set(PROTOCOLS.map(p => p[3]));
      protoSel.onchange = () => {
        const hit = PROTOCOLS.find(p => p[0] === protoSel.value);
        if (!hit) return;
        const urlEl = f.elements.base_url, modEl = f.elements.model;
        if (!urlEl.value.trim() || defaultURLs.has(urlEl.value.trim().replace(/\/+$/, ''))) urlEl.value = hit[2];
        if (!modEl.value.trim() || defaultModels.has(modEl.value.trim())) modEl.value = hit[3];
      };
      // 未主动解锁前不创建密码框，也不提交 api_key，避免密码管理器自动填充覆盖已保存的 Key。
      let keyInput = null;
      $('#aiKeyUnlock').onclick = ev => {
        keyInput = document.createElement('input');
        keyInput.type = 'password';
        keyInput.name = 'api_key';
        keyInput.autocomplete = 'new-password';
        keyInput.spellcheck = false;
        keyInput.placeholder = a.api_key_set ? '留空保持不变' : 'sk-...';
        ev.currentTarget.replaceWith(keyInput);
        keyInput.focus();
      };
      const body = () => ({
        enabled: f.elements.enabled.checked, protocol: f.elements.protocol.value,
        thinking: f.elements.thinking.checked, self_check: f.elements.self_check.checked,
        ...(keyInput ? { api_key: keyInput.value.trim() } : {}),
        model: f.elements.model.value.trim(), timeout_sec: parseInt(f.elements.timeout_sec.value, 10) || 120,
        base_url: f.elements.base_url.value.trim(), proxy_url: f.elements.proxy_url.value.trim(),
      });
      f.onsubmit = async ev => {
        ev.preventDefault();
        try { settings = await api('PUT', 'api/ai/settings', body()); drawSettings(); toast('AI 设置已保存'); }
        catch (e) { toast(errMsg(e), true); }
      };
      $('#aiTest').onclick = async () => {
        const msg = $('#aiTestMsg');
        try {
          settings = await api('PUT', 'api/ai/settings', body());
          drawSettings();
          $('#aiTestMsg').textContent = '正在测试…';
          const r = await api('POST', 'api/ai/test');
          $('#aiTestMsg').innerHTML = `<span style="color:var(--ok)">连接正常</span>：${esc(r.model)} 回复“${esc(r.reply)}”，用时 ${(r.ms / 1000).toFixed(1)} 秒`;
        } catch (e) { ($('#aiTestMsg') || msg).innerHTML = `<span style="color:var(--down)">${esc(errMsg(e))}</span>`; }
      };
      const ck = $('#aiClrKey');
      if (ck) ck.onclick = async () => {
        try { settings = await api('PUT', 'api/ai/settings', { clear_key: true }); drawSettings(); toast('已清除 API Key'); }
        catch (e) { toast(errMsg(e), true); }
      };
    }

    renderLog();
    api('GET', 'api/ai/settings').then(a => {
      settings = a;
      drawSettings();
      // 从设备详情跳转过来：直接诊断这台设备
      if (focusID) {
        history.replaceState(null, '', '#/ai');
        const all = S.topo ? S.topo.nodes.concat(S.topo.switches) : [];
        const d = all.find(x => x.id === focusID);
        const label = d ? [d.name, d.ip].filter(Boolean).join(' ') || d.id : focusID;
        if (a.ready) ask(`诊断设备 ${label}：它现在是否正常？最近有没有异常（中断时段、和昨天比有没有变差）？上联路径是否正常？如果有问题，分析可能原因并给出排查步骤。`, focusID);
        else toast('请先在右侧设置 API Key', true);
      }
    }).catch(e => toast(errMsg(e), true));
    return { destroy() { if (busy) busy.abort(); } };
  }

  // ---------------------------------------------------------------------------
  // 拓扑页
  // 设备类别（与服务器 deviceCategories 一致），AI 一键优化把类别写成同名标签
  const DEVICE_CATS = ['服务器', '虚拟机', '台式机', '笔记本', '手机平板', '打印机', '摄像头', '录像机', '无线AP',
    '路由器防火墙', '网络存储', 'IP电话', '门禁考勤', '智能设备', '工控设备', '其他'];
  function topoPage(app) {
    app.innerHTML = `
    <div class="topo">
      <div class="canvas-wrap">
        <svg id="topoSvg"></svg>
        <div class="canvas-tools left">
          <button class="btn primary small" id="addSw">+ 交换机</button>
          <button class="btn small" id="addNode">+ 节点</button>
          <button class="btn small" id="addLink">+ 链路</button>
          <button class="btn small" id="autoLayout" title="清除所有节点的手动坐标：已接线的按端口排列，未定位的按 IP 整齐排在下方">自动排布</button>
          <button class="btn small accent" id="aiOpt" title="AI 结合设备资料与近 7 天历史数据：自动分类设备、给出配置建议、找出需要检查的设备。建议需勾选后才会应用，可撤销">✦ AI 一键优化</button>
        </div>
        <div class="canvas-tools right">
          <button class="btn small" id="zIn">+</button><button class="btn small" id="zOut">−</button><button class="btn small" id="zFit">适配</button>
        </div>
        <div class="zoom-label" id="zoomLbl"></div>
        <div class="pick-bar hidden" id="pickBar"></div>
        <div class="canvas-hint">拖动空白处平移 · 滚轮缩放 · 拖动卡片/交换机标题调整位置 · 双击查看曲线 · 点网口再点卡片＝接线 · <kbd>/</kbd> 检索 <kbd>Esc</kbd> 清除 <kbd>F</kbd> 适配</div>
      </div>
      <aside class="side">
        <div class="sec"><h3><span class="no">01</span>概况<span class="r" id="ovSite"></span></h3><div class="stat-grid" id="ov"></div></div>
        <div class="sec"><h3><span class="no">02</span>检索 / 筛选<span class="r" id="matchCnt"></span></h3>
          <div style="display:flex;gap:6px"><input type="search" id="q" placeholder="IP / MAC / 厂商 / 端口 / 名称，如 .52、45:8B、Port 12"><button class="btn" id="qClear">清除</button></div>
          <div class="chips" id="chips"></div>
        </div>
        <div class="sec" id="detailSec"></div>
        <div class="sec"><h3><span class="no">04</span>厂商图例<span class="r">点击筛选</span></h3><div class="legend" id="legend"></div>
          <div class="help">厂商依 MAC 前缀（OUI）判断；“本地私有地址”指 MAC 为本地管理位（随机化 MAC 等），无法判定厂商。</div></div>
        <div class="sec"><h3><span class="no">05</span>图例符号</h3><div class="sym" id="symbols"></div></div>
      </aside>
    </div>`;
    const svg = $('#topoSvg');
    view = new TopoView(svg, {
      status: id => S.status[id],
      vendorColor,
      attachLabel,
      onSelect: sel => { S.sel = sel; view.setSelection(sel); renderDetail(); },
      onOpen: sel => { if (sel.kind === 'node' || sel.kind === 'switch') location.hash = '#/node/' + encodeURIComponent(sel.id); },
      onMove: async patch => {
        try { await api('POST', 'api/positions', patch); }
        catch (e) { toast(errMsg(e), true); }
      },
      onZoom: k => { const z = $('#zoomLbl'); if (z) z.textContent = Math.round(k * 100) + '%'; },
      onPort: p => setPick(p && S.pick && S.pick.sw === p.sw && S.pick.port === p.port ? null : p),
      onPickNode: (id, keep) => attachToPick(id, keep),
    });

    // ---- 先点网口、再点设备卡片：快速设置设备接入点 ----
    function setPick(p) {
      S.pick = p || null;
      view.setPick(S.pick);
      const bar = $('#pickBar');
      if (!bar) return;
      if (!S.pick) { bar.classList.add('hidden'); return; }
      const sw = S.topo.switches.find(x => x.id === S.pick.sw);
      const on = S.topo.nodes.filter(n => n.attach.switch === S.pick.sw && n.attach.port === S.pick.port);
      const link = S.topo.links.find(l => (l.from.switch === S.pick.sw && l.from.port === S.pick.port) || (l.to.switch === S.pick.sw && l.to.port === S.pick.port));
      const loc = sw && sw.port_locations && sw.port_locations[S.pick.port];
      const now = on.length ? `当前接了 ${on.length} 台：${on.slice(0, 3).map(n => esc(n.name || n.ip || n.id)).join('、')}${on.length > 3 ? ' 等' : ''}` : '当前空闲';
      bar.title = `${sw ? sw.name || sw.id : S.pick.sw} ${S.pick.port}${loc ? ' · ' + loc : ''}\n${now}\n点击设备卡片把它接到这个口；按住 Shift 可连续接多台；Esc 取消`;
      bar.innerHTML = `<span class="pb-t"><b>${esc(sw ? sw.name || sw.id : S.pick.sw)} · ${esc(S.pick.port)}</b>
        <span class="muted">${on.length ? `已接 ${on.length} 台` : '空闲'}${link ? ' · <span style="color:var(--warn)">级联口</span>' : ''}</span>
        → 点设备卡片接线 <span class="muted">· <kbd>Shift</kbd> 连续接多台</span></span>
        <button class="btn small" id="pickCancel">取消</button>`;
      bar.classList.remove('hidden');
      $('#pickCancel').onclick = () => setPick(null);
    }
    async function attachToPick(id, keep) {
      const p = S.pick;
      const n = S.topo.nodes.find(x => x.id === id);
      if (!p || !n) return;
      if (n.attach.switch === p.sw && n.attach.port === p.port) { toast(`${n.name || n.ip || n.id} 已经接在这个口`); if (!keep) setPick(null); return; }
      const before = JSON.parse(JSON.stringify(n));
      const body = JSON.parse(JSON.stringify(n));
      const sw = S.topo.switches.find(x => x.id === p.sw);
      body.attach = { switch: p.sw, port: p.port };
      body.position = null; // 回到端口旁自动排布
      body.flags = (body.flags || []).filter(f => f !== 'unlocated');
      const loc = sw && sw.port_locations && sw.port_locations[p.port];
      if (loc && !body.location) body.location = loc;
      try {
        await api('PUT', 'api/nodes/' + encodeURIComponent(n.id), body);
        if (!keep) setPick(null);
        await loadTopology();
        if (keep) setPick(p); // 刷新提示栏里的“当前接了几台”
        const name = n.name || n.ip || n.id;
        toast(`已把 ${name} 接到 ${sw ? sw.name || sw.id : p.sw} ${p.port}${before.attach.switch ? `（原来在 ${attachLabel(before)}）` : ''}`, false, {
          label: '撤销', fn: async () => {
            try { await api('PUT', 'api/nodes/' + encodeURIComponent(n.id), before); await loadTopology(); toast('已恢复原来的接入点'); }
            catch (e) { toast(errMsg(e), true); }
          },
        });
      } catch (e) { toast(errMsg(e), true); }
    }
    let fitted = false;

    $('#zIn').onclick = () => view.zoomAt(1.2);
    $('#zOut').onclick = () => view.zoomAt(1 / 1.2);
    $('#zFit').onclick = () => view.fit();
    $('#addSw').onclick = () => { S.sel = { kind: 'newSwitch' }; view.setSelection(null); renderDetail(); };
    $('#addNode').onclick = () => { S.sel = { kind: 'newNode', sw: S.sel && S.sel.kind === 'switch' ? S.sel.id : '' }; view.setSelection(null); renderDetail(); };
    $('#addLink').onclick = () => { S.sel = { kind: 'newLink' }; view.setSelection(null); renderDetail(); };
    // 不用浏览器原生确认框（Firefox 等可能被设置为“不再显示对话框”而直接返回取消），改为执行后可撤销。
    $('#autoLayout').onclick = async () => {
      const saved = {};
      S.topo.nodes.forEach(n => { if (n.position) saved[n.id] = n.position; });
      const n = Object.keys(saved).length;
      if (!n) { toast('所有节点都已是自动排布'); return; }
      try {
        await api('POST', 'api/layout/reset', {});
        await loadTopology();
        toast(`已把 ${n} 个节点恢复为自动排布（已接线的按端口排列，未定位的按 IP 排在下方）`, false, {
          label: '撤销', fn: async () => {
            try { await api('POST', 'api/positions', { nodes: saved }); await loadTopology(); toast('已恢复原来的位置'); }
            catch (e) { toast(errMsg(e), true); }
          },
        });
      } catch (e) { toast(errMsg(e), true); }
    };
    const q = $('#q');
    q.value = S.search;
    q.addEventListener('input', () => { S.search = q.value.trim(); applyMatch(); });
    $('#qClear').onclick = () => { q.value = ''; S.search = ''; S.filter = 'all'; S.vendor = null; applyMatch(); renderChips(); renderLegend(); };

    const onKey = ev => {
      if (ev.target.matches('input, textarea, select')) {
        if (ev.key === 'Escape') ev.target.blur();
        return;
      }
      if (ev.key === '/') { ev.preventDefault(); q.focus(); }
      else if (ev.key === 'Escape' && S.pick) setPick(null);
      else if (ev.key === 'Escape') { q.value = ''; S.search = ''; S.sel = null; view.setSelection(null); applyMatch(); renderDetail(); }
      else if (ev.key === 'f' || ev.key === 'F') view.fit();
    };
    document.addEventListener('keydown', onKey);
    const onResize = () => view.schedule();
    window.addEventListener('resize', onResize);

    function matcher() {
      const qs = S.search.toLowerCase();
      const f = S.filter, v = S.vendor;
      if (!qs && f === 'all' && !v) return null;
      const portCount = {};
      S.topo.nodes.forEach(n => { if (n.attach && n.attach.switch) { const k = n.attach.switch + '|' + n.attach.port; portCount[k] = (portCount[k] || 0) + 1; } });
      return n => {
        if (v && (n.vendor || '未知') !== v) return false;
        const flags = n.flags || [];
        const st = S.status[n.id];
        if (f === 'abn' && !(st && ['down', 'loss', 'slow'].includes(st.state))) return false;
        if (f === 'sus' && !(flags.includes('suspect') || flags.includes('unlocated') || !n.attach.switch)) return false;
        if (f === 'alert' && !(n.monitor && n.monitor.alert)) return false;
        if (f === 'gw' && !(flags.includes('gateway') || flags.includes('critical') || flags.includes('jump_host'))) return false;
        if (f === 'multi' && !(n.attach.switch && portCount[n.attach.switch + '|' + n.attach.port] > 1)) return false;
        if (f.startsWith('sw:') && n.attach.switch !== f.slice(3)) return false;
        if (f.startsWith('cat:') && !(n.tags || []).includes(f.slice(4))) return false;
        if (qs) {
          const hay = [n.ip, n.mac, n.vendor, n.name, n.hostname, n.location, n.note, attachLabel(n), (n.tags || []).join(' '), n.id].join(' ').toLowerCase();
          const qn = qs.replace(/^\./, '');
          if (!hay.includes(qs) && !(qs.startsWith('.') && (n.ip || '').endsWith(qs)) && !hay.includes(qn.replace(/-/g, ':'))) return false;
        }
        return true;
      };
    }
    function applyMatch() {
      const m = matcher();
      view.setMatch(m, !!S.search);
      const cnt = m ? S.topo.nodes.filter(m).length : S.topo.nodes.length;
      $('#matchCnt').textContent = `${cnt} / ${S.topo.nodes.length} 条`;
    }
    function renderChips() {
      const chips = [['all', '全部'], ['abn', '异常'], ['alert', '已开告警'], ['multi', '多 MAC 端口'], ['sus', '存疑 / 未定位'], ['gw', '网关 / 关键']];
      S.topo.switches.forEach(sw => chips.push(['sw:' + sw.id, sw.name || sw.id]));
      const cats = DEVICE_CATS.filter(c => S.topo.nodes.some(n => (n.tags || []).includes(c)));
      cats.forEach(c => chips.push(['cat:' + c, c]));
      $('#chips').innerHTML = chips.map(([k, t]) => `<button class="chip${S.filter === k ? ' on' : ''}${k === 'sus' ? ' dashed' : ''}" data-k="${esc(k)}">${esc(t)}</button>`).join('');
      $$('#chips .chip').forEach(b => b.onclick = () => { S.filter = b.dataset.k; renderChips(); applyMatch(); });
    }
    function renderLegend() {
      $('#legend').innerHTML = S.vendorList.map(v =>
        `<div class="it${S.vendor === v ? ' on' : ''}" data-v="${esc(v)}"><span class="sw" style="background:${vendorColor(v)}"></span>${esc(v)}<span class="n">${S.vendorCounts[v]}</span></div>`).join('');
      $$('#legend .it').forEach(el => el.onclick = () => { S.vendor = S.vendor === el.dataset.v ? null : el.dataset.v; renderLegend(); applyMatch(); });
    }
    function renderOverview() {
      const sm = S.summary || { by_state: {} };
      const b = sm.by_state || {};
      const nodes = S.topo ? S.topo.nodes.length : 0;
      $('#ov').innerHTML = `
        <div><div class="v">${nodes}</div><div class="k">登记节点</div></div>
        <div><div class="v" style="color:var(--ok)">${b.ok || 0}</div><div class="k">正常</div></div>
        <div><div class="v" style="color:var(--warn)">${(b.slow || 0) + (b.loss || 0)}</div><div class="k">丢包/延迟</div></div>
        <div><div class="v" style="color:var(--down)">${b.down || 0}</div><div class="k">离线</div></div>`;
      $('#ovSite').textContent = `监控 ${sm.total || 0} 个地址`;
    }
    $('#symbols').innerHTML = `
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="#1a1a1a"/></svg><span>核心 / 汇聚交换机</span>
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="#fbfaf6" stroke="#1a1a1a" stroke-dasharray="5 3"/></svg><span>接入交换机</span>
      <svg width="44" height="16"><line x1="2" y1="8" x2="42" y2="8" stroke="#1a1a1a" stroke-width="5" stroke-dasharray="8 3"/></svg><span>交换机间链路（点击可编辑）</span>
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="#ff5a1f"/></svg><span>默认网关</span>
      <svg width="44" height="16"><rect x="6" y="2" width="32" height="12" fill="#ff5a1f"/><text x="22" y="11" font-size="9" fill="#fff" text-anchor="middle">告警</text></svg><span>已开启企业微信告警（设备默认不告警）</span>
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="none" stroke="#4a4740" stroke-dasharray="4 3"/></svg><span>多 MAC 端口：口后还有交换机/集线器</span>
      <svg width="44" height="16"><defs><pattern id="h2" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="5" stroke="#8a857a"/></pattern></defs><rect x="1" y="1" width="42" height="14" fill="url(#h2)" stroke="#4a4740"/></svg><span>存疑：复扫未见 / 信息待核实</span>
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="#fff" stroke="#d63a22" stroke-width="2"/></svg><span>离线（红框）/ 丢包或延迟高（橙框）</span>`;

    // ---- AI 一键优化：分类、配置建议、需要检查的设备 ----
    let opt = null; // { busy: AbortController, el }
    $('#aiOpt').onclick = () => openOpt(false);
    function locateDevice(id) {
      const n = S.topo.nodes.find(x => x.id === id), sw = S.topo.switches.find(x => x.id === id);
      if (!n && !sw) { toast('设备已不存在', true); return; }
      closeOpt();
      S.sel = { kind: n ? 'node' : 'switch', id };
      view.setSelection(S.sel);
      renderDetail();
      if (n) { q.value = n.ip || n.id; S.search = q.value; applyMatch(); }
    }
    function closeOpt() {
      if (!opt) return;
      if (opt.busy) opt.busy.abort();
      opt.el.remove();
      document.removeEventListener('keydown', opt.onKey, true);
      opt = null;
    }
    function openOpt(rerun) {
      if (!opt) {
        const el = document.createElement('div');
        el.className = 'modal-mask';
        el.innerHTML = `<div class="modal aiopt"><div class="mh"><b>✦ AI 一键优化</b><span class="muted" id="optMeta"></span>
          <button class="btn small r" id="optClose">关闭</button></div><div class="mb" id="optBody"></div><div class="mf" id="optFoot"></div></div>`;
        document.body.appendChild(el);
        el.addEventListener('mousedown', ev => { if (ev.target === el && !(opt && opt.busy)) closeOpt(); });
        const onKey = ev => { if (ev.key === 'Escape') { ev.stopPropagation(); closeOpt(); } };
        document.addEventListener('keydown', onKey, true);
        opt = { el, onKey };
        $('#optClose').onclick = closeOpt;
      }
      let saved = null;
      try { saved = JSON.parse(sessionStorage.getItem('np_aiopt') || 'null'); } catch (e) { saved = null; }
      if (saved && !rerun) drawOpt(saved); else runOpt();
    }
    async function runOpt() {
      const body = $('#optBody'), foot = $('#optFoot');
      const st = { status: '正在连接…', tools: [], text: '', error: '' };
      const draw = () => {
        if (!opt) return;
        const pre = st.text.split('```')[0].trim();
        body.innerHTML = `<div class="help" style="margin-top:0">AI 会读取全部设备的厂商、主机名、接线、告警设置和近 7 天的丢包/延迟/中断统计，
            给出：<b>设备分类</b>（写成标签）、<b>配置建议</b>、<b>需要检查的设备</b>。所有修改都要你勾选确认后才会应用。设备较多时需要 1~3 分钟。</div>
          ${st.tools.length ? `<div class="ai-tools">${st.tools.map(t => `<span class="ai-tool">查询 · ${esc(t.title || t.name)}</span>`).join('')}</div>` : ''}
          ${pre ? `<div class="ai-md">${mdLite(pre)}</div>` : ''}
          ${st.text.includes('```') && !st.error ? '<div class="ai-status"><span class="ai-spin"></span>正在生成建议清单…</div>' : ''}
          ${st.status ? `<div class="ai-status"><span class="ai-spin"></span>${esc(st.status)}</div>` : ''}
          ${st.error ? `<div class="ai-err">${esc(st.error)}</div>` : ''}`;
      };
      foot.innerHTML = `<button class="btn" id="optStop">停止</button>`;
      $('#optMeta').textContent = '';
      draw();
      const ctl = new AbortController();
      opt.busy = ctl;
      $('#optStop').onclick = () => ctl.abort();
      let result = null, meta = '';
      try {
        const r = await fetch('api/ai/optimize', { method: 'POST', signal: ctl.signal, headers: { 'Content-Type': 'application/json' }, body: '{}' });
        if (!r.ok) {
          const d = await r.json().catch(() => ({}));
          if (r.status === 401 && d.login_required) { toLogin(); return; }
          throw new Error(d.error || 'HTTP ' + r.status);
        }
        const rd = r.body.getReader(), dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { value, done } = await rd.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let k;
          while ((k = buf.indexOf('\n')) >= 0) {
            const line = buf.slice(0, k).trim();
            buf = buf.slice(k + 1);
            if (!line) continue;
            let ev;
            try { ev = JSON.parse(line); } catch (e) { continue; }
            if (ev.t === 'status') st.status = ev.text;
            else if (ev.t === 'stats') st.status = `已汇总 ${ev.stats.nodes} 个节点、${ev.stats.switches} 台交换机（${ev.stats.with_stats} 台有历史数据），正在请求模型…`;
            else if (ev.t === 'delta') { st.text += ev.text; st.status = ''; }
            else if (ev.t === 'break') st.text = '';
            else if (ev.t === 'tool') { st.tools.push(ev); st.status = '正在查询 ' + (ev.title || ev.name) + '…'; }
            else if (ev.t === 'error') { st.error = ev.error; st.status = ''; }
            else if (ev.t === 'result') result = ev.result;
            else if (ev.t === 'done') meta = `${ev.model} · ${(ev.ms / 1000).toFixed(1)} 秒 · 查询 ${ev.tools} 次 · ${(ev.usage || {}).total_tokens || 0} tokens`;
            draw();
          }
        }
        if (!result && !st.error) st.error = '模型没有返回结果，请重试。';
      } catch (e) {
        st.error = e.name === 'AbortError' ? '已停止' : errMsg(e);
      } finally {
        if (opt) opt.busy = null;
      }
      if (!opt) return;
      if (result) {
        result.meta = meta;
        sessionStorage.setItem('np_aiopt', JSON.stringify(result));
        drawOpt(result);
      } else {
        st.status = '';
        draw();
        foot.innerHTML = `<button class="btn primary" id="optRetry">重试</button>`;
        $('#optRetry').onclick = runOpt;
      }
    }
    function drawOpt(res) {
      const body = $('#optBody'), foot = $('#optFoot');
      $('#optMeta').textContent = `  ${res.time || ''}${res.meta ? ' · ' + res.meta : ''}`;
      const cls = res.classify || [], cfg = res.config || [], chk = res.check || [];
      const nCls = cls.reduce((a, g) => a + g.devices.length, 0);
      const pri = { high: ['down', '高'], medium: ['slow', '中'], low: ['unknown', '低'] };
      body.innerHTML = `
        ${res.summary ? `<div class="ai-md">${mdLite(res.summary)}</div>` : ''}
        <div class="opt-sec"><h4>1 · 设备分类 <span class="muted">${nCls} 台 · 作为标签写入，可在拓扑页按类别筛选</span>
          ${nCls ? '<label class="r"><input type="checkbox" id="optAllC" checked> 全选</label>' : ''}</h4>
          ${cls.length ? cls.map((g, gi) => `<div class="opt-grp">
            <label class="opt-gh"><input type="checkbox" class="optG" data-g="${gi}" checked> <b>${esc(g.category)}</b> ${g.devices.length} 台
              <span class="muted">${esc(g.reason || '')}</span></label>
            <div class="opt-devs">${g.devices.map((d, di) => `<label class="opt-dev" title="${esc(d.id)}"><input type="checkbox" class="optC" data-g="${gi}" data-d="${di}" checked>
              ${esc(d.label || d.id)}${d.current ? ` <span class="muted">（原 ${esc(d.current)}）</span>` : ''}</label>`).join('')}</div></div>`).join('')
            : '<div class="help">没有需要新分类的设备。</div>'}
        </div>
        <div class="opt-sec"><h4>2 · 配置建议 <span class="muted">${cfg.length} 条</span>
          ${cfg.some(c => c.applicable) ? '<label class="r"><input type="checkbox" id="optAllS"> 全选</label>' : ''}</h4>
          ${cfg.length ? `<table class="grid flat opt-tbl"><tbody>${cfg.map((c, i) => `<tr>
            <td style="width:24px">${c.applicable ? `<input type="checkbox" class="optS" data-i="${i}" ${c.action === 'monitor_off' ? '' : 'checked'}>` : ''}</td>
            <td style="white-space:nowrap"><span class="badge ${c.applicable ? 'solid' : 'unknown'}">${esc(c.title)}</span></td>
            <td>${c.label ? `<a href="javascript:void 0" class="optLoc" data-id="${esc(c.id)}">${esc(c.label)}</a>` : '<span class="muted">全局</span>'}
              ${c.change ? `<div><b>${esc(c.change)}</b></div>` : ''}<div class="muted">${esc(c.reason || '')}</div></td></tr>`).join('')}</tbody></table>`
            : '<div class="help">当前配置没有需要调整的地方。</div>'}
        </div>
        <div class="opt-sec"><h4>3 · 建议检查的设备 <span class="muted">${chk.length} 台 · 依据近 7 天历史数据</span></h4>
          ${chk.length ? chk.map(c => `<div class="opt-chk">
            <div><span class="badge ${pri[c.priority][0]}">${pri[c.priority][1]}</span> <b>${esc(c.label)}</b> <span class="muted">${esc(c.attach || '')}</span>
              <span class="r btns" style="margin:0"><button class="btn small optLoc" data-id="${esc(c.id)}">在图中定位</button>
              <a class="btn small" href="#/node/${encodeURIComponent(c.id)}">曲线</a>
              <a class="btn small" href="#/ai/${encodeURIComponent(c.id)}">AI 诊断</a></span></div>
            <div>${esc(c.reason || '')}</div>${c.steps ? `<div class="muted">检查：${esc(c.steps)}</div>` : ''}</div>`).join('')
            : '<div class="help">近 7 天没有明显需要检查的设备。</div>'}
        </div>`;
      const count = () => $$('.optC:checked', body).length + $$('.optS:checked', body).length;
      const upd = () => {
        const b = $('#optApply');
        if (b) { const n = count(); b.disabled = !n; b.textContent = `应用所选（${n}）`; }
        const sync = (all, sel) => { if (all) { const xs = $$(sel, body); all.checked = xs.length > 0 && xs.every(x => x.checked); } };
        sync($('#optAllC'), '.optC'); sync($('#optAllS'), '.optS');
        $$('.optG', body).forEach(g => { g.checked = $$(`.optC[data-g="${g.dataset.g}"]`, body).every(x => x.checked); });
      };
      foot.innerHTML = `<button class="btn primary" id="optApply">应用所选</button><button class="btn" id="optRerun">重新分析</button>
        <span class="muted" style="margin-left:auto">应用后可在提示中撤销</span>`;
      $$('.optG', body).forEach(g => g.onchange = () => { $$(`.optC[data-g="${g.dataset.g}"]`, body).forEach(x => { x.checked = g.checked; }); upd(); });
      $$('.optC', body).forEach(x => x.onchange = upd);
      $$('.optS', body).forEach(x => x.onchange = upd);
      const allC = $('#optAllC'), allS = $('#optAllS');
      if (allC) allC.onchange = () => { $$('.optC, .optG', body).forEach(x => { x.checked = allC.checked; }); upd(); };
      if (allS) allS.onchange = () => { $$('.optS', body).forEach(x => { x.checked = allS.checked; }); upd(); };
      $$('.optLoc', body).forEach(a => a.onclick = () => locateDevice(a.dataset.id));
      $('#optRerun').onclick = runOpt;
      $('#optApply').onclick = async () => {
        const classify = $$('.optC:checked', body).map(x => ({ id: cls[+x.dataset.g].devices[+x.dataset.d].id, category: cls[+x.dataset.g].category }));
        const config = $$('.optS:checked', body).map(x => cfg[+x.dataset.i]);
        try {
          const r = await api('POST', 'api/ai/optimize/apply', { classify, config });
          // 已应用的从清单中移除
          const doneC = new Set(classify.map(c => c.id + '|' + c.category));
          res.classify = cls.map(g => ({ ...g, devices: g.devices.filter(d => !doneC.has(d.id + '|' + g.category)) })).filter(g => g.devices.length);
          res.config = cfg.filter(c => !config.includes(c));
          sessionStorage.setItem('np_aiopt', JSON.stringify(res));
          await loadTopology();
          if (opt) drawOpt(res);
          const undo = r.undo || [];
          toast(`已应用：修改了 ${r.changed} 台设备${r.skipped ? `，${r.skipped} 条已失效被跳过` : ''}`, false, undo.length ? {
            label: '撤销', fn: async () => {
              try { await api('POST', 'api/ai/optimize/apply', { restore: undo }); await loadTopology(); toast('已撤销本次修改（可点「重新分析」重新生成建议）'); }
              catch (e) { toast(errMsg(e), true); }
            },
          } : null);
        } catch (e) { toast(errMsg(e), true); }
      };
      upd();
    }

    function onTopology() {
      view.setTopology(S.topo);
      renderChips(); renderLegend(); applyMatch(); renderOverview();
      const box = $('#detailSec');
      if (box && box.dataset.dirty && findSel(S.sel)) updateLive(); else renderDetail();
      if (!fitted) { fitted = true; requestAnimationFrame(() => view.fit()); }
    }
    if (S.topo) onTopology();
    return {
      onTopology,
      update() { view.refresh(); renderOverview(); updateLive(); if (S.filter === 'abn') applyMatch(); },
      destroy() { closeOpt(); S.pick = null; document.removeEventListener('keydown', onKey); window.removeEventListener('resize', onResize); },
    };
  }

  // ---- 右侧详情 / 编辑 ----
  function liveHTML(id) {
    const st = S.status[id];
    if (!st) return '<div class="help">该地址未在监控中（没有 IP 或已停用监控）。</div>';
    return `<div class="live">
      <div><div class="k">状态</div><div class="v">${stateBadge(st.state)}</div></div>
      <div><div class="k">当前</div><div class="v">${fmtMs(st.last_rtt)}</div></div>
      <div><div class="k">1m 平均</div><div class="v">${st.avg_1m ? fmtMs(st.avg_1m) : '-'}</div></div>
      <div><div class="k">1m 丢包</div><div class="v ${lossCls(st.loss_1m, st.loss_warn_pct)}">${pct(st.loss_1m)}</div></div>
      <div><div class="k">5m 丢包</div><div class="v ${lossCls(st.loss_5m, st.loss_warn_pct)}">${pct(st.loss_5m)}</div></div>
      <div><div class="k">1h 丢包</div><div class="v ${lossCls(st.loss_1h, st.loss_warn_pct)}">${pct(st.loss_1h)}</div></div>
    </div>
    <div style="display:flex;align-items:center;gap:10px;margin-bottom:10px">${Charts.spark(st.spark, 220, 26, st.latency_warn_ms)}
      <a href="#/node/${encodeURIComponent(id)}">历史曲线 →</a></div>`;
  }
  function updateLive() {
    const el = $('#liveBox');
    if (el && el.dataset.id) el.innerHTML = liveHTML(el.dataset.id);
  }

  function markClean() { const b = $('#detailSec'); if (b) delete b.dataset.dirty; }
  function renderDetail() {
    const box = $('#detailSec');
    if (!box) return;
    delete box.dataset.dirty;
    box.oninput = () => { box.dataset.dirty = '1'; };
    const sel = S.sel;
    const head = t => `<h3><span class="no">03</span>${t}</h3>`;
    if (!sel) {
      box.innerHTML = head('详情') + `<div class="help">点击终端卡片、交换机标题或交换机间连线查看并编辑。<br>
        图中<b style="color:var(--accent)">橙色</b>为默认网关与上联口；<b>斜纹</b>表示存疑；虚线框为多 MAC 端口或未定位区域。<br>
        节点数据保存在 topology.yaml，可在「设置」中直接编辑 YAML 或从文件重新加载。</div>`;
      return;
    }
    if (sel.kind === 'node' || sel.kind === 'newNode') nodeForm(box, head, sel);
    else if (sel.kind === 'switch' || sel.kind === 'newSwitch') switchForm(box, head, sel);
    else if (sel.kind === 'link' || sel.kind === 'newLink') linkForm(box, head, sel);
  }

  function monFields(m, defaults) {
    m = m || {};
    const d = defaults || {};
    return `<div class="checks" style="margin-bottom:6px"><label><input type="checkbox" name="mon_enabled" ${m.enabled === false ? '' : 'checked'}> 启用 ping 监控</label>
        <label><input type="checkbox" name="mon_alert" ${m.alert ? 'checked' : ''}> 推送企业微信告警</label></div>
      <div class="row2">
        <label class="f"><span>间隔 ms</span><input type="number" name="interval_ms" value="${m.interval_ms || ''}" placeholder="默认 ${d.interval_ms || 1000}"></label>
        <label class="f"><span>超时 ms</span><input type="number" name="timeout_ms" value="${m.timeout_ms || ''}" placeholder="默认 ${d.timeout_ms || 1000}"></label>
        <label class="f"><span>延迟阈值 ms</span><input type="number" name="latency_warn_ms" value="${m.latency_warn_ms || ''}" placeholder="默认 ${d.latency_warn_ms || 100}"></label>
        <label class="f"><span>丢包阈值 %</span><input type="number" name="loss_warn_pct" value="${m.loss_warn_pct || ''}" placeholder="默认 ${d.loss_warn_pct || 20}"></label>
      </div>`;
  }
  function readMon(f) {
    const num = n => { const v = parseFloat(f.elements[n].value); return isFinite(v) && v > 0 ? v : undefined; };
    const m = { interval_ms: num('interval_ms'), timeout_ms: num('timeout_ms'), latency_warn_ms: num('latency_warn_ms'), loss_warn_pct: num('loss_warn_pct') };
    if (!f.elements.mon_enabled.checked) m.enabled = false;
    if (f.elements.mon_alert.checked) m.alert = true;
    return m;
  }
  let pingDefaults = null;
  api('GET', 'api/config').then(c => { pingDefaults = c.ping; }).catch(() => {});

  function nodeForm(box, head, sel) {
    const isNew = sel.kind === 'newNode';
    const n = isNew ? { attach: { switch: sel.sw || '', port: '' }, flags: [], tags: [] } : S.topo.nodes.find(x => x.id === sel.id);
    if (!n) { box.innerHTML = head('详情'); return; }
    const swOpts = ['<option value="">（未定位）</option>'].concat(S.topo.switches.map(s =>
      `<option value="${esc(s.id)}" ${n.attach.switch === s.id ? 'selected' : ''}>${esc(s.name || s.id)}</option>`)).join('');
    const flags = (n.flags || []);
    const flagChecks = Object.keys(TopoFlags).concat(['arp_only']).map(f =>
      `<label><input type="checkbox" name="flag" value="${f}" ${flags.includes(f) ? 'checked' : ''}> ${TopoFlags[f] || '仅 ARP'}</label>`).join('');
    box.innerHTML = head(isNew ? '新增节点' : '节点 · ' + esc(n.ip || n.mac || n.id)) +
      (isNew ? '' : `<div id="liveBox" data-id="${esc(n.id)}">${liveHTML(n.id)}</div>`) +
      `<form id="nf">
        <div class="row2">
          <label class="f"><span>IP 地址</span><input type="text" name="ip" value="${esc(n.ip || '')}" placeholder="192.168.1.10"></label>
          <label class="f"><span>MAC</span><input type="text" name="mac" value="${esc(n.mac || '')}"></label>
          <label class="f"><span>名称${n.hostname ? `<span class="muted">（主机名 ${esc(n.hostname)}）</span>` : ''}</span><input type="text" name="name" value="${esc(n.name || '')}" placeholder="${n.hostname ? '留空时显示主机名 ' + esc(n.hostname) : '可空'}"></label>
          <label class="f"><span>厂商</span><input type="text" name="vendor" value="${esc(n.vendor || '')}" list="vendorList"></label>
          <label class="f" title="网段扫描按 MAC 发现设备换了 IP 时：自动跟踪 / DHCP 会直接更新 IP，静态锁定只提示"><span>IP 分配方式</span><select name="ip_mode">
            ${['', 'dhcp', 'static'].map(v => `<option value="${v}" ${(n.ip_mode || '') === v ? 'selected' : ''}>${{ '': '自动跟踪（默认）', dhcp: 'DHCP 自动获取', static: '静态 IP（锁定）' }[v]}</option>`).join('')}</select></label>
          <label class="f"><span>ID</span><input type="text" name="id" value="${esc(n.id || '')}" placeholder="留空自动生成"></label>
        </div>
        <datalist id="vendorList">${S.vendorList.map(v => `<option value="${esc(v)}">`).join('')}</datalist>
        <div class="row2">
          <label class="f"><span>接入交换机</span><select name="sw">${swOpts}</select></label>
          <label class="f"><span>端口</span><select name="portSel"></select>
            <input type="text" name="port" class="hidden" value="${esc(n.attach.port || '')}" placeholder="输入端口名，如 Gi0/12" style="margin-top:4px"></label>
        </div>
        <label class="f"><span>物理位置（机柜 / U 位 / 房间）</span><input type="text" name="location" value="${esc(n.location || '')}"></label>
        <label class="f"><span>标记</span></label><div class="checks" style="margin:-4px 0 8px">${flagChecks}</div>
        <label class="f"><span>标签（逗号分隔）</span><input type="text" name="tags" value="${esc((n.tags || []).join(', '))}"></label>
        <label class="f"><span>备注</span><textarea name="note" rows="2">${esc(n.note || '')}</textarea></label>
        <div class="checks"><label><input type="checkbox" name="locked" ${n.locked ? 'checked' : ''}> 上锁保护（不参与一键清理）</label></div>
        ${monFields(n.monitor, pingDefaults)}
        <div class="btns">
          <button class="btn primary" type="submit">${isNew ? '添加' : '保存'}</button>
          ${isNew ? '' : '<button class="btn danger" type="button" id="nDel">删除</button>'}
          ${!isNew && n.position ? '<button class="btn" type="button" id="nAuto" title="清除手动坐标，回到端口分组中">回到自动排布</button>' : ''}
          ${!isNew && n.ip ? '<button class="btn" type="button" id="nAI" title="让 AI 结合实时数据和历史曲线诊断这台设备">AI 诊断</button>' : ''}
          <button class="btn" type="button" id="nCancel">关闭</button>
        </div>
      </form>`;
    const f = $('#nf');
    // 端口随“接入交换机”联动：列出该交换机的端口，并标出已接设备 / 级联口 / 端口位置
    const CUSTOM = '\u0000custom';
    const portSel = f.elements.portSel, portIn = f.elements.port;
    let autoLoc = null; // 上一次由端口位置对照自动填入的物理位置
    const fillPorts = keep => {
      const swID = f.elements.sw.value;
      const sw = S.topo.switches.find(x => x.id === swID);
      const cur = keep ? portIn.value.trim() : '';
      if (!sw) {
        portSel.innerHTML = '<option value="">（先选择接入交换机）</option>';
        portSel.disabled = true;
        portIn.value = '';
        portIn.classList.add('hidden');
        return;
      }
      portSel.disabled = false;
      const used = {}, links = {};
      S.topo.nodes.forEach(x => { if (x.id !== n.id && x.attach.switch === swID && x.attach.port) used[x.attach.port] = (used[x.attach.port] || 0) + 1; });
      S.topo.links.forEach(l => {
        if (l.from.switch === swID) links[l.from.port] = l.to.switch;
        if (l.to.switch === swID) links[l.to.port] = l.from.switch;
      });
      const swName = id => { const o = S.topo.switches.find(x => x.id === id); return o ? (o.name || o.id) : id; };
      const ports = (sw.ports || []).slice();
      Object.keys(used).forEach(p => { if (!ports.includes(p)) ports.push(p); });
      const label = p => {
        const bits = [];
        if (links[p]) bits.push('级联 ' + swName(links[p]));
        if (used[p]) bits.push(`已接 ${used[p]} 台`);
        const loc = sw.port_locations && sw.port_locations[p];
        if (loc) bits.push(loc);
        return p + (bits.length ? '  · ' + bits.join(' · ') : '');
      };
      const inList = !cur || ports.includes(cur);
      portSel.innerHTML = '<option value="">（端口未知）</option>' +
        ports.map(p => `<option value="${esc(p)}">${esc(label(p))}</option>`).join('') +
        `<option value="${CUSTOM}">其他端口（手动输入）…</option>`;
      portSel.value = inList ? cur : CUSTOM;
      portIn.value = cur;
      portIn.classList.toggle('hidden', inList);
    };
    portSel.onchange = () => {
      const custom = portSel.value === CUSTOM;
      portIn.classList.toggle('hidden', !custom);
      if (custom) { portIn.value = ''; portIn.focus(); return; }
      portIn.value = portSel.value;
      // 选了端口：交换机有端口位置对照时，自动填入物理位置（不覆盖手工填写的内容）
      const sw = S.topo.switches.find(x => x.id === f.elements.sw.value);
      const loc = sw && sw.port_locations && sw.port_locations[portSel.value];
      const locIn = f.elements.location;
      if (loc && (!locIn.value.trim() || locIn.value === autoLoc)) { locIn.value = loc; autoLoc = loc; }
    };
    // 选了交换机就取消“未定位”标记，改回未定位则勾上
    const syncUnlocated = () => {
      const cb = $('input[name=flag][value=unlocated]', f);
      if (cb) cb.checked = !f.elements.sw.value;
    };
    f.elements.sw.onchange = () => {
      // 新交换机上也有同名端口时保留，否则清空
      const sw = S.topo.switches.find(x => x.id === f.elements.sw.value);
      const keep = sw && (sw.ports || []).includes(portIn.value.trim());
      fillPorts(keep);
      syncUnlocated();
      if (f.closest('#detailSec')) f.closest('#detailSec').dataset.dirty = '1';
    };
    fillPorts(true);
    f.onsubmit = async ev => {
      ev.preventDefault();
      const e = f.elements;
      const body = {
        id: e.id.value.trim(), ip: e.ip.value.trim(), mac: e.mac.value.trim(), name: e.name.value.trim(), hostname: n.hostname || '', vendor: e.vendor.value.trim(), ip_mode: e.ip_mode.value,
        attach: { switch: e.sw.value, port: e.sw.value ? e.port.value.trim() : '' },
        location: e.location.value.trim(), note: e.note.value.trim(), locked: e.locked.checked,
        flags: $$('input[name=flag]:checked', f).map(x => x.value),
        tags: e.tags.value.split(/[,，]/).map(s => s.trim()).filter(Boolean),
        monitor: readMon(f),
        position: isNew ? null : ((S.topo.nodes.find(x => x.id === n.id) || n).position || null),
      };
      if (body.attach.switch) body.flags = body.flags.filter(x => x !== 'unlocated');
      if (isNew && !body.attach.switch && view) {
        const c = view.viewCenter();
        body.position = { x: Math.round(c.x), y: Math.round(c.y) };
      }
      try {
        const r = await api(isNew ? 'POST' : 'PUT', isNew ? 'api/nodes' : 'api/nodes/' + encodeURIComponent(n.id), body);
        toast(isNew ? '已添加' : '已保存');
        S.sel = { kind: 'node', id: r.id };
        if (view) view.setSelection(S.sel);
        markClean(); await loadTopology();
      } catch (err) { toast(errMsg(err), true); }
    };
    $('#nCancel').onclick = () => { S.sel = null; if (view) view.setSelection(null); renderDetail(); };
    if (!isNew) {
      $('#nDel').onclick = async () => {
        if (!confirm(`删除节点 ${n.ip || n.id}？（历史数据保留到过期）`)) return;
        try { await api('DELETE', 'api/nodes/' + encodeURIComponent(n.id)); S.sel = null; toast('已删除'); markClean(); await loadTopology(); }
        catch (err) { toast(errMsg(err), true); }
      };
      const ai = $('#nAI');
      if (ai) ai.onclick = () => { location.hash = '#/ai/' + encodeURIComponent(n.id); };
      const au = $('#nAuto');
      if (au) au.onclick = async () => {
        try { await api('POST', 'api/positions', { nodes: { [n.id]: null } }); markClean(); await loadTopology(); }
        catch (err) { toast(errMsg(err), true); }
      };
    }
  }

  function switchForm(box, head, sel) {
    const isNew = sel.kind === 'newSwitch';
    const s = isNew ? { role: 'access', node_side: 'right', node_columns: 2, ports: [] } : S.topo.switches.find(x => x.id === sel.id);
    if (!s) { box.innerHTML = head('详情'); return; }
    const opt = (list, cur, labels) => list.map(v => `<option value="${v}" ${cur === v ? 'selected' : ''}>${labels ? labels[v] : v}</option>`).join('');
    const attached = isNew ? 0 : S.topo.nodes.filter(n => n.attach.switch === s.id).length;
    box.innerHTML = head(isNew ? '新增交换机' : '交换机 · ' + esc(s.name || s.id)) +
      (!isNew && s.ip ? `<div id="liveBox" data-id="${esc(s.id)}">${liveHTML(s.id)}</div>` : '') +
      `<form id="sf">
        <div class="row2">
          <label class="f"><span>名称</span><input type="text" name="name" value="${esc(s.name || '')}" required></label>
          <label class="f"><span>ID</span><input type="text" name="id" value="${esc(s.id || '')}" placeholder="留空自动生成"></label>
          <label class="f"><span>角色</span><select name="role">${opt(['core', 'aggregation', 'access', 'other'], s.role, { core: '核心', aggregation: '汇聚', access: '接入', other: '其他' })}</select></label>
          <label class="f"><span>层级</span><input type="text" name="layer" value="${esc(s.layer || '')}" placeholder="L2 / L3"></label>
          <label class="f"><span>厂商</span><input type="text" name="vendor" value="${esc(s.vendor || '')}"></label>
          <label class="f"><span>型号</span><input type="text" name="model" value="${esc(s.model || '')}"></label>
          <label class="f"><span>管理 IP（填写后监控）</span><input type="text" name="ip" value="${esc(s.ip || '')}"></label>
          <label class="f"><span>MAC</span><input type="text" name="mac" value="${esc(s.mac || '')}"></label>
        </div>
        <label class="f"><span>物理位置</span><input type="text" name="location" value="${esc(s.location || '')}"></label>
        <label class="f"><span>端口列表（空格或逗号分隔，支持范围写法如 Gi0/1-24 Te0/25-28）</span>
          <textarea name="ports" rows="3" class="mono">${esc((s.ports || []).join(' '))}</textarea></label>
        <label class="f"><span>端口位置对照（每行“端口 位置”，可空；端口定位把设备挂到该口时自动填入设备的物理位置）</span>
          <textarea name="port_locations" rows="3" class="mono" placeholder="12 A03 机柜 配线架 2-12">${esc(Object.keys(s.port_locations || {}).sort(natCmp).map(k => k + ' ' + s.port_locations[k]).join('\n'))}</textarea></label>
        <div class="row2">
          <label class="f"><span>终端显示在</span><select name="node_side">${opt(['left', 'right'], s.node_side, { left: '左侧', right: '右侧' })}</select></label>
          <label class="f"><span>每端口卡片列数</span><input type="number" name="node_columns" min="1" max="6" value="${s.node_columns || 2}"></label>
        </div>
        <label class="f"><span>备注</span><textarea name="note" rows="2">${esc(s.note || '')}</textarea></label>
        ${monFields(s.monitor, pingDefaults)}
        <div class="btns">
          <button class="btn primary" type="submit">${isNew ? '添加' : '保存'}</button>
          ${isNew ? '' : `<button class="btn" type="button" id="sAddNode">+ 下挂节点</button>
          <button class="btn" type="button" id="sAuto">端口自动排布</button>
          ${s.ip ? '<button class="btn" type="button" id="sAI">AI 诊断</button>' : ''}
          <button class="btn danger" type="button" id="sDel">删除</button>`}
          <button class="btn" type="button" id="sCancel">关闭</button>
        </div>
        ${isNew ? '' : `<div class="help">下挂节点 ${attached} 个。拖动拓扑图中交换机的标题区域可移动位置。</div>`}
      </form>`;
    const f = $('#sf');
    f.onsubmit = async ev => {
      ev.preventDefault();
      const e = f.elements;
      const body = {
        id: e.id.value.trim(), name: e.name.value.trim(), role: e.role.value, layer: e.layer.value.trim(), vendor: e.vendor.value.trim(),
        model: e.model.value.trim(), ip: e.ip.value.trim(), mac: e.mac.value.trim(), location: e.location.value.trim(),
        ports: expandPorts(e.ports.value), node_side: e.node_side.value, node_columns: parseInt(e.node_columns.value, 10) || 2,
        port_locations: parsePortLocations(e.port_locations.value),
        note: e.note.value.trim(), monitor: readMon(f), position: isNew ? null : ((S.topo.switches.find(x => x.id === s.id) || s).position || null),
      };
      if (isNew && view) { const c = view.viewCenter(); body.position = { x: Math.round(c.x - 75), y: Math.round(c.y - 100) }; }
      try {
        const r = await api(isNew ? 'POST' : 'PUT', isNew ? 'api/switches' : 'api/switches/' + encodeURIComponent(s.id), body);
        toast(isNew ? '已添加交换机' : '已保存');
        S.sel = { kind: 'switch', id: r.id };
        if (view) view.setSelection(S.sel);
        markClean(); await loadTopology();
      } catch (err) { toast(errMsg(err), true); }
    };
    $('#sCancel').onclick = () => { S.sel = null; if (view) view.setSelection(null); renderDetail(); };
    if (!isNew) {
      $('#sAddNode').onclick = () => { S.sel = { kind: 'newNode', sw: s.id }; renderDetail(); };
      const sai = $('#sAI');
      if (sai) sai.onclick = () => { location.hash = '#/ai/' + encodeURIComponent(s.id); };
      $('#sAuto').onclick = async () => {
        try { const r = await api('POST', 'api/layout/reset', { switch: s.id }); await loadTopology(); toast(`已重置 ${r.reset} 个节点`); }
        catch (err) { toast(errMsg(err), true); }
      };
      $('#sDel').onclick = async () => {
        if (!confirm(`删除交换机 ${s.name || s.id}？\n其下 ${attached} 个节点会变为“未定位”，相关链路会被删除。`)) return;
        try { await api('DELETE', 'api/switches/' + encodeURIComponent(s.id)); S.sel = null; toast('已删除'); markClean(); await loadTopology(); }
        catch (err) { toast(errMsg(err), true); }
      };
    }
  }

  function linkForm(box, head, sel) {
    const isNew = sel.kind === 'newLink';
    const l = isNew ? { from: { switch: '', port: '' }, to: { switch: '', port: '' }, kind: 'uplink', note: '' } : S.topo.links.find(x => x.id === sel.id);
    if (!l) { box.innerHTML = head('详情'); return; }
    const swOpts = cur => S.topo.switches.map(s => `<option value="${esc(s.id)}" ${cur === s.id ? 'selected' : ''}>${esc(s.name || s.id)}</option>`).join('');
    box.innerHTML = head(isNew ? '新增交换机间链路' : '链路 · ' + esc(l.from.port + ' ⇄ ' + l.to.port)) + `<form id="lf">
      <div class="help">“上游”一端填写靠近核心的交换机，告警时用于判断下游交换机故障。</div>
      <div class="row2">
        <label class="f"><span>上游交换机</span><select name="fs">${swOpts(l.from.switch)}</select></label>
        <label class="f"><span>上游端口</span><input type="text" name="fp" value="${esc(l.from.port)}" required></label>
        <label class="f"><span>下游交换机</span><select name="ts">${swOpts(l.to.switch)}</select></label>
        <label class="f"><span>下游端口</span><input type="text" name="tp" value="${esc(l.to.port)}" required></label>
      </div>
      <div class="row2">
        <label class="f"><span>类型</span><select name="kind">${['uplink', 'stack', 'other'].map(k => `<option ${l.kind === k ? 'selected' : ''}>${k}</option>`).join('')}</select></label>
        <label class="f"><span>备注</span><input type="text" name="note" value="${esc(l.note || '')}"></label>
      </div>
      <div class="btns"><button class="btn primary" type="submit">${isNew ? '添加' : '保存'}</button>
        ${isNew ? '' : '<button class="btn danger" type="button" id="lDel">删除</button>'}
        <button class="btn" type="button" id="lCancel">关闭</button></div>
    </form>`;
    const f = $('#lf');
    if (isNew && S.topo.switches.length > 1) f.elements.ts.selectedIndex = 1;
    f.onsubmit = async ev => {
      ev.preventDefault();
      const e = f.elements;
      const body = { id: isNew ? '' : l.id, from: { switch: e.fs.value, port: e.fp.value.trim() }, to: { switch: e.ts.value, port: e.tp.value.trim() }, kind: e.kind.value, note: e.note.value.trim() };
      try {
        await api(isNew ? 'POST' : 'PUT', isNew ? 'api/links' : 'api/links/' + encodeURIComponent(l.id), body);
        toast('已保存'); S.sel = null; markClean(); await loadTopology();
      } catch (err) { toast(errMsg(err), true); }
    };
    $('#lCancel').onclick = () => { S.sel = null; if (view) view.setSelection(null); renderDetail(); };
    if (!isNew) $('#lDel').onclick = async () => {
      if (!confirm('删除这条链路？')) return;
      try { await api('DELETE', 'api/links/' + encodeURIComponent(l.id)); S.sel = null; markClean(); await loadTopology(); }
      catch (err) { toast(errMsg(err), true); }
    };
  }

  // ---------------------------------------------------------------------------
  // 监控列表
  function listPage(app) {
    app.innerHTML = `<div class="page pad">
      <div class="toolbar">
        <input type="search" id="lq" placeholder="筛选 IP / MAC / 厂商 / 位置" value="${esc(S.listFilter)}">
        <select id="ls"><option value="">全部状态</option>${Object.keys(STATE_TXT).map(k => `<option value="${k}" ${S.listState === k ? 'selected' : ''}>${STATE_TXT[k]}</option>`).join('')}<option value="abn" ${S.listState === 'abn' ? 'selected' : ''}>异常（离线/丢包/延迟）</option></select>
        <span class="muted" id="lcnt"></span>
        <button class="btn small danger" id="lCleanup">一键清理无效设备</button>
        <span style="margin-left:auto" class="muted" id="lalert"></span>
        <button class="btn small" id="bulkOn" title="对当前筛选出的所有可监控设备开启告警">筛选结果开启告警</button>
        <button class="btn small" id="bulkOff">筛选结果关闭告警</button>
      </div>
      <table class="grid"><thead><tr>
        <th data-k="state">状态</th><th data-k="ip">IP</th><th data-k="vendor">名称 / 厂商</th><th data-k="loc">接线位置</th>
        <th data-k="last" class="num">当前</th><th data-k="avg1" class="num">1m 平均</th><th data-k="jit" class="num">抖动</th>
        <th data-k="l1" class="num">1m 丢包</th><th data-k="l5" class="num">5m 丢包</th><th data-k="l60" class="num">1h 丢包</th>
        <th>最近 60 次</th><th data-k="since" class="num">状态持续</th><th data-k="alert" title="是否推送企业微信告警">告警</th><th title="上锁后不参与一键清理">上锁保护</th>
      </tr></thead><tbody id="lbody"></tbody></table>
      <dialog id="lCleanupDialog" class="cleanup-dialog">
        <div class="ph">清理无效设备</div>
        <div class="pb">
          <p class="help">清理全部终端中无 IP 或明确关闭监控的设备，不受列表筛选影响。已上锁设备和交换机始终保留；历史数据保留到过期。扫描任务以后仍可能重新发现并添加设备。</p>
          <p id="lCleanupSummary"></p>
          <div class="cleanup-items"><table class="grid"><thead><tr><th>设备</th><th>IP</th><th>清理原因</th></tr></thead><tbody id="lCleanupItems"></tbody></table></div>
          <div class="btns"><button type="button" class="btn danger" id="lCleanupConfirm">确认清理</button><button type="button" class="btn" id="lCleanupCancel">取消</button></div>
          <div class="help" id="lCleanupError" role="alert"></div>
        </div>
      </dialog>
    </div>`;
    $('#lq').oninput = e => { S.listFilter = e.target.value.trim().toLowerCase(); draw(); };
    $('#ls').onchange = e => { S.listState = e.target.value; draw(); };
    $$('th[data-k]').forEach(th => th.onclick = () => {
      const k = th.dataset.k;
      S.listSort = { key: k, dir: S.listSort.key === k ? -S.listSort.dir : 1 };
      draw();
    });
    const ipNum = ip => (ip || '').split('.').reduce((a, b) => a * 256 + (+b || 0), 0);
    const order = { down: 0, loss: 1, slow: 2, unknown: 3, ok: 4, paused: 5, disabled: 6 };
    function rows() {
      if (!S.topo) return [];
      const out = [];
      S.topo.switches.forEach(s => { if (s.ip) out.push({ id: s.id, ip: s.ip, name: s.name, vendor: s.vendor, loc: '交换机', mac: s.mac, sw: true, alert: !!(s.monitor && s.monitor.alert) }); });
      S.topo.nodes.forEach(n => out.push({ id: n.id, ip: n.ip, name: n.name || n.hostname, vendor: n.vendor, loc: attachLabel(n), mac: n.mac, flags: n.flags, locked: !!n.locked, alert: !!(n.monitor && n.monitor.alert) }));
      return out;
    }
    function draw() {
      const q = S.listFilter;
      let list = rows().map(r => Object.assign(r, { st: S.status[r.id] }));
      list = list.filter(r => {
        const state = r.st ? r.st.state : 'disabled';
        if (S.listState === 'abn' ? !['down', 'loss', 'slow'].includes(state) : (S.listState && state !== S.listState)) return false;
        if (!q) return true;
        return [r.ip, r.name, r.vendor, r.loc, r.mac].join(' ').toLowerCase().includes(q);
      });
      const k = S.listSort.key, dir = S.listSort.dir;
      const val = r => {
        const st = r.st || {};
        switch (k) {
          case 'state': return order[st.state || 'disabled'];
          case 'ip': return ipNum(r.ip);
          case 'vendor': return (r.name || r.vendor || '');
          case 'loc': return r.loc;
          case 'last': return st.last_rtt == null ? 1e9 : (st.last_rtt < 0 ? 1e8 : st.last_rtt);
          case 'avg1': return st.avg_1m || 0;
          case 'jit': return st.jitter_1m || 0;
          case 'l1': return st.loss_1m || 0;
          case 'l5': return st.loss_5m || 0;
          case 'l60': return st.loss_1h || 0;
          case 'since': return st.state_since || 0;
          case 'alert': return r.alert ? 0 : 1;
        }
        return 0;
      };
      list.sort((a, b) => { const x = val(a), y = val(b); return (x < y ? -1 : x > y ? 1 : ipNum(a.ip) - ipNum(b.ip)) * dir; });
      $('#lcnt').textContent = `${list.length} 条`;
      lastList = list;
      const on = rows().filter(r => r.alert).length;
      $('#lalert').textContent = `已开告警 ${on} 台`;
      $('#lbody').innerHTML = list.map(r => {
        const st = r.st;
        const lock = r.sw ? '<td class="muted">始终保留</td>' : `<td class="lock-cell"><label title="${r.locked ? '已上锁，不参与一键清理' : '上锁后不参与一键清理'}"><input type="checkbox" data-lock="${esc(r.id)}" ${r.locked ? 'checked' : ''}> ${r.locked ? '已上锁' : '上锁'}</label></td>`;
        if (!st) {
          return `<tr data-id="${esc(r.id)}"><td>${stateBadge('disabled')}</td><td class="mono">${esc(r.ip || '无 IPv4')}</td><td>${esc(r.name || r.vendor || '')}</td><td>${esc(r.loc)}</td><td colspan="8" class="muted">${r.ip ? '未启用监控' : '无 IP，仅登记'}</td><td></td>${lock}</tr>`;
        }
        const warn = st.latency_warn_ms;
        return `<tr data-id="${esc(r.id)}">
          <td>${stateBadge(st.state)}</td>
          <td class="mono"><b>${esc(r.ip)}</b></td>
          <td>${esc(r.name || '')}${r.name && r.vendor ? ' · ' : ''}<span class="muted">${esc(r.vendor || '')}</span></td>
          <td>${esc(r.loc)}</td>
          <td class="num ${st.last_rtt < 0 ? 'v-down' : st.last_rtt >= warn ? 'v-warn' : ''}">${fmtMs(st.last_rtt)}</td>
          <td class="num ${st.avg_1m >= warn ? 'v-warn' : ''}">${st.avg_1m ? fmtMs(st.avg_1m) : '-'}</td>
          <td class="num">${st.jitter_1m ? fmtMs(st.jitter_1m) : '-'}</td>
          <td class="num ${lossCls(st.loss_1m, st.loss_warn_pct)}">${pct(st.loss_1m)}</td>
          <td class="num ${lossCls(st.loss_5m, st.loss_warn_pct)}">${pct(st.loss_5m)}</td>
          <td class="num ${lossCls(st.loss_1h, st.loss_warn_pct)}">${pct(st.loss_1h)}</td>
          <td>${Charts.spark(st.spark, 130, 20, warn)}</td>
          <td class="num">${fmtAgo(st.state_since)}</td>
          <td class="alert-cell"><label><input type="checkbox" data-alert="${esc(r.id)}" ${r.alert ? 'checked' : ''}></label></td>${lock}</tr>`;
      }).join('');
    }
    let lastList = [];
    async function setAlert(ids, enabled) {
      try {
        const r = await api('POST', 'api/alerting', { ids, enabled });
        toast(`${enabled ? '已开启' : '已关闭'} ${r.changed} 台设备的告警`);
        await loadTopology();
      } catch (e) { toast(errMsg(e), true); }
    }
    $('#lbody').onclick = e => {
      const lock = e.target.closest('input[data-lock]');
      if (lock) { setLocked(lock); return; }
      if (e.target.closest('.lock-cell')) return;
      const cb = e.target.closest('input[data-alert]');
      if (cb) { setAlert([cb.dataset.alert], cb.checked); return; }
      if (e.target.closest('.alert-cell')) return;
      const tr = e.target.closest('tr[data-id]');
      if (tr) location.hash = '#/node/' + encodeURIComponent(tr.dataset.id);
    };
    const bulk = enabled => {
      const ids = lastList.filter(r => r.ip && r.alert !== enabled).map(r => r.id);
      if (!ids.length) { toast('筛选结果中没有需要修改的设备'); return; }
      if (!confirm(`对筛选出的 ${ids.length} 台设备${enabled ? '开启' : '关闭'}企业微信告警？`)) return;
      setAlert(ids, enabled);
    };
    $('#bulkOn').onclick = () => bulk(true);
    $('#bulkOff').onclick = () => bulk(false);
    async function setLocked(cb) {
      cb.disabled = true;
      try {
        await api('PUT', 'api/nodes/' + encodeURIComponent(cb.dataset.lock) + '/lock', { locked: cb.checked });
        toast(cb.checked ? '已上锁，一键清理会保留此设备' : '已解除上锁');
        await loadTopology().catch(e => toast('上锁设置已保存，列表刷新失败：' + errMsg(e), true));
      } catch (e) { cb.checked = !cb.checked; toast(errMsg(e), true); }
      finally { cb.disabled = false; }
    }
    const cleanupDialog = $('#lCleanupDialog');
    let cleanupIDs = [], cleaning = false;
    $('#lCleanup').onclick = async ev => {
      const button = ev.currentTarget;
      button.disabled = true;
      try {
        const preview = await api('GET', 'api/nodes/cleanup');
        cleanupIDs = preview.items.map(n => n.id);
        $('#lCleanupSummary').textContent = `待清理 ${cleanupIDs.length} 台，已上锁保护 ${preview.protected} 台。请确认以下清单：`;
        $('#lCleanupItems').innerHTML = preview.items.map(n => `<tr><td>${esc(n.name)}</td><td class="mono">${esc(n.ip || '无 IP')}</td><td>${esc(n.reason)}</td></tr>`).join('') || '<tr><td colspan="3" class="muted">没有需要清理的设备</td></tr>';
        $('#lCleanupConfirm').disabled = !cleanupIDs.length;
        $('#lCleanupConfirm').textContent = cleanupIDs.length ? `确认清理 ${cleanupIDs.length} 台` : '确认清理';
        $('#lCleanupError').textContent = '';
        cleanupDialog.showModal();
      } catch (e) { toast(errMsg(e), true); }
      finally { button.disabled = false; }
    };
    $('#lCleanupCancel').onclick = () => cleanupDialog.close();
    cleanupDialog.oncancel = ev => { if (cleaning) ev.preventDefault(); };
    $('#lCleanupConfirm').onclick = async () => {
      cleaning = true;
      $('#lCleanupConfirm').disabled = true;
      $('#lCleanupCancel').disabled = true;
      try {
        const result = await api('POST', 'api/nodes/cleanup', { ids: cleanupIDs });
        cleanupDialog.close();
        toast(`已清理 ${result.changed} 台设备${result.skipped ? `，${result.skipped} 台已变化或受保护，予以保留` : ''}`);
        await loadTopology().catch(e => toast('清理已完成，列表刷新失败：' + errMsg(e), true));
      } catch (e) { $('#lCleanupError').textContent = errMsg(e); }
      finally {
        cleaning = false;
        $('#lCleanupConfirm').disabled = !cleanupIDs.length;
        $('#lCleanupCancel').disabled = false;
      }
    };
    // 告警复选框被重绘会丢失点击，悬停在列表上时降低刷新频率
    let hover = false;
    $('#lbody').onmouseenter = () => { hover = true; };
    $('#lbody').onmouseleave = () => { hover = false; };
    let tick = 0;
    draw();
    return { update: () => { if (!hover || ++tick % 5 === 0) draw(); }, onTopology: draw };
  }

  // ---------------------------------------------------------------------------
  // 节点详情（历史曲线）
  function nodePage(app, id) {
    let range = sessionStorage.getItem('np_range') || '1h';
    let timer = 0, alive = true;
    app.innerHTML = `<div class="page pad">
      <div class="detail-head">
        <a href="javascript:history.back()" class="btn small">← 返回</a>
        <h2 id="dTitle">${esc(id)}</h2><div id="dSub" class="muted"></div>
        <div style="margin-left:auto" class="btns" id="ranges"></div>
        <a class="btn small" href="#/ai/${encodeURIComponent(id)}" title="让 AI 结合实时数据和历史曲线诊断这台设备">AI 诊断</a>
      </div>
      <div class="tiles" id="dTiles"></div>
      <div class="panel"><div class="ph">延迟<span class="muted" style="font-weight:normal">橙线为平均值，浅色区间为最小~最大</span><span class="r muted" id="dBucket"></span></div><div class="pb"><div id="cLat"></div></div></div>
      <div class="panel"><div class="ph">丢包率<span class="muted" style="font-weight:normal">红色为全部丢失，橙色为部分丢失</span></div><div class="pb"><div id="cLoss"></div></div></div>
    </div>`;
    const R = [['15m', '15 分钟'], ['1h', '1 小时'], ['6h', '6 小时'], ['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天']];
    function drawRanges() {
      $('#ranges').innerHTML = R.map(([k, t]) => `<button class="chip${k === range ? ' on' : ''}" data-r="${k}">${t}</button>`).join('');
      $$('#ranges .chip').forEach(b => b.onclick = () => { range = b.dataset.r; sessionStorage.setItem('np_range', range); drawRanges(); load(); });
    }
    function drawHead() {
      const t = findTarget(id);
      if (!t) { $('#dSub').textContent = '（拓扑中已不存在该节点）'; return; }
      const o = t.obj;
      $('#dTitle').textContent = o.ip || o.mac || o.id;
      const flags = (o.flags || []).map(f => TopoFlags[f] || f);
      $('#dSub').innerHTML = [o.name, o.hostname && o.hostname !== o.name ? '主机名 ' + o.hostname : '', o.vendor, t.kind === 'switch' ? '交换机' : attachLabel(o), o.mac, o.location].filter(Boolean).map(esc).join(' · ') +
        (flags.length ? ' ' + flags.map(f => `<span class="badge solid">${esc(f)}</span>`).join(' ') : '') +
        ' ' + (o.monitor && o.monitor.alert ? '<span class="badge" style="color:var(--accent)">告警已开启</span>' : '<span class="badge unknown">不告警</span>');
    }
    function drawTiles() {
      const st = S.status[id];
      if (!st) { $('#dTiles').innerHTML = '<div><div class="k">状态</div><div class="v">未监控</div></div>'; return; }
      const tile = (k, v, c) => `<div><div class="k">${k}</div><div class="v ${c || ''}">${v}</div></div>`;
      $('#dTiles').innerHTML = tile('状态', stateBadge(st.state)) + tile('当前延迟', fmtMs(st.last_rtt), st.last_rtt < 0 ? 'v-down' : '') +
        tile('1m 平均', st.avg_1m ? fmtMs(st.avg_1m) : '-') + tile('1m 最小/最大', st.avg_1m ? `${fmtMs(st.min_1m)}/${fmtMs(st.max_1m)}` : '-') +
        tile('抖动', st.jitter_1m ? fmtMs(st.jitter_1m) : '-') + tile('1h 平均', st.avg_1h ? fmtMs(st.avg_1h) : '-') +
        tile('1m 丢包', pct(st.loss_1m), lossCls(st.loss_1m, st.loss_warn_pct)) + tile('5m 丢包', pct(st.loss_5m), lossCls(st.loss_5m, st.loss_warn_pct)) +
        tile('1h 丢包', pct(st.loss_1h), lossCls(st.loss_1h, st.loss_warn_pct)) + tile('状态持续', fmtAgo(st.state_since));
    }
    async function load() {
      clearTimeout(timer);
      try {
        const d = await api('GET', `api/history/${encodeURIComponent(id)}?range=${range}`);
        if (!alive) return;
        const st = S.status[id];
        Charts.latency($('#cLat'), d, st ? st.latency_warn_ms : null);
        Charts.loss($('#cLoss'), d);
        const b = d.bucket_ms;
        $('#dBucket').textContent = '每点 ' + (b >= 60000 ? b / 60000 + ' 分钟' : b / 1000 + ' 秒');
      } catch (e) { toast(errMsg(e), true); }
      if (alive) timer = setTimeout(load, ['15m', '1h'].includes(range) ? 5000 : 60000);
    }
    drawRanges(); drawHead(); drawTiles(); load();
    const onResize = () => load();
    window.addEventListener('resize', onResize);
    return {
      update: drawTiles, onTopology: drawHead,
      destroy() { alive = false; clearTimeout(timer); window.removeEventListener('resize', onResize); },
    };
  }

  // ---------------------------------------------------------------------------
  // 告警历史
  function alertsPage(app) {
    let timer = 0;
    app.innerHTML = `<div class="page pad">
      <div class="toolbar"><b>告警记录</b><span class="muted">最近 300 条；企业微信通道在「设置」中配置</span>
        <span style="margin-left:auto" id="aState"></span></div>
      <table class="grid"><thead><tr><th>时间</th><th>类型</th><th>IP</th><th>名称</th><th>位置</th><th>说明</th><th>发送</th></tr></thead><tbody id="abody"></tbody></table>
    </div>`;
    async function load() {
      try {
        const list = await api('GET', 'api/alerts');
        $('#abody').innerHTML = list.length ? list.map(e => {
          const [t, c] = EVENT_TXT[e.type] || [e.type, 'unknown'];
          return `<tr data-id="${esc(e.target_id || '')}"><td class="mono">${esc(Charts.fmtFull(new Date(e.time).getTime()))}</td><td><span class="badge ${c}">${t}</span></td>
            <td class="mono">${esc(e.ip || '')}</td><td>${esc(e.name || '')}</td><td>${esc(e.location || '')}</td><td class="wrap">${esc(e.detail || '')}</td>
            <td>${e.sent ? '<span class="v-ok">已发送</span>' : e.error ? `<span class="v-down" title="${esc(e.error)}">失败</span>` : '<span class="muted">未发送</span>'}</td></tr>`;
        }).join('') : '<tr><td colspan="7" class="empty">暂无告警记录</td></tr>';
      } catch (e) { toast(errMsg(e), true); }
      timer = setTimeout(load, 10000);
    }
    $('#abody').onclick = e => { const tr = e.target.closest('tr[data-id]'); if (tr && tr.dataset.id) location.hash = '#/node/' + encodeURIComponent(tr.dataset.id); };
    api('GET', 'api/config').then(c => {
      const on = S.topo ? S.topo.nodes.filter(n => n.monitor && n.monitor.alert).length + S.topo.switches.filter(x => x.monitor && x.monitor.alert).length : 0;
      $('#aState').innerHTML = (c.alert.enabled ? '<span class="badge ok">企业微信告警已启用</span>' : '<span class="badge down">企业微信告警未启用</span> <a href="#/settings">去设置</a>') +
        ` <span class="muted">· 已开告警设备 ${on} 台（设备默认不告警，可在 <a href="#/list">监控列表</a> 中开启）· ${esc(c.schedule.enabled ? '监控时段：' + describeSchedule(c.schedule) : '全天候监控')}</span>`;
    }).catch(() => {});
    load();
    return { destroy() { clearTimeout(timer); } };
  }

  // ---------------------------------------------------------------------------
  // 设置
  function settingsPage(app) {
    app.innerHTML = `<div class="page pad"><div class="settings">
      <div>
        <div class="panel"><div class="ph">企业微信机器人告警</div><div class="pb"><form id="af"></form></div></div>
        <div class="panel"><div class="ph">监控时段<span class="r" id="schNow"></span></div><div class="pb"><form id="schf"></form></div></div>
        <div class="panel"><div class="ph">默认 Ping 参数</div><div class="pb"><form id="pf"></form></div></div>
        <div class="panel"><div class="ph">机房信息</div><div class="pb"><form id="sitef"></form></div></div>
        <div class="panel"><div class="ph">登录密码<span class="r"><button class="btn small" id="logout">退出登录</button></span></div><div class="pb"><form id="pwf">
          <div class="row3">
            <label class="f"><span>当前密码</span><input type="password" name="old" autocomplete="current-password" required></label>
            <label class="f"><span>新密码（至少 6 位）</span><input type="password" name="p1" autocomplete="new-password" minlength="6" required></label>
            <label class="f"><span>再输一次</span><input type="password" name="p2" autocomplete="new-password" minlength="6" required></label>
          </div>
          <div class="btns"><button class="btn primary" type="submit">修改密码</button></div>
          <div class="help">打开管理页面需要输入这个密码，登录后浏览器 30 天内免登录。修改密码后，其他已登录的浏览器都需要重新登录。忘记密码时，删除 config.yaml 中 auth 下的 password_hash 和 password 两行并重启 netPing，再在运行 netPing 的电脑上打开网页重新设置。</div>
        </form></div></div>
        <div class="panel"><div class="ph">关于</div><div class="pb" id="about"></div></div>
      </div>
      <div>
        <div class="panel"><div class="ph">拓扑文件 topology.yaml<span class="r btns" style="margin:0">
          <button class="btn small" id="yLoad">重新读取</button>
          <a class="btn small" href="api/topology/yaml?download=1">下载</a>
          <button class="btn small" id="yUpload">导入文件…</button><input type="file" id="yFile" accept=".yaml,.yml" class="hidden">
          <button class="btn small" id="yReload" title="程序外部（如 AI / 文本编辑器）修改了文件后，点此载入">从磁盘重新加载</button>
          <button class="btn small primary" id="ySave">校验并保存</button></span></div>
          <div class="pb">
            <div id="yBanner"></div>
            <div class="help">文件头部注释说明了全部字段，可交给 AI 按新机房的 MAC 表 / ARP 表批量生成节点。保存前会校验 id 唯一、IP 格式、交换机引用等。</div>
            <textarea id="yText" spellcheck="false"></textarea>
            <div class="problems" id="yProb"></div>
          </div></div>
      </div></div></div>`;

    let cfg = null;
    const num = (name, label, v, extra) => `<label class="f"><span>${label}</span><input type="number" name="${name}" value="${v}" ${extra || ''}></label>`;
    async function loadCfg() {
      cfg = await api('GET', 'api/config');
      const a = cfg.alert;
      $('#af').innerHTML = `
        <div class="checks" style="margin-bottom:8px"><label><input type="checkbox" name="enabled" ${a.enabled ? 'checked' : ''}> 启用告警推送</label>
          <label><input type="checkbox" name="send_recovery" ${a.send_recovery ? 'checked' : ''}> 发送恢复通知</label></div>
        <label class="f"><span>Webhook 地址（群聊 → 添加群机器人 → 复制 Webhook）</span><input type="text" name="webhook_url" value="${esc(a.webhook_url)}" placeholder="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=..."></label>
        <label class="f"><span>消息标题</span><input type="text" name="site_title" value="${esc(a.site_title)}"></label>
        <div class="row3">
          ${num('down_after_failures', '连续失败 N 次判离线', a.down_after_failures, 'min="1"')}
          ${num('up_after_successes', '连续成功 N 次判恢复', a.up_after_successes, 'min="1"')}
          ${num('merge_window_sec', '合并窗口（秒）', a.merge_window_sec, 'min="1"')}
          ${num('loss_duration_min', '丢包持续（分钟）', a.loss_duration_min, 'min="1"')}
          ${num('latency_duration_min', '延迟持续（分钟）', a.latency_duration_min, 'min="1"')}
          ${num('repeat_interval_min', '重复提醒（分钟，0=不重复）', a.repeat_interval_min, 'min="0"')}
        </div>
        <label class="f"><span>HTTP 代理（服务器不能直接上网时填写，可空）</span><input type="text" name="proxy_url" value="${esc(a.proxy_url || '')}" placeholder="http://10.0.0.1:3128"></label>
        <div class="checks"><label><input type="checkbox" name="tls_skip_verify" ${a.tls_skip_verify ? 'checked' : ''}> 跳过 HTTPS 证书校验（仅当老系统根证书过旧导致发送失败时使用）</label></div>
        <div class="btns"><button class="btn primary" type="submit">保存</button><button class="btn" type="button" id="aTest">发送测试消息</button></div>
        <div class="help">告警规则：离线（连续失败）、丢包（1 分钟丢包率超过节点阈值并持续）、延迟高（1 分钟平均超过阈值并持续）。同一交换机下多台同时离线会合并为一条“交换机故障”消息。</div>`;
      const p = cfg.ping;
      $('#pf').innerHTML = `<div class="row2">
          ${num('interval_ms', 'Ping 间隔 ms', p.interval_ms, 'min="200"')}${num('timeout_ms', '超时 ms', p.timeout_ms, 'min="100"')}
          ${num('packet_size', '数据长度 字节', p.packet_size, 'min="0"')}${num('retention_days', '历史保留天数', cfg.retention_days, 'min="1"')}
          ${num('latency_warn_ms', '延迟阈值 ms（1m 平均）', p.latency_warn_ms, 'min="1"')}${num('loss_warn_pct', '丢包阈值 %（1m）', p.loss_warn_pct, 'min="1" max="100"')}
        </div><div class="btns"><button class="btn primary" type="submit">保存</button></div>
        <div class="help">节点可在拓扑页单独覆盖这些参数。监听地址请直接编辑程序目录下的 config.yaml，重启后生效。</div>`;
      pingDefaults = p;
      wins = JSON.parse(JSON.stringify(cfg.schedule.windows || []));
      schEnabled = !!cfg.schedule.enabled;
      drawSchedule();
    }
    let wins = [];
    let schEnabled = false;
    function dayOn(w, d) { return !w.days || !w.days.length || w.days.includes(d); }
    function drawSchedule() {
      const rowsHTML = wins.map((w, i) => `<div class="win" data-i="${i}">
          <div class="days">${[1, 2, 3, 4, 5, 6, 7].map(d => `<label class="chip${dayOn(w, d) ? ' on' : ''}"><input type="checkbox" class="hidden" value="${d}" ${dayOn(w, d) ? 'checked' : ''}>${WD[d]}</label>`).join('')}</div>
          <input type="time" name="start" value="${esc(w.start)}" required> <span>至</span> <input type="time" name="end" value="${esc(w.end)}" required>
          <button type="button" class="btn small danger" data-del="${i}">删除</button></div>`).join('');
      $('#schf').innerHTML = `
        <div class="checks" style="margin-bottom:8px"><label><input type="checkbox" name="enabled" ${schEnabled ? 'checked' : ''}> 只在下列时段内监控（时段外暂停 ping 与告警）</label></div>
        <div id="winList">${rowsHTML || '<div class="help">尚未添加时段</div>'}</div>
        <div class="btns" style="margin-top:6px">
          <button type="button" class="btn small" data-preset="work">+ 工作日 08:00-18:00</button>
          <button type="button" class="btn small" data-preset="night">+ 每天 20:00-08:00</button>
          <button type="button" class="btn small" data-preset="blank">+ 自定义时段</button>
        </div>
        <div class="btns"><button class="btn primary" type="submit">保存</button></div>
        <div class="help">未勾选时全天候监控。结束时间早于开始时间表示跨过午夜（如 22:00 至 06:00），星期按时段开始的那天计算。手动暂停与监控时段相互独立。</div>`;
      $('#schNow').textContent = S.pauseReason === 'schedule' ? '当前：非监控时段' : (cfg.schedule.enabled ? '当前：监控时段内' : '');
    }
    function readWins() {
      return $$('#winList .win').map(row => {
        const days = $$('.days input:checked', row).map(x => +x.value);
        return { days: days.length === 7 ? [] : days, start: $('input[name=start]', row).value, end: $('input[name=end]', row).value };
      });
    }
    function loadSite() {
      const s = (S.topo && S.topo.site) || {};
      $('#sitef').innerHTML = `<div class="row2">
        <label class="f"><span>机房名称</span><input type="text" name="name" value="${esc(s.name || '')}"></label>
        <label class="f"><span>网段</span><input type="text" name="subnet" value="${esc(s.subnet || '')}"></label>
        <label class="f"><span>默认网关</span><input type="text" name="gateway" value="${esc(s.gateway || '')}"></label>
        <label class="f"><span>说明</span><input type="text" name="description" value="${esc(s.description || '')}"></label>
      </div><div class="btns"><button class="btn primary" type="submit">保存</button></div>`;
    }
    async function loadYAML() {
      try { $('#yText').value = await api('GET', 'api/topology/yaml'); $('#yProb').textContent = ''; }
      catch (e) { toast(errMsg(e), true); }
    }
    function banner() {
      $('#yBanner').innerHTML = S.changedOnDisk ? `<div class="banner">检测到 topology.yaml 在程序外被修改。<button class="btn small primary" id="bReload">从磁盘重新加载</button></div>` : '';
      const b = $('#bReload'); if (b) b.onclick = reloadDisk;
    }
    async function reloadDisk() {
      try { await api('POST', 'api/topology/reload'); toast('已从磁盘重新加载'); markClean(); await loadTopology(); loadYAML(); $('#yProb').textContent = ''; }
      catch (e) { $('#yProb').textContent = errMsg(e); toast('文件校验失败，未加载', true); }
    }

    $('#af').onsubmit = async ev => {
      ev.preventDefault();
      const e = ev.target.elements;
      const c = JSON.parse(JSON.stringify(cfg));
      Object.assign(c.alert, {
        enabled: e.enabled.checked, send_recovery: e.send_recovery.checked, webhook_url: e.webhook_url.value.trim(), site_title: e.site_title.value.trim(),
        down_after_failures: +e.down_after_failures.value, up_after_successes: +e.up_after_successes.value, merge_window_sec: +e.merge_window_sec.value,
        loss_duration_min: +e.loss_duration_min.value, latency_duration_min: +e.latency_duration_min.value, repeat_interval_min: +e.repeat_interval_min.value,
        proxy_url: e.proxy_url.value.trim(), tls_skip_verify: e.tls_skip_verify.checked,
      });
      try { await api('PUT', 'api/config', c); toast('告警设置已保存'); await loadCfg(); } catch (err) { toast(errMsg(err), true); }
    };
    $('#af').addEventListener('click', async ev => {
      if (ev.target.id !== 'aTest') return;
      const e = $('#af').elements;
      const a = Object.assign({}, cfg.alert, { webhook_url: e.webhook_url.value.trim(), site_title: e.site_title.value.trim(), proxy_url: e.proxy_url.value.trim(), tls_skip_verify: e.tls_skip_verify.checked });
      ev.target.disabled = true;
      try { await api('POST', 'api/alert/test', a); toast('测试消息已发送，请在企业微信群中确认'); }
      catch (err) { toast('发送失败：' + errMsg(err), true); }
      ev.target.disabled = false;
    });
    $('#schf').addEventListener('click', ev => {
      const chip = ev.target.closest('.days .chip');
      if (chip) {
        ev.preventDefault();
        const cb = $('input', chip);
        cb.checked = !cb.checked;
        chip.classList.toggle('on', cb.checked);
        return;
      }
      const del = ev.target.closest('[data-del]');
      const pre = ev.target.closest('[data-preset]');
      if (!del && !pre) return;
      wins = readWins();
      schEnabled = $('#schf').elements.enabled.checked;
      if (del) wins.splice(+del.dataset.del, 1);
      if (pre) {
        const k = pre.dataset.preset;
        wins.push(k === 'work' ? { days: [1, 2, 3, 4, 5], start: '08:00', end: '18:00' }
          : k === 'night' ? { days: [], start: '20:00', end: '08:00' } : { days: [], start: '09:00', end: '18:00' });
        schEnabled = true;
      }
      drawSchedule();
    });
    $('#schf').onsubmit = async ev => {
      ev.preventDefault();
      const c = JSON.parse(JSON.stringify(cfg));
      c.schedule = { enabled: ev.target.elements.enabled.checked, windows: readWins() };
      try { await api('PUT', 'api/config', c); toast('监控时段已保存：' + describeSchedule(c.schedule)); await loadCfg(); }
      catch (err) { toast(errMsg(err), true); }
    };
    $('#pf').onsubmit = async ev => {
      ev.preventDefault();
      const e = ev.target.elements;
      const c = JSON.parse(JSON.stringify(cfg));
      Object.assign(c.ping, { interval_ms: +e.interval_ms.value, timeout_ms: +e.timeout_ms.value, packet_size: +e.packet_size.value, latency_warn_ms: +e.latency_warn_ms.value, loss_warn_pct: +e.loss_warn_pct.value });
      c.retention_days = +e.retention_days.value;
      try { await api('PUT', 'api/config', c); toast('已保存'); await loadCfg(); } catch (err) { toast(errMsg(err), true); }
    };
    $('#sitef').onsubmit = async ev => {
      ev.preventDefault();
      const e = ev.target.elements;
      try {
        await api('PUT', 'api/site', { name: e.name.value.trim(), subnet: e.subnet.value.trim(), gateway: e.gateway.value.trim(), description: e.description.value.trim() });
        toast('已保存'); markClean(); await loadTopology(); loadYAML();
      } catch (err) { toast(errMsg(err), true); }
    };
    $('#pwf').onsubmit = async ev => {
      ev.preventDefault();
      const f = ev.target, e = f.elements;
      if (e.p1.value !== e.p2.value) { toast('两次输入的新密码不一致', true); return; }
      try {
        await api('POST', 'api/auth/password', { old_password: e.old.value, password: e.p1.value });
        f.reset();
        toast('密码已修改，其他浏览器需要重新登录');
      } catch (err) { toast(errMsg(err), true); }
    };
    $('#logout').onclick = async () => {
      try { await api('POST', 'api/auth/logout'); } catch (e) { /* 忽略 */ }
      toLogin();
    };
    $('#yLoad').onclick = loadYAML;
    $('#yReload').onclick = reloadDisk;
    $('#ySave').onclick = async () => {
      try {
        const r = await api('PUT', 'api/topology/yaml', $('#yText').value, { text: true });
        $('#yProb').textContent = '';
        toast(`已保存：${r.switches} 台交换机，${r.nodes} 个节点`);
        markClean(); await loadTopology(); loadYAML();
      } catch (e) { $('#yProb').textContent = errMsg(e); toast('校验失败，未保存', true); }
    };
    $('#yUpload').onclick = () => $('#yFile').click();
    $('#yFile').onchange = async ev => {
      const file = ev.target.files[0];
      if (!file) return;
      $('#yText').value = await file.text();
      toast('已载入到编辑框，确认无误后点“校验并保存”');
      ev.target.value = '';
    };
    api('GET', 'api/meta').then(m => {
      $('#about').innerHTML = `<div class="mono" style="font-size:12px;line-height:1.8">版本 ${esc(m.version)}<br>数据目录 ${esc(m.base_dir)}<br>拓扑文件 ${esc(m.topo_file)}<br>
        启动于 ${esc(Charts.fmtFull(m.started))}</div><div class="help">字段说明：docs/topology-fields.md；JSON Schema：schema/topology.schema.json</div>`;
    }).catch(() => {});
    loadCfg().catch(e => toast(errMsg(e), true));
    loadSite(); loadYAML(); banner();
    return { onTopology: loadSite, update: banner };
  }


  // ---------------------------------------------------------------------------
  // MCP 接入说明
  function mcpPage(app) {
    let info = null, cfg = null, tab = sessionStorage.getItem('np_mcp_tab') || 'claude';
    const local = ['127.0.0.1', 'localhost', '::1', '[::1]'].includes(location.hostname);
    const endpoint = location.origin + '/mcp';
    app.innerHTML = `<div class="page pad"><div class="mcp">
      <div class="mcp-hero">
        <div>
          <h2>MCP 接口 <span id="mStat"></span></h2>
          <div class="help" style="margin:0">MCP（Model Context Protocol）是 AI 助手连接外部工具的开放协议。接入后可以直接用自然语言让 AI
            查询设备延迟与丢包、分析历史、找出异常，或按 MAC 表 / ARP 表批量维护节点、开关告警、设置监控时段。</div>
        </div>
      </div>

      <div class="mcp-grid">
        <div>
          <div class="panel"><div class="ph"><span class="no">1</span>连接地址</div><div class="pb">
            <div class="copyline"><code id="mUrl">${esc(endpoint)}</code><button class="btn small" data-copy="mUrl">复制</button></div>
            ${local ? `<div class="help">这是本机地址。其他电脑上的 AI 客户端请把 <b>${esc(location.host)}</b> 换成服务器的局域网 IP，例如 <code>http://192.168.50.x:${esc(location.port || '80')}/mcp</code>，并确认 config.yaml 中 listen 为 0.0.0.0、防火墙放行该端口。</div>` : ''}
            <div class="help">传输方式：<b>Streamable HTTP</b>（POST，JSON 响应，无会话），兼容协议版本 2024-11-05 ~ 2025-06-18。只支持 stdio 的客户端可通过 <code>mcp-remote</code> 桥接（见右侧 Claude Desktop）。</div>
          </div></div>

          <div class="panel"><div class="ph"><span class="no">2</span>访问设置</div><div class="pb"><form id="mForm"></form></div></div>

          <div class="panel"><div class="ph"><span class="no">3</span>连接测试<span class="r"><button class="btn small primary" id="mTest">测试连接</button></span></div>
            <div class="pb"><div id="mTestOut" class="help" style="margin:0">用当前页面按 MCP 协议发送 initialize 和 tools/list，验证接口可用。</div></div></div>
        </div>

        <div>
          <div class="panel"><div class="ph"><span class="no">4</span>客户端配置</div><div class="pb">
            <div class="chips" id="mTabs" style="margin:0 0 10px"></div>
            <div id="mCfg"></div>
          </div></div>
        </div>
      </div>

      <div class="panel"><div class="ph"><span class="no">5</span>可以这样问 AI</div><div class="pb"><div class="examples">
        <div><b>日常查询</b><ul>
          <li>现在机房有哪些设备离线或丢包？按交换机归类说明</li>
          <li>192.168.50.1 网关最近 24 小时的延迟和丢包怎么样？有没有尖峰</li>
          <li>接入交换机 Port 12 下面的设备最近 7 天谁最不稳定</li>
          <li>生成一份今天的网络健康报告（使用提示词 health_report）</li>
        </ul></div>
        <div><b>现场核实</b><ul>
          <li>192.168.50.19 ping 不通，帮我确认它是真离线还是禁 ping（测一下常用端口、看 ARP）</li>
          <li>3 楼打印机老丢包，看看它接的交换机端口有没有错误包、是不是半双工</li>
          <li>接入交换机最近重启过吗？哪些端口有错误计数在增长</li>
          <li>外网打不开，追踪一下到 223.5.5.5 断在哪一跳，再看看 DNS 解析正不正常</li>
          <li>把 AI 诊断的回答前自检打开</li>
        </ul></div>
        <div><b>节点维护</b><ul>
          <li>把 192.168.50.19 的接线位置改成接入交换机 Port 5，位置填 A03 机柜</li>
          <li>新增一台服务器 192.168.50.60，名称“备份存储”，接在核心 Gi0/7，开启告警</li>
          <li>给网关和两台交换机开启告警，其余全部关闭</li>
          <li>工作日 8 点到 20 点监控，其余时间暂停</li>
          <li>扫描一下网段，有哪些新设备、哪些设备换了 IP？把新发现的打印机放到接入交换机 Port 7</li>
          <li>新建一个自动扫描任务“办公区”，每 15 分钟扫描 192.168.10.0/24 和 192.168.11.0/24，新设备打上“办公区”标签，先立即扫一次</li>
          <li>把企业微信告警的重复提醒改成 60 分钟，然后发一条测试消息</li>
          <li>192.168.50.19 这台电脑实际插在哪个口？和登记的一致吗？不一致就帮我改过来</li>
          <li>接入交换机 12 口后面都接了哪些设备？</li>
          <li>先预览一次自动定位，把未定位的设备都挂到对应端口，端口变了的列给我确认</li>
        </ul></div>
        <div><b>迁移机房</b><ul>
          <li>这是新机房的 MAC 表和 ARP 表（附上文本），帮我生成拓扑并导入（使用提示词 generate_topology）</li>
          <li>先 dry run 看看和现在的拓扑有哪些差异，我确认后再写入</li>
        </ul></div>
      </div></div></div>

      <div class="panel"><div class="ph"><span class="no">6</span>工具<span class="r muted" id="mToolCnt"></span></div><div class="pb" style="padding:0">
        <table class="grid flat"><thead><tr><th>工具</th><th>说明</th><th>类型</th></tr></thead><tbody id="mTools"></tbody></table>
      </div></div>

      <div class="mcp-grid">
        <div class="panel"><div class="ph"><span class="no">7</span>资源</div><div class="pb" id="mRes"></div></div>
        <div class="panel"><div class="ph"><span class="no">8</span>提示词模板</div><div class="pb" id="mPrompts"></div></div>
      </div>

      <div class="panel"><div class="ph"><span class="no">9</span>安全说明</div><div class="pb"><ul class="help" style="margin:0;padding-left:18px">
        <li>可写模式下 AI 能修改拓扑、告警开关和监控时段，修改立即写入 topology.yaml / config.yaml。只需查询时请开启「只读模式」。</li>
        <li>管理网页对局域网开放时，建议设置<b>访问令牌</b>；设置后 MCP 只认令牌，不再使用网页的登录账号。</li>
        <li>所有 MCP 写操作都会记录到 <code>logs/netping.log</code>（含来源 IP），删除和整体替换类工具会提示 AI 先征得你的确认。</li>
        <li>整体替换拓扑（replace_topology_yaml）默认只做校验（dry run），需明确 dry_run=false 才会写入；校验失败不会改动任何数据。</li>
      </ul></div></div>
    </div></div>`;

    $$('[data-copy]', app).forEach(b => b.onclick = () => copyText($('#' + b.dataset.copy).textContent));

    function authHeader() {
      if (cfg && cfg.mcp.token) return 'Bearer ' + cfg.mcp.token;
      return '';
    }
    function snippets() {
      const url = local ? endpoint.replace(location.host, '<服务器IP>:' + (location.port || '80')) : endpoint;
      const urlLocal = endpoint;
      const auth = authHeader();
      const hdrJSON = auth ? `,\n      "headers": { "Authorization": "${auth}" }` : '';
      const T = {
        claude: {
          name: 'Claude Code', note: '在终端执行（本机可用 127.0.0.1 地址）。添加后在 Claude Code 中输入 /mcp 可查看连接状态。',
          code: `claude mcp add --transport http netping ${local ? urlLocal : url}${auth ? ` \\\n  --header "Authorization: ${auth}"` : ''}`,
        },
        cursor: {
          name: 'Cursor / Windsurf', note: 'Cursor：~/.cursor/mcp.json 或项目内 .cursor/mcp.json；Windsurf：~/.codeium/windsurf/mcp_config.json（Windsurf 字段名为 serverUrl）。',
          code: `{\n  "mcpServers": {\n    "netping": {\n      "url": "${url}"${hdrJSON}\n    }\n  }\n}`,
        },
        workbuddy: {
          name: 'WorkBuddy', note: 'WorkBuddy：「连接」→「自定义连接」→ 右上角「配置 MCP」，会打开 ~/.workbuddy/mcp.json（Windows 为 %USERPROFILE%\\.workbuddy\\mcp.json）。已有 mcpServers 时只把 netping 这一项合并进去，保存后新开一个会话即可使用。',
          code: JSON.stringify({ mcpServers: { netping: {
            type: 'http', url: endpoint,
            ...(auth ? { headers: { Authorization: auth } } : {}),
          } } }, null, 2),
        },
        vscode: {
          name: 'VS Code', note: 'GitHub Copilot 代理模式：项目内 .vscode/mcp.json，或命令面板「MCP: Add Server」选择 HTTP。',
          code: `{\n  "servers": {\n    "netping": {\n      "type": "http",\n      "url": "${url}"${hdrJSON}\n    }\n  }\n}`,
        },
        desktop: {
          name: 'Claude Desktop', note: 'Claude Desktop 配置文件只支持 stdio，需借助 mcp-remote 桥接（需安装 Node.js）。编辑 claude_desktop_config.json 后重启 Claude Desktop。',
          code: `{\n  "mcpServers": {\n    "netping": {\n      "command": "npx",\n      "args": [\n        "-y", "mcp-remote", "${url}", "--allow-http"${auth ? `,\n        "--header", "Authorization:${auth}"` : ''}\n      ]\n    }\n  }\n}`,
        },
        other: {
          name: '其他客户端', note: 'Cherry Studio、Dify、n8n、LobeChat 等：新建 MCP 服务器，类型选「Streamable HTTP」（或“可流式传输的 HTTP”），填写以下信息。',
          code: `类型：Streamable HTTP\nURL：${url}${auth ? `\n请求头：Authorization: ${auth}` : '\n请求头：无（未设置访问令牌）'}`,
        },
      };
      return T;
    }
    function drawCfg() {
      const T = snippets();
      $('#mTabs').innerHTML = Object.keys(T).map(k => `<button class="chip${k === tab ? ' on' : ''}" data-t="${k}">${T[k].name}</button>`).join('');
      $$('#mTabs .chip').forEach(b => b.onclick = () => { tab = b.dataset.t; sessionStorage.setItem('np_mcp_tab', tab); drawCfg(); });
      const t = T[tab];
      const workbuddy = tab === 'workbuddy';
      const needsToken = !(cfg && cfg.mcp.token);
      const needsEnable = cfg && !cfg.mcp.enabled;
      const copyLabel = workbuddy
        ? (needsEnable ? '启用 MCP 并复制配置' : needsToken ? '生成令牌并复制配置' : '复制配置')
        : '复制';
      $('#mCfg').innerHTML = `<div class="help">${t.note}</div>
        <div class="codebox"><button class="btn small" id="mCopyCode">${copyLabel}</button><pre id="mCode">${esc(t.code)}</pre></div>
        ${workbuddy ? `<div class="help">配置使用当前访问地址，可直接粘贴到 WorkBuddy。${needsEnable ? '点击按钮将启用 MCP 接口。' : ''}${needsToken ? '点击按钮将生成并保存访问令牌，复制的配置会自动带上认证信息。' : '已包含访问令牌。'}${local ? '适用于本机 WorkBuddy；其他电脑请通过服务器局域网 IP 打开本页后再复制。' : ''}</div>` : needsToken ? '<div class="help" style="color:var(--down)">还没有设置访问令牌：管理页面需要登录，AI 客户端按上面的配置会连不上。请在左侧「访问令牌」点「生成」并保存，这里的配置会自动带上令牌。</div>' : ''}
        ${local && tab !== 'claude' && !workbuddy ? '<div class="help">请把 &lt;服务器IP&gt; 替换为运行 netPing 的电脑的 IP；AI 客户端就在本机时也可以用 127.0.0.1。</div>' : ''}`;
      $('#mCopyCode').onclick = async ev => {
        if (!workbuddy) { await copyText(t.code); return; }
        const button = ev.currentTarget;
        button.disabled = true;
        try {
          const current = await api('GET', 'api/config');
          if (!current.mcp.token || !current.mcp.enabled) {
            if (!current.mcp.token) current.mcp.token = generateToken();
            current.mcp.enabled = true;
            await api('PUT', 'api/config', current);
          }
          await load();
          await copyText(snippets().workbuddy.code);
        } catch (e) { toast('配置未复制：' + errMsg(e), true); }
        finally { button.disabled = false; }
      };
    }
    function generateToken() {
      const b = new Uint8Array(24); crypto.getRandomValues(b);
      return 'np_' + Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
    }
    function drawForm() {
      const m = cfg.mcp;
      $('#mForm').innerHTML = `
        <div class="checks" style="margin-bottom:8px">
          <label><input type="checkbox" name="enabled" ${m.enabled ? 'checked' : ''}> 开启 MCP 接口</label>
          <label><input type="checkbox" name="read_only" ${m.read_only ? 'checked' : ''}> 只读模式（AI 只能查询，不能修改）</label>
        </div>
        <label class="f"><span>访问令牌（AI 客户端连接时使用；留空时客户端需用 HTTP Basic 认证提供网页登录密码）</span>
          <div style="display:flex;gap:6px"><input type="text" name="token" class="mono" value="${esc(m.token || '')}" placeholder="未设置" autocomplete="off">
          <button type="button" class="btn small" id="mGen">生成</button><button type="button" class="btn small" id="mClr">清除</button></div></label>
        <div class="btns"><button class="btn primary" type="submit">保存</button></div>`;
      const f = $('#mForm');
      $('#mGen').onclick = () => {
        f.elements.token.value = generateToken();
      };
      $('#mClr').onclick = () => { f.elements.token.value = ''; };
      f.onsubmit = async ev => {
        ev.preventDefault();
        const c = JSON.parse(JSON.stringify(cfg));
        c.mcp = { enabled: f.elements.enabled.checked, read_only: f.elements.read_only.checked, token: f.elements.token.value.trim() };
        try { await api('PUT', 'api/config', c); toast('MCP 设置已保存，立即生效'); await load(); }
        catch (e) { toast(errMsg(e), true); }
      };
    }
    function drawInfo() {
      const s = [];
      s.push(info.enabled ? '<span class="badge ok">已开启</span>' : '<span class="badge down">未开启</span>');
      s.push(info.read_only ? '<span class="badge slow">只读</span>' : '<span class="badge unknown">可读写</span>');
      s.push(info.token_set ? '<span class="badge ok">令牌认证</span>' : info.auth_basic ? '<span class="badge unknown">需要令牌</span>' : '<span class="badge slow">尚未设置密码</span>');
      $('#mStat').innerHTML = s.join(' ');
      const kind = t => t.destructive ? '<span class="badge down">修改·需确认</span>' : t.write ? '<span class="badge slow">修改</span>' : t.probe ? '<span class="badge ok" title="从 netPing 主机实时发包探测，只读">查询·实时探测</span>' : '<span class="badge ok">查询</span>';
      $('#mTools').innerHTML = info.tools.map(t => `<tr${info.read_only && t.write ? ' class="dim"' : ''}><td class="mono"><b>${esc(t.name)}</b><br><span class="muted" style="font-family:var(--sans)">${esc(t.title)}</span></td><td class="wrap">${esc(t.description)}</td><td>${kind(t)}</td></tr>`).join('');
      const nw = info.tools.filter(t => !t.write).length;
      $('#mToolCnt').textContent = `${info.tools.length} 个（查询 ${nw}，修改 ${info.tools.length - nw}）${info.read_only ? '，只读模式下修改类工具不可用' : ''}`;
      $('#mRes').innerHTML = info.resources.map(r => `<div class="kv"><code>${esc(r.uri)}</code><span>${esc(r.description)}</span></div>`).join('');
      $('#mPrompts').innerHTML = info.prompts.map(p => `<div class="kv"><code>${esc(p.name)}</code><span><b>${esc(p.title)}</b>：${esc(p.description)}</span></div>`).join('') +
        '<div class="help">在支持提示词的客户端中（如 Claude Code 输入 <code>/mcp__netping__</code>）可直接选用。</div>';
    }
    async function load() {
      [info, cfg] = await Promise.all([api('GET', 'api/mcp'), api('GET', 'api/config')]);
      drawInfo(); drawForm(); drawCfg();
    }
    async function rpcCall(method, params, id) {
      const headers = { 'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream', 'MCP-Protocol-Version': info.protocol };
      const auth = cfg.mcp.token ? 'Bearer ' + cfg.mcp.token : '';
      if (auth) headers.Authorization = auth;
      const r = await fetch('mcp', { method: 'POST', headers, body: JSON.stringify({ jsonrpc: '2.0', id, method, params }) });
      if (!r.ok) throw new Error(`HTTP ${r.status}：${(await r.text()).trim()}`);
      const d = await r.json();
      if (d.error) throw new Error(d.error.message);
      return d.result;
    }
    $('#mTest').onclick = async () => {
      const out = $('#mTestOut');
      out.innerHTML = '测试中…';
      const t0 = performance.now();
      try {
        const init = await rpcCall('initialize', { protocolVersion: info.protocol, capabilities: {}, clientInfo: { name: 'netPing 网页测试', version: '1' } }, 1);
        const tl = await rpcCall('tools/list', {}, 2);
        const ov = await rpcCall('tools/call', { name: 'get_overview', arguments: {} }, 3);
        const o = JSON.parse(ov.content[0].text);
        const ms = Math.round(performance.now() - t0);
        out.innerHTML = `<div class="v-ok"><b>✓ 连接成功</b>（${ms}ms）</div>
          <div class="mono" style="font-size:12px;line-height:1.8">服务：${esc(init.serverInfo.title || init.serverInfo.name)} ${esc(init.serverInfo.version)} · 协议 ${esc(init.protocolVersion)}<br>
          可用工具 ${tl.tools.length} 个 · get_overview：${o.nodes} 个节点，${(o.abnormal_devices || []).length} 个异常</div>`;
      } catch (e) {
        out.innerHTML = `<div class="v-down"><b>✗ 连接失败</b>：${esc(e.message)}</div>`;
      }
    };
    load().catch(e => toast(errMsg(e), true));
    return {};
  }

  // ---------------------------------------------------------------------------
  // 扫描发现页
  const CHANGE_TXT = {
    added: ['新增', 'ok'], ip_changed: ['IP 变化', 'slow'], ip_released: ['释放旧 IP', 'unknown'],
    mac_learned: ['补全 MAC', 'ok'], conflict: ['冲突', 'down'], hostname: ['主机名', 'ok'],
  };
  const HOST_TXT = {
    registered: ['已登记', 'ok'], switch: ['交换机', 'unknown'], unknown: ['未登记', 'down'], conflict: ['冲突', 'down'],
    added: ['新增', 'ok'], ip_changed: ['IP 已更新', 'slow'], mac_learned: ['补全 MAC', 'ok'], ip_released: ['', 'unknown'],
  };
  function ipNum(ip) { return (ip || '').split('.').reduce((a, b) => a * 256 + (+b || 0), 0); }
  function fmtTime(t) { return t ? Charts.fmtFull(new Date(t).getTime()) : '-'; }

  function natCmp(a, b) { return String(a).localeCompare(String(b), undefined, { numeric: true }); }
  function parsePortLocations(text) {
    const out = {};
    text.split('\n').forEach(l => {
      const m = l.trim().match(/^(\S+)\s+(.+)$/);
      if (m) out[m[1]] = m[2].trim();
    });
    return out;
  }

  function discoveryPage(app) {
    let timer = 0, data = null, cfg = null, hostFilter = '', hostState = '', wasRunning = false, view = '', editing = null;
    app.innerHTML = `<div class="page pad"><div class="settings">
      <div>
        <div class="panel"><div class="ph">自动扫描任务<span class="r" style="display:flex;gap:10px;align-items:center">
          <label class="checks" title="关闭后所有任务都不再定时运行，仍可手动扫描"><input type="checkbox" id="dMaster"> 自动扫描</label></span></div>
          <div class="pb">
            <div id="dProg"></div>
            <div id="dJobs"></div>
            <div class="btns"><button class="btn primary" id="dNew">+ 新建扫描任务</button><button class="btn accent" id="dScanAll">全部扫描一次</button></div>
            <div class="help">每个任务按自己的间隔扫描一个或多个网段，发现的新设备自动添加为“未定位”，并按 MAC 跟踪已有设备的 IP 变化。</div>
          </div></div>
        <div class="panel hidden" id="dEditBox"><div class="ph" id="dEditTitle">扫描任务</div><div class="pb"><form id="dj"></form></div></div>
        <div class="panel"><div class="ph">临时扫描</div><div class="pb"><form id="dt">
          <label class="f"><span>网段（一个或多个 CIDR，逗号或换行分隔，最大 /20；不保存为任务）</span>
            <textarea name="subnets" rows="2" class="mono" placeholder="192.168.10.0/24, 192.168.11.0/24"></textarea></label>
          <div class="checks"><label><input type="checkbox" name="auto_add" checked> 新设备自动添加为“未定位”</label></div>
          <div class="btns"><button class="btn" type="submit">扫描</button></div></form></div></div>
        <div class="panel"><div class="ph">全局设置</div><div class="pb"><form id="df"></form></div></div>
        <div class="panel"><div class="ph">工作方式</div><div class="pb help" style="margin:0;line-height:1.8">
          <b>1. 扫描</b>：并发 ping 任务网段内每个地址，再读取本机 ARP 表得到 IP ↔ MAC 对应关系（不响应 ping 但在 ARP 中的设备同样算在线）。<br>
          <b>2. 以 MAC 识别设备</b>：拓扑中有 MAC 的节点，扫描发现其 MAC 换了 IP 时，<b>直接修改该节点的 IP</b>，
          节点 id、历史曲线、告警开关、接线位置全部保留。<br>
          <b>3. IP 分配方式</b>（节点编辑中的「IP 分配方式」）：<i>自动跟踪</i>（默认）和 <i>DHCP</i> 都会自动更新；
          <i>静态锁定</i>的节点不会被修改，只在「待确认」中提示。<br>
          <b>4. 快速重解析</b>：DHCP 设备换地址后旧 IP 会 ping 不通。开启后，可跟踪的设备连续失败 2 次就立即扫描它所在网段的任务，
          并把它的离线告警暂缓最多 90 秒——找到新 IP 就自动更新，不会误报离线。<br>
          <b>5. 自动添加</b>：拓扑中没有的设备自动加入为「未定位」节点（拓扑图左下虚线区，带「自动发现」标签），有 MAC 时 id 按 MAC 生成；
          可按任务附加标签、同时开启告警。拖到交换机端口或在编辑中选择交换机即可定位。<br>
          <b>6. 补全 MAC</b>：只填了 IP 的节点，扫描时从 ARP 补上 MAC 和厂商。<br>
          <b>保护</b>：一个 MAC 对应大量 IP（代理 ARP / 防火墙）时忽略；两台设备抢同一 IP、交换机管理 IP 等情况记为「冲突」不自动处理。<br>
          <b>限制</b>：ARP 只能看到与本服务器<b>同一二层网段</b>的设备；跨网段（经路由）的任务只能发现在线设备并按 IP 添加，无法跟踪 IP 变化。
        </div></div>
      </div>
      <div>
        <div class="panel" id="dSugBox"><div class="ph">待确认<span class="r" id="dSugCnt"></span></div><div class="pb" id="dSug"></div></div>
        <div class="panel" id="dUnkBox"><div class="ph">未登记设备<span class="r btns" style="margin:0"><button class="btn small" id="dAddAll">全部添加为未定位</button></span></div><div class="pb" id="dUnk"></div></div>
        <div class="panel"><div class="ph">扫描结果<span class="r" id="dViews"></span></div><div class="pb" id="dSum"></div>
          <div class="pb" style="padding-top:0" id="dChg"></div></div>
        <div class="panel"><div class="ph">在线地址<span class="r" style="display:flex;gap:6px">
          <select id="dHs" style="width:auto"><option value="">全部</option><option value="registered">已登记</option><option value="unknown">未登记</option><option value="noping">不响应 ping</option><option value="nomac">无 MAC</option></select>
          <input type="search" id="dHf" placeholder="IP / MAC / 厂商 / 主机名" style="width:180px"></span></div>
          <div class="pb" style="padding:0;max-height:480px;overflow:auto"><table class="grid"><thead><tr><th>IP</th><th>MAC</th><th>厂商</th><th>主机名</th><th>Ping</th><th>状态</th></tr></thead><tbody id="dHosts"></tbody></table></div></div>
        <div class="panel"><div class="ph">变更记录<span class="r muted">最近 200 条</span></div>
          <div class="pb" style="padding:0;max-height:360px;overflow:auto"><table class="grid"><thead><tr><th>时间</th><th>任务</th><th>类型</th><th>设备</th><th>说明</th></tr></thead><tbody id="dEv"></tbody></table></div></div>
      </div></div></div>`;

    const nodeName = id => { const n = S.topo && S.topo.nodes.find(x => x.id === id); return n ? (n.name || n.ip || n.mac || n.id) : id; };
    const nodeLink = (id, label) => id ? `<a href="#/node/${encodeURIComponent(id)}">${esc(label || nodeName(id))}</a>` : esc(label || '');
    const chBadge = t => { const [x, c] = CHANGE_TXT[t] || [t, 'unknown']; return `<span class="badge ${c}">${x}</span>`; };
    const split = v => v.split(/[\s,，]+/).map(x => x.trim()).filter(Boolean);
    const hm = s => s ? s.slice(5, 16) : '';

    function curResult() {
      const rs = data.results || {};
      if (view && rs[view]) return rs[view];
      return data.last || null;
    }

    async function saveCfg(mut, msg) {
      const c = JSON.parse(JSON.stringify(cfg));
      mut(c.discovery);
      await api('PUT', 'api/config', c);
      if (msg) toast(msg);
      await loadCfg();
      await load(true);
    }

    function drawStatus() {
      const st = data.status;
      $('#dMaster').checked = !!st.enabled;
      if (st.running) {
        const pct = st.total ? Math.round(st.done * 100 / st.total) : 0;
        $('#dProg').innerHTML = `<div class="help" style="margin:0 0 6px">正在扫描「${esc(st.job || '')}」：${esc(st.phase || '扫描中')}… ${st.done}/${st.total}</div>
          <div style="height:6px;border:1px solid var(--ink);margin-bottom:10px"><div style="height:100%;width:${pct}%;background:var(--accent)"></div></div>`;
      } else {
        $('#dProg').innerHTML = st.enabled ? '' : '<div class="banner" style="margin-bottom:10px">自动扫描已关闭：任务不会定时运行，仍可手动扫描。</div>';
      }
      const jobs = st.jobs || [];
      $('#dJobs').innerHTML = jobs.length ? jobs.map(j => {
        const on = st.enabled && j.enabled;
        const subs = (j.subnets || []).length ? j.subnets.join('、') : `${(j.effective_subnets || []).join('、') || '未设置'}（机房网段）`;
        const opts = [`每 ${j.interval_min} 分钟`, j.auto_add ? '自动添加' : '只列出新设备'];
        if (j.alert) opts.push('新设备开告警');
        if ((j.tags || []).length) opts.push('标签：' + j.tags.join('、'));
        const lastTxt = j.last_run ? `上次 ${hm(j.last_run)} · 在线 ${j.alive}/${j.scanned} · 变更 ${j.changes}${j.pending ? ` · <b class="v-down">待处理 ${j.pending}</b>` : ''}` : '还未运行';
        return `<div class="job${view === j.id ? ' on' : ''}" data-job="${esc(j.id)}">
          <div class="job-h"><b>${esc(j.name)}</b>
            ${on ? '<span class="badge ok">自动</span>' : '<span class="badge unknown">已停用</span>'}
            <span class="muted mono" style="margin-left:auto;font-size:11px">${on && j.next_run ? '下次 ' + esc(hm(j.next_run)) : ''}</span></div>
          <div class="mono" style="font-size:12px">${esc(subs)}</div>
          <div class="muted" style="font-size:12px">${esc(opts.join(' · '))}</div>
          <div class="muted" style="font-size:12px">${lastTxt}${j.error ? ` · <span class="v-down">${esc(j.error)}</span>` : ''}</div>
          <div class="btns" style="margin-top:6px">
            <button class="btn small accent" data-run="${esc(j.id)}" ${st.running ? 'disabled' : ''}>立即扫描</button>
            <button class="btn small" data-view="${esc(j.id)}" ${j.last_run ? '' : 'disabled'}>查看结果</button>
            <button class="btn small" data-edit="${esc(j.id)}">编辑</button>
            <button class="btn small" data-toggle="${esc(j.id)}">${j.enabled ? '停用' : '启用'}</button>
            <button class="btn small danger" data-del="${esc(j.id)}">删除</button></div></div>`;
      }).join('') : '<div class="muted">还没有扫描任务。点「新建扫描任务」，指定一个或多个网段即可自动扫描、自动添加。</div>';
      $('#dScanAll').disabled = !!st.running || !jobs.length;
      $$('#dt button[type=submit]')[0].disabled = !!st.running;
    }

    function drawResult() {
      const rs = data.results || {};
      const keys = Object.keys(rs).sort((a, b) => (rs[b].finished || '').localeCompare(rs[a].finished || ''));
      const r = curResult();
      $('#dViews').innerHTML = keys.length > 1 ? `<select id="dView" style="width:auto">${keys.map(k =>
        `<option value="${esc(k)}" ${r && r.job === k ? 'selected' : ''}>${esc(rs[k].job_name || k)} · ${esc(fmtTime(rs[k].finished).slice(5, 16))}</option>`).join('')}</select>` : '';
      const sel = $('#dView');
      if (sel) sel.onchange = e => { view = e.target.value; drawStatus(); drawResult(); };
      if (!r) { $('#dSum').innerHTML = '<div class="muted">还没有扫描结果。</div>'; $('#dChg').innerHTML = ''; $('#dHosts').innerHTML = ''; return; }
      const cnt = {};
      (r.changes || []).forEach(c => { cnt[c.type] = (cnt[c.type] || 0) + 1; });
      $('#dSum').innerHTML = `<div style="margin-bottom:6px"><b>${esc(r.job_name || r.job)}</b> <span class="mono muted">${esc((r.subnets || []).join('、'))}</span></div>
        <div class="stat-grid">
          <div><div class="v">${r.alive}</div><div class="k">在线（ping ${r.ping_alive}）</div></div>
          <div><div class="v">${cnt.added || 0}</div><div class="k">自动添加</div></div>
          <div><div class="v">${cnt.ip_changed || 0}</div><div class="k">IP 更新</div></div>
          <div><div class="v">${(r.unknown || []).length + (r.suggestions || []).length}</div><div class="k">待处理</div></div></div>
        <div class="help">${esc(fmtTime(r.started))} · ${esc(r.reason)} · 扫描 ${r.scanned} 个地址，用时 ${(r.duration_ms / 1000).toFixed(1)} 秒 · ARP ${r.arp_entries} 条
          ${cnt.mac_learned ? ' · 补全 MAC ' + cnt.mac_learned : ''}${cnt.conflict ? ' · <span class="v-down">冲突 ' + cnt.conflict + '</span>' : ''}</div>
        ${r.error ? `<div class="v-down">扫描失败：${esc(r.error)}</div>` : ''}
        ${r.arp_error ? `<div class="v-down">读取 ARP 失败：${esc(r.arp_error)}（只能确认在线，无法跟踪 IP）</div>` : ''}
        ${(r.proxy_macs || []).length ? `<div class="help">以下 MAC 对应了大量 IP，判断为代理 ARP，已忽略：<span class="mono">${esc(r.proxy_macs.join(', '))}</span></div>` : ''}`;
      const chg = r.changes || [];
      $('#dChg').innerHTML = chg.length ? `<table class="grid"><tbody>` + chg.map(c => `<tr><td>${chBadge(c.type)}</td>
          <td>${nodeLink(c.node_id, c.name || c.ip || c.mac)}</td><td class="wrap">${esc(c.detail)}</td></tr>`).join('') + '</tbody></table>'
        : '<div class="muted">本次扫描没有变更。</div>';

      const q = hostFilter.toLowerCase();
      const hosts = (r.hosts || []).filter(h => {
        if (hostState === 'registered' && !h.node_id) return false;
        if (hostState === 'unknown' && h.status !== 'unknown') return false;
        if (hostState === 'noping' && h.ping) return false;
        if (hostState === 'nomac' && h.mac) return false;
        return !q || [h.ip, h.mac, h.vendor, h.hostname].join(' ').toLowerCase().includes(q);
      }).sort((a, b) => ipNum(a.ip) - ipNum(b.ip));
      $('#dHosts').innerHTML = hosts.length ? hosts.map(h => {
        const [t, c] = HOST_TXT[h.status] || [h.status || '', 'unknown'];
        const label = h.node_id ? nodeLink(h.node_id, t) : (h.local ? '本机' : t);
        const shared = h.shared_with ? ` <span class="badge unknown" title="同一 MAC 还对应 ${esc(h.shared_with.join('、'))}：通常是无线中继（MAC-NAT）、桥接或多地址设备，按 IP 识别">共享</span>` : '';
        return `<tr><td class="mono">${esc(h.ip)}</td><td class="mono">${esc(h.mac || '-')}${shared}</td><td>${esc(h.vendor || '')}</td><td>${esc(h.hostname || '')}</td>
          <td class="mono">${h.ping ? fmtMs(h.rtt) : '<span class="muted">无响应</span>'}</td><td><span class="badge ${c}">${label}</span></td></tr>`;
      }).join('') : '<tr><td colspan="6" class="empty">无</td></tr>';
    }

    function drawLists() {
      const sug = data.suggestions || [];
      $('#dSugCnt').textContent = sug.length ? sug.length + ' 项' : '';
      $('#dSugBox').classList.toggle('hidden', !sug.length);
      $('#dSug').innerHTML = sug.map((s, i) => `<div style="display:flex;align-items:center;gap:8px;padding:6px 0;border-bottom:1px solid var(--line-2)">
          <span style="flex:1">${nodeLink(s.node_id, s.name)} <span class="mono">${esc(s.current_ip || '无 IP')} → <b>${esc(s.seen_ip)}</b></span>
            <span class="mono muted">${esc(s.mac)}</span><br><span class="muted">${esc(s.reason)}</span></span>
          <button class="btn small" data-sug="${i}" data-mode="-">更新 IP</button>
          <button class="btn small" data-sug="${i}" data-mode="">更新并改为自动跟踪</button></div>`).join('');

      const unk = data.unknown || [];
      $('#dUnkBox').classList.toggle('hidden', !unk.length);
      $('#dUnk').innerHTML = unk.length ? `<table class="grid"><thead><tr><th>IP</th><th>MAC</th><th>厂商</th><th>主机名</th><th></th></tr></thead><tbody>` +
        unk.map(h => `<tr><td class="mono">${esc(h.ip)}</td><td class="mono">${esc(h.mac || '-')}</td><td>${esc(h.vendor || '')}</td><td>${esc(h.hostname || '')}</td>
          <td><button class="btn small" data-add="${esc(h.ip)}">添加</button></td></tr>`).join('') + '</tbody></table>' +
        '<div class="help">任务关闭了「自动添加」时，新设备列在这里。</div>' : '';

      $('#dEv').innerHTML = (data.events || []).length ? data.events.map(e => `<tr><td class="mono">${esc(fmtTime(e.time))}</td><td>${esc(e.job || '')}</td><td>${chBadge(e.type)}</td>
          <td>${nodeLink(e.node_id, e.name || e.ip || e.mac)}</td><td class="wrap">${esc(e.detail)}</td></tr>`).join('')
        : '<tr><td colspan="5" class="empty">暂无记录</td></tr>';
      drawResult();
    }

    function drawJobForm() {
      const box = $('#dEditBox');
      if (!editing) { box.classList.add('hidden'); return; }
      box.classList.remove('hidden');
      const isNew = editing === 'new';
      const j = isNew ? { id: '', name: '', enabled: true, subnets: [], interval_min: 30, auto_add: true, alert: false, tags: [] }
        : (cfg.discovery.jobs || []).find(x => x.id === editing);
      if (!j) { editing = null; box.classList.add('hidden'); return; }
      const site = S.topo && S.topo.site && S.topo.site.subnet;
      $('#dEditTitle').textContent = isNew ? '新建扫描任务' : '编辑扫描任务 · ' + j.name;
      $('#dj').innerHTML = `
        <div class="row2">
          <label class="f"><span>名称</span><input type="text" name="name" value="${esc(j.name)}" placeholder="如：办公区 3 楼"></label>
          <label class="f"><span>扫描间隔（分钟，≥5）</span><input type="number" name="interval_min" min="5" value="${j.interval_min}"></label>
        </div>
        <label class="f"><span>网段（一个或多个 CIDR，逗号或换行分隔，每个最大 /20；留空则使用机房网段 ${esc(site || '（未设置）')}）</span>
          <textarea name="subnets" rows="3" class="mono" placeholder="192.168.10.0/24&#10;192.168.11.0/24">${esc((j.subnets || []).join('\n'))}</textarea></label>
        <div class="checks" style="margin-bottom:8px">
          <label><input type="checkbox" name="enabled" ${j.enabled ? 'checked' : ''}> 定时自动扫描</label>
          <label><input type="checkbox" name="auto_add" ${j.auto_add ? 'checked' : ''}> 新设备自动添加为“未定位”</label>
          <label><input type="checkbox" name="alert" ${j.alert ? 'checked' : ''}> 新设备同时开启告警</label>
        </div>
        <div class="row2">
          <label class="f"><span>自动添加的设备附加标签（逗号分隔，可空）</span><input type="text" name="tags" value="${esc((j.tags || []).join(', '))}"></label>
          <label class="f"><span>ID${isNew ? '（留空自动生成）' : ''}</span><input type="text" name="id" value="${esc(j.id)}" ${isNew ? '' : 'readonly'}></label>
        </div>
        <div class="btns"><button class="btn primary" type="submit">保存</button><button class="btn accent" type="button" id="djRun">保存并立即扫描</button>
          <button class="btn" type="button" id="djCancel">取消</button></div>`;
      const f = $('#dj');
      const submit = async runNow => {
        const e = f.elements;
        const nj = Object.assign({}, j, {
          id: e.id.value.trim(), name: e.name.value.trim(), interval_min: +e.interval_min.value, subnets: split(e.subnets.value),
          enabled: e.enabled.checked, auto_add: e.auto_add.checked, alert: e.alert.checked,
          tags: e.tags.value.split(/[,，]/).map(x => x.trim()).filter(Boolean),
        });
        try {
          await saveCfg(d => {
            d.jobs = d.jobs || [];
            if (isNew) d.jobs.push(nj); else d.jobs = d.jobs.map(x => x.id === j.id ? nj : x);
          }, isNew ? '扫描任务已添加' : '扫描任务已保存');
          const saved = isNew ? cfg.discovery.jobs[cfg.discovery.jobs.length - 1] : nj;
          editing = null; drawJobForm();
          if (runNow) runScan({ job: saved.id }, saved.id);
        } catch (err) { toast(errMsg(err), true); }
      };
      f.onsubmit = ev => { ev.preventDefault(); submit(false); };
      $('#djRun').onclick = () => submit(true);
      $('#djCancel').onclick = () => { editing = null; drawJobForm(); };
      box.scrollIntoView({ block: 'nearest' });
    }

    function drawForm() {
      const d = cfg.discovery;
      $('#df').innerHTML = `
        <div class="checks" style="margin-bottom:8px">
          <label><input type="checkbox" name="fast_resolve" ${d.fast_resolve ? 'checked' : ''}> 快速重解析（离线时先找新 IP）</label>
          <label><input type="checkbox" name="notify" ${d.notify ? 'checked' : ''}> 新增设备 / IP 变化推送企业微信</label>
        </div>
        <div class="row3">
          <label class="f"><span>IP 跟踪范围</span><select name="ip_tracking">
            <option value="all" ${d.ip_tracking === 'all' ? 'selected' : ''}>除“静态锁定”外全部（推荐）</option>
            <option value="dhcp" ${d.ip_tracking === 'dhcp' ? 'selected' : ''}>仅标为 DHCP 的节点</option>
            <option value="off" ${d.ip_tracking === 'off' ? 'selected' : ''}>不自动修改 IP（只提示）</option></select></label>
          <label class="f"><span>单次 ping 超时 ms</span><input type="number" name="timeout_ms" min="100" value="${d.timeout_ms}"></label>
          <label class="f"><span>并发数（1–256）</span><input type="number" name="concurrency" min="1" max="256" value="${d.concurrency}"></label>
        </div>
        <label class="f"><span>排除（IP 或 CIDR，逗号或换行分隔；所有任务都不扫描、不自动添加）</span>
          <textarea name="exclude" rows="2" class="mono">${esc((d.exclude || []).join('\n'))}</textarea></label>
        <label class="f"><span>MAC 前缀 → 厂商（每行“AA:BB:CC 厂商”，可空；拓扑中已有设备的前缀会自动学习）</span>
          <textarea name="oui" rows="2" class="mono" placeholder="60:D7:E3 深大智能">${esc(Object.keys(d.oui || {}).sort().map(k => k + ' ' + d.oui[k]).join('\n'))}</textarea></label>
        <div class="btns"><button class="btn primary" type="submit">保存</button></div>
        <div class="help">处于手动暂停或非监控时段时不做定时扫描。扫描使用普通 ping，不需要管理员权限；/24 网段约 5–10 秒。</div>`;
      const f = $('#df');
      f.onsubmit = async ev => {
        ev.preventDefault();
        const e = f.elements;
        const oui = {};
        e.oui.value.split('\n').forEach(l => {
          const m = l.trim().match(/^(\S+)\s+(.+)$/);
          if (m) oui[m[1]] = m[2].trim();
        });
        try {
          await saveCfg(d => Object.assign(d, {
            fast_resolve: e.fast_resolve.checked, notify: e.notify.checked, exclude: split(e.exclude.value), oui,
            ip_tracking: e.ip_tracking.value, timeout_ms: +e.timeout_ms.value, concurrency: +e.concurrency.value,
          }), '扫描设置已保存');
        } catch (err) { toast(errMsg(err), true); }
      };
    }

    async function loadCfg() { cfg = await api('GET', 'api/config'); drawForm(); drawJobForm(); }
    async function load(once) {
      clearTimeout(timer);
      try {
        data = await api('GET', 'api/discovery');
        drawStatus();
        drawLists();
        if (wasRunning && !data.status.running) {
          await loadTopology();
          drawLists();
          const r = curResult();
          if (r) toast(r.error ? `「${r.job_name}」扫描失败：${r.error}` : `「${r.job_name}」扫描完成：在线 ${r.alive} 个，${(r.changes || []).length} 项变更`, !!r.error);
        }
        wasRunning = data.status.running;
      } catch (e) { toast(errMsg(e), true); }
      if (!once || (data && data.status.running)) timer = setTimeout(load, data && data.status.running ? 800 : 10000);
    }
    async function runScan(body, showJob) {
      try {
        await api('POST', 'api/discovery/scan', body);
        view = showJob || '';
        wasRunning = true; load();
      } catch (e) { toast(errMsg(e), true); }
    }

    $('#dMaster').onchange = async e => {
      const on = e.target.checked;
      try { await saveCfg(d => { d.enabled = on; }, on ? '自动扫描已开启' : '自动扫描已关闭（仍可手动扫描）'); }
      catch (err) { e.target.checked = !on; toast(errMsg(err), true); }
    };
    $('#dNew').onclick = () => { editing = 'new'; drawJobForm(); };
    $('#dScanAll').onclick = () => runScan({}, '');
    $('#dt').onsubmit = ev => {
      ev.preventDefault();
      const e = ev.target.elements;
      const subnets = split(e.subnets.value);
      if (!subnets.length) { toast('请填写要扫描的网段', true); return; }
      runScan({ subnets, auto_add: e.auto_add.checked }, 'adhoc');
    };
    $('#dHf').oninput = e => { hostFilter = e.target.value; drawResult(); };
    $('#dHs').onchange = e => { hostState = e.target.value; drawResult(); };
    async function addIPs(ips) {
      try {
        const r = await api('POST', 'api/discovery/add', { ips });
        toast(`已添加 ${r.added} 台为未定位设备`);
        await loadTopology(); await load(true);
      } catch (e) { toast(errMsg(e), true); }
    }
    $('#dAddAll').onclick = () => {
      const ips = (data.unknown || []).map(h => h.ip);
      if (ips.length && confirm(`把 ${ips.length} 台未登记设备全部添加为“未定位”？`)) addIPs(ips);
    };
    app.addEventListener('click', async ev => {
      const t = ev.target.closest('[data-run],[data-view],[data-edit],[data-toggle],[data-del],[data-add],[data-sug]');
      if (!t) return;
      const ds = t.dataset;
      if (ds.run) { runScan({ job: ds.run }, ds.run); return; }
      if (ds.view) { view = ds.view; drawStatus(); drawResult(); return; }
      if (ds.edit) { editing = ds.edit; drawJobForm(); return; }
      if (ds.toggle) {
        try { await saveCfg(d => { d.jobs.forEach(j => { if (j.id === ds.toggle) j.enabled = !j.enabled; }); }, '已保存'); }
        catch (e) { toast(errMsg(e), true); }
        return;
      }
      if (ds.del) {
        const j = (cfg.discovery.jobs || []).find(x => x.id === ds.del);
        if (!j || !confirm(`删除扫描任务「${j.name}」？（已添加的设备不受影响）`)) return;
        try { await saveCfg(d => { d.jobs = d.jobs.filter(x => x.id !== ds.del); }, '扫描任务已删除'); if (view === ds.del) view = ''; }
        catch (e) { toast(errMsg(e), true); }
        return;
      }
      if (ds.add) { addIPs([ds.add]); return; }
      const s = (data.suggestions || [])[+ds.sug];
      if (!s) return;
      try {
        await api('POST', 'api/discovery/apply', { node_id: s.node_id, ip: s.seen_ip, ip_mode: ds.mode });
        toast(`${s.name || s.node_id} 的 IP 已改为 ${s.seen_ip}`);
        await loadTopology(); await load(true);
      } catch (e) { toast(errMsg(e), true); }
    });
    loadCfg().catch(e => toast(errMsg(e), true));
    load();
    return { destroy() { clearTimeout(timer); } };
  }

  // ---------------------------------------------------------------------------
  // 端口定位页：SNMP 读取交换机 MAC 地址表，自动确定设备接在哪个端口
  function locatePage(app) {
    let data = null, cfg = null, tableSw = '', busy = false;
    let closeTerminal = null, alive = true;
    app.innerHTML = `<div class="page pad"><div class="settings">
      <div>
        <div class="panel"><div class="ph">端口定位<span class="r" id="lState"></span></div><div class="pb">
          <div class="help" style="margin-top:0">通过 SNMP 只读读取交换机的 MAC 地址表，按设备 MAC 找到它实际插在哪个端口：
            <b>“未定位”的设备自动挂到该端口</b>（并按端口位置对照填入物理位置）；已定位但端口变了的设备列在右侧「待确认」，确认后再改。</div>
          <div class="btns"><button class="btn accent" id="lRun">立即定位</button><button class="btn" id="lDry">只预览</button></div>
          <div id="lSum" style="margin-top:10px"></div>
        </div></div>
        <div class="panel"><div class="ph">全局设置</div><div class="pb"><form id="lf"></form></div></div>
        <div class="panel"><div class="ph">交换机上需要的配置（参考）</div><div class="pb">
          <div class="help" style="margin-top:0">自动定位只需开启 SNMP v2c <b>只读</b>团体名，读取标准 BRIDGE-MIB / IF-MIB。需要修改交换机配置时，点右侧「Telnet」手动登录终端。建议限制只允许 netPing 服务器访问。</div>
          <pre class="code">! 锐捷
snmp-server community 团体名 ro
! 华为
snmp-agent sys-info version v2c
snmp-agent community read 团体名
! H3C
snmp-agent sys-info version v2c
snmp-agent community read 团体名</pre></div></div>
      </div>
      <div>
        <div class="panel"><div class="ph">交换机读取设置<span class="r"><button class="btn small primary" id="lSaveSw">保存</button></span></div>
          <div class="pb" style="padding:0"><table class="grid"><thead><tr><th>交换机</th><th>读取地址</th><th>团体名</th><th>读取</th><th>状态</th><th></th></tr></thead><tbody id="lSw"></tbody></table></div>
          <div class="pb help" style="margin:0">读取地址留空使用交换机的管理 IP；团体名留空使用下方默认团体名。核心交换机没有填管理 IP 时，在这里填写它的任一三层接口地址。</div></div>
        <div class="panel" id="lSugBox"><div class="ph">端口变化待确认<span class="r btns" style="margin:0"><button class="btn small" id="lApplyAll">全部采纳</button></span></div><div class="pb" id="lSug"></div></div>
        <div class="panel"><div class="ph">本次定位<span class="r" id="lChgCnt"></span></div><div class="pb" id="lChg"></div></div>
        <div class="panel" id="lUnresBox"><div class="ph">无法确定的设备</div><div class="pb" id="lUnres"></div></div>
        <div class="panel" id="lStrBox"><div class="ph">端口上未登记的 MAC<span class="r muted" id="lStrCnt"></span></div>
          <div class="pb" style="padding:0;max-height:300px;overflow:auto"><table class="grid"><thead><tr><th>MAC</th><th>交换机</th><th>端口</th><th>VLAN</th></tr></thead><tbody id="lStr"></tbody></table></div>
          <div class="pb help" style="margin:0">这些设备在交换机上有流量但不在拓扑中，可用「扫描发现」扫描对应网段添加（添加后会自动定位）。</div></div>
        <div class="panel"><div class="ph">MAC 地址表<span class="r" style="display:flex;gap:6px"><select id="lTblSw" style="width:auto"></select>
          <button class="btn small" id="lTblLoad">读取</button></span></div>
          <div class="pb" style="padding:0;max-height:520px;overflow:auto" id="lTbl"><div class="pb muted">选择交换机后点「读取」。</div></div></div>
      </div></div></div>`;

    const swName = id => { const s = S.topo && S.topo.switches.find(x => x.id === id); return s ? (s.name || s.id) : id; };
    const nodeLink = (id, label) => id ? `<a href="#/node/${encodeURIComponent(id)}">${esc(label || id)}</a>` : esc(label || '');

    function drawSwitches() {
      const ls = cfg.locate;
      const last = data && data.last;
      const stBy = {};
      ((last && last.switches) || []).forEach(x => { stBy[x.switch] = x; });
      const tgt = {};
      (data.targets || []).forEach(x => { tgt[x.switch] = x; });
      $('#lSw').innerHTML = S.topo.switches.map(sw => {
        const o = (ls.switches || {})[sw.id] || {};
        const st = stBy[sw.id];
        let stat = '<span class="muted">—</span>';
        if (!tgt[sw.id]) stat = o.disabled ? '<span class="muted">不读取</span>' : '<span class="v-down">缺读取地址或团体名</span>';
        else if (st) stat = st.ok ? `<span class="v-ok" title="${esc(st.sys_descr || '')}">✓ ${esc(st.sys_name || '')} · ${st.entries} 条</span>`
          : `<span class="v-down" title="${esc(st.error)}">✗ ${esc(st.error.length > 24 ? st.error.slice(0, 24) + '…' : st.error)}</span>`;
        return `<tr data-sw="${esc(sw.id)}"><td><b>${esc(sw.name || sw.id)}</b><br><span class="mono muted" style="font-size:11px">${esc(sw.ip || '无管理 IP')}</span></td>
          <td><input type="text" name="host" class="mono" value="${esc(o.host || '')}" placeholder="${esc(sw.ip || '需填写')}" style="width:130px"></td>
          <td><input type="text" name="community" value="${esc(o.community || '')}" placeholder="默认" style="width:90px" autocomplete="off"></td>
          <td><input type="checkbox" name="enabled" ${o.disabled ? '' : 'checked'}></td>
          <td style="font-size:12px">${stat}</td>
          <td style="white-space:nowrap"><button class="btn small" data-test="${esc(sw.id)}">测试</button> <button class="btn small" data-table="${esc(sw.id)}">MAC 表</button> <button class="btn small" data-telnet="${esc(sw.id)}">Telnet</button> <button class="btn small" data-ssh="${esc(sw.id)}">SSH</button></td></tr>`;
      }).join('');
      $('#lTblSw').innerHTML = S.topo.switches.map(sw => `<option value="${esc(sw.id)}" ${sw.id === tableSw ? 'selected' : ''}>${esc(sw.name || sw.id)}</option>`).join('');
    }

    function drawForm() {
      const l = cfg.locate;
      $('#lf').innerHTML = `
        <div class="checks" style="margin-bottom:8px">
          <label><input type="checkbox" name="enabled" ${l.enabled ? 'checked' : ''}> 定时定位</label>
          <label><input type="checkbox" name="notify" ${l.notify ? 'checked' : ''}> 端口变化推送企业微信</label></div>
        <div class="row3">
          <label class="f"><span>默认只读团体名</span><input type="text" name="community" value="${esc(l.community || '')}" placeholder="未设置" autocomplete="off"></label>
          <label class="f"><span>定位间隔（分钟，≥2）</span><input type="number" name="interval_min" min="2" value="${l.interval_min}"></label>
          <label class="f"><span>SNMP 超时 ms</span><input type="number" name="timeout_ms" min="200" value="${l.timeout_ms}"></label>
        </div>
        <div class="btns"><button class="btn primary" type="submit">保存</button></div>
        <div class="help">扫描发现添加了新设备后也会自动定位一次。设备离线超过交换机的 MAC 老化时间（通常 5 分钟）后会从 MAC 表消失，届时不会被定位。</div>`;
      $('#lf').onsubmit = async ev => {
        ev.preventDefault();
        const e = ev.target.elements;
        await saveLocate(l2 => Object.assign(l2, { enabled: e.enabled.checked, notify: e.notify.checked, community: e.community.value.trim(),
          interval_min: +e.interval_min.value, timeout_ms: +e.timeout_ms.value }), '端口定位设置已保存');
      };
    }

    async function saveLocate(mut, msg) {
      const c = JSON.parse(JSON.stringify(cfg));
      c.locate.switches = c.locate.switches || {};
      mut(c.locate);
      try { await api('PUT', 'api/config', c); toast(msg); await load(); }
      catch (err) { toast(errMsg(err), true); }
    }

    function drawResult() {
      const r = data.last;
      $('#lState').innerHTML = cfg.locate.enabled ? `<span class="badge ok">定时 · 每 ${cfg.locate.interval_min} 分钟</span>` : '<span class="badge unknown">定时定位已关闭</span>';
      if (!r) {
        $('#lSum').innerHTML = (data.targets || []).length ? '<div class="muted">还没有定位结果。</div>'
          : '<div class="banner">还没有可读取的交换机：请在下方设置默认团体名，并确认交换机有读取地址。</div>';
      } else {
        const ok = (r.switches || []).filter(x => x.ok).length;
        $('#lSum').innerHTML = `<div class="stat-grid">
            <div><div class="v">${ok}/${(r.switches || []).length}</div><div class="k">交换机读取成功</div></div>
            <div><div class="v">${r.matched || 0}</div><div class="k">MAC 表中找到的设备</div></div>
            <div><div class="v">${(r.changes || []).filter(c => c.type !== 'location').length}</div><div class="k">自动定位</div></div>
            <div><div class="v">${(r.suggestions || []).length}</div><div class="k">端口变化待确认</div></div></div>
          <div class="help">${esc(fmtTime(r.started))} · ${esc(r.reason)}${r.dry_run ? '（预览，未写入）' : ''} · 用时 ${(r.duration_ms / 1000).toFixed(1)} 秒
            ${(r.switches || []).filter(x => (x.uplinks || []).length).map(x => `<br>${esc(x.name)} 级联口：${esc(x.uplinks.join('、'))}`).join('')}</div>
          ${r.error ? `<div class="v-down">${esc(r.error)}</div>` : ''}`;
      }
      const sug = (r && r.suggestions) || [];
      $('#lSugBox').classList.toggle('hidden', !sug.length);
      $('#lSug').innerHTML = sug.map(s => `<div style="display:flex;align-items:center;gap:8px;padding:6px 0;border-bottom:1px solid var(--line-2)">
          <span style="flex:1">${nodeLink(s.node_id, s.name)} <span class="mono muted">${esc(s.mac)}</span><br>
          <span class="muted">${esc(s.from)}</span> → <b>${esc(s.to)}</b>${s.location ? ` <span class="muted">（${esc(s.location)}）</span>` : ''}</span>
          <button class="btn small" style="flex:none" data-apply="${esc(s.node_id)}">采纳</button></div>`).join('');
      const chg = (r && r.changes) || [];
      $('#lChgCnt').textContent = chg.length ? chg.length + ' 项' : '';
      const typ = { located: ['已定位', 'ok'], moved: ['已改端口', 'slow'], location: ['补全位置', 'unknown'] };
      $('#lChg').innerHTML = chg.length ? `<table class="grid"><tbody>` + chg.map(c => {
        const [t, k] = typ[c.type] || [c.type, 'unknown'];
        return `<tr><td><span class="badge ${k}">${t}</span></td><td>${nodeLink(c.node_id, c.name)}</td><td class="mono">${esc(c.mac)}</td>
          <td class="wrap">${esc(c.to)}${c.location ? ' · ' + esc(c.location) : ''}</td></tr>`;
      }).join('') + '</tbody></table>' : '<div class="muted">没有需要修改的设备。</div>';
      const un = (r && r.unresolved) || [];
      $('#lUnresBox').classList.toggle('hidden', !un.length);
      $('#lUnres').innerHTML = un.map(u => `<div style="padding:5px 0;border-bottom:1px solid var(--line-2)">${nodeLink(u.node_id, u.name)} <span class="mono muted">${esc(u.mac)}</span>
        <div class="muted" style="font-size:12px">${esc(u.reason)}：${esc((u.seen || []).map(s => `${s.switch_name} ${s.port}${s.uplink ? '（级联）' : ''}`).join('、'))}</div></div>`).join('');
      const str = (r && r.strangers) || [];
      $('#lStrBox').classList.toggle('hidden', !str.length);
      $('#lStrCnt').textContent = str.length ? str.length + ' 个' : '';
      $('#lStr').innerHTML = str.map(x => `<tr><td class="mono">${esc(x.mac)}</td><td>${esc(swName(x.switch))}</td><td class="mono">${esc(x.port)}</td><td>${x.vlan || ''}</td></tr>`).join('');
    }

    async function load() {
      [data, cfg] = await Promise.all([api('GET', 'api/locate'), api('GET', 'api/config')]);
      if (!alive) return;
      if (!tableSw && S.topo.switches.length) tableSw = S.topo.switches[0].id;
      drawSwitches(); drawForm(); drawResult();
    }

    async function run(dry) {
      if (busy) return;
      busy = true;
      $('#lRun').disabled = $('#lDry').disabled = true;
      $('#lRun').textContent = '读取中…';
      try {
        const r = await api('POST', 'api/locate/run', { dry_run: dry });
        const res = r.result;
        if (dry) { data.last = res; drawResult(); toast('预览完成（未写入）'); }
        else {
          const n = (res.changes || []).filter(c => c.type !== 'location').length;
          toast(`定位完成：自动定位 ${n} 台，端口变化待确认 ${(res.suggestions || []).length} 台`);
          await loadTopology(); await load();
        }
      } catch (e) { toast(errMsg(e), true); }
      busy = false;
      $('#lRun').disabled = $('#lDry').disabled = false;
      $('#lRun').textContent = '立即定位';
    }

    async function showTable(id, refresh) {
      tableSw = id;
      $('#lTblSw').value = id;
      $('#lTbl').innerHTML = '<div class="pb muted">读取中…</div>';
      try {
        const r = await api('GET', `api/locate/table/${encodeURIComponent(id)}${refresh ? '?refresh=1' : ''}`);
        const rows = (r.ports || []).slice().sort((a, b) => natCmp(a.port, b.port));
        $('#lTbl').innerHTML = rows.length ? `<table class="grid"><thead><tr><th>端口</th><th>MAC</th><th>设备</th></tr></thead><tbody>` +
          rows.map(p => p.macs.map((m, i) => `<tr>${i ? '' : `<td rowspan="${p.macs.length}" class="mono" style="vertical-align:top"><b>${esc(p.port)}</b>
              ${p.uplink ? `<br><span class="badge slow" title="${esc(p.uplink)}">级联</span>` : ''}<br><span class="muted" style="font-size:11px">${p.macs.length} 个</span></td>`}
            <td class="mono">${esc(m.mac)}${m.vlan ? ` <span class="muted">v${m.vlan}</span>` : ''}</td>
            <td>${m.node_id ? nodeLink(m.node_id, m.name || m.ip || m.node_id) : '<span class="muted">未登记</span>'}</td></tr>`).join('')).join('') + '</tbody></table>'
          : '<div class="pb muted">MAC 表为空。</div>';
      } catch (e) { $('#lTbl').innerHTML = `<div class="pb v-down">${esc(errMsg(e))}</div>`; }
    }

    function openTerminal(id, protocol = 'telnet') {
      if (closeTerminal) closeTerminal();
      const isSSH = protocol === 'ssh', title = isSSH ? 'SSH' : 'Telnet';
      const sw = S.topo.switches.find(s => s.id === id);
      const host = ((cfg.locate.switches || {})[id] || {}).host || sw.ip;
      if (!host) { toast('请先保存该交换机的读取地址或管理 IP', true); return; }
      const dialog = document.createElement('dialog');
      dialog.className = 'telnet-dialog' + (isSSH ? ' ssh-dialog' : '');
      dialog.innerHTML = `<div class="ph"><b>${esc(sw.name || id)} · ${title}</b><button class="btn small" data-close>关闭</button></div>
        <div class="pb"><form class="telnet-toolbar"><span class="mono telnet-host">${esc(host)}</span>
          <label>端口 <input name="port" type="number" min="1" max="65535" value="${isSSH ? 22 : 23}" required aria-label="${title} 端口"></label>
          ${isSSH ? `<label>用户名 <input name="username" class="ssh-username" required maxlength="256" autocomplete="off"></label>
          <label>密码 <input name="password" class="ssh-password" type="password" autocomplete="new-password" maxlength="4096"></label>` : ''}
          <button class="btn primary" type="submit">连接</button><button class="btn" type="button" data-disconnect disabled>断开</button>
          <span role="status" data-state>未连接</span></form>
          <div class="help">由 netPing 服务器连接交换机；${isSSH ? '先填写用户名和密码，首次连接需核对主机指纹。' : '请在终端输入账号和密码。Telnet 使用明文传输。'}账号密码不保存在 netPing。配置修改会直接作用于交换机，按设备要求保存配置。</div>
          ${isSSH ? `<div class="ssh-hostkey hidden" data-hostkey><b>首次连接：请核对设备主机指纹</b><div class="mono" data-fingerprint></div>
          <button type="button" class="btn primary" data-trust>信任此主机并继续</button> <button type="button" class="btn" data-reject>取消连接</button></div>` : ''}
          <div class="telnet-screen" aria-label="交换机交互终端"></div>
          <div class="btns"><button class="btn small" data-key="enter">回车</button><button class="btn small" data-key="space">空格 / 翻页</button><button class="btn small" data-key="interrupt">Ctrl+C</button><button class="btn small" data-key="exit">Ctrl+Z</button><span class="muted">支持方向键、Tab 补全；关闭窗口会断开连接。</span></div>
          ${isSSH ? `<form class="ssh-upload"><b>SFTP 文件上传</b><div class="help">设备需启用 SFTP。单个文件最大 1 GiB，默认保留远端同名文件。上传只传送文件，不会自动安装或执行。</div>
            <div class="ssh-upload-fields"><label class="f"><span>选择文件</span><input type="file" name="file" required></label>
            <label class="f"><span>远程目录</span><input type="text" name="directory" value="." required placeholder="如 /tmp 或 flash:/"></label></div>
            <div class="btns"><label><input type="checkbox" name="overwrite"> 覆盖远端同名文件</label><button class="btn primary" type="submit" disabled>上传文件</button><button class="btn" type="button" data-cancel-upload disabled>取消上传</button></div>
            <progress max="100" value="0" aria-label="上传进度"></progress><div role="status" class="help" data-upload-state>请先连接 SSH。</div>
          </form>` : ''}
        </div>`;
      document.body.appendChild(dialog);
      dialog.showModal();
      const term = new Terminal({ cursorBlink: true, fontSize: 14, fontFamily: 'Menlo, Consolas, monospace', scrollback: 3000, allowProposedApi: false });
      const fit = new FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open(dialog.querySelector('.telnet-screen'));
      let socket = null, disposed = false, sessionID = '', transfer = null, connected = false;
      const form = dialog.querySelector('form'), port = form.elements.port;
      const state = dialog.querySelector('[data-state]'), disconnect = dialog.querySelector('[data-disconnect]');
      const connect = form.querySelector('[type=submit]');
      const uploadForm = dialog.querySelector('.ssh-upload');
      const uploadState = dialog.querySelector('[data-upload-state]');
      const refreshUpload = () => {
        if (!uploadForm) return;
        uploadForm.querySelector('[type=submit]').disabled = !sessionID || !!transfer;
        uploadForm.querySelector('[data-cancel-upload]').disabled = !transfer;
        ['file', 'directory', 'overwrite'].forEach(name => { uploadForm.elements[name].disabled = !!transfer; });
      };
      const setState = (text, active) => {
        state.textContent = text; disconnect.disabled = !active; port.disabled = active; connect.disabled = active;
        if (isSSH) { form.elements.username.disabled = active; form.elements.password.disabled = active; }
      };
      const send = msg => { if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(msg)); };
      term.onData(data => { if (connected) send({ type: 'input', data }); });
      term.onResize(({ cols, rows }) => { if (connected) send({ type: 'resize', cols, rows }); });
      const resize = new ResizeObserver(() => { if (!disposed) fit.fit(); });
      resize.observe(dialog.querySelector('.telnet-screen'));
      const stop = () => {
        const ws = socket; socket = null;
        connected = false; sessionID = '';
        if (transfer) transfer.abort();
        if (isSSH) { form.elements.password.value = ''; dialog.querySelector('[data-hostkey]').classList.add('hidden'); }
        refreshUpload();
        if (ws) { ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null; ws.close(); }
        setState('已断开', false);
      };
      closeTerminal = () => {
        if (disposed) return;
        disposed = true; stop(); resize.disconnect(); term.dispose(); dialog.close(); dialog.remove(); closeTerminal = null;
      };
      dialog.querySelector('[data-close]').onclick = () => closeTerminal();
      dialog.addEventListener('cancel', ev => { ev.preventDefault(); closeTerminal(); });
      disconnect.onclick = () => { stop(); term.writeln('\r\n[已断开连接]'); };
      const keys = { enter: '\r', space: ' ', interrupt: '\x03', exit: '\x1a' };
      dialog.querySelectorAll('[data-key]').forEach(b => { b.onclick = () => { if (connected) send({ type: 'input', data: keys[b.dataset.key] }); term.focus(); }; });
      if (isSSH) {
        dialog.querySelector('[data-reject]').onclick = stop;
        uploadForm.querySelector('[data-cancel-upload]').onclick = () => { if (transfer) transfer.abort(); };
        uploadForm.onsubmit = ev => {
          ev.preventDefault();
          const file = uploadForm.elements.file.files[0];
          if (!file || !sessionID || transfer) return;
          if (file.size > 1024 ** 3) { uploadState.textContent = '文件超过 1 GiB，请选择较小的文件。'; return; }
          const url = new URL(`api/locate/ssh-upload/${sessionID}`, document.baseURI);
          url.searchParams.set('name', file.name); url.searchParams.set('dir', uploadForm.elements.directory.value.trim());
          url.searchParams.set('overwrite', uploadForm.elements.overwrite.checked ? '1' : '0');
          const xhr = new XMLHttpRequest(); transfer = xhr;
          const progress = uploadForm.querySelector('progress'); progress.value = 0;
          uploadState.textContent = '正在上传…'; refreshUpload();
          xhr.open('POST', url); xhr.setRequestHeader('Content-Type', 'application/octet-stream');
          xhr.upload.onprogress = ev => { if (ev.lengthComputable) { progress.value = ev.loaded / ev.total * 100; uploadState.textContent = progress.value >= 100 ? '文件已发送，等待设备确认…' : `正在上传 ${Math.floor(progress.value)}%`; } };
          xhr.onload = () => {
            let result = {}; try { result = JSON.parse(xhr.responseText); } catch (_) { /* HTTP error without JSON */ }
            if (xhr.status >= 200 && xhr.status < 300 && result.ok) { progress.value = 100; uploadState.textContent = `上传完成：${result.path}（${result.bytes} 字节）`; }
            else uploadState.textContent = result.error || '上传失败，请检查连接、目录及 SFTP 服务后重试。';
          };
          xhr.onerror = () => { uploadState.textContent = '上传连接中断，请检查网络及远端文件后重试。'; };
          xhr.onabort = () => { uploadState.textContent = '已取消上传；远端可能保留部分文件，请检查后重试。'; };
          xhr.onloadend = () => { transfer = null; refreshUpload(); };
          xhr.send(file);
        };
      }
      form.onsubmit = ev => {
        ev.preventDefault();
        const username = isSSH ? form.elements.username.value.trim() : '';
        let password = isSSH ? form.elements.password.value : '';
        stop(); term.reset(); fit.fit();
        const url = new URL(`api/locate/${protocol}/${encodeURIComponent(id)}`, document.baseURI);
        url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
        url.searchParams.set('port', port.value);
        const ws = new WebSocket(url); socket = ws; ws.binaryType = 'arraybuffer';
        setState('连接中…', true);
        term.writeln(`[正在连接 ${host}:${port.value}…]`);
        ws.onopen = () => {
          if (isSSH) { send({ type: 'auth', username, password, cols: term.cols, rows: term.rows }); password = ''; }
          else send({ type: 'resize', cols: term.cols, rows: term.rows });
        };
        ws.onmessage = ev => {
          if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data));
          else {
            const msg = JSON.parse(ev.data);
            if (msg.message) term.writeln(`\r\n[${msg.message}]`);
            if (msg.type === 'hostkey') {
              const box = dialog.querySelector('[data-hostkey]'); box.classList.remove('hidden');
              box.querySelector('[data-fingerprint]').textContent = `${msg.address}\n${msg.fingerprint}`;
              box.querySelector('[data-trust]').onclick = () => { send({ type: 'trust', fingerprint: msg.fingerprint }); box.classList.add('hidden'); };
              state.textContent = '等待核对主机指纹';
            }
            if (msg.type === 'connected') {
              connected = true; sessionID = msg.session || ''; setState(msg.message, true); refreshUpload();
              if (isSSH) uploadState.textContent = '已连接，可选择文件并上传。';
              send({ type: 'resize', cols: term.cols, rows: term.rows }); term.focus();
            }
          }
        };
        ws.onerror = () => { term.writeln(`\r\n[连接失败：请检查网页登录状态、已保存的地址、${title} 端口及服务器到交换机的网络，然后点击连接重试。]`); };
        ws.onclose = () => { password = ''; stop(); term.writeln('\r\n[连接已关闭]'); };
      };
      fit.fit();
      if (isSSH) form.elements.username.focus(); else form.requestSubmit();
    }

    $('#lRun').onclick = () => run(false);
    $('#lDry').onclick = () => run(true);
    $('#lTblLoad').onclick = () => showTable($('#lTblSw').value, true);
    $('#lSaveSw').onclick = () => saveLocate(l => {
      $$('#lSw tr[data-sw]').forEach(tr => {
        const id = tr.dataset.sw, q = n => tr.querySelector(`[name=${n}]`);
        l.switches[id] = { host: q('host').value.trim(), community: q('community').value.trim(), disabled: !q('enabled').checked };
      });
    }, '交换机读取设置已保存');
    $('#lApplyAll').onclick = async () => {
      const sug = (data.last && data.last.suggestions) || [];
      if (!sug.length || !confirm(`把 ${sug.length} 台设备的接线位置改为 MAC 表中查到的端口？`)) return;
      try {
        const r = await api('POST', 'api/locate/run', { apply_moves: true });
        toast(`已修改 ${(r.result.changes || []).length} 项`);
        await loadTopology(); await load();
      } catch (e) { toast(errMsg(e), true); }
    };
    const onLocateClick = async ev => {
      const b = ev.target.closest('[data-test],[data-table],[data-apply],[data-telnet],[data-ssh]');
      if (!b) return;
      if (b.dataset.ssh) { openTerminal(b.dataset.ssh, 'ssh'); return; }
      if (b.dataset.telnet) { openTerminal(b.dataset.telnet); return; }
      if (b.dataset.table) { showTable(b.dataset.table, true); return; }
      if (b.dataset.apply) {
        try { await api('POST', 'api/locate/apply', { node_id: b.dataset.apply }); toast('已修改接线位置'); await loadTopology(); await load(); }
        catch (e) { toast(errMsg(e), true); }
        return;
      }
      const tr = b.closest('tr');
      const q = n => tr.querySelector(`[name=${n}]`);
      b.disabled = true; b.textContent = '测试中…';
      try {
        const r = await api('POST', 'api/locate/test', { switch: b.dataset.test, host: q('host').value.trim(), community: q('community').value.trim() });
        toast(`✓ ${r.sys_name || ''}：${r.interfaces} 个接口，MAC 表 ${r.mac_entries} 条${(r.warnings || []).length ? '；' + r.warnings.join('；') : ''}`, !r.mac_entries);
      } catch (e) { toast('测试失败：' + errMsg(e), true); }
      b.disabled = false; b.textContent = '测试';
    };
    app.addEventListener('click', onLocateClick);
    load().catch(e => toast(errMsg(e), true));
    return { destroy() { alive = false; app.removeEventListener('click', onLocateClick); if (closeTerminal) closeTerminal(); } };
  }

  // ---------------------------------------------------------------------------
  loadTopology().then(route).catch(e => { toast(errMsg(e), true); route(); });
  connectStream();
})();
