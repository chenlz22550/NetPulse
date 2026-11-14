/* 轻量 SVG 图表：延迟折线（含最小/最大区间）、丢包柱状图、迷你折线。无第三方依赖。 */
(function () {
  'use strict';
  const NS = 'http://www.w3.org/2000/svg';
  const pad2 = n => String(n).padStart(2, '0');
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

  function fmtTime(ms, withDate) {
    const d = new Date(ms);
    const hm = pad2(d.getHours()) + ':' + pad2(d.getMinutes());
    return withDate ? pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + hm : hm;
  }
  function fmtFull(ms) {
    const d = new Date(ms);
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' +
      pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }
  function fmtMs(v) {
    if (v == null) return '-';
    if (v < 0) return '超时';
    return v < 10 ? v.toFixed(2) + 'ms' : v.toFixed(v < 100 ? 1 : 0) + 'ms';
  }

  const STEPS = [60e3, 300e3, 600e3, 900e3, 1800e3, 3600e3, 7200e3, 10800e3, 21600e3, 43200e3, 86400e3, 172800e3, 604800e3];
  function timeTicks(from, to, want) {
    const span = to - from;
    let step = STEPS[STEPS.length - 1];
    for (const s of STEPS) { if (span / s <= want) { step = s; break; } }
    const tzOff = new Date().getTimezoneOffset() * 60e3;
    const out = [];
    let t = Math.ceil((from - tzOff) / step) * step + tzOff;
    for (; t <= to; t += step) out.push(t);
    return { ticks: out, withDate: span > 86400e3 || step >= 86400e3 };
  }
  function niceMax(v) {
    if (v <= 0) return 1;
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
    return 10 * p;
  }

  // 把点按“间隔是否超过 2 个桶”拆成连续段。
  function segments(points, bucket, key) {
    const segs = [];
    let cur = [];
    let prevT = null;
    for (const p of points) {
      const v = typeof key === 'function' ? key(p) : p[key];
      if (v == null || (prevT != null && p.t - prevT > bucket * 2.5)) {
        if (cur.length) segs.push(cur);
        cur = [];
      }
      if (v != null) cur.push(p);
      prevT = p.t;
    }
    if (cur.length) segs.push(cur);
    return segs;
  }

  function setupHover(wrap, svg, geo, points, fmtTip) {
    const tip = document.createElement('div');
    tip.className = 'tip hidden';
    wrap.appendChild(tip);
    const cross = document.createElementNS(NS, 'line');
    cross.setAttribute('class', 'cross hidden');
    cross.setAttribute('y1', geo.top); cross.setAttribute('y2', geo.top + geo.h);
    svg.appendChild(cross);
    svg.addEventListener('mousemove', ev => {
      if (!points.length) return;
      const r = svg.getBoundingClientRect();
      const x = (ev.clientX - r.left) * (geo.W / r.width);
      if (x < geo.left || x > geo.left + geo.w) { tip.classList.add('hidden'); cross.classList.add('hidden'); return; }
      const t = geo.from + (x - geo.left) / geo.w * (geo.to - geo.from);
      let best = points[0];
      for (const p of points) if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p;
      const bx = geo.x(best.t);
      cross.setAttribute('x1', bx); cross.setAttribute('x2', bx);
      cross.classList.remove('hidden');
      tip.innerHTML = fmtTip(best);
      tip.classList.remove('hidden');
      const px = bx / geo.W * r.width;
      tip.style.left = (px + 12 + tip.offsetWidth > r.width ? px - tip.offsetWidth - 12 : px + 12) + 'px';
      tip.style.top = '6px';
    });
    svg.addEventListener('mouseleave', () => { tip.classList.add('hidden'); cross.classList.add('hidden'); });
  }

  function frame(el, height, from, to, yMax, yFmt) {
    el.innerHTML = '';
    el.classList.add('chart');
    const W = Math.max(320, el.clientWidth || 800);
    const H = height;
    const left = 52, right = 14, top = 10, bottom = 24;
    const w = W - left - right, h = H - top - bottom;
    const geo = {
      W, H, left, top, w, h, from, to,
      x: t => left + (t - from) / (to - from) * w,
      y: v => top + h - Math.min(v, yMax) / yMax * h,
    };
    const svg = document.createElementNS(NS, 'svg');
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    svg.setAttribute('height', H);
    let s = '';
    for (let i = 0; i <= 4; i++) {
      const v = yMax * i / 4, y = geo.y(v);
      s += `<line class="gl" x1="${left}" x2="${left + w}" y1="${y}" y2="${y}"/>`;
      s += `<text class="axis" x="${left - 6}" y="${y + 3}" text-anchor="end">${esc(yFmt(v))}</text>`;
    }
    const tt = timeTicks(from, to, Math.max(3, Math.floor(w / 110)));
    for (const t of tt.ticks) {
      const x = geo.x(t);
      s += `<line class="gl" x1="${x}" x2="${x}" y1="${top}" y2="${top + h}"/>`;
      s += `<text class="axis" x="${x}" y="${H - 6}" text-anchor="middle">${fmtTime(t, tt.withDate)}</text>`;
    }
    svg.innerHTML = s;
    el.appendChild(svg);
    return { svg, geo };
  }

  // 延迟图：avg 折线 + min/max 区间 + 告警阈值线。
  function latency(el, data, warnMs) {
    const pts = data.points || [];
    let mx = 0;
    for (const p of pts) if (p.avg != null) mx = Math.max(mx, p.avg);
    let mxBand = 0;
    for (const p of pts) if (p.max != null) mxBand = Math.max(mxBand, p.max);
    // 区间上限可能被个别尖峰拉得很高，这里限制在平均值最大值的 3 倍以内。
    const yMax = niceMax(Math.max(Math.min(mxBand, mx * 3), mx * 1.15, 1, warnMs && warnMs < mx * 4 ? warnMs * 1.1 : 0));
    const { svg, geo } = frame(el, 240, data.from, data.to, yMax, v => (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + 'ms');
    let s = '';
    for (const seg of segments(pts, data.bucket_ms, 'min')) {
      if (seg.length === 1) continue;
      const up = seg.map(p => `${geo.x(p.t).toFixed(1)},${geo.y(p.max).toFixed(1)}`);
      const dn = seg.slice().reverse().map(p => `${geo.x(p.t).toFixed(1)},${geo.y(p.min).toFixed(1)}`);
      s += `<polygon class="band" points="${up.concat(dn).join(' ')}"/>`;
    }
    for (const seg of segments(pts, data.bucket_ms, 'avg')) {
      if (seg.length === 1) {
        s += `<circle cx="${geo.x(seg[0].t)}" cy="${geo.y(seg[0].avg)}" r="2" fill="#ff5a1f"/>`;
        continue;
      }
      s += `<polyline class="avg" points="${seg.map(p => `${geo.x(p.t).toFixed(1)},${geo.y(p.avg).toFixed(1)}`).join(' ')}"/>`;
    }
    if (warnMs && warnMs <= yMax) {
      const y = geo.y(warnMs);
      s += `<line class="warnl" x1="${geo.left}" x2="${geo.left + geo.w}" y1="${y}" y2="${y}"/>`;
      s += `<text class="axis" x="${geo.left + geo.w - 4}" y="${y - 4}" text-anchor="end" fill="#e08a00">阈值 ${warnMs}ms</text>`;
    }
    svg.insertAdjacentHTML('beforeend', s);
    if (!pts.length) el.insertAdjacentHTML('beforeend', '<div class="empty" style="position:absolute;inset:0;padding-top:90px">该时间范围内没有数据</div>');
    setupHover(el, svg, geo, pts, p =>
      `${fmtFull(p.t)}<br>平均 ${fmtMs(p.avg)}　最小 ${fmtMs(p.min)}　最大 ${fmtMs(p.max)}<br>丢包 ${p.loss}%（${p.sent} 次）`);
  }

  // 丢包图：每个桶一根柱子。
  function loss(el, data) {
    const pts = data.points || [];
    const { svg, geo } = frame(el, 130, data.from, data.to, 100, v => v.toFixed(0) + '%');
    const bw = Math.max(1, geo.w * data.bucket_ms / (data.to - data.from) - 0.5);
    let s = '';
    for (const p of pts) {
      if (!p.loss) continue;
      const y = geo.y(p.loss);
      s += `<rect class="bar${p.loss < 100 ? ' part' : ''}" x="${(geo.x(p.t)).toFixed(1)}" y="${y.toFixed(1)}" width="${bw.toFixed(1)}" height="${(geo.top + geo.h - y).toFixed(1)}"/>`;
    }
    svg.insertAdjacentHTML('beforeend', s);
    setupHover(el, svg, geo, pts, p => `${fmtFull(p.t)}<br>丢包 ${p.loss}%（发送 ${p.sent} 次）`);
  }

  // 迷你折线（返回 SVG 字符串），values 中 <0 表示丢包。
  function spark(values, w, h, warn) {
    w = w || 120; h = h || 22;
    if (!values || !values.length) return `<svg width="${w}" height="${h}"></svg>`;
    let mx = 1;
    for (const v of values) if (v > mx) mx = v;
    mx = niceMax(Math.max(mx, (warn || 0) * 0.5));
    const n = Math.max(values.length, 60);
    const dx = w / (n - 1);
    let s = '', pts = [];
    const off = n - values.length;
    values.forEach((v, i) => {
      const x = (i + off) * dx;
      if (v < 0) {
        s += `<rect x="${(x - dx / 2).toFixed(1)}" y="0" width="${Math.max(1, dx).toFixed(1)}" height="${h}" fill="#d63a22" opacity=".55"/>`;
        if (pts.length > 1) s += `<polyline points="${pts.join(' ')}" fill="none" stroke="#1a1a1a" stroke-width="1"/>`;
        pts = [];
      } else {
        pts.push(`${x.toFixed(1)},${(h - 1 - v / mx * (h - 2)).toFixed(1)}`);
      }
    });
    if (pts.length > 1) s += `<polyline points="${pts.join(' ')}" fill="none" stroke="#1a1a1a" stroke-width="1"/>`;
    return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}">${s}</svg>`;
  }

  window.Charts = { latency, loss, spark, fmtMs, fmtFull, esc, aiBlock, aiChart, setDeviceResolver };

  // ---------------------------------------------------------------------------
  // AI 诊断可视化图表：故障路径图（path）、对比柱状图（bar）、趋势曲线（line）、异常时间线（timeline）及 Mermaid 流程图转换。
  let chartSeq = 0;
  let devResolver = null;
  function setDeviceResolver(fn) { devResolver = typeof fn === 'function' ? fn : null; }
  function resolveDev(hint) { return devResolver && hint ? devResolver(String(hint)) : null; }

  const STATE_META = {
    ok:      { color: '#2e8b45', bg: 'rgba(46,139,69,.12)',   label: '正常' },
    slow:    { color: '#e08a00', bg: 'rgba(224,138,0,.14)',   label: '延迟高' },
    warn:    { color: '#e08a00', bg: 'rgba(224,138,0,.14)',   label: '告警' },
    loss:    { color: '#ff5a1f', bg: 'rgba(255,90,31,.14)',   label: '丢包' },
    down:    { color: '#d63a22', bg: 'rgba(214,58,34,.14)',   label: '离线' },
    unknown: { color: '#8a857a', bg: 'rgba(138,133,122,.14)', label: '未知' },
  };
  const SERIES_COLORS = ['#ff5a1f', '#2e8b45', '#1565c0', '#8e24aa', '#00838f', '#6d4c41'];

  function strW(s) {
    s = String(s || '');
    let w = 0;
    for (let i = 0; i < s.length; i++) w += s.charCodeAt(i) > 0x2e7f ? 1.72 : 1;
    return w;
  }
  function truncW(s, max) {
    s = String(s || '');
    let w = 0;
    for (let i = 0; i < s.length; i++) {
      w += s.charCodeAt(i) > 0x2e7f ? 1.72 : 1;
      if (w > max) return s.slice(0, Math.max(1, i - 1)) + '…';
    }
    return s;
  }

  function normState(st, hint) {
    const k = String(st || '').trim().toLowerCase();
    if (['ok', 'up', 'online', 'normal', 'pass', 'healthy', '正常', '在线', '通'].includes(k)) return 'ok';
    if (['down', 'offline', 'error', 'fail', 'failed', 'dead', 'timeout', 'unreachable', '离线', '不通', '中断', '超时', '故障', '掉线'].includes(k)) return 'down';
    if (['loss', 'pktloss', 'packet_loss', 'drop', 'degraded', 'crc', '丢包', '闪断', '错包'].includes(k)) return 'loss';
    if (['slow', 'warn', 'warning', 'high', 'delay', 'latency', 'jitter', '延迟高', '高延迟', '延迟', '告警', '慢', '拥塞'].includes(k)) return 'slow';
    if (['unknown', 'muted', 'gray', 'neutral', 'unmonitored', 'paused', '未知', '未监控', '暂停'].includes(k)) return 'unknown';
    const h = String(hint || '');
    if (/离线|不通|中断|超时|无响应|down|100%\s*丢包|\*\s*\*\s*\*/i.test(h)) return 'down';
    if (/(丢包|CRC|错包|丢弃|drop|loss)/i.test(h) && !/(?:^|\D)0(?:\.0+)?%\s*丢包|丢包\s*0(?:\.0+)?%/i.test(h)) return 'loss';
    if (/延迟高|高延迟|变慢|抖动|拥塞|slow|warn/i.test(h)) return 'slow';
    if (/正常|在线|0(?:\.0+)?%\s*丢包/i.test(h)) return 'ok';
    return '';
  }

  function fmtNum(v) {
    if (v == null || Number.isNaN(+v)) return '-';
    const n = +v;
    if (Math.abs(n) >= 100 || Math.floor(n) === n) return String(Math.round(n));
    if (Math.abs(n) >= 10) return n.toFixed(1).replace(/\.0$/, '');
    return n.toFixed(2).replace(/\.?0+$/, '');
  }

  function parseNum(v) {
    if (typeof v === 'number') return Number.isFinite(v) ? v : null;
    if (v == null || v === '') return null;
    const m = String(v).match(/-?\d+(?:\.\d+)?/);
    return m ? parseFloat(m[0]) : null;
  }

  function relaxJSON(s) {
    // 移除字符串外的单行注释与尾随逗号
    let out = '', inStr = false, escCh = false;
    for (let i = 0; i < s.length; i++) {
      const c = s[i];
      if (inStr) {
        out += c;
        if (escCh) escCh = false;
        else if (c === '\\') escCh = true;
        else if (c === '"') inStr = false;
        continue;
      }
      if (c === '"') { inStr = true; out += c; continue; }
      if (c === '/' && s[i + 1] === '/') {
        while (i < s.length && s[i] !== '\n') i++;
        out += '\n';
        continue;
      }
      out += c;
    }
    return out.replace(/,(\s*[}\]])/g, '$1');
  }

  function tryParseJSON(text) {
    const t = String(text || '').trim();
    if (!t) return null;
    try { return JSON.parse(t); } catch (_) {}
    try { return JSON.parse(relaxJSON(t)); } catch (_) {}
    return null;
  }

  const CHART_LANGS = new Set(['chart', 'viz', 'netchart', 'topology', 'path', 'bar', 'line', 'timeline', 'mermaid']);
  const KNOWN_TYPES = new Set(['path', 'topology', 'flow', 'topo', 'bar', 'rank', 'compare', 'line', 'series', 'trend', 'history', 'timeline', 'outages', 'correlate', 'events']);

  function inferChartType(obj, lang) {
    if (!obj || typeof obj !== 'object') return '';
    const rawT = String(obj.type || obj.kind || obj.chart || '').trim().toLowerCase();
    if (KNOWN_TYPES.has(rawT)) return rawT;
    if (KNOWN_TYPES.has(lang)) return lang;
    if (!CHART_LANGS.has(lang)) return ''; // 普通 json 代码块必须显式声明已知的图表 type
    if (Array.isArray(obj.nodes) || Array.isArray(obj.hops) || Array.isArray(obj.steps) || Array.isArray(obj.edges) || Array.isArray(obj.links)) return 'path';
    if (Array.isArray(obj.rows) || Array.isArray(obj.spans) || (Array.isArray(obj.items) && obj.items.some(x => x && (Array.isArray(x.spans) || Array.isArray(x.events) || x.from != null || x.start != null)))) return 'timeline';
    if (Array.isArray(obj.points) || Array.isArray(obj.series)) return 'line';
    if (Array.isArray(obj.items) || Array.isArray(obj.bars) || Array.isArray(obj.data) || (Array.isArray(obj.labels) && Array.isArray(obj.values))) return 'bar';
    return '';
  }

  function parseChartSpecs(raw, lang) {
    const text = String(raw || '').trim();
    if (!text) return [];
    if (lang === 'mermaid' || /^(?:graph|flowchart)\s+(?:LR|RL|TD|TB|BT)\b/i.test(text)) {
      const mSpec = mermaidToPathSpec(text);
      return mSpec ? [mSpec] : [];
    }
    const parsed = tryParseJSON(text);
    const list = [];
    const collect = v => {
      if (!v) return;
      if (Array.isArray(v)) { v.forEach(collect); return; }
      if (typeof v === 'object') {
        const t = inferChartType(v, lang);
        if (t) list.push(Object.assign({}, v, { type: t }));
      }
    };
    if (parsed) {
      collect(parsed);
      return list;
    }
    // 支持一个代码块内多行独立 JSON 对象
    for (const line of text.split('\n')) {
      const l = line.trim();
      if (l.startsWith('{') && l.endsWith('}')) collect(tryParseJSON(l));
    }
    return list;
  }

  function aiBlock(lang, raw, closed) {
    lang = String(lang || '').trim().toLowerCase().split(/\s+/)[0];
    const isChartFence = CHART_LANGS.has(lang) ||
      ((lang === 'json' || lang === '') && /"type"\s*:\s*"(?:path|topology|flow|bar|rank|compare|line|series|trend|timeline|outages|correlate)"/i.test(raw || ''));
    if (!closed) {
      if (isChartFence) return `<div class="ai-chart ai-chart-pending"><span class="ai-spin"></span><span>正在绘制可视化图表…</span></div>`;
      return '';
    }
    if (!isChartFence) return '';
    const specs = parseChartSpecs(raw, lang);
    if (!specs.length) return '';
    return specs.map(aiChart).filter(Boolean).join('');
  }

  function aiChart(spec) {
    if (!spec || typeof spec !== 'object') return '';
    const t = inferChartType(spec, 'chart');
    switch (t) {
      case 'path': case 'topology': case 'flow': case 'topo':
        return renderPathChart(spec);
      case 'bar': case 'rank': case 'compare':
        return renderBarChart(spec);
      case 'line': case 'series': case 'trend': case 'history':
        return renderLineChart(spec);
      case 'timeline': case 'outages': case 'correlate': case 'events':
        return renderTimelineChart(spec);
      default:
        return '';
    }
  }

  function wrapChart(tag, title, legendHTML, svgHTML, note, hasDevLink) {
    const leg = (legendHTML || '') + (hasDevLink ? `<span class="ai-leg-hint">点击设备可看详情</span>` : '');
    return `<figure class="ai-chart">
      <figcaption class="ai-chart-head">
        <span class="ai-chart-tag">${esc(tag)}</span>
        ${title ? `<b class="ai-chart-title">${esc(title)}</b>` : ''}
        ${leg ? `<span class="ai-chart-legend">${leg}</span>` : ''}
      </figcaption>
      <div class="ai-chart-body">${svgHTML}</div>
      ${note ? `<div class="ai-chart-note">${esc(note)}</div>` : ''}
      <div class="tip hidden"></div>
    </figure>`;
  }

  function stateLegend(states) {
    const order = ['ok', 'slow', 'loss', 'down', 'unknown'];
    const seen = new Set(states.filter(Boolean));
    return order.filter(s => seen.has(s)).map(s => {
      const m = STATE_META[s] || STATE_META.unknown;
      return `<span class="ai-leg"><i style="background:${m.color}"></i>${esc(m.label)}</span>`;
    }).join('');
  }

  function devAttrs(hint) {
    if (!hint) return { attr: '', linked: false, dev: null };
    const dev = resolveDev(hint);
    const idAttr = dev && dev.id ? ` data-node-id="${esc(dev.id)}"` : '';
    return {
      attr: ` data-dev="${esc(hint)}"${idAttr}`,
      linked: !!(dev && dev.id),
      dev,
    };
  }

  // ---------------------------------------------------------------------------
  // 1. 故障路径 / 拓扑影响图（path）
  function renderPathChart(spec) {
    const rawNodes = spec.nodes || spec.hops || spec.steps || spec.items || [];
    const rawEdges = spec.edges || spec.links || [];
    const nodeMap = new Map();
    const nodes = [];
    const edges = [];

    const addNode = (n, idx) => {
      if (n == null) return null;
      if (typeof n === 'string') n = { id: 'n' + idx, label: n };
      const id = String(n.id || n.name || n.label || ('n' + idx)).trim();
      if (!id) return null;
      const label = String(n.label || n.name || n.title || id).trim();
      const parts = [n.sub, n.detail, n.desc, n.ip, n.metric, n.value, n.latency].filter(x => x != null && x !== '').map(String);
      const sub = parts.length ? parts[0] : '';
      const st = normState(n.state || n.status, label + ' ' + sub + ' ' + (n.badge || '')) || 'unknown';
      const badge = String(n.badge || (st !== 'unknown' ? STATE_META[st].label : '') || '');
      const port = String(n.port || n.via || n.edge || n.hop || '');
      if (nodeMap.has(id)) {
        const ex = nodeMap.get(id);
        if (!ex.sub && sub) ex.sub = sub;
        if (ex.state === 'unknown' && st !== 'unknown') { ex.state = st; ex.badge = badge; }
        return ex;
      }
      const obj = { id, label, sub, state: st, badge, port, edgeState: n.edge_state || '' };
      nodeMap.set(id, obj);
      nodes.push(obj);
      if (Array.isArray(n.children)) {
        n.children.forEach((ch, ci) => {
          const childObj = addNode(ch, idx + '_' + ci);
          if (childObj) edges.push({ from: id, to: childObj.id, label: childObj.port, state: childObj.edgeState || (childObj.state === 'down' || childObj.state === 'loss' ? childObj.state : '') });
        });
      }
      return obj;
    };

    rawNodes.forEach((n, i) => addNode(n, i));
    rawEdges.forEach((e, i) => {
      if (!e) return;
      if (typeof e === 'string') {
        const m = e.match(/^(.+?)\s*[-=]+>\s*([^:：]+)(?:[:：]\s*(.+))?$/);
        if (!m) return;
        e = { from: m[1].trim(), to: m[2].trim(), label: (m[3] || '').trim() };
      }
      const from = String(e.from || e.source || e.src || '').trim();
      const to = String(e.to || e.target || e.dst || '').trim();
      if (!from || !to) return;
      if (!nodeMap.has(from)) addNode({ id: from, label: from }, 'ef' + i);
      if (!nodeMap.has(to)) addNode({ id: to, label: to }, 'et' + i);
      edges.push({
        from, to,
        label: String(e.label || e.port || e.via || e.note || '').trim(),
        state: normState(e.state || e.status, e.label || ''),
      });
    });

    if (!nodes.length) return '';
    // 未显式给 edges 时，按顺序串联成链路
    const autoChain = !edges.length && nodes.length > 1;
    if (autoChain) {
      for (let i = 1; i < nodes.length; i++) {
        const cur = nodes[i];
        edges.push({
          from: nodes[i - 1].id,
          to: cur.id,
          label: cur.port,
          state: normState(cur.edgeState, cur.port) || (cur.state === 'down' || cur.state === 'loss' ? cur.state : ''),
        });
      }
    }

    // 计算拓扑层级（最长路径分层，限制迭代防环）
    const inDeg = new Map(nodes.map(n => [n.id, 0]));
    const adj = new Map(nodes.map(n => [n.id, []]));
    const parents = new Map(nodes.map(n => [n.id, []]));
    edges.forEach(e => {
      if (e.from === e.to) return;
      adj.get(e.from)?.push(e.to);
      parents.get(e.to)?.push(e.from);
      inDeg.set(e.to, (inDeg.get(e.to) || 0) + 1);
    });
    const colOf = new Map(nodes.map(n => [n.id, 0]));
    const maxStep = Math.max(1, nodes.length - 1);
    for (let iter = 0; iter < maxStep; iter++) {
      let changed = false;
      for (const e of edges) {
        if (e.from === e.to) continue;
        const cFrom = colOf.get(e.from) || 0;
        const cTo = colOf.get(e.to) || 0;
        // 避免环路无限推高：仅当不是回边时推进
        if (cFrom + 1 > cTo && cFrom + 1 <= maxStep) {
          colOf.set(e.to, cFrom + 1);
          changed = true;
        }
      }
      if (!changed) break;
    }

    // 整理列并减少交叉
    let maxCol = 0;
    nodes.forEach(n => { const c = colOf.get(n.id) || 0; if (c > maxCol) maxCol = c; });
    const cols = Array.from({ length: maxCol + 1 }, () => []);
    nodes.forEach((n, idx) => { n._order = idx; cols[colOf.get(n.id) || 0].push(n); });

    const rowIdx = new Map();
    cols[0].forEach((n, r) => rowIdx.set(n.id, r));
    for (let c = 1; c <= maxCol; c++) {
      cols[c].forEach(n => {
        const ps = (parents.get(n.id) || []).filter(p => (colOf.get(p) || 0) < c && rowIdx.has(p));
        n._bary = ps.length ? ps.reduce((s, p) => s + rowIdx.get(p), 0) / ps.length : n._order;
      });
      cols[c].sort((a, b) => a._bary - b._bary || a._order - b._order);
      cols[c].forEach((n, r) => rowIdx.set(n.id, r));
    }

    const CW = 160, CH = 48, HGAP = 68, VGAP = 16, PADX = 16, PADY = 16;
    const isSingleChain = cols.every(c => c.length === 1) && cols.length > 4;
    const WRAP_COLS = 4;

    let W, H;
    if (isSingleChain) {
      // 超长单链（如多跳路由追踪）按每行 4 跳折行排布
      const nRows = Math.ceil(nodes.length / WRAP_COLS);
      const colsUsed = Math.min(WRAP_COLS, nodes.length);
      W = PADX * 2 + colsUsed * CW + (colsUsed - 1) * HGAP;
      H = PADY * 2 + nRows * CH + (nRows - 1) * (VGAP + 24);
      nodes.forEach((n, idx) => {
        const r = Math.floor(idx / WRAP_COLS);
        const c = idx % WRAP_COLS;
        n.x = PADX + c * (CW + HGAP);
        n.y = PADY + r * (CH + VGAP + 24);
        n._row = r;
        n._col = c;
      });
    } else {
      const maxRows = Math.max(...cols.map(c => c.length), 1);
      const innerH = Math.max(CH, maxRows * CH + (maxRows - 1) * VGAP);
      W = Math.max(360, PADX * 2 + cols.length * CW + Math.max(0, cols.length - 1) * HGAP);
      H = PADY * 2 + innerH;
      cols.forEach((col, ci) => {
        const colH = col.length * CH + (col.length - 1) * VGAP;
        const startY = PADY + (innerH - colH) / 2;
        col.forEach((n, ri) => {
          n.x = PADX + ci * (CW + HGAP);
          n.y = startY + ri * (CH + VGAP);
        });
      });
      // 自右向左将父节点对齐到子节点的纵向中心（单父或无冲突时）
      for (let ci = cols.length - 2; ci >= 0; ci--) {
        const col = cols[ci];
        col.forEach(n => {
          const chs = (adj.get(n.id) || []).map(id => nodeMap.get(id)).filter(ch => ch && (colOf.get(ch.id) || 0) === ci + 1);
          if (chs.length) {
            n._wantY = chs.reduce((s, ch) => s + ch.y, 0) / chs.length;
          }
        });
        if (col.length === 1 && col[0]._wantY != null) {
          col[0].y = Math.max(PADY, Math.min(H - PADY - CH, col[0]._wantY));
        }
      }
    }

    const uid = ++chartSeq;
    let defs = '<defs>';
    Object.keys(STATE_META).forEach(st => {
      const col = STATE_META[st].color;
      defs += `<marker id="ai-arr-${uid}-${st}" viewBox="0 0 10 8" refX="8" refY="4" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
        <path d="M0,1 L8,4 L0,7 z" fill="${col}"/></marker>`;
    });
    defs += '</defs>';

    let edgeSVG = '';
    edges.forEach(e => {
      const u = nodeMap.get(e.from), v = nodeMap.get(e.to);
      if (!u || !v || u === v) return;
      const st = e.state || (v.state === 'down' ? 'down' : v.state === 'loss' ? 'loss' : v.state === 'slow' ? 'slow' : 'ok');
      const meta = STATE_META[st] || STATE_META.unknown;
      const dash = st === 'down' ? ' stroke-dasharray="5 3"' : (st === 'loss' || st === 'slow') ? ' stroke-dasharray="4 2"' : '';
      const sw = st === 'down' || st === 'loss' ? 1.75 : 1.35;

      let dPath = '', lx = 0, ly = 0;
      if (isSingleChain && u._row !== undefined && v._row !== undefined && u._row !== v._row) {
        // 折行连接线：从上一行末尾底部绕到下一行开头顶部
        const x1 = u.x + CW / 2, y1 = u.y + CH;
        const x2 = v.x + CW / 2, y2 = v.y;
        const my = (y1 + y2) / 2;
        dPath = `M${x1},${y1} L${x1},${my} L${x2},${my} L${x2},${y2}`;
        lx = (x1 + x2) / 2; ly = my;
      } else if (u.x < v.x) {
        const x1 = u.x + CW, y1 = u.y + CH / 2;
        const x2 = v.x, y2 = v.y + CH / 2;
        const mx = (x1 + x2) / 2;
        dPath = `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`;
        lx = mx; ly = (y1 + y2) / 2;
      } else {
        const x1 = u.x + CW / 2, y1 = u.y <= v.y ? u.y + CH : u.y;
        const x2 = v.x + CW / 2, y2 = u.y <= v.y ? v.y : v.y + CH;
        dPath = `M${x1},${y1} L${x2},${y2}`;
        lx = (x1 + x2) / 2; ly = (y1 + y2) / 2;
      }

      edgeSVG += `<path d="${dPath}" fill="none" stroke="${meta.color}" stroke-width="${sw}"${dash} marker-end="url(#ai-arr-${uid}-${st})"/>`;
      if (e.label) {
        const txt = truncW(e.label, 14);
        const bw = Math.max(28, Math.round(strW(txt) * 5.8 + 10)), bh = 16;
        edgeSVG += `<g class="ai-edge-lbl" data-tip="${esc(u.label + ' → ' + v.label + '<br>' + e.label)}">
          <rect x="${(lx - bw / 2).toFixed(1)}" y="${(ly - bh / 2).toFixed(1)}" width="${bw}" height="${bh}" rx="2" fill="#faf8f3" stroke="${st === 'ok' ? '#cfc8b8' : meta.color}" stroke-width="1"/>
          <text x="${lx.toFixed(1)}" y="${(ly + 3.5).toFixed(1)}" text-anchor="middle" class="axis" fill="${st === 'ok' ? '#4a4740' : meta.color}">${esc(txt)}</text>
        </g>`;
      }
    });

    let nodeSVG = '';
    let hasDevLink = false;
    nodes.forEach(n => {
      const meta = STATE_META[n.state] || STATE_META.unknown;
      const borderCol = n.state === 'down' || n.state === 'loss' || n.state === 'slow' ? meta.color : '#1a1a1a';
      const dv = devAttrs(n.id + ' ' + n.label + ' ' + n.sub);
      if (dv.linked) hasDevLink = true;
      const badgeTxt = truncW(n.badge, 8);
      const badgeW = badgeTxt ? Math.round(strW(badgeTxt) * 6 + 8) : 0;
      const titleMaxW = badgeTxt ? Math.max(8, Math.floor((CW - badgeW - 22) / 6.6)) : 21;
      const subText = n.sub || (!autoChain && n.port ? n.port : '');
      const tipLines = [`<b>${esc(n.label)}</b>${n.badge ? ` <span style="color:${meta.color}">[${esc(n.badge)}]</span>` : ''}`];
      if (subText) tipLines.push(esc(subText));
      if (dv.linked) tipLines.push('<span class="tip-hint">点击查看设备详情 →</span>');

      nodeSVG += `<g class="ai-node${dv.linked ? ' clickable' : ''}"${dv.attr} data-tip="${esc(tipLines.join('<br>'))}">
        <rect x="${n.x + 2}" y="${n.y + 2}" width="${CW}" height="${CH}" rx="2" fill="rgba(26,26,26,.08)"/>
        <rect class="ai-node-bg" x="${n.x}" y="${n.y}" width="${CW}" height="${CH}" rx="2" fill="#fff" stroke="${borderCol}" stroke-width="1.3"/>
        <rect x="${n.x}" y="${n.y}" width="4" height="${CH}" rx="1" fill="${meta.color}"/>
        ${badgeTxt ? `<rect x="${n.x + CW - badgeW - 6}" y="${n.y + 6}" width="${badgeW}" height="15" rx="2" fill="${meta.bg}"/>
          <text x="${n.x + CW - badgeW / 2 - 6}" y="${n.y + 16.5}" text-anchor="middle" font-size="10" font-weight="600" fill="${meta.color}">${esc(badgeTxt)}</text>` : ''}
        <text x="${n.x + 10}" y="${n.y + (subText ? 20 : 28)}" font-size="12" font-weight="600" fill="#1a1a1a">${esc(truncW(n.label, titleMaxW))}</text>
        ${subText ? `<text x="${n.x + 10}" y="${n.y + 38}" class="axis" font-size="10.5" fill="#4a4740">${esc(truncW(subText, 22))}</text>` : ''}
      </g>`;
    });

    const svg = `<svg viewBox="0 0 ${W} ${H}" height="${H}" style="min-width:${Math.min(W, 480)}px">${defs}${edgeSVG}${nodeSVG}</svg>`;
    return wrapChart('链路路径', spec.title || '网络故障路径与影响面', stateLegend(nodes.map(n => n.state)), svg, spec.note || spec.summary || spec.footer, hasDevLink);
  }

  // ---------------------------------------------------------------------------
  // 2. 对比与排行柱状图（bar）
  function renderBarChart(spec) {
    let rawItems = spec.items || spec.data || spec.bars || [];
    if (!rawItems.length && Array.isArray(spec.labels) && Array.isArray(spec.values)) {
      rawItems = spec.labels.map((l, i) => ({ label: l, value: spec.values[i] }));
    }
    const unit = spec.unit != null ? String(spec.unit) :
      (/%|丢包|可用率|占比/.test(spec.title || '') ? '%' : /ms|毫秒|延迟|抖动/.test(spec.title || '') ? 'ms' : /分钟|min/.test(spec.title || '') ? '分钟' : '');
    const threshold = parseNum(spec.threshold ?? spec.warn ?? spec.baseline);

    const items = rawItems.map((it, i) => {
      if (it == null) return null;
      if (typeof it === 'number') it = { label: '#' + (i + 1), value: it };
      const label = String(it.label || it.name || it.device || it.id || ('#' + (i + 1))).trim();
      const val = parseNum(it.value ?? it.val ?? it.y ?? it.loss ?? it.latency ?? it.downtime ?? it.count);
      if (val == null) return null;
      const note = String(it.note || it.sub || it.detail || it.port || '').trim();
      let st = normState(it.state || it.status, '');
      if (!st) {
        if (/中位数|基线|平均|正常|昨天|上周/.test(label)) st = 'ok';
        else if (threshold != null && threshold > 0) st = val >= threshold * 1.5 ? 'down' : val >= threshold ? 'loss' : 'ok';
        else st = val === 0 ? 'ok' : 'loss';
      }
      return { label, value: Math.max(0, val), note, state: st, id: it.id || '' };
    }).filter(Boolean);

    if (!items.length) return '';

    const W = 640;
    const maxLw = Math.max(...items.map(x => strW(x.label)), 8);
    const hasNote = items.some(x => x.note);
    const left = Math.min(200, Math.max(104, Math.round(maxLw * 6.8 + 18)));
    const right = hasNote ? 150 : 86;
    const top = threshold != null && threshold > 0 ? 22 : 10;
    const bottom = 22;
    const ROW_H = 28;
    const h = items.length * ROW_H;
    const H = top + h + bottom;
    const w = W - left - right;

    const maxVal = Math.max(...items.map(x => x.value), parseNum(spec.max) || 0, threshold ? threshold * 1.15 : 0, 1);
    const xMax = niceMax(maxVal);
    const xOf = v => left + Math.min(Math.max(0, v), xMax) / xMax * w;

    let s = '';
    for (let i = 0; i <= 4; i++) {
      const v = xMax * i / 4, x = xOf(v);
      s += `<line class="gl" x1="${x.toFixed(1)}" x2="${x.toFixed(1)}" y1="${top}" y2="${top + h}"/>`;
      s += `<text class="axis" x="${x.toFixed(1)}" y="${H - 6}" text-anchor="middle">${esc(fmtNum(v) + unit)}</text>`;
    }

    let hasDevLink = false;
    items.forEach((it, idx) => {
      const ry = top + idx * ROW_H;
      const by = ry + 6, bh = 15;
      const bw = it.value > 0 ? Math.max(3, xOf(it.value) - left) : 0;
      const meta = STATE_META[it.state] || STATE_META.loss;
      const dv = devAttrs((it.id ? it.id + ' ' : '') + it.label + ' ' + it.note);
      if (dv.linked) hasDevLink = true;
      const valStr = fmtNum(it.value) + unit;
      const tip = `<b>${esc(it.label)}</b><br>数值：${esc(valStr)}${it.note ? '<br>' + esc(it.note) : ''}${dv.linked ? '<br><span class="tip-hint">点击查看设备详情 →</span>' : ''}`;

      s += `<g class="ai-bar-row${dv.linked ? ' clickable' : ''}"${dv.attr} data-tip="${esc(tip)}">
        <rect x="0" y="${ry}" width="${W}" height="${ROW_H}" fill="transparent"/>
        <text x="${left - 8}" y="${ry + 17.5}" text-anchor="end" font-size="12" fill="#1a1a1a">${esc(truncW(it.label, Math.floor((left - 14) / 6.5)))}</text>
        <rect x="${left}" y="${by}" width="${w}" height="${bh}" rx="1" fill="#ebe6da"/>
        ${bw > 0 ? `<rect x="${left}" y="${by}" width="${bw.toFixed(1)}" height="${bh}" rx="1" fill="${meta.color}"/>` : ''}
        <text x="${(left + bw + 6).toFixed(1)}" y="${ry + 17.5}" class="axis" font-size="11" fill="#1a1a1a">
          <tspan font-weight="600">${esc(valStr)}</tspan>${it.note ? `<tspan fill="#8a857a"> · ${esc(truncW(it.note, 16))}</tspan>` : ''}
        </text>
      </g>`;
    });

    if (threshold != null && threshold > 0 && threshold <= xMax) {
      const tx = xOf(threshold);
      const tLbl = spec.threshold_label || ('阈值 ' + fmtNum(threshold) + unit);
      s += `<line class="warnl" x1="${tx.toFixed(1)}" x2="${tx.toFixed(1)}" y1="${top - 2}" y2="${top + h}"/>`;
      s += `<text class="axis" x="${tx.toFixed(1)}" y="${top - 6}" text-anchor="middle" fill="#e08a00">${esc(tLbl)}</text>`;
    }

    const svg = `<svg viewBox="0 0 ${W} ${H}" height="${H}">${s}</svg>`;
    return wrapChart('指标对比', spec.title || '设备指标对比', stateLegend(items.map(x => x.state)), svg, spec.note || spec.summary || spec.footer, hasDevLink);
  }

  // ---------------------------------------------------------------------------
  // 3. 趋势与分布曲线（line）
  function renderLineChart(spec) {
    const unit = spec.unit != null ? String(spec.unit) :
      (/%|丢包|可用率/.test(spec.title || '') ? '%' : /ms|毫秒|延迟|抖动/.test(spec.title || '') ? 'ms' : '');
    const threshold = parseNum(spec.threshold ?? spec.warn);

    // 统一为 labels + series 结构，同时保留单序列的 min/max/loss
    let labels = [];
    let series = [];
    let bandPts = null;
    let lossPts = null;

    if (Array.isArray(spec.series) && spec.series.length) {
      labels = (spec.labels || spec.x || []).map(String);
      series = spec.series.map((sr, si) => {
        const vals = (sr.values || sr.data || sr.points || []).map(v => (v && typeof v === 'object') ? parseNum(v.value ?? v.avg ?? v.y) : parseNum(v));
        if (!labels.length) labels = vals.map((_, i) => '#' + (i + 1));
        const st = normState(sr.state, sr.name);
        return {
          name: String(sr.name || sr.label || ('序列' + (si + 1))),
          values: vals,
          color: (st && STATE_META[st] ? STATE_META[st].color : '') || sr.color || SERIES_COLORS[si % SERIES_COLORS.length],
        };
      }).filter(sr => sr.values.length);
    } else {
      const rawPts = spec.points || spec.data || spec.items || [];
      if (!rawPts.length) return '';
      const vals = [], mins = [], maxs = [], losses = [];
      let hasBand = false, hasLoss = false;
      rawPts.forEach((p, i) => {
        if (p == null) return;
        if (typeof p === 'number') p = { label: '#' + (i + 1), value: p };
        let lbl = p.label ?? p.time ?? p.hour ?? p.t ?? ('#' + (i + 1));
        if (typeof lbl === 'number' && lbl > 1e9) lbl = fmtTime(lbl > 1e12 ? lbl : lbl * 1000, false);
        labels.push(String(lbl));
        const v = parseNum(p.value ?? p.avg ?? p.latency ?? p.y ?? (unit === '%' ? p.loss : null));
        vals.push(v != null && v >= 0 ? v : null);
        const mn = parseNum(p.min), mx = parseNum(p.max);
        if (mn != null && mx != null && mn >= 0 && mx >= mn) { hasBand = true; mins.push(mn); maxs.push(mx); }
        else { mins.push(null); maxs.push(null); }
        const ls = parseNum(p.loss);
        if (ls != null && unit !== '%') { if (ls > 0) hasLoss = true; losses.push(Math.min(100, Math.max(0, ls))); }
        else losses.push(null);
      });
      series = [{ name: spec.metric || (unit === '%' ? '丢包率' : '数值'), values: vals, color: '#ff5a1f' }];
      if (hasBand) bandPts = { mins, maxs };
      if (hasLoss) lossPts = losses;
    }

    const N = labels.length;
    if (!N || !series.length) return '';

    const W = 640;
    const left = 52, right = 18, top = 12;
    const mainH = 152;
    const lossH = lossPts ? 42 : 0;
    const lossGap = lossPts ? 16 : 0;
    const bottom = 24;
    const H = top + mainH + lossGap + lossH + bottom;
    const w = W - left - right;

    let maxV = 0;
    series.forEach(sr => sr.values.forEach(v => { if (v != null && v > maxV) maxV = v; }));
    if (bandPts) bandPts.maxs.forEach(v => { if (v != null) maxV = Math.max(maxV, Math.min(v, maxV * 3 || v)); });
    const yMax = niceMax(Math.max(maxV * 1.15, threshold && threshold < maxV * 4 ? threshold * 1.1 : 0, 1));
    const xOf = i => N === 1 ? left + w / 2 : left + (i / (N - 1)) * w;
    const yOf = v => top + mainH - Math.min(Math.max(0, v), yMax) / yMax * mainH;

    let s = '';
    for (let i = 0; i <= 4; i++) {
      const v = yMax * i / 4, y = yOf(v);
      s += `<line class="gl" x1="${left}" x2="${left + w}" y1="${y.toFixed(1)}" y2="${y.toFixed(1)}"/>`;
      s += `<text class="axis" x="${left - 6}" y="${(y + 3).toFixed(1)}" text-anchor="end">${esc(fmtNum(v) + unit)}</text>`;
    }

    const tickStep = Math.max(1, Math.ceil(N / 7));
    for (let i = 0; i < N; i += tickStep) {
      const x = xOf(i);
      s += `<line class="gl" x1="${x.toFixed(1)}" x2="${x.toFixed(1)}" y1="${top}" y2="${top + mainH}"/>`;
      s += `<text class="axis" x="${x.toFixed(1)}" y="${H - 6}" text-anchor="middle">${esc(truncW(labels[i], 10))}</text>`;
    }
    if ((N - 1) % tickStep !== 0 && N > 1) {
      const x = xOf(N - 1);
      const prevX = xOf(Math.floor((N - 1) / tickStep) * tickStep);
      if (x - prevX > 42) s += `<text class="axis" x="${x.toFixed(1)}" y="${H - 6}" text-anchor="middle">${esc(truncW(labels[N - 1], 10))}</text>`;
    }

    // 离线/100%丢包时段底色高亮
    const colW = N > 1 ? w / (N - 1) : 24;
    for (let i = 0; i < N; i++) {
      const isDown = (series[0].values[i] == null) || (lossPts && lossPts[i] >= 100);
      if (isDown) {
        const bx = Math.max(left, xOf(i) - colW / 2);
        const bw = Math.min(left + w, xOf(i) + colW / 2) - bx;
        s += `<rect x="${bx.toFixed(1)}" y="${top}" width="${Math.max(2, bw).toFixed(1)}" height="${mainH}" fill="rgba(214,58,34,.11)"/>`;
      }
    }

    // 最小~最大波动区间带
    if (bandPts) {
      let seg = [];
      const flushBand = () => {
        if (seg.length < 2) { seg = []; return; }
        const up = seg.map(i => `${xOf(i).toFixed(1)},${yOf(bandPts.maxs[i]).toFixed(1)}`);
        const dn = seg.slice().reverse().map(i => `${xOf(i).toFixed(1)},${yOf(bandPts.mins[i]).toFixed(1)}`);
        s += `<polygon class="band" points="${up.concat(dn).join(' ')}"/>`;
        seg = [];
      };
      for (let i = 0; i < N; i++) {
        if (bandPts.mins[i] != null && bandPts.maxs[i] != null) seg.push(i);
        else flushBand();
      }
      flushBand();
    }

    if (threshold != null && threshold > 0 && threshold <= yMax) {
      const ty = yOf(threshold);
      s += `<line class="warnl" x1="${left}" x2="${left + w}" y1="${ty.toFixed(1)}" y2="${ty.toFixed(1)}"/>`;
      s += `<text class="axis" x="${left + w - 4}" y="${(ty - 4).toFixed(1)}" text-anchor="end" fill="#e08a00">${esc(spec.threshold_label || ('阈值 ' + fmtNum(threshold) + unit))}</text>`;
    }

    // 折线与数据点
    series.forEach(sr => {
      let pts = [];
      const flushLine = () => {
        if (pts.length > 1) s += `<polyline fill="none" stroke="${sr.color}" stroke-width="1.75" points="${pts.join(' ')}"/>`;
        pts = [];
      };
      sr.values.forEach((v, i) => {
        if (v == null) { flushLine(); return; }
        const px = xOf(i).toFixed(1), py = yOf(v).toFixed(1);
        pts.push(`${px},${py}`);
      });
      flushLine();
      sr.values.forEach((v, i) => {
        if (v == null) return;
        s += `<circle cx="${xOf(i).toFixed(1)}" cy="${yOf(v).toFixed(1)}" r="${N <= 24 ? 2.6 : 1.8}" fill="${sr.color}"/>`;
      });
    });

    // 底部同步丢包率柱状条
    if (lossPts) {
      const ly0 = top + mainH + lossGap;
      s += `<line class="gl" x1="${left}" x2="${left + w}" y1="${ly0}" y2="${ly0}"/>`;
      s += `<line class="gl" x1="${left}" x2="${left + w}" y1="${ly0 + lossH}" y2="${ly0 + lossH}"/>`;
      s += `<text class="axis" x="${left - 6}" y="${ly0 + 9}" text-anchor="end">100%</text>`;
      s += `<text class="axis" x="${left - 6}" y="${ly0 + lossH}" text-anchor="end">丢包</text>`;
      const bw = Math.max(3, Math.min(18, colW * 0.6));
      lossPts.forEach((ls, i) => {
        if (!ls || ls <= 0) return;
        const bh = Math.max(2, (ls / 100) * lossH);
        const bx = xOf(i) - bw / 2, by = ly0 + lossH - bh;
        s += `<rect class="bar${ls < 100 ? ' part' : ''}" x="${bx.toFixed(1)}" y="${by.toFixed(1)}" width="${bw.toFixed(1)}" height="${bh.toFixed(1)}"/>`;
      });
    }

    // 悬停提示感应列
    for (let i = 0; i < N; i++) {
      const hx = Math.max(left, xOf(i) - colW / 2);
      const hw = Math.min(left + w, xOf(i) + colW / 2) - hx;
      const lines = [`<b>${esc(labels[i])}</b>`];
      series.forEach(sr => {
        const v = sr.values[i];
        const txt = v == null ? '<span style="color:#d63a22">离线 / 超时</span>' : esc(fmtNum(v) + unit);
        lines.push(`${series.length > 1 ? esc(sr.name) + '：' : '数值：'}${txt}`);
      });
      if (bandPts && bandPts.mins[i] != null) lines.push(`区间：${esc(fmtNum(bandPts.mins[i]) + ' ~ ' + fmtNum(bandPts.maxs[i]) + unit)}`);
      if (lossPts && lossPts[i] != null) lines.push(`丢包：${esc(fmtNum(lossPts[i]) + '%')}`);
      s += `<g class="ai-line-pt" data-tip="${esc(lines.join('<br>'))}">
        <line class="ai-hover-line" x1="${xOf(i).toFixed(1)}" x2="${xOf(i).toFixed(1)}" y1="${top}" y2="${top + mainH + lossGap + lossH}"/>
        <rect x="${hx.toFixed(1)}" y="${top}" width="${Math.max(4, hw).toFixed(1)}" height="${mainH + lossGap + lossH}" fill="transparent"/>
      </g>`;
    }

    let legHTML = '';
    if (series.length > 1) {
      legHTML = series.map(sr => `<span class="ai-leg"><i style="background:${sr.color}"></i>${esc(sr.name)}</span>`).join('');
    } else {
      legHTML = `<span class="ai-leg"><i style="background:${series[0].color}"></i>${esc(unit === '%' ? '丢包率' : '延迟/数值')}</span>` +
        (lossPts ? `<span class="ai-leg"><i style="background:#d63a22"></i>丢包率</span>` : '');
    }

    const svg = `<svg viewBox="0 0 ${W} ${H}" height="${H}">${s}</svg>`;
    return wrapChart('趋势曲线', spec.title || '历史变化趋势', legHTML, svg, spec.note || spec.summary || spec.footer, false);
  }

  // ---------------------------------------------------------------------------
  // 4. 多设备异常时段对齐 / 时间线（timeline）
  function parseTimelineVal(v) {
    if (v == null || v === '') return null;
    if (typeof v === 'number') {
      if (v > 1e12) return { v: v / 60000, mode: 'epoch' };
      if (v > 1e9) return { v: v / 60, mode: 'epoch' };
      return { v, mode: 'num' };
    }
    const s = String(v).trim();
    let m;
    if ((m = s.match(/^(\d{1,2}):(\d{2})(?::(\d{2}))?$/))) {
      return { v: (+m[1]) * 60 + (+m[2]) + (+(m[3] || 0)) / 60, mode: 'clock' };
    }
    if ((m = s.match(/^(?:(\d{4})-)?(\d{1,2})-(\d{1,2})\s+(\d{1,2}):(\d{2})(?::(\d{2}))?$/))) {
      const yr = m[1] ? +m[1] : new Date().getFullYear();
      const dt = new Date(yr, +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] || 0));
      return { v: dt.getTime() / 60000, mode: 'epoch', withDate: true };
    }
    const n = parseNum(s);
    return n != null ? { v: n, mode: 'num' } : null;
  }

  function fmtTimelineTick(val, mode, withDate) {
    if (mode === 'clock') {
      const tot = Math.max(0, Math.round(val));
      const hh = Math.floor(tot / 60) % 24, mm = tot % 60;
      return pad2(hh) + ':' + pad2(mm);
    }
    if (mode === 'epoch') return fmtTime(val * 60000, !!withDate);
    return fmtNum(val);
  }

  function renderTimelineChart(spec) {
    const rawItems = spec.items || spec.rows || spec.devices || spec.events || [];
    const rows = [];
    const rowByLabel = new Map();
    let mode = 'clock', withDate = false;

    const addSpan = (rowLbl, rowId, sp) => {
      if (!sp) return;
      const fromRaw = sp.from ?? sp.start ?? sp.begin ?? sp.time ?? sp.t;
      const toRaw = sp.to ?? sp.end ?? sp.finish ?? fromRaw;
      const p0 = parseTimelineVal(fromRaw), p1 = parseTimelineVal(toRaw);
      if (!p0 && !p1) return;
      const t0 = (p0 || p1).v;
      const t1 = Math.max(t0, (p1 || p0).v);
      mode = (p0 || p1).mode || mode;
      if ((p0 && p0.withDate) || (p1 && p1.withDate)) withDate = true;
      const lbl = String(sp.label || sp.note || sp.reason || sp.detail || '').trim();
      const st = normState(sp.state || sp.status, lbl) || 'down';
      let r = rowByLabel.get(rowLbl);
      if (!r) {
        r = { label: rowLbl, id: rowId || '', spans: [] };
        rowByLabel.set(rowLbl, r);
        rows.push(r);
      }
      r.spans.push({
        t0, t1, state: st, label: lbl,
        fromStr: String(fromRaw ?? ''), toStr: String(toRaw ?? ''),
      });
    };

    rawItems.forEach((it, i) => {
      if (!it) return;
      const lbl = String(it.label || it.name || it.device || it.id || ('设备' + (i + 1))).trim();
      const spans = it.spans || it.events || it.periods || it.segments;
      if (Array.isArray(spans)) {
        spans.forEach(sp => addSpan(lbl, it.id, sp));
      } else {
        addSpan(lbl, it.id, it);
      }
    });

    if (!rows.length) return '';

    let tMin = Infinity, tMax = -Infinity;
    const startCounts = new Map();
    rows.forEach(r => r.spans.forEach(sp => {
      if (sp.t0 < tMin) tMin = sp.t0;
      if (sp.t1 > tMax) tMax = sp.t1;
      if (sp.state !== 'ok') {
        const k = Math.round(sp.t0);
        startCounts.set(k, (startCounts.get(k) || 0) + 1);
      }
    }));
    if (!Number.isFinite(tMin) || !Number.isFinite(tMax)) return '';
    const span = Math.max(10, tMax - tMin);
    const pad = Math.max(2, span * 0.06);
    const from = tMin - pad, to = tMax + pad;

    // 找出 2 台及以上设备同时开始异常的时刻
    const syncTimes = [];
    startCounts.forEach((cnt, t) => { if (cnt >= 2) syncTimes.push(t); });

    const W = 640;
    const maxLw = Math.max(...rows.map(r => strW(r.label)), 8);
    const left = Math.min(200, Math.max(108, Math.round(maxLw * 6.8 + 18)));
    const right = 20;
    const top = syncTimes.length ? 22 : 10;
    const bottom = 24;
    const ROW_H = 28;
    const h = rows.length * ROW_H;
    const H = top + h + bottom;
    const w = W - left - right;
    const xOf = t => left + (t - from) / (to - from) * w;

    let s = '';
    for (let i = 0; i <= 5; i++) {
      const tv = from + (to - from) * i / 5, x = xOf(tv);
      s += `<line class="gl" x1="${x.toFixed(1)}" x2="${x.toFixed(1)}" y1="${top}" y2="${top + h}"/>`;
      s += `<text class="axis" x="${x.toFixed(1)}" y="${H - 6}" text-anchor="middle">${esc(fmtTimelineTick(tv, mode, withDate))}</text>`;
    }

    let hasDevLink = false;
    const allStates = [];
    rows.forEach((r, idx) => {
      const ry = top + idx * ROW_H;
      const by = ry + 7, bh = 14;
      const dv = devAttrs((r.id ? r.id + ' ' : '') + r.label);
      if (dv.linked) hasDevLink = true;
      s += `<g class="ai-tl-row${dv.linked ? ' clickable' : ''}"${dv.attr}>
        <text x="${left - 8}" y="${ry + 18}" text-anchor="end" font-size="12" fill="#1a1a1a">${esc(truncW(r.label, Math.floor((left - 14) / 6.5)))}</text>
        <rect x="${left}" y="${by}" width="${w}" height="${bh}" rx="1" fill="#ebe6da"/>`;
      r.spans.forEach(sp => {
        allStates.push(sp.state);
        const meta = STATE_META[sp.state] || STATE_META.down;
        const x1 = Math.max(left, xOf(sp.t0));
        const x2 = Math.min(left + w, Math.max(x1 + 5, xOf(sp.t1)));
        const bw = x2 - x1;
        const timeRange = sp.fromStr && sp.toStr && sp.fromStr !== sp.toStr ? `${sp.fromStr} ~ ${sp.toStr}` : (sp.fromStr || fmtTimelineTick(sp.t0, mode, withDate));
        const tip = `<b>${esc(r.label)}</b><br>时段：${esc(timeRange)}（${esc(meta.label)}）${sp.label ? '<br>' + esc(sp.label) : ''}${dv.linked ? '<br><span class="tip-hint">点击查看设备详情 →</span>' : ''}`;
        s += `<g data-tip="${esc(tip)}">
          <rect x="${x1.toFixed(1)}" y="${by}" width="${bw.toFixed(1)}" height="${bh}" rx="1" fill="${meta.color}"/>
          ${sp.label && bw >= 54 ? `<text x="${(x1 + bw / 2).toFixed(1)}" y="${ry + 17.5}" text-anchor="middle" font-size="10" fill="#fff">${esc(truncW(sp.label, Math.floor(bw / 6.5)))}</text>` : ''}
        </g>`;
      });
      s += `</g>`;
    });

    syncTimes.slice(0, 4).forEach(t => {
      const sx = xOf(t);
      s += `<line class="warnl" x1="${sx.toFixed(1)}" x2="${sx.toFixed(1)}" y1="${top - 2}" y2="${top + h}" stroke="#d63a22"/>`;
      s += `<text class="axis" x="${sx.toFixed(1)}" y="${top - 6}" text-anchor="middle" fill="#d63a22">${esc(fmtTimelineTick(t, mode, false) + ' 同时异常')}</text>`;
    });

    const svg = `<svg viewBox="0 0 ${W} ${H}" height="${H}">${s}</svg>`;
    return wrapChart('异常时间线', spec.title || '多设备异常时段对齐', stateLegend(allStates), svg, spec.note || spec.summary || spec.footer, hasDevLink);
  }

  // ---------------------------------------------------------------------------
  // 5. Mermaid 流程图（graph / flowchart）轻量解析转故障路径图
  function mermaidToPathSpec(raw) {
    const lines = String(raw || '').replace(/\r/g, '').split('\n');
    const nodes = new Map();
    const edges = [];
    let title = '';

    const parseNodeToken = tok => {
      tok = String(tok || '').trim();
      if (!tok) return null;
      const m = tok.match(/^([A-Za-z0-9_.-]+)\s*(?:\[\s*"?([\s\S]*?)"?\s*\]|\(\s*"?([\s\S]*?)"?\s*\)|\{\s*"?([\s\S]*?)"?\s*\})?$/);
      const id = (m ? m[1] : tok).trim();
      const rawLbl = (m && (m[2] || m[3] || m[4]) ? (m[2] || m[3] || m[4]) : '').replace(/<br\s*\/?>|\\n/gi, '\n').trim();
      if (!nodes.has(id)) nodes.set(id, { id, label: id, sub: '' });
      if (rawLbl) {
        const parts = rawLbl.split(/\n/).map(x => x.trim()).filter(Boolean);
        const cur = nodes.get(id);
        cur.label = parts[0] || id;
        if (parts.length > 1) cur.sub = parts.slice(1).join(' · ');
      }
      return id;
    };

    for (const rawLine of lines) {
      const l = rawLine.trim();
      if (!l || l.startsWith('%%')) continue;
      if (/^(?:graph|flowchart)\b/i.test(l) || /^(?:subgraph|end|classDef|class|direction)\b/i.test(l)) continue;
      const tm = l.match(/^title\s*[:：]?\s*(.+)$/i);
      if (tm) { title = tm[1].trim(); continue; }
      if (/^style\s+/i.test(l)) {
        const sm = l.match(/^style\s+(\S+)\s+(.+)$/i);
        if (sm && nodes.has(sm[1])) {
          const st = /#[fF dD eE][0-9a-fA-F]{2,5}|red|#d63a22/i.test(sm[2]) ? 'down' : /#[fF][fF a-eA-E]|orange|yellow/i.test(sm[2]) ? 'loss' : /#[2-9][eE a-fA-F]|green/i.test(sm[2]) ? 'ok' : '';
          if (st) nodes.get(sm[1]).state = st;
        }
        continue;
      }
      // 解析边：A -->|label| B 或 A -- label --> B 或 A --> B
      const edgeRe = /^(.+?)\s*(?:--\s*([^-=>]+?)\s*-->|-\.-\s*([^-=>]+?)\s*\.->|([-=.]+>))\s*(?:\|([^|]*)\|\s*)?(.+)$/;
      const em = l.match(edgeRe);
      if (em) {
        const from = parseNodeToken(em[1]);
        const to = parseNodeToken(em[6]);
        const lbl = (em[2] || em[3] || em[5] || '').trim();
        const dashed = (em[4] && em[4].includes('.')) || !!em[3];
        if (from && to) edges.push({ from, to, label: lbl, state: dashed ? 'loss' : '' });
      } else {
        parseNodeToken(l);
      }
    }
    if (!nodes.size) return null;
    return { type: 'path', title: title || '网络拓扑与故障路径', nodes: Array.from(nodes.values()), edges };
  }

  // ---------------------------------------------------------------------------
  // AI 图表交互（事件委托：悬停提示与点击跳转设备详情）
  if (typeof document !== 'undefined') {
    document.addEventListener('mousemove', ev => {
      const t = ev.target && ev.target.closest ? ev.target.closest('.ai-chart [data-tip]') : null;
      document.querySelectorAll('.ai-chart .tip:not(.hidden)').forEach(el => {
        if (!t || !el.parentElement.contains(t)) el.classList.add('hidden');
      });
      if (!t) return;
      const fig = t.closest('.ai-chart');
      const tip = fig && fig.querySelector('.tip');
      if (!tip) return;
      tip.innerHTML = t.getAttribute('data-tip') || '';
      tip.classList.remove('hidden');
      const r = fig.getBoundingClientRect();
      const px = ev.clientX - r.left, py = ev.clientY - r.top;
      const leftPos = px + 14 + tip.offsetWidth > r.width ? Math.max(6, px - tip.offsetWidth - 12) : px + 14;
      tip.style.left = leftPos + 'px';
      tip.style.top = Math.max(6, py - 10) + 'px';
    });
    document.addEventListener('click', ev => {
      const el = ev.target && ev.target.closest ? ev.target.closest('.ai-chart [data-dev]') : null;
      if (!el) return;
      const id = el.getAttribute('data-node-id') || (resolveDev(el.getAttribute('data-dev')) || {}).id;
      if (id) location.hash = '#/node/' + encodeURIComponent(id);
    });
  }
})();
