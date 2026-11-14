// 无第三方依赖；直接执行交付的 charts.js，测试其公开接口。
const { test, beforeEach } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
let charts;
beforeEach(() => {
  const context = { window: {} };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'static/charts.js'), 'utf8'), context);
  charts = context.window.Charts;
});

const specs = [
  { type: 'path', nodes: [{ id: 'fw', label: '防火墙', state: 'ok' }, { id: 'ap', label: '无线AP', state: 'down' }], edges: [{ from: 'fw', to: 'ap', label: 'LAN1' }] },
  { type: 'bar', items: [{ label: '防火墙', value: 12 }, { label: '无线AP', value: 0 }], unit: 'ms', threshold: 10 },
  { type: 'line', points: [{ label: '10:00', avg: 12, min: 1, max: 20 }, { label: '10:01', avg: -1, loss: 100 }, { label: '10:02', avg: 8, min: 2, max: 15 }], unit: 'ms' },
  { type: 'timeline', items: [{ label: '防火墙', from: '10:00', to: '10:05', state: 'down' }, { label: '无线AP', from: '10:00', to: '10:03', state: 'loss' }] },
];
for (const spec of specs) {
  test(`${spec.type}: JSON 代码块转为有效 SVG 图表`, () => {
    const html = charts.aiBlock('chart', JSON.stringify(spec), true);
    assert.match(html, /<figure class="ai-chart">/);
    assert.match(html, /<svg viewBox=/);
    assert.doesNotMatch(html, /NaN|Infinity|undefined/);
    if (spec.type === 'timeline') assert.match(html, /同时异常/);
  });
}

test('流式输出：未闭合时显示等待，完成后才渲染图表', () => {
  assert.match(charts.aiBlock('chart', '{"type":"path",', false), /ai-chart-pending/);
  assert.doesNotMatch(charts.aiBlock('chart', '{"type":"path",', false), /<svg/);
  assert.equal(charts.aiBlock('json', '{"devices":[]}', true), '');
  assert.equal(charts.aiBlock('javascript', 'alert(1)', true), '');
  assert.equal(charts.aiBlock('chart', 'broken json', true), '');
  assert.equal(charts.aiBlock('chart', '', true), '');
});

test('兼容带注释、尾逗号、数组以及逐行 JSON 的模型回答', () => {
  assert.match(charts.aiBlock('bar', '{//注释\n"items":[{"label":"AP","value":2,}],}', true), /<svg/);
  for (const raw of [JSON.stringify(specs.slice(0, 2)), specs.slice(0, 2).map(JSON.stringify).join('\n')]) {
    assert.equal((charts.aiBlock('chart', raw, true).match(/<figure/g) || []).length, 2);
  }
});

test('Mermaid 故障路径转换保留节点和连线说明', () => {
  const html = charts.aiBlock('mermaid', 'flowchart LR\nFW[防火墙] -->|LAN1| AP[无线AP]', true);
  for (const text of ['<svg', '防火墙', '无线AP', 'LAN1']) assert.ok(html.includes(text), text);
});

test('模型文本在标题、备注、设备标签与属性中转义', () => {
  const payload = '"><img src=x onerror=alert(1)>';
  const html = charts.aiChart({ type: 'bar', title: payload, note: payload, items: [{ label: payload, value: 5, note: payload }] });
  assert.doesNotMatch(html, /<img|<script/);
  assert.match(html, /&lt;img/);
});

test('设备解析器传递跳转 ID，解除解析器后移除跳转提示', () => {
  charts.setDeviceResolver(() => ({ id: 'fw-1' }));
  assert.match(charts.aiChart(specs[0]), /data-node-id="fw-1"/);
  assert.match(charts.aiChart(specs[0]), /点击设备可看详情/);
  charts.setDeviceResolver(null);
  assert.doesNotMatch(charts.aiChart(specs[0]), /data-node-id|点击设备可看详情/);
});

test('空数据与单个采样点不产生无效坐标', () => {
  for (const type of ['bar', 'line', 'timeline', 'path']) assert.equal(charts.aiChart({ type }), '');
  const html = charts.aiChart({ type: 'line', points: [{ label: '10:00', avg: 0 }] });
  assert.match(html, /<svg/);
  assert.doesNotMatch(html, /NaN|Infinity/);
});
