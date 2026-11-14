/* 拓扑图：交换机 / 端口 / 终端卡片的布局与 SVG 渲染，支持拖动、缩放、平移。 */
(function () {
  'use strict';
  const esc = s => String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  const natural = (a, b) => String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });

  const CARD_W = 214, CARD_H = 40, GAP = 6, GPAD = 8, GHEAD = 18;
  const SW_W = 124, SW_HEAD = 76, SW_FOOT = 24, PITCH = 17;
  // 卡片列：与交换机的距离、走线通道间距、卡片块之间的竖向间距、同一对端口的出线错开量
  const LANE0 = 18, LANE = 6, TAIL = 46, VGAP = 12;
  // 卡片上方的标签（自动发现、存疑、告警等）高出卡片 7px，上下相邻的卡片要留出这段空间
  const ROW = CARD_H + 14;
  const JW = 17, JH = 13, SFPW = 21, COLGAP = 6, BLOCK_GAP = 6, SFP_GAP = 16;

  const FLAG_TAGS = { gateway: '默认网关', suspect: '存疑', unlocated: '未定位', jump_host: '跳板机', no_ipv4: '无 IPv4', critical: '关键', discovered: '自动发现', ai_discovered: 'AI 发现' };

  function trunc(s, n) { s = String(s || ''); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
  // 按显示宽度截断：中文等宽字符算 1.7 个西文字符宽
  function truncW(s, max) {
    s = String(s || '');
    let w = 0;
    for (let i = 0; i < s.length; i++) {
      w += s.charCodeAt(i) > 0x2e7f ? 1.7 : 1;
      if (w > max) return s.slice(0, Math.max(1, i - 1)) + '…';
    }
    return s;
  }

  // ---------------------------------------------------------------------------
  // 布局
  function computeLayout(topo, overrides) {
    const L = { switches: {}, cards: [], groups: [], links: [], unl: null, bbox: null };
    const nodePos = n => overrides.nodes[n.id] || n.position;
    const swByID = {};
    topo.switches.forEach(s => { swByID[s.id] = s; });

    // 每台交换机的端口行
    topo.switches.forEach(sw => {
      const pos = overrides.switches[sw.id] || sw.position || { x: 100, y: 40 };
      const side = sw.node_side === 'left' ? 'left' : 'right';
      const cols = Math.max(1, sw.node_columns || 2);
      const linkPorts = {};
      topo.links.forEach(l => {
        if (l.from.switch === sw.id) linkPorts[l.from.port] = l.to.switch;
        if (l.to.switch === sw.id) linkPorts[l.to.port] = l.from.switch;
      });
      const byPort = {};
      topo.nodes.forEach(n => {
        if (n.attach && n.attach.switch === sw.id) {
          const p = n.attach.port || '?';
          (byPort[p] = byPort[p] || []).push(n);
        }
      });
      const used = Object.keys(linkPorts).concat(Object.keys(byPort));
      const blocks = portBlocks(sw, used);
      const W = SW_W, cx = pos.x + W / 2;
      const rows = [], jacks = [], faces = [];
      let y = pos.y + SW_HEAD;
      // 端口始终按实物间距排列，不随挂接设备多少而拉开
      blocks.forEach((blk, bi) => {
        if (bi) y += SFP_GAP;
        const top = y;
        const jw = blk.kind === 'sfp' ? SFPW : JW;
        for (let i = 0; i < blk.ports.length; i += 2) {
          if (i && i % 8 === 0) y += BLOCK_GAP;
          const pair = [blk.ports[i], blk.ports[i + 1]];
          const isUsed = p => p != null && (p in linkPorts || p in byPort);
          // 所有端口（包括级联口）一律从自己卡扣朝向的一侧出线（左列朝左、右列朝右），两列的线不会交叠
          const cy = y + PITCH / 2;
          pair.forEach((p, col) => {
            if (p == null) return;
            const used = isUsed(p);
            const wy = cy;
            if (used) {
              const all = byPort[p] || [];
              rows.push({ port: p, col, cy, wy, exit: col ? 1 : -1, all, auto: all.filter(n => !nodePos(n)), link: linkPorts[p] });
            }
            jacks.push({ port: p, col, kind: blk.kind, cy, wy, used, link: linkPorts[p],
              x: col ? cx + COLGAP / 2 : cx - COLGAP / 2 - jw, w: jw });
          });
          y += PITCH;
        }
        faces.push({ kind: blk.kind, y0: top - 5, y1: y + 3 });
      });
      const h = y - pos.y + 10 + SW_FOOT;
      L.switches[sw.id] = { sw, x: pos.x, y: pos.y, w: W, h, side, cols, rows, jacks, faces, rowByPort: {},
        total: blocks.reduce((n, b) => n + b.ports.filter(p => p !== '?').length, 0) };
      rows.forEach(r => { L.switches[sw.id].rowByPort[r.port] = r; });
    });

    // 终端卡片（自动排布部分）：在交换机旁排成一列。
    // 与卡片同侧出线的端口（direct）尽量与端口对齐；另一列的端口（wrap）从自己一侧出线，
    // 绕过交换机底部回到卡片一侧，这些卡片排在 direct 卡片下方。
    Object.values(L.switches).forEach(S => {
      const right = S.side === 'right', dir = right ? 1 : -1;
      const edge = right ? S.x + S.w : S.x, far = right ? S.x : S.x + S.w, bottom = S.y + S.h;
      const mk = r => {
        const n = r.auto.length, c = Math.min(S.cols, n);
        const w = n === 1 ? CARD_W : c * CARD_W + (c - 1) * GAP + 2 * GPAD;
        const h = n === 1 ? CARD_H : GHEAD + Math.ceil(n / c) * ROW - (ROW - CARD_H) + 2 * GPAD;
        return { r, w, h, cols: c, want: r.wy - h / 2 };
      };
      const withCards = S.rows.filter(r => r.auto.length);
      const direct = withCards.filter(r => r.exit === dir).map(mk).sort((a, b) => a.r.wy - b.r.wy);
      // 绕行的线层层嵌套：越靠下的端口在越内圈，它的卡片排得越靠上，这样互不交叉
      const wrap = withCards.filter(r => r.exit !== dir).map(mk).sort((a, b) => b.r.wy - a.r.wy);

      // direct：一维避让，重叠的块合并成簇，簇的位置取各块理想位置的平均
      const clusters = [];
      direct.forEach(it => {
        let c = { items: [it], H: it.h, sum: it.want, T: it.want };
        while (clusters.length) {
          const p = clusters[clusters.length - 1];
          if (p.T + p.H + VGAP <= c.T) break;
          clusters.pop();
          const off = p.H + VGAP;
          c = { items: p.items.concat(c.items), H: off + c.H, sum: p.sum + c.sum - off * c.items.length };
          c.T = c.sum / c.items.length;
        }
        clusters.push(c);
      });
      let minTop = S.y;
      clusters.forEach(c => {
        let t = Math.max(c.T, minTop);
        c.items.forEach(it => { it.y = t; t += it.h + VGAP; });
        minTop = t;
      });
      // wrap：接在 direct 下面依次排
      let t = direct.length ? minTop + VGAP : S.y;
      wrap.forEach(it => { it.y = t; t += it.h + VGAP; });
      direct.concat(wrap).forEach(it => { it.ty = it.y + it.h / 2; it.lane = -1; });

      // direct 走线通道：向下与向上的线各自分配，区间重叠的线用不同通道
      const assign = list => {
        const done = [];
        list.forEach(it => {
          const a = Math.min(it.r.wy, it.ty) - 3, b = Math.max(it.r.wy, it.ty) + 3;
          let lane = 0;
          done.forEach(o => { if (o.a < b && a < o.b) lane = Math.max(lane, o.lane + 1); });
          it.lane = lane;
          done.push({ a, b, lane });
        });
      };
      assign(direct.filter(it => it.ty > it.r.wy + 0.5).sort((a, b) => b.r.wy - a.r.wy));
      assign(direct.filter(it => it.ty < it.r.wy - 0.5).sort((a, b) => a.r.wy - b.r.wy));
      const dl = direct.reduce((m, it) => Math.max(m, it.lane + 1), 0);
      // wrap 通道放在 direct 通道外侧；底部横线放在交换机和所有 direct 竖线、wrap 卡片中心的下方
      const wz = LANE0 + dl * LANE + (dl ? 8 : 0);
      let base = bottom;
      direct.forEach(it => { base = Math.max(base, it.r.wy, it.ty); });
      wrap.forEach(it => { base = Math.max(base, it.ty); });
      base += 14;
      const reach = Math.max(dl ? LANE0 + (dl - 1) * LANE : 0, wrap.length ? wz + (wrap.length - 1) * LANE : 0, LANE0);
      const dist = reach + TAIL;
      direct.concat(wrap).forEach(it => {
        const r = it.r;
        const gx = right ? edge + dist : edge - dist - it.w;
        const gEdge = right ? gx : gx + it.w;
        const k = wrap.indexOf(it);
        if (k >= 0) {
          const ox = far - dir * (14 + k * LANE);       // 另一侧的竖线
          const by = base + k * LANE;                    // 底部横线
          const lx = edge + dir * (wz + k * LANE);       // 回到卡片一侧的竖线
          r.route = [[far, r.wy], [ox, r.wy], [ox, by], [lx, by], [lx, it.ty], [gEdge, it.ty]];
        } else {
          const lx = edge + dir * (LANE0 + Math.max(0, it.lane) * LANE);
          r.route = it.lane < 0
            ? [[edge, r.wy], [gEdge, r.wy]]
            : [[edge, r.wy], [lx, r.wy], [lx, it.ty], [gEdge, it.ty]];
        }
        r.label = r.auto.length === 1 ? { x: gEdge - dir * 5, y: it.ty - 4, anchor: right ? 'end' : 'start' } : null;
        if (r.auto.length === 1) {
          L.cards.push({ node: r.auto[0], x: gx, y: it.y, auto: true });
          return;
        }
        L.groups.push({ sw: S.sw, port: r.port, x: gx, y: it.y, w: it.w, h: it.h, count: r.all.length });
        r.auto.forEach((n, i) => {
          const col = i % it.cols, row = Math.floor(i / it.cols);
          L.cards.push({ node: n, auto: true, x: gx + GPAD + col * (CARD_W + GAP), y: it.y + GPAD + GHEAD + row * ROW });
        });
      });
    });

    // 指定了坐标的卡片
    // 未定位区放在交换机、自动排布的卡片和绕行连线的下方，左边与整张图对齐
    let maxBottom = -Infinity, minX = Infinity, maxX = -Infinity;
    const span = (x0, x1, y1) => { minX = Math.min(minX, x0); maxX = Math.max(maxX, x1); maxBottom = Math.max(maxBottom, y1); };
    Object.values(L.switches).forEach(S => {
      span(S.x, S.x + S.w, S.y + S.h);
      S.rows.forEach(r => (r.route || []).forEach(([x, y]) => span(x, x, y)));
    });
    L.cards.forEach(c => span(c.x, c.x + CARD_W, c.y + CARD_H));
    L.groups.forEach(g => span(g.x, g.x + g.w, g.y + g.h));
    if (!isFinite(maxBottom)) { maxBottom = 0; minX = 80; maxX = 80 + 3 * CARD_W; }
    const unlAuto = [];
    topo.nodes.forEach(n => {
      const p = nodePos(n);
      const S = n.attach && n.attach.switch ? L.switches[n.attach.switch] : null;
      if (p) {
        const card = { node: n, x: p.x, y: p.y, auto: false };
        if (S) {
          const r = S.rowByPort[n.attach.port || '?'];
          if (r) {
            card.wire = { x: r.exit > 0 ? S.x + S.w : S.x, y: r.wy, dir: r.exit, bottom: S.y + S.h };
          }
        }
        L.cards.push(card);
      } else if (!S) {
        unlAuto.push(n);
      }
    });
    // 按 IP 排序，列数随整张图的宽度（至少 3 列，最多 8 列）
    const ipKey = n => {
      const m = String(n.ip || '').match(/^(\d+)\.(\d+)\.(\d+)\.(\d+)$/);
      return m ? ((+m[1] * 256 + +m[2]) * 256 + +m[3]) * 256 + +m[4] : Infinity;
    };
    unlAuto.sort((a, b) => ipKey(a) - ipKey(b) || natural(a.name || a.mac || a.id, b.name || b.mac || b.id));
    const UW = CARD_W + GAP + 6;
    const ucols = Math.max(3, Math.min(8, Math.floor((maxX - minX + GAP + 6) / UW)));
    // 网格不能压住手动拖动过的卡片：有重叠就整体移到那张卡片下方
    const manual = L.cards.filter(c => !c.auto);
    const urows = Math.ceil(unlAuto.length / ucols);
    let uy = maxBottom + 60;
    if (unlAuto.length) {
      const gx0 = minX - 10, gx1 = minX + ucols * UW + 10;
      for (let guard = 0; guard < manual.length + 1; guard++) {
        const gy0 = uy - 28, gy1 = uy + (urows - 1) * ROW + CARD_H + 10;
        const hit = manual.filter(c => c.x < gx1 && c.x + CARD_W > gx0 && c.y - 8 < gy1 && c.y + CARD_H > gy0);
        if (!hit.length) break;
        uy = Math.max(...hit.map(c => c.y + CARD_H)) + 46;
      }
    }
    unlAuto.forEach((n, i) => {
      L.cards.push({ node: n, auto: true, unl: true, x: minX + (i % ucols) * UW, y: uy + Math.floor(i / ucols) * ROW });
    });

    // 未定位区域外框
    // 只框住自动排布的未定位卡片；手动拖到别处的不算，免得外框把交换机也圈进去
    const unl = L.cards.filter(c => c.unl);
    if (unl.length) {
      let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
      unl.forEach(c => { x0 = Math.min(x0, c.x); y0 = Math.min(y0, c.y); x1 = Math.max(x1, c.x + CARD_W); y1 = Math.max(y1, c.y + CARD_H); });
      L.unl = { x: x0 - 10, y: y0 - 32, w: x1 - x0 + 20, h: y1 - y0 + 42, count: unl.length };
    }

    // 交换机间链路
    // 级联口从自己一侧出线；对端交换机在另一侧时，先绕过本交换机底部再连过去，不从相邻网口上穿过
    const linkEnd = (S, r, face) => {
      const edge = d => d > 0 ? S.x + S.w : S.x;
      if (!r) return { pts: [[edge(face), S.y + 40]] };
      const out = r.exit;
      if (out === face) return { pts: [[edge(out), r.wy]] };
      const ox = edge(out) + out * 10, yb = S.y + S.h + 10, px = edge(face) + face * 10;
      return { pts: [[edge(out), r.wy], [ox, r.wy], [ox, yb], [px, yb]], wrapped: true };
    };
    topo.links.forEach(l => {
      const A = L.switches[l.from.switch], B = L.switches[l.to.switch];
      if (!A || !B) return;
      const aRight = A.x + A.w / 2 < B.x + B.w / 2;
      const ea = linkEnd(A, A.rowByPort[l.from.port], aRight ? 1 : -1);
      const eb = linkEnd(B, B.rowByPort[l.to.port], aRight ? -1 : 1);
      const pa = ea.pts[ea.pts.length - 1], pb = eb.pts[eb.pts.length - 1];
      const mx = (pa[0] + pb[0]) / 2;
      const pts = ea.pts.concat([[mx, pa[1]], [mx, pb[1]]], eb.pts.slice().reverse());
      L.links.push({ link: l, pts, mx, my: Math.min(pa[1], pb[1]), A, B });
    });

    // 外接矩形
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    const grow = (x, y, w, h) => { x0 = Math.min(x0, x); y0 = Math.min(y0, y); x1 = Math.max(x1, x + w); y1 = Math.max(y1, y + h); };
    Object.values(L.switches).forEach(S => grow(S.x, S.y, S.w, S.h));
    L.cards.forEach(c => grow(c.x, c.y, CARD_W, CARD_H));
    L.groups.forEach(g => grow(g.x, g.y, g.w, g.h));
    if (L.unl) grow(L.unl.x, L.unl.y, L.unl.w, L.unl.h);
    L.bbox = isFinite(x0) ? { x: x0, y: y0, w: x1 - x0, h: y1 - y0 } : { x: 0, y: 0, w: 800, h: 600 };
    return L;
  }

  // ---------------------------------------------------------------------------
  // 渲染
  function latText(st, node) {
    if (!node.ip) return { t: '不监控', c: 'disabled' };
    if (node.monitor && node.monitor.enabled === false) return { t: '已停用', c: 'disabled' };
    if (!st) return { t: '…', c: 'unknown' };
    if (st.state === 'paused') return { t: '暂停', c: 'paused' };
    if (st.state === 'down') return { t: '离线', c: 'down' };
    if (st.state === 'unknown') return { t: '…', c: 'unknown' };
    const v = st.last_rtt >= 0 ? st.last_rtt : null;
    let t = v == null ? '超时' : Charts.fmtMs(v);
    if (st.loss_1m > 0) t += ` ${Math.round(st.loss_1m)}%`;
    return { t, c: v == null ? 'loss' : st.state };
  }

  function renderCard(c, ctx) {
    const n = c.node;
    const st = ctx.status(n.id);
    const lt = latText(st, n);
    const flags = n.flags || [];
    const cls = ['card'];
    if (flags.includes('gateway')) cls.push('gateway');
    if (lt.c === 'down' || lt.c === 'slow' || lt.c === 'loss') cls.push(lt.c);
    if (lt.c === 'disabled') cls.push('disabled');
    if (ctx.sel && ctx.sel.kind === 'node' && ctx.sel.id === n.id) cls.push('sel');
    const m = ctx.match ? ctx.match(n) : null;
    if (m === false) cls.push('fade');
    if (m === true && ctx.searching) cls.push('hit');
    const color = ctx.vendorColor(n.vendor);
    // 右上角：名称 > 主机名 > 厂商
    const right = truncW(n.name || n.hostname || n.vendor || '', 15);
    let s = `<g class="${cls.join(' ')}" data-kind="node" data-id="${esc(n.id)}" transform="translate(${c.x},${c.y})">`;
    s += `<rect class="bg" width="${CARD_W}" height="${CARD_H}"/>`;
    if (flags.includes('suspect')) s += `<rect width="${CARD_W}" height="${CARD_H}" fill="url(#hatch)" pointer-events="none"/>`;
    s += `<rect x="0" y="0" width="5" height="${CARD_H}" fill="${color}"/>`;
    s += `<text class="ip" x="12" y="16">${esc(n.ip || '无 IPv4')}</text>`;
    s += `<text class="vendor" x="${CARD_W - 7}" y="15" text-anchor="end">${esc(right)}</text>`;
    s += `<text class="mac" x="12" y="32">${esc(n.mac || 'MAC 未记录')}</text>`;
    s += `<text class="lat ${lt.c}" x="${CARD_W - 7}" y="32" text-anchor="end">${esc(lt.t)}</text>`;
    // 标记标签（最右侧为“告警”）
    let tx = CARD_W - 4;
    if (n.monitor && n.monitor.alert) {
      tx -= 28;
      s += `<rect class="tag-bg al" x="${tx}" y="-9" width="28" height="13"/><text class="tag" x="${tx + 14}" y="1" text-anchor="middle">告警</text>`;
      tx -= 3;
    }
    const tagList = flags.slice();
    if (n.ip_mode === 'dhcp') tagList.push('_dhcp');
    tagList.reverse().forEach(f => {
      const label = f === '_dhcp' ? 'DHCP' : FLAG_TAGS[f];
      if (!label || f === 'unlocated') return;
      const w = f === '_dhcp' ? 30 : label.length * 10 + 8;
      tx -= w;
      const outline = f === 'suspect' ? ' o' : '';
      s += `<rect class="tag-bg${outline}" x="${tx}" y="-9" width="${w}" height="13"/>`;
      s += `<text class="tag${outline}" x="${tx + w / 2}" y="1" text-anchor="middle">${label}</text>`;
      tx -= 3;
    });
    s += `<title>${esc([n.name, n.hostname && n.hostname !== n.name ? '主机名 ' + n.hostname : '', n.ip, n.mac, n.vendor, ctx.attachLabel(n), n.location, n.note].filter(Boolean).join('\n'))}</title>`;
    return s + '</g>';
  }

  // ---------------------------------------------------------------------------
  // 交换机竖放：端口按实物排列（每对端口左奇右偶，每 8 口一组），光口单独一组放在下方
  function portKind(name) {
    return /^(te|tf|tg|xg|xge|ten|sfp|fo|forty|hu|twe|25g|40g|100g)/i.test(String(name)) ? 'sfp' : 'rj45';
  }
  function shortPort(p) { const m = String(p).match(/(\d+)\s*$/); return m ? m[1] : String(p).slice(-3); }

  function portBlocks(sw, used) {
    const list = (sw.ports || []).slice();
    const extra = used.filter(p => p !== '?' && !list.includes(p)).sort(natural);
    extra.forEach(p => list.push(p));
    let copper = list.filter(p => portKind(p) === 'rj45');
    let sfp = list.filter(p => portKind(p) === 'sfp');
    // 纯数字编号：超出 8 的整数倍的尾部端口按光口画（24+2、24+4、48+4 等常见型号）
    if (!sfp.length && (sw.ports || []).length > 8 && copper.length % 8 && copper.every(p => /^\d+$/.test(p))) {
      const n = copper.length - copper.length % 8;
      sfp = copper.slice(n);
      copper = copper.slice(0, n);
    }
    if (used.includes('?')) copper.push('?');
    const out = [];
    if (copper.length) out.push({ kind: 'rj45', ports: copper });
    if (sfp.length) out.push({ kind: 'sfp', ports: sfp });
    return out;
  }

  // 节点 / 端口状态 → 指示灯样式
  function nodeLed(n, ctx) {
    if (!n.ip || (n.monitor && n.monitor.enabled === false)) return 'idle';
    const st = ctx.status(n.id);
    if (!st) return 'idle';
    if (st.state === 'down') return 'down';
    if (st.state === 'slow' || st.state === 'loss') return 'warn';
    if (st.state === 'ok') return 'ok';
    return 'idle';
  }
  const LED_RANK = { down: 4, warn: 3, ok: 2, idle: 1, none: 0 };
  function portState(S, j, ctx) {
    if (j.link) {
      const o = ctx.L.switches[j.link];
      const st = o && o.sw.ip ? ctx.status(o.sw.id) : null;
      return { led: st && st.state === 'down' ? 'down' : 'link', desc: '级联 ' + (o ? (o.sw.name || o.sw.id) : j.link) };
    }
    const r = S.rowByPort[j.port];
    if (!r) return { led: 'none', desc: '空闲' };
    let led = 'none';
    r.all.forEach(n => { const l = nodeLed(n, ctx); if (LED_RANK[l] > LED_RANK[led]) led = l; });
    const names = r.all.map(n => n.name || n.ip || n.mac || n.id);
    return { led, desc: `${r.all.length} 台：${names.slice(0, 6).join('、')}${names.length > 6 ? ' 等' : ''}` };
  }

  // 竖放的网口：卡扣朝外（左列朝左、右列朝右），左右两侧各一个指示灯
  function jackSVG(j, led) {
    const x = j.x, y = j.cy - JH / 2;
    if (j.kind === 'sfp') {
      return `<rect class="sfp-cage" x="${x}" y="${y}" width="${j.w}" height="${JH}"/>` +
        `<rect class="sfp-hole" x="${x + 3}" y="${y + 3}" width="${j.w - 6}" height="${JH - 6}"/>` +
        `<rect class="led ${led}" x="${j.col ? x + j.w - 3 : x + 1}" y="${y + 1}" width="2" height="2"/>`;
    }
    const w = j.w, h = JH, tab = 4, tw = 5;
    const ty = y + (h - tw) / 2;
    const d = j.col
      ? `M${x},${y}H${x + w - tab}V${ty}H${x + w}V${ty + tw}H${x + w - tab}V${y + h}H${x}Z`
      : `M${x + w},${y}H${x + tab}V${ty}H${x}V${ty + tw}H${x + tab}V${y + h}H${x + w}Z`;
    const pinX = j.col ? x + 2 : x + w - 3.2;
    const act = led === 'ok' || led === 'link' ? 'act' : 'none';
    return `<path class="jack" d="${d}"/><path class="pins" d="M${pinX},${y + 2.5}v${h - 5}"/>` +
      `<rect class="led ${led}" x="${j.col ? x + w - tab - 3 : x + tab + 1}" y="${y + 1.2}" width="2" height="2.6"/>` +
      `<rect class="led ${act}" x="${j.col ? x + w - tab - 3 : x + tab + 1}" y="${y + h - 3.8}" width="2" height="2.6"/>`;
  }

  function renderSwitch(S, ctx) {
    const sw = S.sw;
    const dark = sw.role === 'core' || sw.role === 'aggregation';
    const sel = ctx.sel && ctx.sel.kind === 'switch' && ctx.sel.id === sw.id;
    const st = sw.ip ? ctx.status(sw.id) : null;
    const R = S.x + S.w, cx = S.x + S.w / 2;
    let s = `<g class="sw ${dark ? 'sw-dark' : 'sw-light'}${sel ? ' sel' : ''}">`;
    s += `<rect class="sw-body${dark ? '' : ' light'}" x="${S.x}" y="${S.y}" width="${S.w}" height="${S.h}" rx="3"/>`;
    s += `<rect class="sw-cap" x="${S.x}" y="${S.y}" width="${S.w}" height="4" rx="2"/>`;
    // 头部（拖动手柄）
    s += `<g class="head" data-kind="switch" data-id="${esc(sw.id)}">`;
    s += `<rect x="${S.x}" y="${S.y}" width="${S.w}" height="${SW_HEAD - 6}" fill="transparent"/>`;
    const roleTxt = ({ core: 'CORE', aggregation: 'AGG', access: 'ACCESS', other: 'SWITCH' }[sw.role] || 'SWITCH') + (sw.layer ? ' · ' + sw.layer : '');
    s += `<text class="role" x="${S.x + 8}" y="${S.y + 17}">${esc(roleTxt)}</text>`;
    if (sw.monitor && sw.monitor.alert) {
      s += `<rect class="tag-bg al" x="${R - 34}" y="${S.y + 8}" width="28" height="13"/><text class="tag" x="${R - 20}" y="${S.y + 18}" text-anchor="middle">告警</text>`;
    }
    s += `<text class="name" x="${S.x + 8}" y="${S.y + 35}">${esc(trunc(sw.name || sw.id, 8))}</text>`;
    const brand = [sw.vendor, sw.model].filter(Boolean).join(' ');
    s += `<text class="ipt" x="${S.x + 8}" y="${S.y + 50}">${esc(sw.ip || '无管理 IP')}</text>`;
    if (st) {
      const lt = latText(st, { ip: sw.ip, monitor: sw.monitor });
      s += `<text class="ipt lat ${lt.c}" x="${S.x + 8}" y="${S.y + 63}">${esc(lt.t)}</text>`;
    } else if (brand) {
      s += `<text class="brand" x="${S.x + 8}" y="${S.y + 63}">${esc(trunc(brand, 14))}</text>`;
    }
    const sysLed = st ? (st.state === 'down' ? 'down' : st.state === 'ok' ? 'ok' : (st.state === 'slow' || st.state === 'loss') ? 'warn' : 'idle') : 'ok';
    [['PWR', 'ok'], ['SYS', sysLed]].forEach(([t, l], i) => {
      const lx = R - 30 + i * 18;
      s += `<circle class="led ${l}" cx="${lx}" cy="${S.y + 52}" r="2.4"/><text class="ledt" x="${lx}" y="${S.y + 63}" text-anchor="middle">${t}</text>`;
    });
    s += `<title>${esc([sw.name, brand, sw.ip, sw.mac, sw.location, sw.note].filter(Boolean).join('\n'))}</title></g>`;
    s += `<line class="sw-sep" x1="${S.x + 6}" x2="${R - 6}" y1="${S.y + SW_HEAD - 8}" y2="${S.y + SW_HEAD - 8}"/>`;
    // 端口面板底板
    S.faces.forEach(f => {
      const half = (f.kind === 'sfp' ? SFPW : JW) + COLGAP / 2 + 15;
      s += `<rect class="pn-face" x="${cx - half}" y="${f.y0}" width="${half * 2}" height="${f.y1 - f.y0}" rx="2"/>`;
      if (f.kind === 'sfp') s += `<text class="pn-num" x="${cx}" y="${f.y0 - 3}" text-anchor="middle">SFP</text>`;
    });
    // 跳线：从网口引到机身边缘（先画，网口盖在上面）
    const states = {};
    S.jacks.forEach(j => {
      const ps = portState(S, j, ctx);
      states[j.port] = ps;
      if (!j.used) return;
      const right = j.col === 1;
      const x0 = right ? j.x + j.w : j.x, x1 = right ? R : S.x;
      s += `<line class="patch ${ps.led}" x1="${x0}" y1="${j.wy}" x2="${x1}" y2="${j.wy}"/>`;
      const row = S.rowByPort[j.port];
      if (!j.link && row && !row.auto.length) {
        s += `<text class="plabel out" x="${right ? R + 4 : S.x - 4}" y="${j.wy - 4}" text-anchor="${right ? 'start' : 'end'}">${esc(j.port)}</text>`;
      }
    });
    S.jacks.forEach(j => {
      const ps = states[j.port];
      const ploc = sw.port_locations && sw.port_locations[j.port];
      // 网口可点击：先点网口、再点设备卡片即可把设备接到这个口
      const picked = ctx.pick && ctx.pick.sw === sw.id && ctx.pick.port === j.port;
      const attrs = j.port === '?' ? '' : ` data-kind="port" data-id="${esc(sw.id)}" data-port="${esc(j.port)}"`;
      s += `<g class="jk${j.used ? ' used' : ''}${picked ? ' picked' : ''}"${attrs}><title>${esc(j.port)}${ploc ? ' · ' + esc(ploc) : ''} · ${esc(ps.desc)}${j.port === '?' ? '' : '\n点击后再点设备卡片，把设备接到这个口'}</title>` +
        `<rect class="jk-hit" x="${j.x - 3}" y="${j.cy - PITCH / 2}" width="${j.w + 6}" height="${PITCH}"/>${jackSVG(j, ps.led)}` +
        (picked ? `<rect class="pick-ring" x="${j.x - 3}" y="${j.cy - JH / 2 - 3}" width="${j.w + 6}" height="${JH + 6}" rx="2"/>` : '') + '</g>';
      const nx = j.col ? j.x + j.w + 3 : j.x - 3;
      s += `<text class="pn-num${j.used ? ' u' : ''}" x="${nx}" y="${j.cy + 2.5}" text-anchor="${j.col ? 'start' : 'end'}">${esc(j.port === '?' ? '?' : shortPort(j.port))}</text>`;
    });
    // 底部：端口占用 + 散热孔
    const fy = S.y + S.h - SW_FOOT;
    s += `<line class="sw-sep" x1="${S.x + 6}" x2="${R - 6}" y1="${fy}" y2="${fy}"/>`;
    s += `<text class="ipt foot" x="${S.x + 8}" y="${fy + 15}">${S.rows.length}/${Math.max(S.total, S.rows.length)} 口</text>`;
    let v = '';
    for (let k = 0; k < 6; k++) v += `M${R - 10 - k * 6},${fy + 6}l-3,10`;
    s += `<path class="vent" d="${v}"/>`;
    return s + '</g>';
  }

  function render(svg, L, view, ctx) {
    let s = `<defs>
      <pattern id="grid" width="40" height="40" patternUnits="userSpaceOnUse"><path d="M40 0H0V40" fill="none" class="t-grid"/></pattern>
      <linearGradient id="swDarkG" x1="0" x2="1" y1="0" y2="0"><stop offset="0" stop-color="#2e2e2e"/><stop offset=".5" stop-color="#1f1f1f"/><stop offset="1" stop-color="#151515"/></linearGradient>
      <linearGradient id="swLightG" x1="0" x2="1" y1="0" y2="0"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#eeeae0"/></linearGradient>
      <pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="6" stroke="#8a857a" stroke-width="1" opacity=".35"/></pattern>
    </defs>`;
    s += `<g transform="translate(${view.tx},${view.ty}) scale(${view.k})">`;
    s += `<rect x="-20000" y="-20000" width="40000" height="40000" fill="url(#grid)" data-kind="bg"/>`;
    // 未定位区
    if (L.unl) {
      const u = L.unl;
      s += `<rect class="unl-box" x="${u.x}" y="${u.y}" width="${u.w}" height="${u.h}"/>`;
      s += `<text class="unl-t" x="${u.x + 8}" y="${u.y + 16}">未定位 / 仅 ARP · ${u.count} 条 · 未挂端口</text>`;
    }
    // 端口分组框
    L.groups.forEach(g => {
      s += `<rect class="grp" x="${g.x}" y="${g.y}" width="${g.w}" height="${g.h}"/>`;
      s += `<text class="grp-t" x="${g.x + 8}" y="${g.y + 15}"><tspan class="p">${esc(g.port)}</tspan>  ${g.count} MAC · 口后还有设备</text>`;
    });
    // 连线
    Object.values(L.switches).forEach(S => {
      S.rows.forEach(r => {
        if (!r.route) return;
        s += `<polyline class="wire" points="${r.route.map(p => p.join(',')).join(' ')}"/>`;
        if (r.label) s += `<text class="wlabel" x="${r.label.x}" y="${r.label.y}" text-anchor="${r.label.anchor}">${esc(r.port)}</text>`;
      });
    });
    L.cards.forEach(c => {
      if (!c.wire) return;
      const w = c.wire, toLeft = c.x > w.x;
      const cx = toLeft ? c.x : c.x + CARD_W, cy = c.y + CARD_H / 2;
      // 拐点靠近卡片，避开交换机旁的自动走线通道
      const mx = toLeft ? Math.max(w.x + 8, cx - 16) : Math.min(w.x - 8, cx + 16);
      let pts;
      if ((w.dir > 0) === toLeft) {
        pts = [[w.x, w.y], [mx, w.y], [mx, cy], [cx, cy]];
      } else {
        // 卡片在端口出线方向的反侧：先从端口一侧出去，再绕过交换机底部
        const ox = w.x + w.dir * 10, by = w.bottom + 10;
        const mx2 = toLeft ? cx - 16 : cx + 16;
        pts = [[w.x, w.y], [ox, w.y], [ox, by], [mx2, by], [mx2, cy], [cx, cy]];
      }
      s += `<polyline class="wire free" points="${pts.map(p => p.join(',')).join(' ')}"/>`;
    });
    // 交换机
    Object.values(L.switches).forEach(S => { s += renderSwitch(S, ctx); });
    // 交换机间链路
    L.links.forEach(k => {
      const d = 'M' + k.pts.map(p => p.join(',')).join(' L');
      const sel = ctx.sel && ctx.sel.kind === 'link' && ctx.sel.id === k.link.id;
      s += `<g data-kind="link" data-id="${esc(k.link.id)}"><path class="link-hit" d="${d}"/><path class="link-line${sel ? ' sel' : ''}" d="${d}"/></g>`;
      // 端口名标在出线处，朝线的走向
      const lbl = (p, next, text) => {
        const dir = next[0] >= p[0] ? 1 : -1;
        return `<text class="link-t" x="${p[0] + dir * 8}" y="${p[1] - 9}" text-anchor="${dir > 0 ? 'start' : 'end'}">${esc(text)}</text>`;
      };
      s += lbl(k.pts[0], k.pts[1], k.link.from.port);
      s += lbl(k.pts[k.pts.length - 1], k.pts[k.pts.length - 2], k.link.to.port);
      if (k.link.note) s += `<text class="link-t" x="${k.mx}" y="${k.my - 8}" text-anchor="middle">${esc(k.link.note)}</text>`;
    });
    // 卡片最后画，位于最上层
    L.cards.forEach(c => { s += renderCard(c, ctx); });
    s += '</g>';
    svg.innerHTML = s;
  }

  // ---------------------------------------------------------------------------
  class TopoView {
    constructor(svg, opts) {
      this.svg = svg;
      this.opts = opts;
      this.topo = null;
      this.view = { tx: 40, ty: 20, k: 0.8 };
      this.overrides = { nodes: {}, switches: {} };
      this.sel = null;
      this.match = null;
      this.searching = false;
      this.drag = null;
      this.lastClick = null;
      this.raf = 0;
      this.bind();
    }

    setTopology(topo, keepOverrides) {
      this.topo = topo;
      if (!keepOverrides) this.overrides = { nodes: {}, switches: {} };
      this.schedule();
    }
    setSelection(sel) { this.sel = sel; this.schedule(); }
    // 选中的网口（{sw, port}），之后点设备卡片即接到这个口
    setPick(p) { this.pick = p || null; this.svg.classList.toggle('picking', !!p); this.schedule(); }
    setMatch(fn, searching) { this.match = fn; this.searching = !!searching; this.schedule(); }
    refresh() { if (!this.drag || !this.drag.moved) this.schedule(); }
    isDragging() { return !!(this.drag && this.drag.moved); }

    schedule() {
      if (this.raf) return;
      this.raf = requestAnimationFrame(() => { this.raf = 0; this.draw(); });
    }

    draw() {
      if (!this.topo) return;
      this.L = computeLayout(this.topo, this.overrides);
      const o = this.opts;
      render(this.svg, this.L, this.view, {
        L: this.L, sel: this.sel, match: this.match, searching: this.searching, pick: this.pick,
        status: o.status, vendorColor: o.vendorColor, attachLabel: o.attachLabel,
      });
      if (o.onZoom) o.onZoom(this.view.k);
    }

    fit() {
      if (!this.topo) return;
      const L = this.L || computeLayout(this.topo, this.overrides);
      const r = this.svg.getBoundingClientRect();
      const m = 40;
      const k = Math.min(1.2, (r.width - 2 * m) / L.bbox.w, (r.height - 2 * m) / L.bbox.h);
      this.view.k = Math.max(0.15, k);
      this.view.tx = (r.width - L.bbox.w * this.view.k) / 2 - L.bbox.x * this.view.k;
      this.view.ty = m - L.bbox.y * this.view.k + Math.max(0, (r.height - 2 * m - L.bbox.h * this.view.k) / 2);
      this.schedule();
    }

    zoomAt(factor, sx, sy) {
      const r = this.svg.getBoundingClientRect();
      if (sx == null) { sx = r.width / 2; sy = r.height / 2; }
      const k2 = Math.min(3, Math.max(0.12, this.view.k * factor));
      this.view.tx = sx - (sx - this.view.tx) * k2 / this.view.k;
      this.view.ty = sy - (sy - this.view.ty) * k2 / this.view.k;
      this.view.k = k2;
      this.schedule();
    }

    // 把屏幕坐标转换为世界坐标
    toWorld(cx, cy) {
      const r = this.svg.getBoundingClientRect();
      return { x: (cx - r.left - this.view.tx) / this.view.k, y: (cy - r.top - this.view.ty) / this.view.k };
    }

    // 新节点等需要一个可见区域中心的坐标
    viewCenter() {
      const r = this.svg.getBoundingClientRect();
      return this.toWorld(r.left + r.width / 2, r.top + r.height / 2);
    }

    bind() {
      const svg = this.svg;
      svg.addEventListener('wheel', ev => {
        ev.preventDefault();
        const r = svg.getBoundingClientRect();
        this.zoomAt(Math.exp(-ev.deltaY * 0.0015), ev.clientX - r.left, ev.clientY - r.top);
      }, { passive: false });

      svg.addEventListener('pointerdown', ev => {
        if (ev.button !== 0) return;
        const el = ev.target.closest('[data-kind]');
        const kind = el ? el.getAttribute('data-kind') : 'bg';
        const id = el ? el.getAttribute('data-id') : null;
        const d = { kind, id, sx: ev.clientX, sy: ev.clientY, moved: false };
        if (kind === 'node' && this.L) {
          const c = this.L.cards.find(c => c.node.id === id);
          if (c) d.start = { x: c.x, y: c.y };
        } else if (kind === 'switch' && this.L) {
          const S = this.L.switches[id];
          if (S) d.start = { x: S.x, y: S.y };
        } else {
          d.start = { x: this.view.tx, y: this.view.ty };
          d.kind = kind === 'link' || kind === 'port' ? kind : 'bg';
          if (kind === 'port') d.port = el.getAttribute('data-port');
        }
        d.shift = ev.shiftKey;
        this.drag = d;
        svg.setPointerCapture(ev.pointerId);
      });

      svg.addEventListener('pointermove', ev => {
        const d = this.drag;
        if (!d) return;
        const dx = ev.clientX - d.sx, dy = ev.clientY - d.sy;
        if (!d.moved && Math.abs(dx) + Math.abs(dy) < 4) return;
        d.moved = true;
        if (d.kind === 'bg' || d.kind === 'link' || d.kind === 'port') {
          svg.classList.add('panning');
          this.view.tx = d.start.x + dx;
          this.view.ty = d.start.y + dy;
        } else if (d.kind === 'node' && d.start) {
          this.overrides.nodes[d.id] = { x: Math.round(d.start.x + dx / this.view.k), y: Math.round(d.start.y + dy / this.view.k) };
        } else if (d.kind === 'switch' && d.start) {
          this.overrides.switches[d.id] = { x: Math.round(d.start.x + dx / this.view.k), y: Math.round(d.start.y + dy / this.view.k) };
        }
        this.schedule();
      });

      const end = () => {
        const d = this.drag;
        this.drag = null;
        svg.classList.remove('panning');
        if (!d) return;
        if (!d.moved) {
          const now = Date.now();
          const dbl = this.lastClick && this.lastClick.id === d.id && now - this.lastClick.t < 400;
          this.lastClick = { id: d.id, t: now };
          if (d.kind === 'port') { if (this.opts.onPort) this.opts.onPort({ sw: d.id, port: d.port }); return; }
          if (this.pick && d.kind === 'node' && this.opts.onPickNode) { this.opts.onPickNode(d.id, d.shift); return; }
          if (this.pick && this.opts.onPort) this.opts.onPort(null); // 选了网口后点别处：取消
          if (d.kind === 'bg') { this.opts.onSelect(null); return; }
          if (dbl && this.opts.onOpen) { this.opts.onOpen({ kind: d.kind, id: d.id }); return; }
          this.opts.onSelect({ kind: d.kind, id: d.id });
          return;
        }
        if (d.kind === 'node') this.opts.onMove({ nodes: { [d.id]: this.overrides.nodes[d.id] } });
        if (d.kind === 'switch') this.opts.onMove({ switches: { [d.id]: this.overrides.switches[d.id] } });
      };
      svg.addEventListener('pointerup', end);
      svg.addEventListener('pointercancel', end);
    }
  }

  window.TopoView = TopoView;
  window.TopoFlags = FLAG_TAGS;
})();
