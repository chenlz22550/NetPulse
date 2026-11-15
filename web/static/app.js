/* 网脉 NetPulse 管理页面主程序：路由、数据同步、各页面渲染。 */
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
    const init = { method, headers: {}, signal: opts.signal };
    if (S.activeProfile) init.headers['X-NetPing-Profile'] = S.activeProfile;
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
      if ((r.status === 409 || (data && data.active_profile && S.activeProfile && data.active_profile !== S.activeProfile)) && !opts.skipProfileReset) {
        resetProfileView();
      }
      const e = new Error((data && data.error) || ('HTTP ' + r.status));
      e.problems = data && data.problems;
      throw e;
    }
    return data;
  }
  // 读取 NDJSON 流式接口（AI 分析），每行一个事件交给 onEvent
  async function streamNDJSON(url, body, signal, onEvent) {
    const r = await fetch(url, { method: 'POST', signal, headers: { 'Content-Type': 'application/json', ...(S.activeProfile ? { 'X-NetPing-Profile': S.activeProfile } : {}) }, body: JSON.stringify(body || {}) });
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      if (r.status === 401 && d.login_required) { toLogin(); throw new Error('登录已过期，请重新登录'); }
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
        onEvent(ev);
      }
    }
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
  // 下拉框里的设备名：非交换机附上类型，如“出口 · 防火墙”
  function swLabel(s) { const t = TopoTypes.of(s); return (s.name || s.id) + (t === 'switch' ? '' : ' · ' + TopoTypes.info[t].name); }
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
  if (Charts.setDeviceResolver) {
    Charts.setDeviceResolver(hint => {
      if (!S.topo || !hint) return null;
      const all = S.topo.switches.concat(S.topo.nodes);
      const s = String(hint).trim();
      for (const d of all) {
        if (d.id === s || (d.ip && d.ip === s) || (d.name && d.name === s)) return d;
      }
      const ipMatch = s.match(/\b(\d{1,3}(?:\.\d{1,3}){3})\b/);
      if (ipMatch) {
        const hit = all.find(d => d.ip === ipMatch[1]);
        if (hit) return hit;
      }
      const named = all.filter(d => d.name && d.name.length >= 2 && s.includes(d.name))
        .sort((a, b) => b.name.length - a.name.length);
      return named[0] || null;
    });
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
  function resetProfileView(targetHash) {
    if (page && page.destroy) page.destroy();
    if (closeTerminal) closeTerminal();
    ['np_report_cur', 'np_ai', 'np_aiopt', 'np_aid'].forEach(k => sessionStorage.removeItem(k));
    if (targetHash) {
      history.replaceState(null, '', targetHash);
    } else if (/^#\/(node|report|ai)\//.test(location.hash)) {
      history.replaceState(null, '', '#/topo');
    }
    location.reload();
  }
  async function loadTopology() {
    const d = await api('GET', 'api/topology');
    if (S.activeProfile && d.active_profile && S.activeProfile !== d.active_profile) { resetProfileView(); return; }
    S.activeProfile = d.active_profile || '';
    S.activeProfileName = d.active_profile_name || '';
    S.topo = d.topology;
    S.rev = d.rev;
    S.changedOnDisk = d.changed_on_disk;
    computeVendorColors();
    const site = S.topo.site || {};
    $('#siteTitle').innerHTML = `${esc(site.name || '网脉 NetPulse')}<span class="slash">/</span>网络拓扑`;
    $('#siteSub').textContent = [site.subnet, site.gateway ? '网关 ' + site.gateway : '', `${S.topo.switches.length} 交换机 · ${S.topo.nodes.length} 节点`].filter(Boolean).join(' · ');
    document.title = (site.name || '网脉 NetPulse') + ' · 网脉 NetPulse';
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
      if (d.active_profile && S.activeProfile && d.active_profile !== S.activeProfile) { resetProfileView(); return; }
      S.connected = true;
      S.status = d.statuses || {};
      S.tcpStatus = d.tcp_statuses || [];
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
    if (closeTerminal) closeTerminal();
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
      case 'plugins': page = pluginsPage(app); break;
      case 'discovery': page = discoveryPage(app); break;
      case 'locate': page = locatePage(app); break;
      case 'guide': page = guidePage(app); break;
      case 'ai': page = aiPage(app, decodeURIComponent(arg || '')); break;
      case 'report': page = reportPage(app, decodeURIComponent(arg || '')); break;
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
          <li><b>运行程序。</b>Windows：把 <code>网脉 NetPulse.exe</code> 放到固定目录（如 <code>D:\\网脉 NetPulse\\</code>）双击运行，不需要管理员权限；
            macOS：把 <code>网脉 NetPulse.app</code> 拖到「应用程序」后打开，首次提示“无法验证开发者”时右键 →「打开」。
            程序常驻在任务栏托盘 / 菜单栏，图标颜色：<span class="g-dot ok"></span>正常 <span class="g-dot warn"></span>丢包或延迟高 <span class="g-dot down"></span>有设备离线 <span class="g-dot idle"></span>暂停。</li>
          <li><b>设置登录密码。</b>第一次打开管理页面时，在<b>运行 网脉 NetPulse 的电脑上</b>设置密码（至少 6 位）。之后每次打开都要输入密码，登录后这个浏览器 30 天内免登录。</li>
          <li><b>核对机房和设备。</b>在「设置 → 机房信息」填写网段和默认网关；到「拓扑」页检查交换机、端口和设备是否和实际一致。设备多时可以用「扫描发现」自动添加。</li>
          <li><b>配置告警。</b>在「设置」里填写所需通知渠道的 Webhook 并发送测试消息；再到「监控列表」给需要告警的设备打勾。<b>设备默认只监控、不告警。</b></li>
          <li><b>按需开启更多功能：</b>「端口定位」自动查出设备插在哪个交换机口；「MCP」让 AI 助手用自然语言查询和维护。</li>
        </ol>
        <div class="g-note">其他电脑访问：浏览器打开 <code>http://服务器IP:8090/</code>。打不开时检查防火墙是否放行 8090 端口，以及 config.yaml 中 <code>listen</code> 是否为 <code>0.0.0.0:8090</code>。</div>`],

      ['topo', '看懂拓扑图', `
        <h4>交换机与网口</h4>
        <ul>
          <li>交换机竖着画，网口按实物排列：<b>每排左边奇数口、右边偶数口</b>，每 8 个口一组，光口（SFP）单独放在下面。</li>
          <li>网口指示灯：<span class="g-dot ok"></span>设备在线 <span class="g-dot warn"></span>丢包 / 延迟高 <span class="g-dot down"></span>离线，级联口用主题色表示。鼠标停在网口上可看到端口名和下面挂了哪些设备。</li>
          <li>交换机底部显示已用口数，如 <code>17/30 口</code>。</li>
          <li>路由器、防火墙、上网行为管理、光猫、无线 AP、无线控制器、负载均衡也用同样的机身画法，左上角图标和色条区分类型（见右侧「图例符号」）；它们和交换机一样可以接终端、互相连线，比如 <b>光猫 → 防火墙 → 核心交换机</b>。用工具栏「+ 网络设备」添加，选择类型后自动填入常用端口。</li>
        </ul>
        <h4>设备卡片</h4>
        <ul>
          <li>卡片显示 IP、厂商（或名称）、MAC 和实时延迟。<b>红框</b>为离线，<b>橙框</b>为丢包或延迟高，左侧色条代表厂商（右侧「厂商图例」可点击筛选）。</li>
          <li>卡片上方的标签：默认网关、关键、存疑（斜纹）、DHCP、自动发现、跳板机、<span class="g-tag">告警</span>（已开启告警通知）。</li>
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
          <li>工具栏「+ 网络设备 / + 节点 / + 链路」新增；「自动排布」把拖乱的卡片恢复为按端口排列，未定位的设备按 IP 整齐排在下方，10 秒内可点「撤销」。</li>
          <li>交换机的编辑表单里可以设置端口列表、终端卡片显示在左侧还是右侧、每个端口的卡片列数、端口物理位置对照。</li>
          <li>右侧「检索 / 筛选」可按 IP、MAC、厂商、端口、名称搜索，或筛选异常、已开告警、多 MAC 端口等。</li>
        </ul>
        ${go('topo', '打开拓扑')}`],

      ['list', '监控列表与延迟曲线', `
        <ul>
          <li>「监控列表」列出所有地址的当前延迟、1 分钟平均、抖动、1 分钟 / 5 分钟 / 1 小时丢包率和最近 60 次的迷你曲线，点表头排序，可按状态和关键字筛选。</li>
          <li>「告警」列勾选即开启这台设备的告警通知；先筛选再点「筛选结果开启告警」可批量开启。</li>
          <li>点设备进入详情：延迟曲线（平均值、最小~最大区间、阈值线）和丢包柱状图，范围从 15 分钟到 30 天。</li>
          <li>历史数据按分钟汇总保存，默认保留 30 天（「设置 → 默认 Ping 参数」可改）。</li>
        </ul>
        ${go('list', '打开监控列表')}`],

      ['alert', '告警通知', `
        <ol class="g-steps">
          <li>选择企业微信、钉钉、飞书、Slack 或通用 Webhook，获取对应地址；钉钉、飞书可填写签名密钥。</li>
          <li>填到「设置 → 告警通知渠道」，勾选「启用告警推送」，保存后点「发送测试消息」，确认各渠道收到。</li>
          <li>给需要告警的设备开启告警：「监控列表」勾选，或在拓扑页设备表单中勾选「推送告警通知」。</li>
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
          <li><b>AI 自主发现</b>：不知道还有哪些网段、哪些设备时，点「✦ AI 自主发现」（可写上要求或指定网段）。AI 会根据本机网卡、ARP 表、已有设备推断要探测的网段，
            只读扫描后识别新设备的类型（看开放端口、网页标题、SNMP 描述、主机名、厂商），能定位的查出接在哪个交换机口，最后列出建议添加的设备、建议新建的扫描任务和其他发现
            （如私接路由器）。名称、类别、告警都可以改，勾选后才添加，添加后可以撤销。只探测私有地址和已配置的网段，需要先在「AI 诊断」页设置模型。</li>
          <li>扫描时读取电脑的 ARP 表获得 MAC，所以<b>只能看到和运行 网脉 NetPulse 的电脑在同一网段（VLAN）的设备 MAC</b>；其他网段只能发现在线。</li>
        </ul>
        <div class="g-note">macOS 上第一次扫描时，如果系统询问“允许 网脉 NetPulse 查找本地网络上的设备”，请选择<b>允许</b>，否则读不到 MAC。</div>
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
          <li>数据目录：Windows 为 exe 所在目录，macOS 为 <code>~/Library/Application Support/网脉 NetPulse</code>，可从托盘菜单「打开数据目录」进入。<code>data/</code> 是历史数据和告警记录，<code>logs/</code> 是日志。</li>
          <li><b>迁移到新机房</b>：把新机房交换机的 MAC 表和 ARP 表交给 AI，按 <code>docs/topology-fields.md</code> 里的提示词生成新的 topology.yaml；接入 MCP 后也可以直接让 AI 生成并写入。</li>
        </ul>
        ${go('settings', '打开设置')}`],

      ['aidiag', 'AI 诊断：让大模型帮你看问题', `
        <ul>
          <li>「AI 诊断」页用大模型（默认 DeepSeek 的 deepseek-flash）结合 网脉 NetPulse 的实时数据和历史曲线诊断网络问题，给出<b>结论、依据和排查步骤</b>。</li>
          <li>第一次使用：在页面右侧填写 API Key（在 DeepSeek 开放平台创建），点「测试连接」确认正常。服务器不能直接上网时，可填写 HTTP 代理（留空则使用告警设置里的代理）。</li>
          <li>点「一键诊断」「最近 1 小时」「分析告警」等快捷按钮，或直接描述现象，如“3 楼打印机经常连不上”。可以接着追问。</li>
          <li>在拓扑页的设备详情、设备曲线页点「AI 诊断」，会直接诊断这一台设备。</li>
          <li>AI 会按需查询设备列表、历史曲线、告警记录、扫描与端口定位结果，回答上方会显示它查了什么；遇到复杂故障还会直接画出<b>故障路径图、对比柱状图、趋势曲线或异常时间线</b>。AI <b>只能查询，不能修改</b>任何配置。</li>
          <li>「深度思考」更慢、更贵，复杂问题时再开。每次诊断下方显示用时、查询次数和 token 用量。</li>
        </ul>
        <div class="g-note">诊断时会把相关设备的 IP、MAC、名称和延迟丢包数据发送给模型服务商。API Key 保存在 config.yaml，网页和 MCP 都不会返回它。</div>
        ${go('ai', '打开 AI 诊断')}`],

      ['mcp', 'AI 接入（MCP）', `
        <ul>
          <li>网脉 NetPulse 内置 MCP 接口，可以接入 Claude Code、Cursor、WorkBuddy、VS Code、Claude Desktop、Cherry Studio 等 AI 客户端，用自然语言查延迟丢包、分析历史、维护设备和告警。</li>
          <li>接入步骤：在「MCP」页点「生成」访问令牌并保存 → 选择你用的客户端，复制配置片段 → 粘贴到客户端的 MCP 配置里。点「测试连接」可确认接口可用。</li>
          <li>只想让 AI 查询、不允许修改时，勾选「只读模式」。AI 的每次修改都会记录在日志里。</li>
          <li>页面下方有示例问法，比如“现在机房有哪些设备离线或丢包？”、“扫描一下网段，把新发现的打印机放到接入交换机 Port 7”。</li>
        </ul>
        ${go('mcp', '打开 MCP')}`],

      ['plugins', '插件', `
        <ul>
          <li>插件可以给 网脉 NetPulse 增加新的<b>通知渠道</b>（邮件、短信、工单系统…）、在告警或设备变化时<b>执行自定义动作</b>，或给 AI 助手提供<b>新的查询工具</b>。</li>
          <li>安装：把插件文件夹放进数据目录的 <code>plugins</code> 目录 → 在「插件」页点「重新扫描」→ 填写设置 → 点「测试运行」确认正常 → 点「启用」。新插件默认不启用。</li>
          <li>通知类插件和企业微信等内置渠道一起收到告警；只用插件发通知时，不填内置渠道地址也可以打开告警开关。</li>
          <li>插件是独立的小程序，以 网脉 NetPulse 的权限运行，<b>只安装来源可信的插件</b>。网页和 AI 助手都不能上传或安装插件。</li>
          <li>想自己写插件：用任何语言都可以，「插件」页的「开发指南」里有协议说明和完整示例。</li>
        </ul>
        ${go('plugins', '打开插件')}`],

      ['account', '登录与安全', `
        <ul>
          <li>打开管理页面需要输入密码，登录后这个浏览器 30 天内免登录；网脉 NetPulse 重启不影响。</li>
          <li>「设置 → 登录密码」可以修改密码或退出登录。修改密码后，其他已登录的浏览器都需要重新登录。</li>
          <li>同一来源连续输错 5 次密码会被暂时锁定（1 分钟起，最长 15 分钟）。</li>
          <li><b>忘记密码</b>：删除 config.yaml 中 <code>auth</code> 下的 <code>password_hash</code> 和 <code>password</code> 两行，重启 网脉 NetPulse，再在运行 网脉 NetPulse 的电脑上打开网页重新设置。</li>
        </ul>`],

      ['faq', '常见问题', `
        <dl class="g-faq">
          <dt>所有设备都显示离线？</dt>
          <dd>先在运行 网脉 NetPulse 的电脑上用命令行 <code>ping</code> 一个设备 IP，确认网络能通。电脑不在机房网段、或设备防火墙禁 ping 时都会显示离线。</dd>
          <dt>群里收不到告警？</dt>
          <dd>先点「发送测试消息」看报错；确认设备已开启告警（卡片上有「告警」标签）；检查是否处于暂停或非监控时段。服务器不能上网时需要配置代理。</dd>
          <dt>扫描发现读不到 MAC、不能跟踪 IP？</dt>
          <dd>只有和运行 网脉 NetPulse 的电脑在同一网段的设备才能读到 MAC。macOS 还需要在「系统设置 → 隐私与安全性 → 本地网络」里允许 网脉 NetPulse。</dd>
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
  // 轻量 Markdown 渲染：先转义再处理标题、列表、粗体、行内代码、代码块（含可视化图表）、表格
  // opt.doc：文档模式（运维报告），# ~ #### 分别输出 h1 ~ h4，并支持 --- 分隔线
  function mdLite(src, opt) {
    const doc = !!(opt && opt.doc);
    const inline = t => esc(t)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
    const lines = String(src || '').replace(/\r/g, '').split('\n');
    let html = '', list = null, i = 0;
    const closeList = () => { if (list) { html += `</${list}>`; list = null; } };
    while (i < lines.length) {
      const l = lines[i];
      let fm;
      if ((fm = l.match(/^\s*```(.*)$/))) {
        closeList();
        const lang = (fm[1] || '').trim().toLowerCase();
        const buf = [];
        i++;
        while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) buf.push(lines[i++]);
        const closed = i < lines.length;
        if (closed) i++;
        const raw = buf.join('\n');
        const chartHTML = window.Charts && Charts.aiBlock ? Charts.aiBlock(lang, raw, closed) : '';
        if (chartHTML) { html += chartHTML; continue; }
        html += `<pre class="ai-code">${esc(raw)}</pre>`;
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
      if ((m = l.match(/^(#{1,4})\s+(.*)$/))) { closeList(); const h = doc ? m[1].length : 4; html += `<h${h}>${inline(m[2])}</h${h}>`; }
      else if (doc && /^\s*(-{3,}|\*{3,})\s*$/.test(l)) { closeList(); html += '<hr>'; }
      else if ((m = l.match(/^\s*[-*•]\s+(.*)$/))) { if (list !== 'ul') { closeList(); html += '<ul>'; list = 'ul'; } html += `<li>${inline(m[1])}</li>`; }
      else if ((m = l.match(/^\s*\d+[.、)]\s+(.*)$/))) { if (list !== 'ol') { closeList(); html += '<ol>'; list = 'ol'; } html += `<li>${inline(m[1])}</li>`; }
      else if (!l.trim()) { closeList(); }
      else { closeList(); html += `<p>${inline(l)}</p>`; }
      i++;
    }
    closeList();
    return html;
  }

  // ---------------------------------------------------------------------------
  // AI 思考过程面板：运行中展开显示执行步骤、工具调用和模型服务返回的思考内容；完成、出错或停止后自动折叠。
  // 只展示模型服务主动返回的思考内容/摘要（需所选模型支持并开启「深度思考」），不会要求模型额外输出内部推理。
  const THINKS = new Map();
  let thinkSeq = 0;
  function thinkNew() {
    const th = { id: 'th' + Date.now().toString(36) + (++thinkSeq), phase: 'running', open: true, steps: [], reasoning: '', reviewReasoning: '', start: Date.now(), ms: 0, calls: 0, runId: '', thinkingOn: null };
    THINKS.set(th.id, th);
    thinkTrim();
    return th;
  }
  function thinkTrim() { while (THINKS.size > 200) THINKS.delete(THINKS.keys().next().value); }
  function thinkAdopt(th) { if (th && th.id) { if (th.phase === 'running') { th.phase = 'stopped'; th.open = false; } THINKS.set(th.id, th); thinkTrim(); } return th; }
  function thinkEvent(th, ev) {
    if (!th) return;
    switch (ev.t) {
      case 'run': if (!th.runId) th.runId = ev.run_id || ''; if (ev.thinking != null) th.thinkingOn = !!ev.thinking; break;
      case 'status': {
        const last = th.steps[th.steps.length - 1];
        if (ev.text && !(last && last.k === 'status' && last.text === ev.text)) th.steps.push({ k: 'status', text: ev.text, at: Date.now() });
        break;
      }
      case 'thinking': if (ev.phase === 'review') th.reviewReasoning += ev.text || ''; else th.reasoning += ev.text || ''; break;
      case 'tool':
        th.calls++;
        th.steps.push({ k: 'tool', name: ev.name, title: ev.title, args: ev.args || '', phase: ev.phase || '', status: ev.status || 'ok', ms: ev.ms, error: ev.error || '', call_id: ev.call_id || '', bytes: ev.result_bytes });
        break;
      case 'review': th.steps.push({ k: 'status', text: '开始自检：对照数据核对结论', at: Date.now() }); break;
      case 'review_end': th.steps.push({ k: 'status', text: { pass: '自检通过', revised: '自检发现问题，已修正回答', failed: '自检未完成', skipped: '跳过自检' }[ev.result] || '自检结束', at: Date.now() }); break;
      case 'error': thinkFinish(th, 'error', ev.error); break;
      case 'done': thinkFinish(th, 'done'); break;
    }
  }
  function thinkFinish(th, phase, note) {
    if (!th || th.phase !== 'running') return;
    th.phase = phase; th.ms = Date.now() - th.start; th.open = false;
    if (note) th.note = String(note);
  }
  function thinkHTML(th) {
    if (!th) return '';
    const running = th.phase === 'running';
    const secs = ((running ? Date.now() - th.start : th.ms) / 1000).toFixed(1);
    const label = { running: '思考中', done: '思考过程', error: '思考过程（出错中断）', stopped: '思考过程（已停止）' }[th.phase] || '思考过程';
    const steps = th.steps.map(x => {
      if (x.k === 'status') return `<li class="st">${esc(x.text)}</li>`;
      const bad = x.status && x.status !== 'ok';
      const args = x.args && x.args !== '{}' ? ` <code class="mono">${esc(x.args)}</code>` : '';
      const extra = [x.ms != null ? x.ms + ' ms' : '', x.bytes != null ? (x.bytes >= 1024 ? (x.bytes / 1024).toFixed(1) + ' KB' : x.bytes + ' B') : ''].filter(Boolean).join(' · ');
      return `<li class="tool${bad ? ' bad' : ''}" ${x.call_id ? `title="调用 ID：${esc(x.call_id)}"` : ''}>${x.phase === 'review' ? '核实' : '调用'} <b>${esc(x.title || x.name)}</b>${args}
        <span class="${bad ? 'v-down' : 'v-ok'}">${bad ? (x.status === 'denied' ? '已拒绝' : '失败') : '成功'}</span>${extra ? ` <span class="muted">${extra}</span>` : ''}${x.error ? `<div class="v-down">${esc(x.error)}</div>` : ''}</li>`;
    }).join('');
    const rsn = (txt, title) => txt ? `<div class="ai-think-label">${title}</div><div class="ai-think-text">${esc(txt)}</div>` : '';
    let reasoning = rsn(th.reasoning, '模型返回的思考内容（由模型服务提供，可能为摘要）') + rsn(th.reviewReasoning, '自检阶段模型返回的思考内容');
    if (!reasoning && !running) {
      reasoning = `<div class="help">${th.thinkingOn === false ? '未开启「深度思考」，这里只显示执行步骤与工具调用。' : '模型服务没有返回思考内容（取决于所选模型与协议），这里只显示执行步骤与工具调用。'}</div>`;
    }
    return `<details class="ai-think${running ? ' running' : ''}" data-think="${esc(th.id)}" ${th.open ? 'open' : ''}>
      <summary>${running ? '<span class="ai-spin"></span>' : ''}${label}<span class="muted"> · ${th.steps.length} 步 · 工具 ${th.calls} 次 · ${secs} 秒</span></summary>
      <div class="ai-think-body">${steps ? `<ol class="ai-think-steps">${steps}</ol>` : ''}${reasoning}
        ${th.note && th.phase === 'error' ? `<div class="v-down">${esc(th.note)}</div>` : ''}
        ${th.runId ? `<div class="help">关联 ID：<code class="mono">${esc(th.runId)}</code> · <a href="#/mcp" data-run-link="${esc(th.runId)}">查看本次全部工具调用的完整参数与返回</a></div>` : ''}</div></details>`;
  }
  document.addEventListener('click', ev => {
    const a = ev.target.closest && ev.target.closest('[data-run-link]');
    if (a) sessionStorage.setItem('np_mcp_run', a.dataset.runLink);
  });
  // details 每次重绘都会重建，记住用户手动展开/折叠的状态。
  document.addEventListener('toggle', ev => {
    const d = ev.target;
    if (!d || !d.matches || !d.matches('details.ai-think[data-think]')) return;
    const th = THINKS.get(d.dataset.think);
    if (th) th.open = d.open;
  }, true);

  function aiPage(app, focusID) {
    // 会话内保存最近 40 条；思考内容只保留末尾一段，避免超出浏览器存储上限（完整工具调用见 MCP 页调用日志）。
    const store = () => {
      const slim = chat.slice(-40).map(m => m.think ? { ...m, think: { ...m.think, reasoning: (m.think.reasoning || '').slice(-4000), reviewReasoning: (m.think.reviewReasoning || '').slice(-2000), steps: (m.think.steps || []).slice(-60) } } : m);
      try { sessionStorage.setItem('np_ai', JSON.stringify(slim)); }
      catch (e) {
        try { sessionStorage.setItem('np_ai', JSON.stringify(slim.slice(-10).map(m => { const c = { ...m }; delete c.think; return c; }))); } catch (_) { /* 存储已满时放弃保存，不影响当前页面 */ }
      }
    };
    let chat = [];
    try { chat = JSON.parse(sessionStorage.getItem('np_ai') || '[]'); } catch (e) { chat = []; }
    chat.forEach(m => { if (m.think) thinkAdopt(m.think); });
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
        <div class="panel"><div class="ph">全局 · 模型设置<span class="r" id="aiState"></span></div><div class="pb"><form id="aiSet"></form></div></div>
        <div class="panel"><div class="ph">说明</div><div class="pb"><ul class="help" style="margin:0;padding-left:18px">
          <li>模型与 API Key 为全局设置（所有机房共用）；诊断与探测仅针对当前激活机房。</li>
          <li>AI 会先看当前概况，再按需查询设备列表、历史曲线、告警记录、扫描与端口定位结果，最后给出结论和排查步骤。</li>
          <li>复杂问题会自动附带<b>可视化图表</b>（故障路径图、多设备对比柱状图、延迟丢包趋势曲线、异常时段对齐时间线），点击图中的设备可直接查看详情。</li>
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
      const tools = m.think ? '' : (m.tools || []).map(t => `<span class="ai-tool${t.phase === 'review' ? ' review' : ''}" title="${esc(t.args || '')}">${t.phase === 'review' ? '核实' : '查询'} · ${esc(t.title || t.name)}${t.args && t.args !== '{}' ? ' <span class="muted">' + esc(t.args.slice(0, 60)) + '</span>' : ''}</span>`).join('');
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
      return `<div class="ai-msg bot" data-i="${i}">${m.think ? thinkHTML(m.think) : ''}${tools ? `<div class="ai-tools">${tools}</div>` : ''}<div class="ai-md">${mdLite(m.content)}</div>${check}${draft}${status}${err}${meta}</div>`;
    };
    const renderLog = () => {
      log.innerHTML = chat.length ? chat.map(msgHTML).join('') :
        `<div class="ai-empty"><b>有什么网络问题？</b><br>点下面的快捷按钮，或直接描述现象。AI 会结合 网脉 NetPulse 的实时数据和历史曲线给出判断。</div>`;
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
      const bot = { role: 'assistant', content: '', tools: [], status: '正在连接…', think: thinkNew() };
      chat.push(bot);
      const bi = chat.length - 1;
      renderLog();
      const ctl = new AbortController();
      setBusy(ctl);
      let pendingBreak = false;
      try {
        const r = await fetch('api/ai/diagnose', {
          method: 'POST', headers: { 'Content-Type': 'application/json', ...(S.activeProfile ? { 'X-NetPing-Profile': S.activeProfile } : {}) }, signal: ctl.signal,
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
            thinkEvent(bot.think, ev);
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
        thinkFinish(bot.think, bot.error ? 'error' : 'done');
      } catch (e) {
        thinkFinish(bot.think, e.name === 'AbortError' ? 'stopped' : 'error', e.name === 'AbortError' ? '' : errMsg(e));
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
  // 运维报告页：AI 按日报 / 周报 / 月报撰写运维报告，保存在服务器，可下载 Markdown、导出 HTML 或打印为 PDF
  const REPORT_RANGES = [['24h', '日报', '近 24 小时'], ['7d', '周报', '近 7 天'], ['30d', '月报', '近 30 天']];
  function reportKPIs(k) {
    if (!k) return '';
    const cell = (label, v, cls) => `<div class="rpt-kpi"><div class="k">${label}</div><div class="v ${cls || ''}">${v}</div></div>`;
    const av = k.availability_pct;
    const avCls = av == null ? 'muted' : av >= 99.9 ? 'v-ok' : av >= 99 ? 'v-warn' : 'v-down';
    const lossCls = k.loss_pct == null ? 'muted' : k.loss_pct >= 1 ? 'v-down' : k.loss_pct > 0 ? 'v-warn' : 'v-ok';
    const noDataBanner = k.has_data === false ? '<div class="banner" style="margin-bottom:12px">本周期无观测样本：可用率、丢包率、平均延迟与累计中断均显示「无数据」，不作健康判定。</div>' : '';
    return `${noDataBanner}<div class="rpt-kpis">
      ${cell('可用率', av == null ? '无数据' : av.toFixed(2) + '%', avCls)}
      ${cell('丢包率', k.loss_pct == null ? '无数据' : k.loss_pct.toFixed(2) + '%', lossCls)}
      ${cell('平均延迟', k.avg_ms == null ? '无数据' : fmtMs(k.avg_ms), k.avg_ms == null ? 'muted' : '')}
      ${cell('累计中断', k.down_minutes == null ? '无数据' : k.down_minutes + ' 分钟', k.down_minutes == null ? 'muted' : k.down_minutes ? 'v-warn' : '')}
      ${cell('告警事件', (k.alerts != null ? k.alerts : 0) + ' 条')}
      ${cell('当前异常', (k.abnormal_now != null ? k.abnormal_now : 0) + ' 台', k.abnormal_now ? 'v-down' : 'v-ok')}
      ${cell('监控设备', (k.with_data != null ? k.with_data : 0) + ' / ' + (k.monitored != null ? k.monitored : 0))}${k.observed_minutes != null ? cell('观测设备分钟', k.observed_minutes) : ''}${k.fault_episodes != null ? cell('故障次数', k.fault_episodes) : ''}
    </div>`;
  }
  // 报告正文 HTML（页面查看与导出共用）
  function reportDocHTML(rp) {
    const modelLabel = rp.model === 'deterministic' ? '程序确定性统计（无需 AI）' : rp.model;
    return `<div class="rpt-meta">${esc(rp.kind)} · ${esc(rp.range_label)}（${esc(rp.from)} ~ ${esc(rp.to)}） · 生成于 ${esc(rp.time)} · ${esc(modelLabel)}</div>
      ${reportKPIs(rp.summary)}<div class="ai-md rpt-md">${mdLite(rp.markdown, { doc: true })}</div>`;
  }
  let reportCSS = null;
  // 导出为自包含 HTML：内联样式与已渲染的 SVG 图表，可离线打开、转发或打印为 PDF
  async function reportStandalone(rp) {
    if (reportCSS == null) {
      try { reportCSS = await (await fetch('style.css')).text(); } catch (e) { reportCSS = ''; }
    }
    const foot = rp.model === 'deterministic'
      ? '本报告由 网脉 NetPulse 程序确定性统计生成（无需 AI），仅统计激活期间的实际观测数据。'
      : '本报告由 网脉 NetPulse AI 生成，正文结论由大模型基于监控数据撰写，附录数据由程序直接统计。';
    return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(rp.title)}</title>
<style>${reportCSS}
html, body { height: auto; background: #fff; }
body { padding: 32px 16px; }
.rpt-doc { max-width: 900px; margin: 0 auto; background: #fff; }
.rpt-foot { max-width: 900px; margin: 24px auto 0; color: #8a857a; font-size: 11px; border-top: 1px solid #e0dacd; padding-top: 8px; }
@media print { body { padding: 0; } .ai-chart, table, .rpt-kpis { break-inside: avoid; } h1, h2, h3 { break-after: avoid; } }
</style></head>
<body><article class="rpt-doc">${reportDocHTML(rp)}</article>
<div class="rpt-foot">${foot}</div>
</body></html>`;
  }
  function download(name, text, type) {
    const url = URL.createObjectURL(new Blob([text], { type }));
    const a = document.createElement('a');
    a.href = url; a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 2000);
  }

  function reportPage(app, openID) {
    let list = [], cur = null, busy = null, live = null, settings = null;
    let rg = sessionStorage.getItem('np_report_range') || '24h';
    app.innerHTML = `<div class="page pad"><div class="ai rpt">
      <div class="panel rpt-main"><div class="ph"><span id="rTitle">运维报告</span>
        <span class="r btns" style="margin:0" id="rActs"></span></div>
        <div class="rpt-view" id="rView"></div>
      </div>
      <div class="ai-side">
        <div class="panel"><div class="ph">生成报告<span class="r" id="rState"></span></div><div class="pb">
          <div class="help" style="margin-top:0">统计周期</div>
          <div class="chips" id="rRange" style="margin:0 0 10px">${REPORT_RANGES.map(([v, k, l]) => `<button class="chip" data-r="${v}">${k} · ${l}</button>`).join('')}</div>
          <label class="f"><span>额外要求（可选）</span><textarea id="rNote" rows="3" maxlength="500" placeholder="如：重点关注 3 楼摄像头；写给领导看，简洁一些；列出需要采购更换的设备"></textarea></label>
          <div class="btns"><button class="btn accent" id="rGen">✦ AI 运维报告</button><button class="btn primary" id="rStats">生成统计报告（无需 AI）</button></div>
          <div class="help">AI 先读取程序统计的全网可用率、丢包、延迟、中断、告警和当前异常，必要时再查询设备历史，按「总体结论 → 关键指标 → 稳定性分析 → 告警与故障 → 风险隐患 → 处理建议」撰写报告，并附趋势和排行图表。报告末尾附程序直接统计的原始数据，便于核对。</div>
        </div></div>
        <div class="panel"><div class="ph">历史报告<span class="r muted" id="rCount"></span></div><div class="rpt-list" id="rList"></div></div>
      </div>
    </div></div>`;

    const drawRange = () => $$('#rRange .chip').forEach(b => b.classList.toggle('on', b.dataset.r === rg));
    $$('#rRange .chip').forEach(b => b.onclick = () => { rg = b.dataset.r; sessionStorage.setItem('np_report_range', rg); drawRange(); });
    drawRange();

    function drawList() {
      $('#rCount').textContent = list.length ? list.length + ' 份' : '';
      $('#rList').innerHTML = list.length ? list.map(r => {
        const k = r.summary || {};
        const tag = r.model === 'deterministic' ? '<span class="badge ok">程序统计</span> ' : '';
        return `<div class="rpt-item${cur && cur.id === r.id ? ' on' : ''}" data-id="${esc(r.id)}">
          <div class="t"><span class="badge unknown">${esc(r.kind)}</span> ${tag}${esc(r.title)}</div>
          <div class="muted">${esc(r.time)} · 可用率 ${k.availability_pct != null ? k.availability_pct.toFixed(2) + '%' : '无数据'} · 告警 ${k.alerts != null ? k.alerts : '-'}</div></div>`;
      }).join('') : '<div class="help" style="padding:12px;margin:0">还没有报告。选择周期后可点「生成统计报告（无需 AI）」或「✦ AI 运维报告」。</div>';
      $$('#rList .rpt-item').forEach(el => el.onclick = () => { if (!busy) show(el.dataset.id); });
    }
    function drawActs() {
      const acts = $('#rActs');
      if (busy) { acts.innerHTML = '<button class="btn small danger" id="rStop">停止生成</button>'; $('#rStop').onclick = () => busy.abort(); return; }
      if (!cur) { acts.innerHTML = ''; return; }
      acts.innerHTML = `<button class="btn small" id="rMd" title="下载 Markdown 原文（图表为 chart 代码块）">下载 Markdown</button>
        <button class="btn small" id="rHtml" title="自包含 HTML，图表已渲染，可离线打开或发给他人">导出 HTML</button>
        <button class="btn small" id="rPdf" title="打开打印对话框，目标选择「另存为 PDF」">打印 / PDF</button>
        <button class="btn small" id="rCopy">复制原文</button>
        <button class="btn small danger" id="rDel">删除</button>`;
      const base = '网脉 NetPulse-运维报告-' + cur.id;
      $('#rMd').onclick = async () => {
        try {
          const md = await api('GET', 'api/ai/reports/' + encodeURIComponent(cur.id) + '.md');
          download(base + '.md', md, 'text/markdown;charset=utf-8');
        } catch (e) { toast(errMsg(e), true); }
      };
      $('#rHtml').onclick = async () => download(base + '.html', await reportStandalone(cur), 'text/html;charset=utf-8');
      $('#rPdf').onclick = async () => {
        const html = await reportStandalone(cur);
        const fr = document.createElement('iframe');
        fr.style.cssText = 'position:fixed;right:0;bottom:0;width:0;height:0;border:0';
        document.body.appendChild(fr);
        fr.onload = () => {
          const w = fr.contentWindow;
          w.document.title = base;
          setTimeout(() => { w.focus(); w.print(); setTimeout(() => fr.remove(), 1000); }, 200);
        };
        fr.srcdoc = html;
      };
      $('#rCopy').onclick = () => copyText(cur.markdown);
      $('#rDel').onclick = async () => {
        if (!confirm(`删除报告「${cur.title}」？`)) return;
        try {
          await api('DELETE', 'api/ai/reports/' + encodeURIComponent(cur.id));
          list = list.filter(r => r.id !== cur.id); cur = null; sessionStorage.removeItem('np_report_cur');
          drawList(); drawView(); toast('已删除');
        } catch (e) { toast(errMsg(e), true); }
      };
    }
    function drawView() {
      const v = $('#rView');
      if (live) {
        const tools = '';
        $('#rTitle').textContent = '正在生成' + (REPORT_RANGES.find(x => x[0] === live.range) || [, ''])[1] + '…';
        const stick = v.scrollHeight - v.scrollTop - v.clientHeight < 60;
        v.innerHTML = `<div class="rpt-doc">${live.stats ? reportKPIs(live.stats) : ''}
          ${thinkHTML(live.think)}${tools}
          <div class="ai-md rpt-md">${mdLite(live.content, { doc: true })}</div>
          ${live.status ? `<div class="ai-status"><span class="ai-spin"></span>${esc(live.status)}</div>` : ''}
          ${live.error ? `<div class="ai-err">${esc(live.error)}</div>` : ''}</div>`;
        if (stick) v.scrollTop = v.scrollHeight;
      } else if (cur) {
        $('#rTitle').textContent = cur.title;
        v.innerHTML = `<article class="rpt-doc">${reportDocHTML(cur)}</article>`;
        v.scrollTop = 0;
      } else {
        $('#rTitle').textContent = '运维报告';
        v.innerHTML = `<div class="ai-empty"><b>网络健康与运维报告</b><br>选择日报、周报或月报：<br>1. <b>生成统计报告（无需 AI）</b>：程序按当前激活机房实际观测样本直接汇总，无样本时明确标注“无数据”。<br>2. <b>✦ AI 运维报告</b>：结合监控统计、告警和历史曲线撰写分析报告与图表。<br>生成后可下载 Markdown、导出 HTML，或打印为 PDF。</div>`;
      }
      drawActs();
    }
    async function show(id) {
      live = null;
      try {
        cur = await api('GET', 'api/ai/reports/' + encodeURIComponent(id));
        sessionStorage.setItem('np_report_cur', id);
      } catch (e) { cur = null; sessionStorage.removeItem('np_report_cur'); toast(errMsg(e), true); }
      drawList(); drawView();
    }
    async function generate() {
      if (busy) return;
      if (!settings || !settings.ready) { toast('请先在「AI 诊断」页设置 API Key 并启用，或点「生成统计报告（无需 AI）」', true); return; }
      const ctl = new AbortController();
      busy = ctl;
      live = { range: rg, content: '', tools: [], status: '正在连接…', stats: null, error: '', think: thinkNew() };
      $('#rGen').disabled = true;
      drawView();
      let got = null;
      try {
        await streamNDJSON('api/ai/report', { range: rg, note: $('#rNote').value.trim() }, ctl.signal, ev => {
          thinkEvent(live.think, ev);
          if (ev.t === 'status') live.status = ev.text;
          else if (ev.t === 'stats') live.stats = ev.stats;
          else if (ev.t === 'delta') { live.content += ev.text; live.status = ''; }
          else if (ev.t === 'tool') { live.tools.push(ev); live.status = '正在查询 ' + (ev.title || ev.name) + '…'; }
          else if (ev.t === 'break') { live.content = ''; } // 工具调用前的过渡文字不属于报告
          else if (ev.t === 'warn') toast(ev.text, true);
          else if (ev.t === 'error') { live.error = ev.error; live.status = ''; }
          else if (ev.t === 'report') got = ev.report;
          drawView();
        });
        if (!got && !live.error) live.error = '没有收到完整的报告，请重试。';
        thinkFinish(live.think, got ? 'done' : 'error');
      } catch (e) {
        live.error = e.name === 'AbortError' ? '已停止生成' : errMsg(e);
        thinkFinish(live.think, e.name === 'AbortError' ? 'stopped' : 'error', e.name === 'AbortError' ? '' : live.error);
      } finally {
        busy = null;
        $('#rGen').disabled = false;
      }
      if (got) {
        live = null; cur = got;
        sessionStorage.setItem('np_report_cur', got.id);
        list = [got].concat(list.filter(r => r.id !== got.id));
        drawList(); drawView();
        toast('报告已生成并保存');
      } else { live.status = ''; drawView(); }
    }
    $('#rGen').onclick = generate;
    $('#rStats').onclick = async () => {
      if (busy) return;
      const ctl = new AbortController(); busy = ctl;
      $('#rStats').disabled = true; $('#rGen').disabled = true;
      try {
        const got = await api('POST', 'api/health-report', { range: rg }, { signal: ctl.signal });
        live = null; cur = got; list = [got].concat(list.filter(r => r.id !== got.id));
        sessionStorage.setItem('np_report_cur', got.id); drawList();
        toast('统计报告已保存；仅统计激活期间的实际观测');
      } catch (e) { if (e.name !== 'AbortError') toast(errMsg(e), true); }
      finally { busy = null; if ($('#rStats')) { $('#rStats').disabled = false; $('#rGen').disabled = false; drawView(); } }
    };

    drawView(); drawList();
    api('GET', 'api/ai/settings').then(a => {
      settings = a;
      $('#rState').innerHTML = a.ready ? `<span class="muted">${esc(a.model)}</span>` : '<a href="#/ai" class="badge slow">先设置 AI</a>';
    }).catch(() => {});
    api('GET', 'api/ai/reports').then(l => {
      list = l || [];
      drawList();
      const want = openID || sessionStorage.getItem('np_report_cur');
      if (want && list.some(r => r.id === want)) show(want);
      if (openID) history.replaceState(null, '', '#/report');
    }).catch(e => toast(errMsg(e), true));
    return { destroy() { if (busy) busy.abort(); } };
  }

  // ---------------------------------------------------------------------------
  // 拓扑页
  // 设备类别（与服务器 deviceCategories 一致），AI 一键优化把类别写成同名标签
  const DEVICE_CATS = ['服务器', '虚拟机', '台式机', '笔记本', '手机平板', '打印机', '摄像头', '录像机', '无线AP',
    '路由器防火墙', '光猫', '网络存储', 'IP电话', '门禁考勤', '智能设备', '工控设备', '其他'];
  function topoPage(app) {
    app.innerHTML = `
    <div class="topo">
      <div class="canvas-wrap">
        <svg id="topoSvg"></svg>
        <div class="canvas-tools left">
          <button class="btn primary small" id="addSw" title="交换机、路由器、防火墙、光猫、无线 AP 等带端口、可连线的设备">+ 网络设备</button>
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
        <div class="canvas-hint">拖动空白处平移 · 滚轮缩放 · 拖动卡片/设备标题调整位置 · 双击查看曲线 · 点网口再点卡片＝接线 · <kbd>/</kbd> 检索 <kbd>Esc</kbd> 清除 <kbd>F</kbd> 适配</div>
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
      onSSH: id => openTerminal(id, 'ssh', '', true),
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
      <svg width="44" height="16"><line x1="2" y1="8" x2="42" y2="8" stroke="#1a1a1a" stroke-width="5" stroke-dasharray="8 3"/></svg><span>设备间链路（点击可编辑）</span>
      ${TopoTypes.list.map(t => `<span class="ty">${TopoTypes.icon(t, 16)}</span><span>${esc(TopoTypes.info[t].name)}</span>`).join('')}
      <svg width="44" height="16"><rect x="1" y="1" width="42" height="14" fill="#ff5a1f"/></svg><span>默认网关</span>
      <svg width="44" height="16"><rect x="6" y="2" width="32" height="12" fill="#ff5a1f"/><text x="22" y="11" font-size="9" fill="#fff" text-anchor="middle">告警</text></svg><span>已开启告警通知（设备默认不告警）</span>
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
      const st = { status: '正在连接…', tools: [], text: '', error: '', think: thinkNew() };
      const draw = () => {
        if (!opt) return;
        const pre = st.text.split('```')[0].trim();
        body.innerHTML = `<div class="help" style="margin-top:0">AI 会读取全部设备的厂商、主机名、接线、告警设置和近 7 天的丢包/延迟/中断统计，
            给出：<b>设备分类</b>（写成标签）、<b>配置建议</b>、<b>需要检查的设备</b>。所有修改都要你勾选确认后才会应用。设备较多时需要 1~3 分钟。</div>
          ${thinkHTML(st.think)}
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
        const r = await fetch('api/ai/optimize', { method: 'POST', signal: ctl.signal, headers: { 'Content-Type': 'application/json', ...(S.activeProfile ? { 'X-NetPing-Profile': S.activeProfile } : {}) }, body: '{}' });
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
            thinkEvent(st.think, ev);
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
        thinkFinish(st.think, result ? 'done' : 'error', st.error);
      } catch (e) {
        st.error = e.name === 'AbortError' ? '已停止' : errMsg(e);
        thinkFinish(st.think, e.name === 'AbortError' ? 'stopped' : 'error', e.name === 'AbortError' ? '' : st.error);
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
      box.innerHTML = head('详情') + `<div class="help">点击终端卡片、网络设备标题或设备间连线查看并编辑。<br>
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
    return `<input type="hidden" name="tcp_ports" value="${esc(JSON.stringify(m.tcp_ports || []))}"><div class="checks" style="margin-bottom:6px"><label><input type="checkbox" name="mon_enabled" ${m.enabled === false ? '' : 'checked'}> 启用监控（Ping / TCPing）</label>
        <label><input type="checkbox" name="mon_alert" ${m.alert ? 'checked' : ''}> 推送告警通知</label></div>
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
    m.tcp_ports = JSON.parse(f.elements.tcp_ports.value);
    return m;
  }
  let pingDefaults = null;
  api('GET', 'api/config').then(c => { pingDefaults = c.ping; }).catch(() => {});

  function nodeForm(box, head, sel) {
    const isNew = sel.kind === 'newNode';
    const n = isNew ? { attach: { switch: sel.sw || '', port: '' }, flags: [], tags: [] } : S.topo.nodes.find(x => x.id === sel.id);
    if (!n) { box.innerHTML = head('详情'); return; }
    const swOpts = ['<option value="">（未定位）</option>'].concat(S.topo.switches.map(s =>
      `<option value="${esc(s.id)}" ${n.attach.switch === s.id ? 'selected' : ''}>${esc(swLabel(s))}</option>`)).join('');
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
          <label class="f"><span>接入设备</span><select name="sw">${swOpts}</select></label>
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
        portSel.innerHTML = '<option value="">（先选择接入设备）</option>';
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
    const s = isNew ? { type: sel.type || '', role: 'access', node_side: 'right', node_columns: 2, ports: [] } : S.topo.switches.find(x => x.id === sel.id);
    if (!s) { box.innerHTML = head('详情'); return; }
    const opt = (list, cur, labels) => list.map(v => `<option value="${v}" ${cur === v ? 'selected' : ''}>${labels ? labels[v] : v}</option>`).join('');
    const attached = isNew ? 0 : S.topo.nodes.filter(n => n.attach.switch === s.id).length;
    const typeName = TopoTypes.name(s);
    const typeNames = {};
    TopoTypes.list.forEach(t => { typeNames[t] = TopoTypes.info[t].name; });
    box.innerHTML = head(isNew ? '新增网络设备' : TopoTypes.icon(TopoTypes.of(s)) + ' ' + esc(typeName) + ' · ' + esc(s.name || s.id)) +
      (!isNew && s.ip ? `<div id="liveBox" data-id="${esc(s.id)}">${liveHTML(s.id)}</div>` : '') +
      `<form id="sf">
        <div class="row2">
          <label class="f"><span>设备类型</span><select name="type">${opt(TopoTypes.list, TopoTypes.of(s), typeNames)}</select></label>
          <label class="f"><span>名称</span><input type="text" name="name" value="${esc(s.name || '')}" required></label>
          <label class="f"><span>ID</span><input type="text" name="id" value="${esc(s.id || '')}" placeholder="留空自动生成"></label>
          <label class="f" title="决定拓扑图中的配色与故障路径分析：核心/汇聚画成黑色机身；故障路径以核心为终点"><span>网络层次</span><select name="role">${opt(['core', 'aggregation', 'access', 'other'], s.role, { core: '核心', aggregation: '汇聚', access: '接入', other: '其他' })}</select></label>
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
        ${isNew ? '' : `<div class="help">下挂节点 ${attached} 个。拖动拓扑图中设备的标题区域可移动位置。</div>`}
      </form>`;
    const f = $('#sf');
    // 新建时切换类型：端口列表为空或仍是上一个类型的默认端口时，换成新类型的常用端口
    let autoPorts = isNew ? TopoTypes.info[TopoTypes.of(s)].ports : null;
    if (isNew && autoPorts && !f.elements.ports.value.trim()) f.elements.ports.value = autoPorts;
    f.elements.type.onchange = () => {
      if (!isNew) return;
      const cur = f.elements.ports.value.trim();
      const next = TopoTypes.info[f.elements.type.value].ports;
      if (!cur || cur === autoPorts) { f.elements.ports.value = next; autoPorts = next; }
    };
    f.onsubmit = async ev => {
      ev.preventDefault();
      const e = f.elements;
      const body = {
        id: e.id.value.trim(), name: e.name.value.trim(), type: e.type.value === 'switch' ? '' : e.type.value, role: e.role.value, layer: e.layer.value.trim(), vendor: e.vendor.value.trim(),
        model: e.model.value.trim(), ip: e.ip.value.trim(), mac: e.mac.value.trim(), location: e.location.value.trim(),
        ports: expandPorts(e.ports.value), node_side: e.node_side.value, node_columns: parseInt(e.node_columns.value, 10) || 2,
        port_locations: parsePortLocations(e.port_locations.value),
        note: e.note.value.trim(), monitor: readMon(f), position: isNew ? null : ((S.topo.switches.find(x => x.id === s.id) || s).position || null),
      };
      if (isNew && view) { const c = view.viewCenter(); body.position = { x: Math.round(c.x - 75), y: Math.round(c.y - 100) }; }
      try {
        const r = await api(isNew ? 'POST' : 'PUT', isNew ? 'api/switches' : 'api/switches/' + encodeURIComponent(s.id), body);
        toast(isNew ? '已添加' + TopoTypes.info[e.type.value].name : '已保存');
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
        if (!confirm(`删除${typeName} ${s.name || s.id}？\n其下 ${attached} 个节点会变为“未定位”，相关链路会被删除。`)) return;
        try { await api('DELETE', 'api/switches/' + encodeURIComponent(s.id)); S.sel = null; toast('已删除'); markClean(); await loadTopology(); }
        catch (err) { toast(errMsg(err), true); }
      };
    }
  }

  function linkForm(box, head, sel) {
    const isNew = sel.kind === 'newLink';
    const l = isNew ? { from: { switch: '', port: '' }, to: { switch: '', port: '' }, kind: 'uplink', note: '' } : S.topo.links.find(x => x.id === sel.id);
    if (!l) { box.innerHTML = head('详情'); return; }
    const swOpts = cur => S.topo.switches.map(s => `<option value="${esc(s.id)}" ${cur === s.id ? 'selected' : ''}>${esc(swLabel(s))}</option>`).join('');
    box.innerHTML = head(isNew ? '新增设备间链路' : '链路 · ' + esc(l.from.port + ' ⇄ ' + l.to.port)) + `<form id="lf">
      <div class="help">“上游”一端填写靠近出口的设备（如 光猫 → 防火墙 → 核心交换机），告警时用于判断下游设备故障。</div>
      <div class="row2">
        <label class="f"><span>上游设备</span><select name="fs">${swOpts(l.from.switch)}</select></label>
        <label class="f"><span>上游端口</span><input type="text" name="fp" value="${esc(l.from.port)}" required></label>
        <label class="f"><span>下游设备</span><select name="ts">${swOpts(l.to.switch)}</select></label>
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
      <div class="table-wrap"><table class="grid"><thead><tr>
        <th data-k="state">状态</th><th data-k="ip">IP</th><th data-k="vendor">名称 / 厂商</th><th data-k="loc">接线位置</th>
        <th data-k="last" class="num">当前</th><th data-k="avg1" class="num">1m 平均</th><th data-k="jit" class="num">抖动</th>
        <th data-k="l1" class="num">1m 丢包</th><th data-k="l5" class="num">5m 丢包</th><th data-k="l60" class="num">1h 丢包</th>
        <th>最近 60 次</th><th data-k="since" class="num">状态持续</th><th data-k="alert" title="是否推送告警通知">告警</th><th title="上锁后不参与一键清理">上锁保护</th>
      </tr></thead><tbody id="lbody"></tbody></table></div>
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
      S.topo.switches.forEach(s => { if (s.ip) out.push({ id: s.id, ip: s.ip, name: s.name, vendor: s.vendor, loc: TopoTypes.name(s), mac: s.mac, sw: true, alert: !!(s.monitor && s.monitor.alert) }); });
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
      if (!confirm(`对筛选出的 ${ids.length} 台设备${enabled ? '开启' : '关闭'}告警通知？`)) return;
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
  function parseTCPPorts(text, max) {
    const ports = new Set();
    for (const token of text.trim().split(/[,，\s]+/).filter(Boolean)) {
      const m = token.match(/^(\d+)(?:-(\d+))?$/);
      if (!m) throw new Error('端口格式应为 22,443 或 1-1024');
      const first = Number(m[1]), last = Number(m[2] || m[1]);
      if (first < 1 || last > 65535 || first > last) throw new Error('端口范围必须在 1–65535 之间');
      if (last - first + 1 > max) throw new Error(`最多 ${max} 个端口`);
      for (let p = first; p <= last; p++) { ports.add(p); if (ports.size > max) throw new Error(`最多 ${max} 个端口`); }
    }
    return [...ports];
  }

  function nodePage(app, id) {
    let range = sessionStorage.getItem('np_range') || '1h';
    let timer = 0, alive = true, selectedPort = 0, scanController = null, savingPorts = false, loadVersion = 0;
    app.innerHTML = `<div class="page pad">
      <div class="detail-head">
        <a href="javascript:history.back()" class="btn small">← 返回</a>
        <h2 id="dTitle">${esc(id)}</h2><div id="dSub" class="muted"></div>
        <div style="margin-left:auto" class="btns" id="ranges"></div>
        <a class="btn small" href="#/ai/${encodeURIComponent(id)}" title="让 AI 结合实时数据和历史曲线诊断这台设备">AI 诊断</a>
      </div>
      <div class="panel"><div class="ph">端口扫描与 TCPing 监控</div><div class="pb">
        <p class="help">从 网脉 NetPulse 服务器探测。持续监控独立于 Ping 结果，沿用设备的间隔、超时、告警和监控时段；拒绝连接或超时均记为失败。</p>
        <div class="row2 tcp-config">
          <form id="portScanForm"><label class="f"><span>扫描端口（最多 1024 个，支持 1-1024）</span><input name="ports" value="22,23,80,443,445,554,3389,8000,8080,9100" required></label>
            <div class="btns"><button class="btn" type="submit">扫描端口</button><button class="btn" type="button" id="cancelPortScan" disabled>取消</button></div></form>
          <form id="tcpMonitorForm"><label class="f"><span>持续监控端口（最多 16 个，逗号分隔；留空清除）</span><input name="ports" placeholder="22,443,3389"></label>
            <button class="btn primary" type="submit">保存 TCPing 监控</button></form>
        </div>
        <div id="portScanState" role="status" class="help"></div><div id="portScanResults" class="table-wrap"></div>
        <div id="tcpMonitorRows" class="table-wrap"></div>
      </div></div>
      <div class="btns"><label>查看曲线 <select id="monitorSeries"><option value="0">ICMP Ping</option></select></label></div>
      <div class="tiles" id="dTiles"></div>
      <div class="panel"><div class="ph"><span id="dLatencyLabel">延迟</span><span class="muted" style="font-weight:normal">橙线为平均值，浅色区间为最小~最大</span><span class="r muted" id="dBucket"></span></div><div class="pb"><div id="cLat"></div></div></div>
      <div class="panel"><div class="ph"><span id="dLossLabel">丢包率</span><span class="muted" id="dLossHint" style="font-weight:normal">红色为全部丢失，橙色为部分丢失</span></div><div class="pb"><div id="cLoss"></div></div></div>
    </div>`;
    function configuredPorts() {
      const target = findTarget(id);
      return target && target.obj.monitor && target.obj.monitor.tcp_ports || [];
    }
    function currentStatus() {
      return selectedPort ? (S.tcpStatus || []).find(st => st.parent_id === id && st.port === selectedPort) : S.status[id];
    }
    function drawPorts(refreshInput = false) {
      const ports = configuredPorts();
      if (refreshInput && document.activeElement !== $('#tcpMonitorForm').elements.ports) $('#tcpMonitorForm').elements.ports.value = ports.join(',');
      const select = $('#monitorSeries');
      const options = '<option value="0">ICMP Ping</option>' + ports.map(p => `<option value="${p}">TCPing :${p}</option>`).join('');
      if (select.innerHTML !== options) { select.innerHTML = options; select.value = String(selectedPort); }
      if (selectedPort && !ports.includes(selectedPort)) { selectedPort = 0; select.value = '0'; load(); }
      $('#tcpMonitorRows').innerHTML = ports.length ? '<table><thead><tr><th>端口</th><th>状态</th><th>连接耗时</th><th>1m 失败率</th><th>操作</th></tr></thead><tbody>' + ports.map(port => {
        const st = (S.tcpStatus || []).find(st => st.parent_id === id && st.port === port);
        const target = findTarget(id), enabled = !(target && target.obj.monitor && target.obj.monitor.enabled === false);
        return `<tr><td class="mono">${port}</td><td>${stateBadge(st ? st.state : enabled ? 'unknown' : 'disabled')}</td><td>${st ? fmtMs(st.last_rtt) : '—'}</td><td>${st ? pct(st.loss_1m) : '—'}</td><td><button class="btn small" data-tcp-history="${port}">曲线</button> <button class="btn small" data-tcp-remove="${port}" ${savingPorts ? 'disabled' : ''}>移除</button></td></tr>`;
      }).join('') + '</tbody></table>' : '<p class="help">尚未配置 TCPing 监控。</p>';
    }
    async function savePorts(ports) {
      if (savingPorts) return;
      if (ports.length > 16) { toast('最多监控 16 个 TCP 端口', true); return; }
      savingPorts = true;
      $('#tcpMonitorForm').querySelector('button').disabled = true;
      try {
        await api('PUT', 'api/ports/monitoring', {id, ports});
        await loadTopology();
        if (alive) { drawPorts(true); toast('TCPing 监控已保存'); }
      } catch (e) { if (alive) toast(errMsg(e), true); }
      finally { savingPorts = false; if (alive) { $('#tcpMonitorForm').querySelector('button').disabled = false; drawPorts(); } }
    }
    $('#tcpMonitorForm').onsubmit = ev => {
      ev.preventDefault();
      try { savePorts(parseTCPPorts(ev.currentTarget.elements.ports.value, 16)); } catch(e) { toast(e.message, true); }
    };
    $('#monitorSeries').onchange = ev => { selectedPort = Number(ev.target.value); drawTiles(); load(); };
    $('#tcpMonitorRows').onclick = ev => {
      const b = ev.target.closest('button'); if (!b) return;
      if (b.dataset.tcpRemove) savePorts(configuredPorts().filter(p => p !== Number(b.dataset.tcpRemove)));
      if (b.dataset.tcpHistory) { selectedPort = Number(b.dataset.tcpHistory); $('#monitorSeries').value = String(selectedPort); drawTiles(); load(); }
    };
    $('#portScanResults').onclick = ev => {
      const b = ev.target.closest('[data-monitor-port]'); if (!b) return;
      savePorts([...new Set([...configuredPorts(), Number(b.dataset.monitorPort)])]);
    };
    $('#cancelPortScan').onclick = () => { if (scanController) scanController.abort(); };
    $('#portScanForm').onsubmit = async ev => {
      ev.preventDefault(); if (scanController) return;
      let ports;
      try { ports = parseTCPPorts(ev.currentTarget.elements.ports.value, 1024); if (!ports.length) throw new Error("请至少填写一个扫描端口"); } catch(e) { toast(e.message, true); return; }
      scanController = new AbortController();
      $('#portScanForm').querySelector('button').disabled = true; $('#cancelPortScan').disabled = false;
      $('#portScanState').textContent = `正在扫描 ${ports.length} 个端口…`;
      $('#portScanResults').innerHTML = '';
      try {
        const result = await api('POST', 'api/ports/scan', {id, ports}, {signal: scanController.signal});
        if (!alive) return;
        $('#portScanState').textContent = result.summary.verdict;
        const labels = {open:'开放', refused:'拒绝连接', timeout:'超时', error:'错误'};
        $('#portScanResults').innerHTML = '<table><thead><tr><th>端口 / 服务</th><th>探测结果</th><th>耗时</th><th></th></tr></thead><tbody>' + result.ports.map(p => `<tr><td>${p.port} ${esc(p.service || '')}</td><td title="${esc(p.error || '')}">${labels[p.state] || esc(p.state)}</td><td>${p.ms == null ? '—' : fmtMs(p.ms)}</td><td><button class="btn small" data-monitor-port="${p.port}">加入监控</button></td></tr>`).join('') + '</tbody></table>';
      } catch(e) { if (alive) $('#portScanState').textContent = e.name === 'AbortError' ? '扫描已取消' : errMsg(e); }
      finally { scanController = null; if (alive) { $('#portScanForm').querySelector('button').disabled = false; $('#cancelPortScan').disabled = true; } }
    };
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
      $('#dSub').innerHTML = [o.name, o.hostname && o.hostname !== o.name ? '主机名 ' + o.hostname : '', o.vendor, t.kind === 'switch' ? TopoTypes.name(o) : attachLabel(o), o.mac, o.location].filter(Boolean).map(esc).join(' · ') +
        (flags.length ? ' ' + flags.map(f => `<span class="badge solid">${esc(f)}</span>`).join(' ') : '') +
        ' ' + (o.monitor && o.monitor.alert ? '<span class="badge" style="color:var(--accent)">告警已开启</span>' : '<span class="badge unknown">不告警</span>');
    }
    function drawTiles() {
      $('#dLatencyLabel').textContent = selectedPort ? `TCP :${selectedPort} 连接耗时` : '延迟';
      $('#dLossLabel').textContent = selectedPort ? 'TCP 连接失败率' : '丢包率';
      $('#dLossHint').textContent = selectedPort ? '红色为全部失败，橙色为部分失败' : '红色为全部丢失，橙色为部分丢失';
      const st = currentStatus();
      if (!st) { $('#dTiles').innerHTML = '<div><div class="k">状态</div><div class="v">未监控</div></div>'; return; }
      const tile = (k, v, c) => `<div><div class="k">${k}</div><div class="v ${c || ''}">${v}</div></div>`;
      $('#dTiles').innerHTML = tile('状态', stateBadge(st.state)) + tile(selectedPort ? 'TCP 连接耗时' : '当前延迟', fmtMs(st.last_rtt), st.last_rtt < 0 ? 'v-down' : '') +
        tile('1m 平均', st.avg_1m ? fmtMs(st.avg_1m) : '-') + tile('1m 最小/最大', st.avg_1m ? `${fmtMs(st.min_1m)}/${fmtMs(st.max_1m)}` : '-') +
        tile('抖动', st.jitter_1m ? fmtMs(st.jitter_1m) : '-') + tile('1h 平均', st.avg_1h ? fmtMs(st.avg_1h) : '-') +
        tile(selectedPort ? '1m 失败率' : '1m 丢包', pct(st.loss_1m), lossCls(st.loss_1m, st.loss_warn_pct)) + tile(selectedPort ? '5m 失败率' : '5m 丢包', pct(st.loss_5m), lossCls(st.loss_5m, st.loss_warn_pct)) +
        tile(selectedPort ? '1h 失败率' : '1h 丢包', pct(st.loss_1h), lossCls(st.loss_1h, st.loss_warn_pct)) + tile('状态持续', fmtAgo(st.state_since));
    }
    async function load() {
      clearTimeout(timer);
      const version = ++loadVersion;
      try {
        const d = await api('GET', `api/history/${encodeURIComponent(id)}?range=${range}${selectedPort ? '&port=' + selectedPort : ''}`);
        if (!alive || version !== loadVersion) return;
        const st = currentStatus();
        Charts.latency($('#cLat'), d, st ? st.latency_warn_ms : null);
        Charts.loss($('#cLoss'), d);
        const b = d.bucket_ms;
        $('#dBucket').textContent = '每点 ' + (b >= 60000 ? b / 60000 + ' 分钟' : b / 1000 + ' 秒');
      } catch (e) { toast(errMsg(e), true); }
      if (alive && version === loadVersion) timer = setTimeout(load, ['15m', '1h'].includes(range) ? 5000 : 60000);
    }
    drawRanges(); drawHead(); drawPorts(true); drawTiles(); load();
    const onResize = () => load();
    window.addEventListener('resize', onResize);
    return {
      update: () => { drawTiles(); drawPorts(); }, onTopology: () => { drawHead(); drawPorts(true); },
      destroy() { alive = false; if (scanController) scanController.abort(); clearTimeout(timer); window.removeEventListener('resize', onResize); },
    };
  }

  // ---------------------------------------------------------------------------
  // 告警历史
  function alertsPage(app) {
    let timer = 0;
    app.innerHTML = `<div class="page pad">
      <div class="toolbar"><b>告警记录</b><span class="muted">当前机房最近 300 条；通知渠道在「设置」中配置</span>
        <span style="margin-left:auto" id="aState"></span></div>
      <div class="table-wrap"><table class="grid"><thead><tr><th>时间</th><th>类型</th><th>IP</th><th>名称</th><th>位置</th><th>说明</th><th>发送</th></tr></thead><tbody id="abody"></tbody></table></div>
    </div>`;
    function rootCauseHint(id) {
      if (!id) return '';
      const hit = findTarget(id);
      const label = hit ? [hit.obj.name || hit.obj.id, hit.obj.ip].filter(Boolean).join(' · ') : id;
      return `<div class="help">疑似上游故障：<a href="#/node/${encodeURIComponent(id)}" class="root-cause-link" data-root-cause="${esc(id)}">${esc(label)}</a>${hit && hit.obj.id ? ` <span class="mono muted">(${esc(id)})</span>` : ''}。依据拓扑上游连接与当前离线状态关联，仅作辅助定位提示，不代表已确认物理故障。</div>`;
    }
    function sendStatusHTML(e) {
      if (e.suppressed === 'maintenance') {
        return e.maintenance_id === 'invalid-maintenance-file'
          ? '<span class="v-warn" title="维护窗口文件异常，已启用保护性静默">维护数据异常 · 保护性静默</span>'
          : `<span class="muted" title="${esc(e.maintenance_id || '')}">维护窗口内 · 已抑制通知</span>`;
      }
      if (e.sent) return '<span class="v-ok">已发送</span>';
      if (e.error) return `<span class="v-down" title="${esc(e.error)}">失败</span>`;
      return '<span class="muted">未发送</span>';
    }
    async function load() {
      try {
        const list = await api('GET', 'api/alerts');
        $('#abody').innerHTML = list.length ? list.map(e => {
          const [t, c] = EVENT_TXT[e.type] || [e.type, 'unknown'];
          return `<tr data-id="${esc(e.target_id || '')}"><td class="mono">${esc(Charts.fmtFull(new Date(e.time).getTime()))}</td><td><span class="badge ${c}">${t}</span></td>
            <td class="mono">${esc(e.ip || '')}</td><td>${esc(e.name || '')}</td><td>${esc(e.location || '')}</td><td class="wrap">${esc(e.detail || '')}${rootCauseHint(e.root_cause_id)}</td>
            <td>${sendStatusHTML(e)}</td></tr>`;
        }).join('') : '<tr><td colspan="7" class="empty">暂无告警记录</td></tr>';
      } catch (e) { toast(errMsg(e), true); }
      timer = setTimeout(load, 10000);
    }
    $('#abody').onclick = e => {
      if (e.target.closest('a[data-root-cause]')) { e.stopPropagation(); return; }
      const tr = e.target.closest('tr[data-id]'); if (tr && tr.dataset.id) location.hash = '#/node/' + encodeURIComponent(tr.dataset.id);
    };
    api('GET', 'api/config').then(c => {
      const on = S.topo ? S.topo.nodes.filter(n => n.monitor && n.monitor.alert).length + S.topo.switches.filter(x => x.monitor && x.monitor.alert).length : 0;
      $('#aState').innerHTML = (c.alert.enabled ? '<span class="badge ok">告警通知已启用</span>' : '<span class="badge down">告警通知未启用</span> <a href="#/settings">去设置</a>') +
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
        <div class="panel"><div class="ph">告警通知渠道 · 当前机房</div><div class="pb"><form id="af"></form></div></div>
        <div class="panel"><div class="ph">监控时段 · 当前机房<span class="r" id="schNow"></span></div><div class="pb"><form id="schf"></form></div></div>
        <div class="panel"><div class="ph">默认 Ping 参数 · 当前机房</div><div class="pb"><form id="pf"></form></div></div>
        <div class="panel"><div class="ph">机房配置 <span class="r" id="roomActiveHint">同时仅激活一套</span></div><div class="pb">
          <div id="roomList">正在加载…</div><div id="roomInspect" aria-live="polite"></div>
          <form id="roomCreate"><label class="f"><span>新配置名称</span><input name="name" required maxlength="80" placeholder="例如：总部机房"></label>
          <div class="checks"><label><input name="copy" type="checkbox">复制当前机房配置、设备和拓扑</label></div>
          <div class="btns"><button class="btn" type="submit">保存为新配置</button></div></form>
          <div class="room-import-new"><label class="f"><span>从导出文件导入为新机房</span><input id="roomImportNewFile" type="file" accept=".json,application/json"></label>
          <div class="help">导入为新机房不会激活，也不会影响现有机房；导入前会先显示将要导入的内容供确认。</div></div>
          <div class="help">保存新配置不会自动激活；仅当前激活机房执行 ping 探测、扫描、SNMP 与告警。通知渠道、扫描任务、SNMP、监控时段、Ping 参数、维护窗口和历史数据均按机房隔离；登录密码、历史保留天数、MCP 与 AI 连接为全局设置。</div>
        </div></div>
        <div class="panel"><div class="ph">维护窗口 · 当前机房</div><div class="pb">
          <div id="maintenanceErrorBanner" class="banner hidden" style="margin-bottom:10px"></div>
          <form id="maintenanceForm"><label class="f"><span>维护范围</span><select name="scope"><option value="site">整机房</option><option value="devices">指定设备</option></select></label>
          <label class="f hidden" id="maintenanceTargetsField"><span>指定设备（可多选）</span><select name="targets" multiple size="5"></select></label>
          <div class="row2">
            <label class="f"><span>开始时间（本地时间）</span><input name="start" type="datetime-local" required></label>
            <label class="f"><span>结束时间（本地时间）</span><input name="end" type="datetime-local" required></label>
          </div>
          <label class="f"><span>维护说明</span><input name="reason" maxlength="500" placeholder="例如：核心交换机割接升级" required></label>
          <div class="btns"><button class="btn primary" type="submit">保存维护窗口</button></div></form>
          <p class="help">维护期间继续探测并记录历史，抑制相关告警通知；到期或取消后自动恢复原告警规则。仅激活机房参与监控。</p><div id="maintenanceList" aria-live="polite">正在加载…</div>
        </div></div>
        <div class="panel"><div class="ph">机房信息 · 当前机房</div><div class="pb"><form id="sitef"></form></div></div>
        <div class="panel"><div class="ph">全局 · 登录密码<span class="r"><button class="btn small" id="logout">退出登录</button></span></div><div class="pb"><form id="pwf">
          <div class="row3">
            <label class="f"><span>当前密码</span><input type="password" name="old" autocomplete="current-password" required></label>
            <label class="f"><span>新密码（至少 6 位）</span><input type="password" name="p1" autocomplete="new-password" minlength="6" required></label>
            <label class="f"><span>再输一次</span><input type="password" name="p2" autocomplete="new-password" minlength="6" required></label>
          </div>
          <div class="btns"><button class="btn primary" type="submit">修改密码</button></div>
          <div class="help">打开管理页面需要输入这个密码，登录后浏览器 30 天内免登录。修改密码后，其他已登录的浏览器都需要重新登录。忘记密码时，删除 config.yaml 中 auth 下的 password_hash 和 password 两行并重启 网脉 NetPulse，再在运行 网脉 NetPulse 的电脑上打开网页重新设置。</div>
        </form></div></div>
        <div class="panel"><div class="ph">关于</div><div class="pb" id="about"></div></div>
      </div>
      <div>
        <div class="panel"><div class="ph">拓扑文件 topology.yaml · 当前机房<span class="r btns" style="margin:0">
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

    const pad2 = n => String(n).padStart(2, '0');
    function toLocalInput(d) {
      return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
    }
    function initMaintenanceDefaults() {
      const f = $('#maintenanceForm');
      if (!f) return;
      const now = new Date();
      now.setSeconds(0, 0);
      const end = new Date(now.getTime() + 2 * 3600 * 1000);
      if (!f.elements.start.value) f.elements.start.value = toLocalInput(now);
      if (!f.elements.end.value) f.elements.end.value = toLocalInput(end);
      syncMaintenanceScope();
    }
    function syncMaintenanceScope() {
      const f = $('#maintenanceForm');
      if (!f) return;
      const isDevices = f.elements.scope.value === 'devices';
      $('#maintenanceTargetsField').classList.toggle('hidden', !isDevices);
      f.elements.targets.disabled = !isDevices;
    }
    function renderMaintenanceTargets() {
      const sel = $('#maintenanceForm') && $('#maintenanceForm').elements.targets;
      if (!sel) return;
      const prev = new Set(Array.from(sel.selectedOptions).map(o => o.value));
      const topo = S.topo || { switches: [], nodes: [] };
      const all = [...(topo.switches || []), ...(topo.nodes || [])];
      sel.innerHTML = all.map(n => `<option value="${esc(n.id)}"${prev.has(n.id) ? ' selected' : ''}>${esc(n.name || n.id)}${n.ip ? ' · ' + esc(n.ip) : ''}</option>`).join('') || '<option disabled>当前机房暂无设备</option>';
    }
    function fmtMaintTime(v) {
      const ms = Date.parse(v);
      return Number.isFinite(ms) ? Charts.fmtFull(ms) : String(v || '');
    }
    function fmtTargetList(ids) {
      return (ids || []).map(id => {
        const hit = findTarget(id);
        return hit ? `${hit.obj.name || hit.obj.id}${hit.obj.ip ? ' (' + hit.obj.ip + ')' : ''}` : id;
      }).join('、') || '指定设备';
    }
    $('#maintenanceForm').elements.scope.onchange = syncMaintenanceScope;
    renderMaintenanceTargets();
    initMaintenanceDefaults();

    async function loadMaintenance() {
      const rows = await api('GET', 'api/maintenance');
      if (!$('#maintenanceList')) return;
      const labels = { active: ['维护中', 'slow'], scheduled: ['待开始', 'unknown'], expired: ['已到期', ''], cancelled: ['已取消', ''], error: ['维护数据异常', 'down'] };
      const hasError = rows.some(w => w.status === 'error');
      const errBanner = $('#maintenanceErrorBanner');
      if (errBanner) {
        errBanner.classList.toggle('hidden', !hasError);
        if (hasError) {
          const errRow = rows.find(w => w.status === 'error');
          errBanner.textContent = (errRow && errRow.reason) || '维护窗口文件损坏，通知已保护性静默，请修复文件后重新加载。';
        }
      }
      const submitBtn = $('#maintenanceForm button[type=submit]');
      if (submitBtn) submitBtn.disabled = hasError;
      $('#maintenanceList').innerHTML = rows.map(w => {
        const [stLabel, stCls] = labels[w.status] || [w.status, 'unknown'];
        const scopeText = w.status === 'error' ? '损坏保护' : w.scope === 'site' ? '整机房' : fmtTargetList(w.target_ids);
        const timeHTML = w.status === 'error' ? '' : `<div class="mono muted" style="font-size:12px">${esc(fmtMaintTime(w.start))} → ${esc(fmtMaintTime(w.end))}</div>`;
        return `<div class="room-version"><div><div><span class="badge ${stCls}">${esc(stLabel)}</span> <b>${esc(scopeText)}</b></div>${timeHTML}<div class="help">${esc(w.reason || '')}</div>${w.status === 'error' ? '<p class="v-down" style="margin:4px 0 0">维护文件无法读取，告警通知已进入保护性静默。请修复服务器维护数据后重试。</p>' : ''}</div>${['active', 'scheduled'].includes(w.status) ? `<button class="btn small" data-cancel-maintenance="${esc(w.id)}">取消维护</button>` : ''}</div>`;
      }).join('') || '<p class="help">暂无维护窗口</p>';
    }
    $('#maintenanceList').onclick = async ev => {
      const b = ev.target.closest('[data-cancel-maintenance]'); if (!b || !confirm('取消维护窗口？受影响目标将按监控时段和告警规则恢复通知。')) return;
      b.disabled = true;
      try { await api('DELETE', 'api/maintenance/' + encodeURIComponent(b.dataset.cancelMaintenance)); await loadMaintenance(); toast('维护已取消，已恢复原告警规则'); }
      catch (e) { toast(errMsg(e), true); b.disabled = false; }
    };
    $('#maintenanceForm').onsubmit = async ev => {
      ev.preventDefault(); const f = ev.target;
      const start = new Date(f.elements.start.value), end = new Date(f.elements.end.value);
      if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) { toast('请填写有效的起止时间', true); return; }
      if (!(end > start)) { toast('结束时间必须晚于开始时间', true); return; }
      if (!(end > new Date())) { toast('结束时间必须晚于当前时间', true); return; }
      const ids = Array.from(f.elements.targets.selectedOptions).map(o => o.value).filter(Boolean);
      if (f.elements.scope.value === 'devices' && !ids.length) { toast('请选择至少一个维护设备', true); return; }
      if (!confirm('保存维护窗口？窗口内目标继续探测并记录历史，通知将被抑制，到期后自动恢复告警规则。')) return;
      const b = f.querySelector('button[type=submit]'); b.disabled = true;
      try {
        await api('POST', 'api/maintenance', { scope: f.elements.scope.value, target_ids: f.elements.scope.value === 'devices' ? ids : [], start: start.toISOString(), end: end.toISOString(), reason: f.elements.reason.value.trim() });
        f.reset();
        initMaintenanceDefaults();
        await loadMaintenance();
        toast('维护窗口已保存');
      } catch (e) { toast(errMsg(e), true); } finally { b.disabled = false; }
    };
    loadMaintenance().catch(e => { if ($('#maintenanceList')) $('#maintenanceList').textContent = '读取失败：' + errMsg(e); });

    let rooms = [];
    let roomBusy = false;
    async function loadRooms() {
      rooms = await api('GET', 'api/profiles');
      if (!$('#roomList')) return;
      const activeRoom = rooms.find(r => r.active);
      if ($('#roomActiveHint')) {
        $('#roomActiveHint').textContent = activeRoom ? `当前激活：${activeRoom.name}` : '同时仅激活一套';
      }
      $('#roomList').innerHTML = rooms.map(p => `<div class="room-card${p.active ? ' active' : ''}">
        <div class="room-card-head"><b>${esc(p.name)}</b> ${p.active ? '<span class="badge ok">已激活</span>' : '<span class="badge">未激活</span>'}</div>
        <div class="help">${p.switches} 台交换机 · ${p.nodes} 个节点${p.observation_minutes ? ` · 已观测 ${p.observation_minutes} 分钟` : ''}${p.site_name ? ' · ' + esc(p.site_name) : ''}</div>
        <div class="btns">${p.active ? '' : `<button class="btn small primary" data-room="${esc(p.id)}" data-action="activate">激活预览</button>`}
        <button class="btn small" data-room="${esc(p.id)}" data-action="settings">机房设置</button>
        <button class="btn small" data-room="${esc(p.id)}" data-action="versions">配置版本</button>
        <button class="btn small" data-room="${esc(p.id)}" data-action="export">导出</button>
        <button class="btn small" data-room="${esc(p.id)}" data-action="import">导入覆盖</button>
        <button class="btn small" data-room="${esc(p.id)}" data-action="rename">改名</button>
        ${p.active ? '' : `<button class="btn small danger" data-room="${esc(p.id)}" data-action="delete">删除</button>`}</div></div>`).join('') || '<p class="help">暂无机房配置</p>';
    }
    $('#roomCreate').onsubmit = async ev => {
      ev.preventDefault(); if (roomBusy) return;
      const f = ev.target;
      const btn = f.querySelector('button[type=submit]');
      roomBusy = true; if (btn) btn.disabled = true;
      try {
        await api('POST', 'api/profiles', { name: f.elements.name.value.trim(), copy_current: f.elements.copy.checked });
        f.reset(); await loadRooms(); toast('机房配置已保存（未激活，当前监控未切换）');
      } catch (e) { toast(errMsg(e), true); } finally { roomBusy = false; if (btn) btn.disabled = false; }
    };
    $('#roomList').onclick = async ev => {
      const b = ev.target.closest('[data-room]'); if (!b || roomBusy) return;
      const p = rooms.find(p => p.id === b.dataset.room); if (!p) return;
      const action = b.dataset.action;
      let name;
      if (action === 'rename') { name = prompt('配置名称', p.name); if (name === null) return; name = name.trim(); if (!name) return; }
      if (action === 'delete' && !confirm(`删除未激活配置「${p.name}」？此操作会移除该机房的拓扑、独立配置与历史数据。`)) return;

      roomBusy = true; b.disabled = true;
      try {
        if (action === 'versions') { await showVersions(p); return; }
        if (action === 'activate') { await showPreview(p); return; }
        if (action === 'settings') { await showProfileSettings(p); return; }
        if (action === 'export') { showExport(p); return; }
        if (action === 'import') { showImport(p); return; }
        await api(action === 'delete' ? 'DELETE' : action === 'rename' ? 'PUT' : 'POST', 'api/profiles/' + encodeURIComponent(p.id), { action, name });
        if (action === 'activate') { resetProfileView('#/settings'); return; }
        if (action === 'delete' && $('#roomInspect')) $('#roomInspect').innerHTML = '';
        await loadRooms(); toast(action === 'delete' ? '未激活机房已删除' : '机房名称已更新');
      } catch (e) { toast(errMsg(e), true); } finally { roomBusy = false; b.disabled = false; }
    };
    function bindInspectClose() {
      const closeBtn = $('#roomInspectClose');
      if (closeBtn) closeBtn.onclick = () => { $('#roomInspect').innerHTML = ''; };
    }
    function renderPreviewExtra(d) {
      const m = d.summary || {};
      const blocks = [];
      const metaParts = [];
      if (m.site_name) metaParts.push('站点：' + esc(m.site_name));
      if (m.subnet) metaParts.push('网段：' + esc(m.subnet));
      if (m.gateway) metaParts.push('网关：' + esc(m.gateway));
      if (m.links != null) metaParts.push('链路 ' + m.links + ' 条');
      if (m.discovery_jobs != null) metaParts.push('扫描任务 ' + m.discovery_jobs + ' 个');
      if (m.locate_switches != null) metaParts.push('SNMP 交换机 ' + m.locate_switches + ' 台');
      if (m.alert_enabled != null) metaParts.push('告警推送：' + (m.alert_enabled ? '已开启' : '未开启'));
      if (m.schedule_enabled != null) metaParts.push('监控时段：' + (m.schedule_enabled ? (m.in_schedule_now === false ? '已开启（当前在时段外）' : '已开启') : '全天候'));
      if (metaParts.length) blocks.push(`<div class="help" style="margin:6px 0">${metaParts.join(' · ')}</div>`);

      if (Array.isArray(d.scan_ranges)) {
        blocks.push(`<div class="room-kv"><span class="k">扫描范围</span><span>${d.scan_ranges.length ? d.scan_ranges.map(x => `<code>${esc(x)}</code>`).join(' ') : '<span class="muted">未配置扫描范围</span>'}</span></div>`);
      } else {
        blocks.push(`<div class="room-kv"><span class="k">扫描范围</span><span>${m.subnet ? `<code>${esc(m.subnet)}</code> <span class="muted">（机房网段；完整扫描任务范围待后端返回）</span>` : '<span class="muted">未设置（待后端返回扫描范围）</span>'}</span></div>`);
      }

      if (Array.isArray(d.notification_destinations)) {
        const destHtml = d.notification_destinations.length
          ? d.notification_destinations.map(x => `<span class="badge unknown">${esc(x.channel || '渠道')}</span> <code class="mono">${esc(x.destination || '已脱敏')}</code>`).join('<br>')
          : '<span class="muted">未配置通知渠道</span>';
        blocks.push(`<div class="room-kv"><span class="k">通知去向（已脱敏）</span><span>${destHtml}</span></div>`);
      } else {
        blocks.push('<div class="room-kv"><span class="k">通知去向（已脱敏）</span><span class="muted">待后端返回脱敏去向</span></div>');
      }

      if (Array.isArray(d.connectivity)) {
        const connLabel = { reachable: ['可达', 'ok'], unreachable: ['不可达', 'down'], unknown: ['未知', 'unknown'] };
        const connHtml = d.connectivity.length
          ? d.connectivity.map(c => {
            const [lbl, cls] = connLabel[c.status] || [c.status || '未知', 'unknown'];
            return `<div><span class="badge ${cls}">${esc(lbl)}</span> <span class="mono">${esc(c.ip || '')}</span>${c.detail ? ` <span class="muted">${esc(c.detail)}</span>` : ''}</div>`;
          }).join('')
          : '<span class="muted">无待检查目标地址</span>';
        blocks.push(`<div class="room-kv"><span class="k">连通性检查（仅供参考）</span><div>${connHtml}</div></div>`);
      } else {
        blocks.push('<div class="room-kv"><span class="k">连通性检查（仅供参考）</span><span class="muted">待后端返回连通性结果（不可达不限制切换）</span></div>');
      }
      $('#roomPreviewExtra').innerHTML = blocks.join('');
    }
    async function showPreview(p) {
      $('#roomInspect').innerHTML = `<div class="room-preview"><p class="muted">正在加载「${esc(p.name)}」激活预览与连通性检查…</p></div>`;
      let d;
      try {
        d = await api('GET', 'api/profiles/' + encodeURIComponent(p.id) + '/preview');
      } catch (e) {
        $('#roomInspect').innerHTML = `<div class="room-preview"><div class="room-preview-head"><b>激活预览 · ${esc(p.name)}</b><button class="btn small" id="roomInspectClose">关闭</button></div><p class="v-down">预览读取失败：${esc(errMsg(e))}</p></div>`;
        bindInspectClose();
        throw e;
      }
      const m = d.summary || {};
      const errs = (d.errors || []).map(x => `<div class="v-down">• ${esc(x)}</div>`).join('');
      const warns = (d.warnings || []).map(x => `<div class="v-warn">• ${esc(x)}</div>`).join('');
      $('#roomInspect').innerHTML = `<div class="room-preview">
        <div class="room-preview-head"><h3>激活预览 · ${esc(p.name)}</h3><button class="btn small" id="roomInspectClose">关闭</button></div>
        <p style="margin:6px 0"><b>${m.switches || 0}</b> 台交换机 · <b>${m.nodes || 0}</b> 个节点 · <b>${m.monitored || 0}</b> 个监控目标</p>
        <div id="roomPreviewExtra"></div>
        ${errs ? `<div style="margin-top:8px">${errs}</div>` : ''}
        ${warns ? `<div style="margin-top:8px">${warns}</div>` : ''}
        ${d.busy ? `<p class="v-warn" style="margin:8px 0">${esc(d.busy_reason || '后台任务正在运行')}</p>` : ''}
        <p class="help">激活切换后，当前机房将停止监控，页面中未保存编辑将丢失。连通性检查仅作参考提示，不限制用户切换到当前不可达的机房。</p>
        <div class="btns"><button class="btn primary" id="roomConfirm" ${!d.can_activate || d.already_active ? 'disabled' : ''}>${d.already_active ? '当前已激活' : '确认激活'}</button></div></div>`;
      bindInspectClose();
      renderPreviewExtra(d);
      $('#roomConfirm').onclick = async ev => {
        if (!confirm(`确认激活「${p.name}」并停止当前机房监控？`)) return;
        ev.target.disabled = true;
        try { await api('POST', 'api/profiles/' + encodeURIComponent(p.id), { action: 'activate' }); resetProfileView('#/settings'); }
        catch (e) { toast(errMsg(e), true); ev.target.disabled = false; }
      };
    }
    // ---- 按机房导入导出 ----
    const SECRET_ACTION_TXT = { imported: '使用文件中的值', kept: '文件中为脱敏占位，保留目标机房原值', cleared: '文件中为脱敏占位且目标机房没有原值，导入后需重新填写' };
    function showExport(p) {
      const url = include => 'api/profiles/' + encodeURIComponent(p.id) + '/export' + (include ? '?secrets=include' : '');
      $('#roomInspect').innerHTML = `<div class="room-preview">
        <div class="room-preview-head"><h3>导出机房配置 · ${esc(p.name)}</h3><button class="btn small" id="roomInspectClose">关闭</button></div>
        <p class="help">导出文件包含该机房的拓扑（交换机、链路、设备）和机房级设置（告警通知、监控时段、Ping 参数、网段扫描、端口定位）。不包含登录密码、MCP、AI 等全局配置，也不包含历史监控数据、告警记录和配置版本。</p>
        <div class="checks"><label><input type="checkbox" id="roomExportSecrets">包含明文凭据（Webhook 地址、签名密钥、SNMP 团体名、代理账号）</label></div>
        <p class="help" id="roomExportSecretsHint">默认导出时凭据会脱敏；导入到已有这些凭据的机房时会保留其原值，导入到新机房则需要重新填写。</p>
        <div class="btns"><a class="btn primary" id="roomExportLink" href="${url(false)}" download>下载导出文件</a></div></div>`;
      bindInspectClose();
      $('#roomExportSecrets').onchange = ev => {
        $('#roomExportLink').href = url(ev.target.checked);
        $('#roomExportSecretsHint').innerHTML = ev.target.checked
          ? '<span class="v-warn">导出文件将包含明文凭据，请妥善保管，不要发送到不可信的地方。</span>'
          : '默认导出时凭据会脱敏；导入到已有这些凭据的机房时会保留其原值，导入到新机房则需要重新填写。';
      };
    }
    function renderImportPreview(d) {
      const c = d.current || {}, n = d.incoming || {};
      const row = (label, a, b) => `<tr><td>${label}</td>${d.new_profile ? '' : `<td>${esc(String(a == null ? '-' : a))}</td>`}<td><b>${esc(String(b == null ? '-' : b))}</b></td></tr>`;
      const secrets = (d.secrets || []).map(x => `<div><code>${esc(x.field)}</code>：<span class="${x.action === 'cleared' ? 'v-warn' : ''}">${esc(SECRET_ACTION_TXT[x.action] || x.action)}</span></div>`).join('');
      const warns = (d.warnings || []).map(x => `<div class="v-warn">• ${esc(x)}</div>`).join('');
      const exported = Number.isFinite(Date.parse(d.exported_at)) ? Charts.fmtFull(Date.parse(d.exported_at)) : '';
      return `<p style="margin:6px 0">${d.new_profile
        ? `将创建新机房「<b>${esc(d.target_name)}</b>」（不激活）。`
        : `将<b>整体替换</b>机房「<b>${esc(d.target_name)}</b>」${d.target_active ? '（当前激活机房）' : ''}的拓扑和机房设置，原有内容不会合并保留。`}</p>
        <div class="help">来源：${esc(d.source_name || '未知机房')}${exported ? ' · 导出于 ' + esc(exported) : ''} · 文件凭据：${d.file_secrets === 'included' ? '包含明文' : '已脱敏'}</div>
        <table class="grid flat" style="margin:8px 0"><thead><tr><th></th>${d.new_profile ? '' : '<th>当前</th>'}<th>导入后</th></tr></thead><tbody>
          ${row('站点名称', c.site_name, n.site_name)}${row('交换机', c.switches, n.switches)}${row('设备', c.nodes, n.nodes)}${row('链路', c.links, n.links)}${row('扫描任务', c.scan_jobs, n.scan_jobs)}
        </tbody></table>
        ${d.new_profile ? '' : `<div class="room-kv"><span class="k">会被替换的部分</span><span>${(d.changed_sections || []).length ? d.changed_sections.map(x => `<span class="badge">${esc(x)}</span>`).join(' ') : '<span class="muted">内容与当前一致</span>'}</span></div>`}
        ${secrets ? `<div class="room-kv"><span class="k">凭据处理</span><div>${secrets}</div></div>` : ''}
        <div class="room-kv"><span class="k">不受影响</span><span>${(d.unaffected || []).map(esc).join('；')}</span></div>
        ${warns ? `<div style="margin-top:8px">${warns}</div>` : ''}
        ${d.new_profile ? '' : '<p class="help">确认后会先把该机房当前配置自动保存为「导入前自动备份」版本，可在「配置版本」中恢复。</p>'}`;
    }
    function readFileText(file) {
      return new Promise((resolve, reject) => {
        if (file.size > 8 * 1024 * 1024) { reject(new Error('导入文件过大（上限 8 MB）')); return; }
        const fr = new FileReader();
        fr.onload = () => resolve(String(fr.result || ''));
        fr.onerror = () => reject(new Error('读取文件失败'));
        fr.readAsText(file, 'utf-8');
      });
    }
    // p 为空表示导入为新机房。
    async function showImport(p, presetFile) {
      const target = p ? 'api/profiles/' + encodeURIComponent(p.id) + '/import' : 'api/profiles/import';
      const title = p ? `导入覆盖 · ${esc(p.name)}` : '导入为新机房';
      $('#roomInspect').innerHTML = `<div class="room-preview">
        <div class="room-preview-head"><h3>${title}</h3><button class="btn small" id="roomInspectClose">关闭</button></div>
        ${p ? '<label class="f"><span>选择导出文件（.json）</span><input type="file" id="roomImportFile" accept=".json,application/json"></label>' : ''}
        <div id="roomImportPreview"><p class="help">${p ? '选择文件后先校验并显示覆盖说明，确认前不会修改任何配置。' : '正在校验文件…'}</p></div></div>`;
      bindInspectClose();
      let text = '';
      const preview = async file => {
        const box = $('#roomImportPreview');
        box.innerHTML = '<p class="muted">正在校验文件…</p>';
        try {
          text = await readFileText(file);
          const res = await api('POST', target + '?dry_run=1', text, { text: true });
          const d = res.preview || {};
          box.innerHTML = renderImportPreview(d) + `<div class="btns"><button class="btn primary" id="roomImportConfirm">${d.new_profile ? '确认导入为新机房' : '确认覆盖导入'}</button></div>`;
          $('#roomImportConfirm').onclick = async ev => {
            if (!d.new_profile && !confirm(`确认用文件内容整体替换「${d.target_name}」的拓扑和机房设置？${d.target_active ? '\n这是当前激活机房，导入后立即生效。' : ''}\n当前配置会先自动备份。`)) return;
            ev.target.disabled = true;
            try {
              const r = await api('POST', target, text, { text: true });
              if (!d.new_profile && d.target_active) { resetProfileView('#/settings'); return; }
              await loadRooms();
              box.innerHTML = `<p class="v-ok">${d.new_profile ? `已导入为新机房「${esc(d.target_name)}」（未激活）。` : `已导入到「${esc(d.target_name)}」，原配置已备份为 <code>${esc(r.backup_version || '')}</code>。`}</p>`;
              toast(d.new_profile ? '已导入为新机房' : '机房配置已导入');
            } catch (e) { toast(errMsg(e), true); ev.target.disabled = false; }
          };
        } catch (e) {
          box.innerHTML = `<p class="v-down">文件未通过校验，未做任何修改：${esc(e.message)}</p>${e.problems ? '<ul>' + e.problems.map(x => `<li>${esc(x)}</li>`).join('') + '</ul>' : ''}`;
        }
      };
      if (p) $('#roomImportFile').onchange = ev => { const f = ev.target.files[0]; if (f) preview(f); };
      if (presetFile) await preview(presetFile);
    }
    $('#roomImportNewFile').onchange = async ev => {
      const f = ev.target.files[0]; if (!f) return;
      try { await showImport(null, f); } finally { ev.target.value = ''; }
    };
    async function showVersions(p) {
      const base = 'api/profiles/' + encodeURIComponent(p.id) + '/versions';
      $('#roomInspect').innerHTML = `<div class="room-preview"><p class="muted">正在加载「${esc(p.name)}」配置版本…</p></div>`;
      let versions;
      try {
        versions = await api('GET', base);
      } catch (e) {
        $('#roomInspect').innerHTML = `<div class="room-preview"><div class="room-preview-head"><b>配置版本 · ${esc(p.name)}</b><button class="btn small" id="roomInspectClose">关闭</button></div><p class="v-down">版本列表读取失败：${esc(errMsg(e))}</p></div>`;
        bindInspectClose();
        throw e;
      }
      $('#roomInspect').innerHTML = `<div class="room-preview">
        <div class="room-preview-head"><h3>配置版本 · ${esc(p.name)}</h3><div class="btns" style="margin:0"><button class="btn small primary" id="roomSnapshot">保存当前版本</button><button class="btn small" id="roomInspectClose">关闭</button></div></div>
        <div id="roomVersions"></div>
        <div id="roomVersionDetail"></div></div>`;
      bindInspectClose();
      const reasonTxt = { manual: '手动保存', pre_activate: '激活前自动备份', pre_rollback: '回滚前自动备份', pre_import: '导入前自动备份', topo_edit: '拓扑变更', settings_edit: '设置变更' };
      $('#roomVersions').innerHTML = (versions || []).map(v => {
        const ts = v.created_at || v.time || '';
        const formatted = Number.isFinite(Date.parse(ts)) ? Charts.fmtFull(Date.parse(ts)) : ts;
        const meta = [
          v.reason ? esc(reasonTxt[v.reason] || v.reason) : '',
          v.site_name ? esc(v.site_name) : '',
          v.switches != null ? `${v.switches} 交换机` : '',
          v.nodes != null ? `${v.nodes} 节点` : '',
          v.size_bytes ? `${(v.size_bytes / 1024).toFixed(1)} KB` : '',
        ].filter(Boolean).join(' · ');
        return `<div class="room-version">
          <div>
            <div><span class="mono"><b>${esc(v.id)}</b></span> · <span>${esc(formatted)}</span>${v.note ? ` · ${esc(v.note)}` : ''}</div>
            ${meta ? `<div class="help" style="margin:2px 0 0">${meta}</div>` : ''}
          </div>
          <div class="btns" style="margin:0">
            <button class="btn small" data-version-detail="${esc(v.id)}">查看</button>
            <button class="btn small danger" data-version="${esc(v.id)}">恢复</button>
          </div>
        </div>`;
      }).join('') || '<p class="help">暂无版本</p>';
      $('#roomSnapshot').onclick = async () => {
        const note = prompt('版本备注（可选）', '手动保存'); if (note === null) return;
        try { await api('POST', base, { note }); await showVersions(p); toast('配置版本已保存'); } catch (e) { toast(errMsg(e), true); }
      };
      $('#roomVersions').onclick = async ev => {
        const detailBtn = ev.target.closest('[data-version-detail]');
        if (detailBtn) {
          detailBtn.disabled = true;
          try {
            const vd = await api('GET', base + '/' + encodeURIComponent(detailBtn.dataset.versionDetail));
            const meta = vd.meta || {};
            const topo = vd.topology || {};
            const st = vd.settings || {};
            const ping = st.ping || {};
            const al = st.alert || {};
            const sch = st.schedule || {};
            $('#roomVersionDetail').innerHTML = `<div class="room-version-detail">
              <div><b>版本快照详情 · ${esc(meta.id || detailBtn.dataset.versionDetail)}</b></div>
              <div class="help">站点：${esc((topo.site && topo.site.name) || meta.site_name || '-')} · ${(topo.switches || []).length} 台交换机 · ${(topo.nodes || []).length} 个节点 · ${(topo.links || []).length} 条链路</div>
              <div class="help">Ping：间隔 ${ping.interval_ms || '-'}ms / 超时 ${ping.timeout_ms || '-'}ms · 告警推送：${al.enabled ? '开启' : '关闭'} · 监控时段：${sch.enabled ? '按时段' : '全天候'}</div>
            </div>`;
          } catch (e) { toast(errMsg(e), true); }
          finally { detailBtn.disabled = false; }
          return;
        }
        const b = ev.target.closest('[data-version]'); if (!b) return;
        if (!confirm(`恢复「${p.name}」的此版本？将替换该机房的配置与拓扑（当前配置会先自动备份）。历史监控数据不回退。`)) return;
        b.disabled = true;
        try {
          const res = await api('POST', base + '/' + encodeURIComponent(b.dataset.version) + '/rollback', {});
          const backupTip = res && res.backup_version ? `（原配置已备份为 ${res.backup_version}）` : '';
          if (p.active) { resetProfileView('#/settings'); }
          else { await showVersions(p); await loadRooms(); toast(`版本已恢复${backupTip}，机房仍未激活`); }
        } catch (e) { toast(errMsg(e), true); b.disabled = false; }
      };
    }
    async function showProfileSettings(p) {
      $('#roomInspect').innerHTML = `<div class="room-preview"><p class="muted">正在读取「${esc(p.name)}」独立机房设置…</p></div>`;
      let res;
      try {
        res = await api('GET', 'api/profiles/' + encodeURIComponent(p.id) + '/settings');
      } catch (e) {
        $('#roomInspect').innerHTML = `<div class="room-preview"><div class="room-preview-head"><b>机房独立设置 · ${esc(p.name)}</b><button class="btn small" id="roomInspectClose">关闭</button></div><p class="v-down">读取失败：${esc(errMsg(e))}</p></div>`;
        bindInspectClose();
        throw e;
      }
      const st = res.settings || {};
      const ping = st.ping || {};
      const al = st.alert || {};
      const sch = st.schedule || {};
      const disc = st.discovery || {};
      const loc = st.locate || {};
      const maskedChannels = [
        ['企业微信', al.webhook_url],
        ['钉钉', al.dingtalk_url],
        ['飞书', al.feishu_url],
        ['Slack', al.slack_url],
        ['通用 Webhook', al.generic_webhook_url],
      ].filter(([, v]) => !!v);
      $('#roomInspect').innerHTML = `<div class="room-preview">
        <div class="room-preview-head"><h3>机房独立设置 · ${esc(res.name || p.name)} ${res.active ? '<span class="badge ok">已激活</span>' : '<span class="badge">未激活</span>'}</h3><button class="btn small" id="roomInspectClose">关闭</button></div>
        <p class="help">此表单仅修改该机房的隔离参数（密钥与 Webhook 地址已脱敏展示；如需修改当前激活机房的密钥或时段细节，请使用左侧对应面板）。</p>
        <form id="roomSettingsForm">
          <div class="checks" style="margin-bottom:8px">
            <label><input type="checkbox" name="alert_enabled" ${al.enabled ? 'checked' : ''}> 启用告警推送</label>
            <label><input type="checkbox" name="send_recovery" ${al.send_recovery ? 'checked' : ''}> 发送恢复通知</label>
            <label><input type="checkbox" name="schedule_enabled" ${sch.enabled ? 'checked' : ''}> 按监控时段运行</label>
            <label><input type="checkbox" name="discovery_enabled" ${disc.enabled ? 'checked' : ''}> 启用自动扫描</label>
            <label><input type="checkbox" name="locate_enabled" ${loc.enabled ? 'checked' : ''}> 启用定时定位</label>
          </div>
          <label class="f"><span>告警消息标题</span><input type="text" name="site_title" value="${esc(al.site_title || '')}"></label>
          <div class="row3">
            ${num('interval_ms', 'Ping 间隔 ms', ping.interval_ms || 3000, 'min="200"')}
            ${num('timeout_ms', 'Ping 超时 ms', ping.timeout_ms || 1000, 'min="100"')}
            ${num('packet_size', '数据包字节', ping.packet_size != null ? ping.packet_size : 32, 'min="0"')}
            ${num('latency_warn_ms', '延迟阈值 ms', ping.latency_warn_ms || 100, 'min="1"')}
            ${num('loss_warn_pct', '丢包阈值 %', ping.loss_warn_pct || 20, 'min="1" max="100"')}
            ${num('locate_interval_min', '定位间隔(分)', loc.interval_min || 10, 'min="2"')}
          </div>
          <div class="help">已配置通知渠道（脱敏）：${maskedChannels.length ? maskedChannels.map(([c, v]) => `${esc(c)}(<code>${esc(v)}</code>)`).join('、') : '未配置'}</div>
          <div class="btns"><button class="btn primary" type="submit">保存机房设置</button></div>
        </form>
      </div>`;
      bindInspectClose();
      $('#roomSettingsForm').onsubmit = async ev => {
        ev.preventDefault();
        const f = ev.target, e = f.elements;
        const next = JSON.parse(JSON.stringify(st));
        next.alert = Object.assign({}, next.alert, { enabled: e.alert_enabled.checked, send_recovery: e.send_recovery.checked, site_title: e.site_title.value.trim() });
        next.schedule = Object.assign({}, next.schedule, { enabled: e.schedule_enabled.checked });
        next.discovery = Object.assign({}, next.discovery, { enabled: e.discovery_enabled.checked });
        next.locate = Object.assign({}, next.locate, { enabled: e.locate_enabled.checked, interval_min: +e.locate_interval_min.value });
        next.ping = Object.assign({}, next.ping, { interval_ms: +e.interval_ms.value, timeout_ms: +e.timeout_ms.value, packet_size: +e.packet_size.value, latency_warn_ms: +e.latency_warn_ms.value, loss_warn_pct: +e.loss_warn_pct.value });
        const btn = f.querySelector('button[type=submit]'); btn.disabled = true;
        try {
          await api('PUT', 'api/profiles/' + encodeURIComponent(p.id) + '/settings', next);
          toast(`机房「${p.name}」配置已保存${p.active ? '（当前机房已生效）' : '（未激活机房将在激活后生效）'}`);
          if (p.active) await loadCfg();
        } catch (err) { toast(errMsg(err), true); }
        finally { btn.disabled = false; }
      };
    }
    loadRooms().catch(e => { if ($('#roomList')) $('#roomList').textContent = errMsg(e); });

    let cfg = null;
    const num = (name, label, v, extra) => `<label class="f"><span>${label}</span><input type="number" name="${name}" value="${v}" ${extra || ''}></label>`;
    async function loadCfg() {
      cfg = await api('GET', 'api/config');
      const a = cfg.alert;
      $('#af').innerHTML = `
        <div class="checks" style="margin-bottom:8px"><label><input type="checkbox" name="enabled" ${a.enabled ? 'checked' : ''}> 启用告警推送</label>
          <label><input type="checkbox" name="send_recovery" ${a.send_recovery ? 'checked' : ''}> 发送恢复通知</label></div>
        <label class="f"><span>企业微信 Webhook 地址（可选）</span><input type="text" name="webhook_url" value="${esc(a.webhook_url)}" placeholder="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=..."></label>
        <div class="help">填写地址的渠道都会接收告警；清空地址即可停用该渠道。多个渠道同时发送，使用下方统一规则。</div>
        <label class="f"><span>钉钉 Webhook</span><input type="text" name="dingtalk_url" value="${esc(a.dingtalk_url || '')}" autocomplete="off"></label>
        <label class="f"><span>钉钉加签密钥（可选）</span><input type="password" name="dingtalk_secret" value="${esc(a.dingtalk_secret || '')}" autocomplete="off"></label>
        <label class="f"><span>飞书 Webhook</span><input type="text" name="feishu_url" value="${esc(a.feishu_url || '')}" autocomplete="off"></label>
        <label class="f"><span>飞书签名密钥（可选）</span><input type="password" name="feishu_secret" value="${esc(a.feishu_secret || '')}" autocomplete="off"></label>
        <label class="f"><span>Slack Incoming Webhook</span><input type="text" name="slack_url" value="${esc(a.slack_url || '')}" autocomplete="off"></label>
        <label class="f"><span>通用 Webhook（JSON：title、text）</span><input type="text" name="generic_webhook_url" value="${esc(a.generic_webhook_url || '')}" autocomplete="off"></label>
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
          ${num('packet_size', '数据长度 字节', p.packet_size, 'min="0"')}${num('retention_days', '历史保留天数（全局）', cfg.retention_days, 'min="1"')}
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
        enabled: e.enabled.checked, send_recovery: e.send_recovery.checked, webhook_url: e.webhook_url.value.trim(), dingtalk_url: e.dingtalk_url.value.trim(), dingtalk_secret: e.dingtalk_secret.value.trim(), feishu_url: e.feishu_url.value.trim(), feishu_secret: e.feishu_secret.value.trim(), slack_url: e.slack_url.value.trim(), generic_webhook_url: e.generic_webhook_url.value.trim(), site_title: e.site_title.value.trim(),
        down_after_failures: +e.down_after_failures.value, up_after_successes: +e.up_after_successes.value, merge_window_sec: +e.merge_window_sec.value,
        loss_duration_min: +e.loss_duration_min.value, latency_duration_min: +e.latency_duration_min.value, repeat_interval_min: +e.repeat_interval_min.value,
        proxy_url: e.proxy_url.value.trim(), tls_skip_verify: e.tls_skip_verify.checked,
      });
      try { await api('PUT', 'api/config', c); toast('告警设置已保存'); await loadCfg(); } catch (err) { toast(errMsg(err), true); }
    };
    $('#af').addEventListener('click', async ev => {
      if (ev.target.id !== 'aTest') return;
      const e = $('#af').elements;
      const a = Object.assign({}, cfg.alert, { webhook_url: e.webhook_url.value.trim(), dingtalk_url: e.dingtalk_url.value.trim(), dingtalk_secret: e.dingtalk_secret.value.trim(), feishu_url: e.feishu_url.value.trim(), feishu_secret: e.feishu_secret.value.trim(), slack_url: e.slack_url.value.trim(), generic_webhook_url: e.generic_webhook_url.value.trim(), site_title: e.site_title.value.trim(), proxy_url: e.proxy_url.value.trim(), tls_skip_verify: e.tls_skip_verify.checked });
      ev.target.disabled = true;
      try { await api('POST', 'api/alert/test', a); toast('测试消息已发送，请在已配置的各渠道中确认'); }
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
      $('#about').innerHTML = `<div class="mono" style="font-size:12px;line-height:1.8">版本 ${esc(m.version)}${m.active_profile ? `<br>激活机房 ${esc(m.active_profile)}` : ''}<br>数据目录 ${esc(m.base_dir)}<br>拓扑文件 ${esc(m.topo_file)}<br>
        启动于 ${esc(Charts.fmtFull(m.started))}</div><div class="help">字段说明：docs/topology-fields.md；JSON Schema：schema/topology.schema.json</div>`;
    }).catch(() => {});
    loadCfg().catch(e => toast(errMsg(e), true));
    loadSite(); loadYAML(); banner();
    return { onTopology() { loadSite(); renderMaintenanceTargets(); loadRooms().catch(e => toast(errMsg(e), true)); }, update: banner };
  }


  // ---------------------------------------------------------------------------
  // 插件页：启停、设置、测试运行；插件文件需手工放进数据目录的 plugins/
  function pluginsPage(app) {
    const HOOK_TXT = {
      notify: ['通知渠道', '告警、扫描发现、端口定位要推送消息时，把消息交给插件发送'],
      alert: ['告警事件', '离线、恢复、丢包、延迟高等事件的结构化数据'],
      discovery: ['扫描发现', '网段扫描发现新设备、IP 变化等'],
      locate: ['端口定位', '设备被自动挂到交换机端口等'],
    };
    let data = null;
    const outputs = {}; // 插件 id -> 最近一次测试输出的 HTML
    app.innerHTML = `<div class="page pad"><div class="mcp plug">
      <div class="mcp-hero">
        <div>
          <h2>插件 <span id="pStat"></span></h2>
          <div class="help" style="margin:0">插件可以给 网脉 NetPulse 增加新的通知渠道（短信、邮件、工单系统…）、在告警和设备变化时执行自定义动作，
            或给 AI 助手提供新的查询工具。插件是放在数据目录里的独立小程序，用任何语言都能写。</div>
        </div>
      </div>
      <div class="panel"><div class="ph">安装插件<span class="r"><button class="btn small" id="pReload">重新扫描</button>
        <a class="btn small" href="api/plugins/guide" target="_blank" rel="noopener">开发指南</a></span></div><div class="pb">
        <div class="copyline"><code id="pDir">-</code><button class="btn small" data-copy="pDir">复制</button></div>
        <div class="help" style="margin-bottom:0">把插件文件夹（里面有 <code>plugin.yaml</code>）放进上面的目录，点「重新扫描」即可看到。新插件默认<b>未启用</b>，
          填好设置并点「启用」后才会运行。修改了 <code>plugin.yaml</code> 也需要重新扫描；只改插件程序本身则立即生效。</div>
      </div></div>
      <div id="pList"></div>
      <div class="panel"><div class="ph">最近调用<span class="r muted">重启程序后清空，完整记录见 logs/netping.log</span></div><div class="pb" style="padding:0">
        <table class="grid flat"><thead><tr><th>时间</th><th>插件</th><th>类型</th><th>结果</th><th>耗时</th><th>说明</th></tr></thead><tbody id="pRuns"></tbody></table>
      </div></div>
      <div class="panel"><div class="ph">安全说明</div><div class="pb"><ul class="help" style="margin:0;padding-left:18px">
        <li>插件以 网脉 NetPulse 的系统账号运行，能读写这台电脑上 网脉 NetPulse 能访问的一切。<b>只安装来源可信、看得懂的插件。</b></li>
        <li>网页和 MCP 都不能上传或安装插件，只能由能登录这台电脑的人把文件放进插件目录。AI 助手不能启停或配置插件。</li>
        <li>设置里的密钥保存在 <code>data/plugins.json</code>，网页不回显；调用时通过标准输入传给插件，不出现在命令行里。</li>
        <li>标记为「修改」的插件工具在 MCP 只读模式下不可用，网页「AI 诊断」永远不会调用。</li>
      </ul></div></div>
    </div></div>`;
    $$('[data-copy]', app).forEach(b => b.onclick = () => copyText($('#' + b.dataset.copy).textContent));

    const hookLabel = h => h === 'tool' ? '工具' : (HOOK_TXT[h] ? HOOK_TXT[h][0] : h);
    function settingInput(s) {
      const name = 's_' + s.key, req = s.required ? ' <b style="color:var(--down)">*</b>' : '';
      const help = s.help ? `<div class="help" style="margin:2px 0 0">${esc(s.help)}</div>` : '';
      if (s.type === 'bool') return `<label class="f"><span>${esc(s.label)}</span><label style="font-size:12px"><input type="checkbox" name="${name}" ${s.value ? 'checked' : ''}> 开启</label>${help}</label>`;
      let input;
      if (s.type === 'select') input = `<select name="${name}">${s.required ? '' : '<option value=""></option>'}${s.options.map(o => `<option${o === s.value ? ' selected' : ''}>${esc(o)}</option>`).join('')}</select>`;
      else if (s.type === 'text') input = `<textarea name="${name}" rows="3">${esc(s.value || '')}</textarea>`;
      else if (s.type === 'secret') input = `<input type="password" name="${name}" autocomplete="new-password" placeholder="${s.set ? '已填写，留空表示不修改' : '未填写'}">`;
      else if (s.type === 'number') input = `<input type="number" step="any" name="${name}" value="${esc(String(s.value == null ? '' : s.value))}">`;
      else input = `<input type="text" name="${name}" value="${esc(s.value || '')}" autocomplete="off">`;
      return `<label class="f"><span>${esc(s.label)}${req}</span>${input}${help}</label>`;
    }
    function pluginPanel(p) {
      const state = p.error ? '<span class="badge down">清单无效</span>' : p.enabled ? '<span class="badge ok">已启用</span>' : '<span class="badge unknown">未启用</span>';
      const head = `<div class="ph"><span>${esc(p.name)}</span>${p.version ? `<span class="muted mono" style="font-weight:normal">${esc(p.version)}</span>` : ''}${state}
        <span class="r">${p.error ? '' : `<button class="btn small ${p.enabled ? '' : 'primary'}" data-toggle="${esc(p.id)}">${p.enabled ? '停用' : '启用'}</button>`}</span></div>`;
      if (p.error) {
        return `<div class="panel">${head}<div class="pb"><div class="problems" style="margin:0 0 8px">${esc(p.error)}</div>
          <div class="help" style="margin:0">目录：<code>${esc(p.dir)}</code>。修正 plugin.yaml 后点「重新扫描」。</div></div></div>`;
      }
      const hooks = p.hooks.map(h => `<span class="badge unknown" title="${esc(HOOK_TXT[h] ? HOOK_TXT[h][1] : '')}">${esc(hookLabel(h))}</span>`).join(' ');
      const tools = p.tools.length ? `<table class="grid flat" style="margin-top:10px"><thead><tr><th>工具</th><th>说明</th><th>类型</th></tr></thead><tbody>${p.tools.map(t =>
        `<tr><td class="mono"><b>${esc(t.full_name)}</b><br><span class="muted" style="font-family:var(--sans)">${esc(t.title)}</span></td><td class="wrap">${esc(t.description)}</td>
         <td>${t.write ? '<span class="badge slow">修改</span>' : '<span class="badge ok">查询</span>'}${t.ai ? ' <span class="badge ok" title="网页「AI 诊断」可以调用">AI 诊断</span>' : ''}</td></tr>`).join('')}</tbody></table>` : '';
      const settings = p.settings.length ? `<form data-settings="${esc(p.id)}" style="margin-top:12px;max-width:560px">
        ${p.settings.map(settingInput).join('')}<div class="btns"><button class="btn primary" type="submit">保存设置</button></div></form>` : '';
      const opts = p.hooks.map(h => `<option value="hook:${esc(h)}">${esc(hookLabel(h))}（示例数据）</option>`)
        .concat(p.tools.map(t => `<option value="tool:${esc(t.name)}">工具 ${esc(t.name)}</option>`)).join('');
      return `<div class="panel">${head}<div class="pb">
        ${p.description ? `<div style="margin-bottom:8px">${esc(p.description)}</div>` : ''}
        <div class="kv"><code>功能</code><span>${hooks}${p.tools.length ? ` <span class="badge unknown">工具 ${p.tools.length} 个</span>` : ''}</span></div>
        <div class="kv"><code>运行命令</code><span class="mono">${esc(p.command.join(' '))}（超时 ${p.timeout_sec} 秒）</span></div>
        <div class="kv"><code>目录</code><span class="mono">${esc(p.dir)}</span></div>
        ${p.author || p.homepage ? `<div class="kv"><code>作者</code><span>${esc(p.author || '')} ${/^https?:\/\//.test(p.homepage || '') ? `<a href="${esc(p.homepage)}" target="_blank" rel="noopener noreferrer">${esc(p.homepage)}</a>` : ''}</span></div>` : ''}
        ${tools}${settings}
        <form data-test="${esc(p.id)}" style="margin-top:12px">
          <div style="display:flex;gap:6px;flex-wrap:wrap;align-items:center">
            <select name="what" style="width:auto">${opts}</select>
            <input type="text" name="args" class="mono" style="flex:1;min-width:200px;display:none" placeholder='工具参数（JSON），如 {"ip":"192.168.1.10"}' autocomplete="off">
            <button class="btn small" type="submit">测试运行</button>
            <span class="muted" style="font-size:12px">会真实运行一次插件程序${p.enabled ? '' : '（未启用也可以测试）'}</span>
          </div>
          <div data-out="${esc(p.id)}">${outputs[p.id] || ''}</div>
        </form>
      </div></div>`;
    }
    function testOutput(r) {
      const j = v => esc(JSON.stringify(v, null, 2));
      return `<div style="margin-top:8px">${r.ok ? `<div class="v-ok"><b>✓ 成功</b>（${r.ms}ms）${r.message ? '：' + esc(r.message) : ''}</div>`
        : `<div class="v-down"><b>✗ 失败</b>（${r.ms}ms）：${esc(r.error || '')}</div>`}
        ${r.result !== undefined && r.result !== null ? `<div class="help" style="margin:6px 0 2px">插件返回的 result</div><pre class="code" style="max-height:240px">${j(r.result)}</pre>` : ''}
        ${r.stderr ? `<div class="help" style="margin:6px 0 2px">标准错误输出（stderr）</div><pre class="code" style="max-height:160px">${esc(r.stderr)}</pre>` : ''}
        <details style="margin-top:6px"><summary class="help" style="cursor:pointer;margin:0">查看发给插件的请求</summary><pre class="code" style="max-height:320px">${j(r.request)}</pre></details></div>`;
    }
    function draw() {
      const ps = data.plugins;
      const on = ps.filter(p => p.enabled).length, bad = ps.filter(p => p.error).length;
      $('#pStat').innerHTML = `<span class="badge ${on ? 'ok' : 'unknown'}">已启用 ${on} / ${ps.length}</span>${bad ? ` <span class="badge down">${bad} 个清单无效</span>` : ''}`;
      $('#pDir').textContent = data.dir || '-';
      $('#pList').innerHTML = ps.length ? ps.map(pluginPanel).join('')
        : `<div class="panel"><div class="empty">还没有安装插件。把插件文件夹放进上面的目录后点「重新扫描」；想自己写一个，请看「开发指南」。</div></div>`;
      $('#pRuns').innerHTML = data.runs.length ? data.runs.map(r => `<tr><td class="mono">${esc(fmtTime(r.time))}</td><td>${esc(r.plugin)}</td>
        <td>${esc(hookLabel(r.hook))}${r.tool ? ' ' + esc(r.tool) : ''}${r.test ? ' <span class="badge unknown">测试</span>' : ''}</td>
        <td>${r.ok ? '<span class="badge ok">成功</span>' : '<span class="badge down">失败</span>'}</td><td class="mono">${r.ms}ms</td>
        <td class="wrap">${esc(r.ok ? (r.message || '') : (r.error || ''))}</td></tr>`).join('')
        : '<tr><td colspan="6" class="muted" style="text-align:center;padding:16px">暂无调用记录</td></tr>';
      $$('[data-toggle]', app).forEach(b => b.onclick = async () => {
        const p = ps.find(x => x.id === b.dataset.toggle);
        if (!p.enabled && !confirm(`启用插件「${p.name}」？\n\n启用后它会以 网脉 NetPulse 的权限在这台电脑上运行：\n${p.command.join(' ')}\n\n请确认插件来源可信。`)) return;
        try {
          data = await api('PUT', 'api/plugins/' + encodeURIComponent(p.id), { enabled: !p.enabled });
          toast(p.enabled ? `已停用「${p.name}」` : `已启用「${p.name}」`); draw();
        } catch (e) { toast(errMsg(e), true); }
      });
      $$('form[data-settings]', app).forEach(f => f.onsubmit = async ev => {
        ev.preventDefault();
        const p = ps.find(x => x.id === f.dataset.settings), settings = {};
        for (const s of p.settings) {
          const el = f.elements['s_' + s.key];
          if (s.type === 'bool') settings[s.key] = el.checked;
          else if (s.type === 'number') settings[s.key] = el.value.trim() === '' ? null : Number(el.value);
          else settings[s.key] = el.value;
        }
        try { data = await api('PUT', 'api/plugins/' + encodeURIComponent(p.id), { settings }); toast('设置已保存，下次调用生效'); draw(); }
        catch (e) { toast(errMsg(e), true); }
      });
      $$('form[data-test]', app).forEach(f => {
        const sync = () => { f.elements.args.style.display = f.elements.what.value.startsWith('tool:') ? '' : 'none'; };
        f.elements.what.onchange = sync; sync();
        f.onsubmit = async ev => {
          ev.preventDefault();
          const id = f.dataset.test, [kind, name] = f.elements.what.value.split(':');
          const body = kind === 'tool' ? { tool: name, arguments: {} } : { hook: name };
          if (kind === 'tool' && f.elements.args.value.trim()) {
            try { body.arguments = JSON.parse(f.elements.args.value); } catch (e) { toast('工具参数不是有效的 JSON', true); return; }
          }
          const out = $(`[data-out="${id}"]`, f);
          out.innerHTML = '<div class="help" style="margin:8px 0 0">运行中…</div>';
          try {
            outputs[id] = testOutput(await api('POST', 'api/plugins/' + encodeURIComponent(id) + '/test', body));
            const keep = { what: f.elements.what.value, args: f.elements.args.value };
            await load();
            const nf = $(`form[data-test="${id}"]`, app);
            if (nf) { nf.elements.what.value = keep.what; nf.elements.args.value = keep.args; nf.elements.what.onchange(); }
          } catch (e) { out.innerHTML = `<div class="v-down" style="margin-top:8px">${esc(errMsg(e))}</div>`; }
        };
      });
    }
    async function load() { data = await api('GET', 'api/plugins'); draw(); }
    $('#pReload').onclick = async () => {
      try { data = await api('POST', 'api/plugins/reload'); draw(); toast(`已重新扫描，共 ${data.plugins.length} 个插件`); }
      catch (e) { toast(errMsg(e), true); }
    };
    load().catch(e => toast(errMsg(e), true));
    return {};
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

          <div class="panel"><div class="ph"><span class="no">2</span>全局 · 访问设置</div><div class="pb"><div class="help" style="margin-top:0">MCP 开关、只读模式与访问令牌为全局设置；AI 工具读写作用在当前激活机房。</div><form id="mForm"></form></div></div>

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
        <div><b>自主发现</b><ul>
          <li>帮我找找网络里还有哪些没登记的设备，识别一下分别是什么、接在哪个口（使用提示词 discover_devices）</li>
          <li>只读探测 192.168.2.0/24，看看有没有摄像头和打印机，先别添加</li>
          <li>识别一下 192.168.50.77 是什么设备，开了哪些服务</li>
          <li>看看有没有私接的路由器或无线 AP</li>
        </ul></div>
        <div><b>节点维护</b><ul>
          <li>把 192.168.50.19 的接线位置改成接入交换机 Port 5，位置填 A03 机柜</li>
          <li>新增一台服务器 192.168.50.60，名称“备份存储”，接在核心 Gi0/7，开启告警</li>
          <li>给网关和两台交换机开启告警，其余全部关闭</li>
          <li>工作日 8 点到 20 点监控，其余时间暂停</li>
          <li>扫描一下网段，有哪些新设备、哪些设备换了 IP？把新发现的打印机放到接入交换机 Port 7</li>
          <li>新建一个自动扫描任务“办公区”，每 15 分钟扫描 192.168.10.0/24 和 192.168.11.0/24，新设备打上“办公区”标签，先立即扫一次</li>
          <li>把告警通知的重复提醒改成 60 分钟，然后发一条测试消息</li>
          <li>192.168.50.19 这台电脑实际插在哪个口？和登记的一致吗？不一致就帮我改过来</li>
          <li>接入交换机 12 口后面都接了哪些设备？</li>
          <li>先预览一次自动定位，把未定位的设备都挂到对应端口，端口变了的列给我确认</li>
        </ul></div>
        <div><b>迁移机房</b><ul>
          <li>这是新机房的 MAC 表和 ARP 表（附上文本），帮我生成拓扑并导入（使用提示词 generate_topology）</li>
          <li>先 dry run 看看和现在的拓扑有哪些差异，我确认后再写入</li>
        </ul></div>
      </div></div></div>

      <div class="panel" id="mCallsPanel"><div class="ph">工具调用日志<span class="r btns" style="margin:0"><button class="btn small" id="mcRefresh">刷新</button></span></div><div class="pb">
        <div class="help" style="margin-top:0">记录网页 AI（诊断、自检、报告、一键优化、自主发现）和外部 MCP 客户端发起的每一次工具调用：关联 ID、机房、工具、完整参数与返回、错误、开始时间和耗时。
          Webhook 地址、密钥、令牌、SNMP 团体名等凭据写入前已脱敏；单条记录不截断，日志按天保存于 <code>data/mcp_calls/</code>，保留 <span id="mcKeep">30</span> 天。</div>
        <form class="mc-filter" id="mcFilter">
          <select name="profile"><option value="current">当前机房</option><option value="all">全部机房</option></select>
          <select name="source"><option value="">全部来源</option><option value="ai">网页 AI</option><option value="mcp">外部 MCP</option></select>
          <select name="status"><option value="">全部状态</option><option value="ok">成功</option><option value="error">失败</option><option value="denied">已拒绝</option></select>
          <input name="tool" placeholder="工具名">
          <input name="run" placeholder="关联 ID（run-…）">
          <input name="q" placeholder="关键字（参数、错误）">
          <button class="btn small primary" type="submit">筛选</button>
        </form>
        <div class="table-wrap"><table class="grid flat mc-table"><thead><tr><th>时间</th><th>机房</th><th>来源</th><th>工具</th><th>状态</th><th class="num">耗时</th><th class="num">返回</th><th>关联 ID</th></tr></thead><tbody id="mcRows"><tr><td colspan="8" class="muted">正在加载…</td></tr></tbody></table></div>
        <div class="btns"><button class="btn small hidden" id="mcMore">加载更早的记录</button></div>
        <div id="mcDetail"></div>
      </div></div>

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
        <li>所有 MCP 写操作都会记录到 <code>logs/netping.log</code>（含来源 IP），删除和整体替换类工具会提示 AI 先征得你的确认；每次工具调用的完整参数与返回见上方「工具调用日志」。</li>
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
        ${local && tab !== 'claude' && !workbuddy ? '<div class="help">请把 &lt;服务器IP&gt; 替换为运行 网脉 NetPulse 的电脑的 IP；AI 客户端就在本机时也可以用 127.0.0.1。</div>' : ''}`;
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
      const kind = t => (t.plugin ? '<span class="badge unknown">插件</span> ' : '') + (t.destructive ? '<span class="badge down">修改·需确认</span>' : t.write ? '<span class="badge slow">修改</span>' : t.probe ? '<span class="badge ok" title="从 网脉 NetPulse 主机实时发包探测，只读">查询·实时探测</span>' : '<span class="badge ok">查询</span>');
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
        const init = await rpcCall('initialize', { protocolVersion: info.protocol, capabilities: {}, clientInfo: { name: '网脉 NetPulse 网页测试', version: '1' } }, 1);
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

    // ---- 工具调用日志 ----
    const SRC_TXT = { ai_diagnose: 'AI 诊断', ai_report: 'AI 报告', ai_optimize: 'AI 优化', ai_discover: 'AI 发现', ai: '网页 AI', mcp: '外部 MCP' };
    const ST_TXT = { ok: ['成功', 'ok'], error: ['失败', 'down'], denied: ['已拒绝', 'slow'] };
    const fmtBytes = b => b == null ? '-' : b >= 1048576 ? (b / 1048576).toFixed(1) + ' MB' : b >= 1024 ? (b / 1024).toFixed(1) + ' KB' : b + ' B';
    let calls = [], callsMore = false;
    const presetRun = sessionStorage.getItem('np_mcp_run');
    if (presetRun) { sessionStorage.removeItem('np_mcp_run'); $('#mcFilter').elements.run.value = presetRun; setTimeout(() => $('#mCallsPanel').scrollIntoView({ block: 'start' }), 50); }
    function callsQuery(after) {
      const f = $('#mcFilter').elements, q = new URLSearchParams();
      for (const k of ['profile', 'source', 'status', 'tool', 'run', 'q']) { const v = f[k].value.trim(); if (v) q.set(k, v); }
      q.set('limit', '50');
      if (after) q.set('after', after);
      return 'api/mcp/calls?' + q.toString();
    }
    function drawCalls() {
      $('#mcRows').innerHTML = calls.map(c => {
        const [st, cls] = ST_TXT[c.status] || [c.status, 'unknown'];
        return `<tr data-call="${esc(c.id)}"><td class="mono">${esc(Charts.fmtFull(c.started_at))}</td><td>${esc(c.profile_name || c.profile_id)}</td>
          <td>${esc(SRC_TXT[c.source] || c.source)}${c.phase === 'review' ? ' · 自检' : ''}</td>
          <td class="mono" title="${esc(c.args_preview || '')}"><b>${esc(c.tool)}</b>${c.title ? `<br><span class="muted" style="font-family:var(--sans)">${esc(c.title)}</span>` : ''}</td>
          <td><span class="badge ${cls}">${esc(st)}</span>${c.model_truncated ? ' <span class="badge unknown" title="模型收到的是按长度上限截断的结果，日志中保存了完整结果">模型侧截断</span>' : ''}</td>
          <td class="num">${c.duration_ms} ms</td><td class="num">${fmtBytes(c.result_bytes)}</td><td class="mono">${esc(c.run_id || c.rpc_id || '-')}</td></tr>`;
      }).join('') || '<tr><td colspan="8" class="muted">没有符合条件的调用记录</td></tr>';
      $('#mcMore').classList.toggle('hidden', !callsMore);
    }
    async function loadCalls(append) {
      const after = append && calls.length ? calls[calls.length - 1].id : '';
      try {
        const d = await api('GET', callsQuery(after));
        calls = append ? calls.concat(d.items || []) : (d.items || []);
        callsMore = !!d.more;
        if (d.retention_days) $('#mcKeep').textContent = d.retention_days;
        drawCalls();
      } catch (e) { $('#mcRows').innerHTML = `<tr><td colspan="8" class="v-down">读取失败：${esc(errMsg(e))}</td></tr>`; }
    }
    const pretty = t => { try { return JSON.stringify(JSON.parse(t), null, 2); } catch (e) { return t || ''; } };
    async function showCall(id) {
      const box = $('#mcDetail');
      box.innerHTML = '<p class="muted">正在读取完整记录…</p>';
      try {
        const c = await api('GET', 'api/mcp/calls/' + encodeURIComponent(id));
        const [st, cls] = ST_TXT[c.status] || [c.status, 'unknown'];
        const kv = (k, v) => v ? `<div class="kv"><span>${k}</span><code>${esc(String(v))}</code></div>` : '';
        box.innerHTML = `<div class="mc-detail">
          <div class="room-preview-head"><h3>${esc(c.tool)} <span class="badge ${cls}">${esc(st)}</span></h3>
            <div class="btns" style="margin:0"><a class="btn small" href="api/mcp/calls/${encodeURIComponent(c.id)}/download" download>下载完整记录</a><button class="btn small" id="mcClose">关闭</button></div></div>
          ${kv('调用 ID', c.id)}${kv('关联 ID', c.run_id)}${kv('模型 tool_call_id', c.tool_call_id)}${kv('JSON-RPC id', c.rpc_id)}
          ${kv('来源', (SRC_TXT[c.source] || c.source) + (c.phase === 'review' ? ' · 自检' : ''))}${kv('客户端', c.remote)}
          ${kv('机房', (c.profile_name || '') + ' (' + c.profile_id + ')')}${kv('开始时间', Charts.fmtFull(c.started_at))}${kv('耗时', c.duration_ms + ' ms')}
          ${c.redacted ? '<div class="help">部分凭据字段已脱敏（显示为 ***已脱敏***）。</div>' : ''}
          ${c.model_truncated ? '<div class="help v-warn">模型实际收到的结果因长度上限被截断；下面是完整结果。</div>' : ''}
          ${c.error ? `<div class="ai-err">错误：${esc(c.error)}</div>` : ''}
          <div class="ai-think-label">参数（${fmtBytes((c.args || '').length)}）</div><pre class="ai-code mc-pre">${esc(pretty(c.args))}</pre>
          <div class="ai-think-label">返回（${fmtBytes(c.result_bytes)}）</div><pre class="ai-code mc-pre">${esc(pretty(c.result)) || '<span class="muted">（无）</span>'}</pre></div>`;
        $('#mcClose').onclick = () => { box.innerHTML = ''; };
      } catch (e) { box.innerHTML = `<p class="v-down">读取失败：${esc(errMsg(e))}</p>`; }
    }
    $('#mcFilter').onsubmit = ev => { ev.preventDefault(); $('#mcDetail').innerHTML = ''; loadCalls(false); };
    $('#mcRefresh').onclick = () => loadCalls(false);
    $('#mcMore').onclick = () => loadCalls(true);
    $('#mcRows').onclick = ev => { const tr = ev.target.closest('tr[data-call]'); if (tr) showCall(tr.dataset.call); };
    loadCalls(false);
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
        <div class="panel"><div class="ph">✦ AI 自主发现</div><div class="pb"><form id="dAi">
          <div class="help" style="margin-top:0">让 AI 自己找设备：先汇总本机网卡、ARP 表、已有设备和扫描任务等线索，决定探测哪些网段；
            再识别新设备是什么（端口、网页标题、SNMP 描述、主机名、厂商），能定位的查出接在哪个交换机端口；最后给出<b>新增设备</b>、<b>新增扫描任务</b>和<b>其他发现</b>，勾选确认后才添加。</div>
          <label class="f"><span>要求（可空）</span><input type="text" name="hint" placeholder="如：重点找二楼的摄像头和打印机；看看有没有私接的路由器"></label>
          <div class="row2">
            <label class="f"><span>指定网段（可空，留空由 AI 决定）</span><input type="text" name="subnets" class="mono" placeholder="192.168.2.0/24, 192.168.3.0/24"></label>
            <label class="f"><span>探测范围</span><select name="budget">
              <option value="4">小：最多约 4 个 /24</option><option value="8" selected>中：最多约 8 个 /24</option><option value="16">大：最多约 16 个 /24</option></select></label>
          </div>
          <div class="btns"><button class="btn accent" type="submit">开始自主发现</button><button class="btn hidden" type="button" id="dAiLast">查看上次结果</button></div>
          <div class="help">只探测私有地址和已配置的网段；探测过程不修改任何配置。需要先在「AI 诊断」页设置模型，通常需要 1~5 分钟。</div>
        </form></div></div>
        <div class="panel"><div class="ph">自动扫描任务 · 当前机房<span class="r" style="display:flex;gap:10px;align-items:center">
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
        <div class="panel"><div class="ph">默认扫描设置 · 当前机房</div><div class="pb"><form id="df"></form></div></div>
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
          <label><input type="checkbox" name="notify" ${d.notify ? 'checked' : ''}> 新增设备 / IP 变化推送通知</label>
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
    // ---- AI 自主发现 ----
    let aid = null; // { el, busy, onKey }
    const AID_KEY = 'np_aidisc';
    const confTxt = { high: ['ok', '把握高'], medium: ['slow', '把握中'], low: ['unknown', '把握低'] };
    function closeAid() {
      if (!aid) return;
      if (aid.busy) aid.busy.abort();
      aid.el.remove();
      document.removeEventListener('keydown', aid.onKey, true);
      aid = null;
    }
    function openAid() {
      if (aid) return;
      const el = document.createElement('div');
      el.className = 'modal-mask';
      el.innerHTML = `<div class="modal aiopt"><div class="mh"><b>✦ AI 自主发现</b><span class="muted" id="aidMeta"></span>
        <button class="btn small r" id="aidClose">关闭</button></div><div class="mb" id="aidBody"></div><div class="mf" id="aidFoot"></div></div>`;
      document.body.appendChild(el);
      el.addEventListener('mousedown', ev => { if (ev.target === el && !(aid && aid.busy)) closeAid(); });
      const onKey = ev => { if (ev.key === 'Escape') { ev.stopPropagation(); closeAid(); } };
      document.addEventListener('keydown', onKey, true);
      aid = { el, onKey };
      $('#aidClose').onclick = closeAid;
    }
    function lastAid() { try { return JSON.parse(sessionStorage.getItem(AID_KEY) || 'null'); } catch (e) { return null; } }
    function syncLastBtn() { $('#dAiLast').classList.toggle('hidden', !lastAid()); }
    async function runAid(req) {
      openAid();
      const body = $('#aidBody'), foot = $('#aidFoot');
      const st = { status: '正在连接…', tools: [], text: '', error: '', think: thinkNew() };
      const draw = () => {
        if (!aid) return;
        const pre = st.text.split('```')[0].trim();
        body.innerHTML = `${req.hint || (req.subnets || []).length ? `<div class="help" style="margin-top:0">${req.hint ? '要求：' + esc(req.hint) : ''}
            ${(req.subnets || []).length ? ' 指定网段：<span class="mono">' + esc(req.subnets.join('、')) + '</span>' : ''}</div>` : ''}
          ${thinkHTML(st.think)}
          ${pre ? `<div class="ai-md">${mdLite(pre)}</div>` : ''}
          ${st.text.includes('```') && !st.error ? '<div class="ai-status"><span class="ai-spin"></span>正在生成发现清单…</div>' : ''}
          ${st.status ? `<div class="ai-status"><span class="ai-spin"></span>${esc(st.status)}</div>` : ''}
          ${st.error ? `<div class="ai-err">${esc(st.error)}</div>` : ''}`;
      };
      foot.innerHTML = `<button class="btn" id="aidStop">停止</button>`;
      $('#aidMeta').textContent = '';
      draw();
      const ctl = new AbortController();
      aid.busy = ctl;
      $('#aidStop').onclick = () => ctl.abort();
      let result = null, meta = '';
      // 工具参数里的网段 / 地址，显示在进度标签上
      const shortArgs = a => { try { const o = JSON.parse(a || '{}'); return [].concat(o.subnets || [], o.ips || [], o.ip || [], o.mac || [], o.switch || []).join(' ').slice(0, 60); } catch (e) { return ''; } };
      try {
        await streamNDJSON('api/ai/discover', req, ctl.signal, ev => {
          thinkEvent(st.think, ev);
          if (ev.t === 'status') st.status = ev.text;
          else if (ev.t === 'stats') st.status = `已汇总网络线索：${ev.stats.candidates} 个候选网段，已登记 ${ev.stats.nodes} 个节点、${ev.stats.switches} 台交换机，正在请求模型…`;
          else if (ev.t === 'delta') { st.text += ev.text; st.status = ''; }
          else if (ev.t === 'break') st.text = '';
          else if (ev.t === 'tool') {
            ev.short = shortArgs(ev.args);
            st.tools.push(ev);
            st.status = (ev.name === 'probe_subnet' ? '正在探测网段 ' : ev.name === 'fingerprint_host' ? '正在识别设备 ' : '正在查询 ' + (ev.title || ev.name) + ' ') + (ev.short || '') + '…';
          }
          else if (ev.t === 'error') { st.error = ev.error; st.status = ''; }
          else if (ev.t === 'result') result = ev.result;
          else if (ev.t === 'done') meta = `${ev.model} · ${(ev.ms / 1000).toFixed(1)} 秒 · 工具 ${ev.tools} 次 · ${(ev.usage || {}).total_tokens || 0} tokens`;
          draw();
        });
        if (!result && !st.error) st.error = '模型没有返回结果，请重试。';
        thinkFinish(st.think, result ? 'done' : 'error', st.error);
      } catch (e) {
        st.error = e.name === 'AbortError' ? '已停止' : errMsg(e);
        thinkFinish(st.think, e.name === 'AbortError' ? 'stopped' : 'error', e.name === 'AbortError' ? '' : st.error);
      } finally {
        if (aid) aid.busy = null;
      }
      if (!aid) return;
      if (result) {
        result.meta = meta;
        result.req = req;
        sessionStorage.setItem(AID_KEY, JSON.stringify(result));
        syncLastBtn();
        drawAid(result);
        load(true);
      } else {
        st.status = '';
        draw();
        foot.innerHTML = `<button class="btn primary" id="aidRetry">重试</button>`;
        $('#aidRetry').onclick = () => runAid(req);
      }
    }
    function drawAid(res) {
      openAid();
      const body = $('#aidBody'), foot = $('#aidFoot');
      $('#aidMeta').textContent = `  ${res.time || ''}${res.meta ? ' · ' + res.meta : ''}`;
      const devs = res.devices || [], jobs = res.scan_jobs || [], notes = res.notes || [], dropped = res.dropped || [];
      const swName = id => { const w = S.topo && S.topo.switches.find(x => x.id === id); return w ? (w.name || w.id) : id; };
      const cats = res.categories || [];
      body.innerHTML = `
        ${res.summary ? `<div class="ai-md">${mdLite(res.summary)}</div>` : ''}
        <div class="opt-sec"><h4>1 · 新增设备 <span class="muted">${devs.length} 台 · 名称和类别可修改</span>
          ${devs.length ? '<label class="r"><input type="checkbox" id="aidAllD" checked> 全选</label>' : ''}</h4>
          ${devs.length ? `<div style="overflow:auto"><table class="grid flat opt-tbl"><thead><tr><th></th><th>地址</th><th>名称 / 类别</th><th>依据</th><th>接线</th><th>告警</th></tr></thead><tbody>${devs.map((d, i) => `<tr>
            <td style="width:24px"><input type="checkbox" class="aidD" data-i="${i}" checked></td>
            <td class="mono" style="white-space:nowrap">${esc(d.ip)}<div class="muted">${esc(d.mac || '无 MAC')}</div></td>
            <td style="min-width:180px"><input type="text" class="aidN" data-i="${i}" value="${esc(d.name || '')}" style="margin-bottom:4px">
              ${d.kind === 'switch' ? `<span class="badge solid">${esc(TopoTypes.info[d.type || 'switch'] ? TopoTypes.info[d.type || 'switch'].name : '交换机')}</span>` : `<select class="aidC" data-i="${i}"><option value="">（不分类）</option>${cats.map(c => `<option ${c === d.category ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select>`}</td>
            <td class="wrap"><span class="badge ${(confTxt[d.confidence] || confTxt.medium)[0]}">${(confTxt[d.confidence] || confTxt.medium)[1]}</span>
              ${esc([d.vendor, d.hostname].filter(Boolean).join(' · '))}<div class="muted">${esc(d.evidence || '')}</div>
              ${d.model ? `<div class="muted mono" style="font-size:11px">${esc(d.model)}</div>` : ''}</td>
            <td style="white-space:nowrap">${d.switch ? esc(d.attach || swName(d.switch) + ' 口 ' + d.port) : '<span class="muted">未定位</span>'}</td>
            <td><input type="checkbox" class="aidA" data-i="${i}" ${d.alert ? 'checked' : ''} title="推送告警通知"></td></tr>`).join('')}</tbody></table></div>`
            : '<div class="help">没有发现需要添加的新设备。</div>'}
          ${dropped.length ? `<details class="help"><summary>已忽略 ${dropped.length} 条不可信或重复的建议</summary>${dropped.map(x => `<div><span class="mono">${esc(x.ip)}</span> ${esc(x.reason)}</div>`).join('')}</details>` : ''}
        </div>
        <div class="opt-sec"><h4>2 · 新增扫描任务 <span class="muted">${jobs.length} 个 · 之后定时扫描、自动跟踪 IP 变化</span></h4>
          ${jobs.length ? jobs.map((j, i) => `<label class="opt-chk" style="display:block"><input type="checkbox" class="aidJ" data-i="${i}" checked>
            <b>${esc(j.name)}</b> <span class="mono">${esc(j.subnets.join('、'))}</span> <span class="muted">每 ${j.interval_min} 分钟 · ${j.auto_add ? '自动添加新设备' : '只列出新设备'}</span>
            <div class="muted">${esc(j.reason || '')}</div></label>`).join('') : '<div class="help">现有扫描任务已覆盖发现的网段。</div>'}
        </div>
        <div class="opt-sec"><h4>3 · 其他发现 <span class="muted">${notes.length} 条</span></h4>
          ${notes.length ? `<ul style="margin:0;padding-left:20px;line-height:1.8">${notes.map(n => `<li>${esc(n)}</li>`).join('')}</ul>` : '<div class="help">无。</div>'}
        </div>`;
      const count = () => $$('.aidD:checked', body).length + $$('.aidJ:checked', body).length;
      const upd = () => {
        const b = $('#aidApply'), n = count();
        if (b) { b.disabled = !n; b.textContent = `添加所选（${n}）`; }
        const all = $('#aidAllD');
        if (all) { const xs = $$('.aidD', body); all.checked = xs.length > 0 && xs.every(x => x.checked); }
      };
      foot.innerHTML = `<button class="btn primary" id="aidApply">添加所选</button><button class="btn" id="aidRerun">重新发现</button>
        <span class="muted" style="margin-left:auto">添加后可在提示中撤销</span>`;
      $$('.aidD, .aidJ', body).forEach(x => x.onchange = upd);
      const allD = $('#aidAllD');
      if (allD) allD.onchange = () => { $$('.aidD', body).forEach(x => { x.checked = allD.checked; }); upd(); };
      $('#aidRerun').onclick = () => runAid(res.req || {});
      $('#aidApply').onclick = async () => {
        const pick = $$('.aidD:checked', body).map(x => {
          const i = +x.dataset.i, c = $(`.aidC[data-i="${i}"]`, body);
          return Object.assign({}, devs[i], { name: $(`.aidN[data-i="${i}"]`, body).value.trim(), category: c ? c.value : '', alert: $(`.aidA[data-i="${i}"]`, body).checked });
        });
        const pickJ = $$('.aidJ:checked', body).map(x => jobs[+x.dataset.i]);
        try {
          const r = await api('POST', 'api/ai/discover/apply', { devices: pick, scan_jobs: pickJ });
          const doneIP = new Set(pick.map(d => d.ip));
          res.devices = devs.filter(d => !doneIP.has(d.ip));
          res.scan_jobs = jobs.filter(j => !pickJ.includes(j));
          sessionStorage.setItem(AID_KEY, JSON.stringify(res));
          await loadTopology(); await loadCfg(); await load(true);
          if (aid) drawAid(res);
          const parts = [];
          if (r.nodes) parts.push(`${r.nodes} 台设备${r.located ? `（${r.located} 台已定位到端口）` : '（未定位）'}`);
          if (r.switches) parts.push(`${r.switches} 台交换机`);
          if (r.jobs) parts.push(`${r.jobs} 个扫描任务${r.discovery_enabled ? '' : '（自动扫描总开关未开）'}`);
          const sk = (r.skipped || []).length;
          const u = r.undo || {};
          toast(`已添加 ${parts.join('、') || '0 项'}${sk ? `，${sk} 条已失效被跳过` : ''}`, false,
            (u.nodes || []).length + (u.switches || []).length + (u.jobs || []).length ? {
              label: '撤销', fn: async () => {
                try {
                  await api('POST', 'api/ai/discover/apply', { undo: u });
                  const cur = lastAid(); // 撤销的建议放回清单
                  if (cur) { cur.devices = pick.concat(cur.devices || []); cur.scan_jobs = pickJ.concat(cur.scan_jobs || []); sessionStorage.setItem(AID_KEY, JSON.stringify(cur)); }
                  await loadTopology(); await loadCfg(); await load(true);
                  toast('已撤销本次添加（可点「查看上次结果」重新选择）');
                } catch (e) { toast(errMsg(e), true); }
              },
            } : null);
        } catch (e) { toast(errMsg(e), true); }
      };
      upd();
    }
    $('#dAi').onsubmit = ev => {
      ev.preventDefault();
      const e = ev.target.elements;
      runAid({ hint: e.hint.value.trim(), subnets: split(e.subnets.value), budget: +e.budget.value });
    };
    $('#dAiLast').onclick = () => { const r = lastAid(); if (r) drawAid(r); };
    syncLastBtn();

    loadCfg().catch(e => toast(errMsg(e), true));
    load();
    return { destroy() { clearTimeout(timer); closeAid(); } };
  }

  // ---------------------------------------------------------------------------
  // 端口定位页：SNMP 读取交换机 MAC 地址表，自动确定设备接在哪个端口
  let closeTerminal = null;
  function openTerminal(id, protocol = 'telnet', readHost = '', node = false) {
    if (closeTerminal) closeTerminal();
    const isSSH = protocol === 'ssh', title = isSSH ? 'SSH' : 'Telnet';
    const sw = (node ? S.topo.nodes : S.topo.switches).find(s => s.id === id);
    if (!sw) return;
    readHost = String(readHost || sw.ip || '').trim();
    const host = readHost.replace(/^(\[[^\]]+\]|[^:]+):[^:]*$/, '$1');
    if (!host) { toast('请先保存该设备的读取地址或管理 IP', true); return; }
    const dialog = document.createElement('dialog');
    dialog.className = 'telnet-dialog' + (isSSH ? ' ssh-dialog' : '');
    dialog.innerHTML = `<div class="ph"><b>${esc(sw.name || id)} · ${title}</b><button class="btn small" data-close>关闭</button></div>
      <div class="pb"><form class="telnet-toolbar"><span class="mono telnet-host">${esc(host)}</span>
        <label>端口 <input name="port" type="number" min="1" max="65535" value="${isSSH ? 22 : 23}" required aria-label="${title} 端口"></label>
        ${isSSH ? `<label>用户名 <input name="username" class="ssh-username" required maxlength="256" autocomplete="off"></label>
        <label>密码 <input name="password" class="ssh-password" type="password" autocomplete="new-password" maxlength="4096"></label>` : ''}
        <button class="btn primary" type="submit">连接</button><button class="btn" type="button" data-disconnect disabled>断开</button>
        <span role="status" data-state>未连接</span></form>
        <div class="help">由 网脉 NetPulse 服务器连接设备；${isSSH ? '先填写用户名和密码，首次连接需核对主机指纹。' : '请在终端输入账号和密码。Telnet 使用明文传输。'}账号密码不保存在 网脉 NetPulse。配置修改会直接作用于设备，按设备要求保存配置。</div>
        ${isSSH ? `<div class="ssh-hostkey hidden" data-hostkey><b>首次连接：请核对设备主机指纹</b><div class="mono" data-fingerprint></div>
        <button type="button" class="btn primary" data-trust>信任此主机并继续</button> <button type="button" class="btn" data-reject>取消连接</button></div>` : ''}
        <div class="telnet-screen" aria-label="设备交互终端"></div>
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
      const url = new URL(`${node ? 'api/ssh/nodes' : 'api/locate/' + protocol}/${encodeURIComponent(id)}`, document.baseURI);
      url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
      url.searchParams.set('port', port.value);
      const ws = new WebSocket(url); socket = ws; ws.binaryType = 'arraybuffer';
      setState('连接中…', true);
      const endpoint = `${host.includes(':') && !host.startsWith('[') ? `[${host}]` : host}:${port.value}`;
      term.writeln(`[正在连接 ${endpoint}…]`);
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
      ws.onerror = () => { term.writeln(`\r\n[连接失败：请检查网页登录状态、已保存的地址、${title} 端口及服务器到设备的网络，然后点击连接重试。]`); };
      ws.onclose = () => { password = ''; stop(); term.writeln('\r\n[连接已关闭]'); };
    };
    fit.fit();
    if (isSSH) form.elements.username.focus(); else form.requestSubmit();
  }


  function locatePage(app) {
    let data = null, cfg = null, tableSw = '', busy = false;
    let alive = true;
    app.innerHTML = `<div class="page pad"><div class="settings">
      <div>
        <div class="panel"><div class="ph">端口定位<span class="r" id="lState"></span></div><div class="pb">
          <div class="help" style="margin-top:0">通过 SNMP 只读读取交换机的 MAC 地址表，按设备 MAC 找到它实际插在哪个端口：
            <b>“未定位”的设备自动挂到该端口</b>（并按端口位置对照填入物理位置）；已定位但端口变了的设备列在右侧「待确认」，确认后再改。</div>
          <div class="btns"><button class="btn accent" id="lRun">立即定位</button><button class="btn" id="lDry">只预览</button></div>
          <div id="lSum" style="margin-top:10px"></div>
        </div></div>
        <div class="panel"><div class="ph">默认 SNMP 设置 · 当前机房</div><div class="pb"><form id="lf"></form></div></div>
        <div class="panel"><div class="ph">交换机上需要的配置（参考）</div><div class="pb">
          <div class="help" style="margin-top:0">自动定位只需开启 SNMP v2c <b>只读</b>团体名，读取标准 BRIDGE-MIB / IF-MIB。需要修改交换机配置时，点右侧「Telnet」手动登录终端。建议限制只允许 网脉 NetPulse 服务器访问。</div>
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
          <div class="pb table-wrap" style="padding:0"><table class="grid"><thead><tr><th>交换机</th><th>读取地址</th><th>团体名</th><th>读取</th><th>状态</th><th></th></tr></thead><tbody id="lSw"></tbody></table></div>
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
          <label><input type="checkbox" name="notify" ${l.notify ? 'checked' : ''}> 端口变化推送通知</label></div>
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
      if (b.dataset.ssh) { openTerminal(b.dataset.ssh, 'ssh', ((cfg.locate.switches || {})[b.dataset.ssh] || {}).host); return; }
      if (b.dataset.telnet) { openTerminal(b.dataset.telnet, 'telnet', ((cfg.locate.switches || {})[b.dataset.telnet] || {}).host); return; }
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
