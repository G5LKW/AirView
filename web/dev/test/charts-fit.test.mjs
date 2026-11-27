// charts.js label fitting (r101 charts). Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import * as charts from '../../static/js/charts.js';

test('fitLabels: containLabel grids also contain the axis names', () => {
  const opt = charts.fitLabels({ grid: { left: 8, top: 18, containLabel: true }, xAxis: { name: 'K (dB)' }, yAxis: { name: 'windows' } });
  assert.equal(opt.grid.containLabel, undefined);
  assert.equal(opt.grid.outerBoundsMode, 'same');
  assert.equal(opt.grid.outerBoundsContain, 'all');
  assert.equal(opt.grid.left, 8);
});

test('fitLabels: arrays, opt-outs and partial options', () => {
  const opt = charts.fitLabels({ grid: [{ containLabel: true }, { containLabel: false }, { containLabel: true, outerBoundsContain: 'axisLabel' }] });
  assert.equal(opt.grid[0].outerBoundsContain, 'all');
  assert.equal(opt.grid[1].containLabel, false);
  assert.equal(opt.grid[1].outerBoundsMode, undefined);
  assert.equal(opt.grid[2].outerBoundsContain, 'axisLabel');
  assert.deepEqual(charts.fitLabels({ series: [] }), { series: [] });
  assert.equal(charts.fitLabels(null), null);
});

test('presets: every cartesian preset contains its axis names after fitting', () => {
  const presets = [
    charts.line({ x: ['a', 'b'], xName: 'hour', yName: 'msg/s', series: [{ name: 'a', data: [1, 2] }] }),
    charts.bar({ x: ['a'], xName: 'day', yName: 'n', y2Name: 'km', series: [{ name: 'a', data: [1] }, { name: 'b', data: [2] }] }),
    charts.scatter({ xName: 'km', yName: 'dBFS', series: [{ data: [[1, 2]] }] }),
    charts.heatmap({ x: ['a'], y: ['b'], data: [[0, 0, 1]], xName: 'hour', yName: 'day' }),
  ];
  for (const p of presets) {
    charts.fitLabels(p);
    const grids = Array.isArray(p.grid) ? p.grid : [p.grid];
    for (const g of grids) {
      assert.equal(g.containLabel, undefined);
      assert.equal(g.outerBoundsContain, 'all');
    }
  }
  assert.equal(charts.theme.grid.outerBoundsContain, 'all');
  assert.equal(charts.theme.grid.containLabel, undefined);
  // the sparkline stays edge to edge
  const sp = charts.fitLabels(charts.sparklineOption({ data: [1, 2, 3] }));
  assert.equal(sp.grid.containLabel, false);
  assert.equal(sp.grid.outerBoundsMode, undefined);
});

test('polar preset leaves room for angle labels at the card edge', () => {
  const p = charts.polar({ series: [{ data: [[1, 0]] }] });
  assert.ok(parseFloat(p.polar.radius) <= 72, `radius ${p.polar.radius}`);
});
