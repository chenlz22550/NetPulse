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

  window.Charts = { latency, loss, spark, fmtMs, fmtFull, esc };
})();
