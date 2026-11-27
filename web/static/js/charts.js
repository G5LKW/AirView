// charts.js - Apache ECharts wrapper with the AirView dark-blue theme.
//
// ECharts (1.1 MB) is loaded lazily on first use. charts.create() returns a handle immediately;
// options set before the library has arrived are queued and applied in order.
//
//   const c = ctx.charts.create(el, ctx.charts.line({ x: times, series: [{ name: 'Range', data: values }] }));
//   c.set(newOption);          // merge-update (pass { replace: true } to rebuild)
//   c.dispose();               // optional: ctx.charts disposes everything when the page unmounts
//
// Give the host element a height (e.g. class "chart", which uses --h, default 260px).
// Charts follow their container size automatically (ResizeObserver).

const ECHARTS_URL = new URL('../vendor/echarts/echarts.esm.min.mjs', import.meta.url).href;

/** Series colours, in order. */
export const palette = ['#3b8bff', '#22d3ee', '#fbbf24', '#a78bfa', '#34d399', '#f472b6', '#fb923c', '#94a3b8'];

/** Named colours matching css/tokens.css for use in chart options. */
export const colors = {
  blue: '#3b8bff', blueHi: '#6aa8ff', cyan: '#22d3ee', amber: '#fbbf24', violet: '#a78bfa', teal: '#2dd4bf',
  red: '#ff4d5e', green: '#34d399', grey: '#8494b3',
  text: '#e8f0ff', text2: '#adbfdf', text3: '#7388ad', text4: '#4a5d82',
  line: 'rgba(120,165,255,0.13)', line2: 'rgba(120,165,255,0.24)', panel: 'rgba(6,14,32,0.96)',
  adsb: '#22d3ee', mlat: '#fbbf24', modes: '#a78bfa', tisb: '#2dd4bf', other: '#8494b3',
};

/** Sequential ramps (low -> high) for heatmaps / visualMap. */
export const ramps = {
  blue: ['#04102a', '#0b2a6b', '#1552c8', '#3b8bff', '#8cc2ff', '#e6f2ff'],
  heat: ['#04102a', '#12308a', '#2f7dff', '#22d3ee', '#a3e635', '#fde047', '#fb923c', '#ff4d5e'],
  signal: ['#3b1d6e', '#3b4cc0', '#2f8fe0', '#22c7c0', '#6ee06a', '#f5e04a'],
  diverging: ['#ff4d5e', '#7a2a4a', '#0d1c3c', '#1c4fa8', '#22d3ee'],
};

const FONT_UI = "'Inter Variable','Segoe UI',system-ui,sans-serif";
const FONT_MONO = "'JetBrains Mono Variable','Cascadia Mono',Consolas,monospace";

const axisCommon = () => ({
  axisLine: { show: true, lineStyle: { color: colors.line2 } },
  axisTick: { show: false },
  axisLabel: { color: colors.text3, fontFamily: FONT_MONO, fontSize: 10.5, hideOverlap: true },
  splitLine: { show: true, lineStyle: { color: colors.line, type: [2, 4] } },
  splitArea: { show: false },
  nameTextStyle: { color: colors.text3, fontFamily: FONT_UI, fontSize: 11 },
  axisPointer: { lineStyle: { color: 'rgba(140,190,255,0.5)', type: 'dashed' }, label: { backgroundColor: '#12305f', color: colors.text, fontFamily: FONT_MONO, fontSize: 10.5 } },
});

/** The registered ECharts theme object ("airview"). */
export const theme = {
  color: palette,
  backgroundColor: 'transparent',
  textStyle: { fontFamily: FONT_UI, color: colors.text2, fontSize: 12 },
  title: { textStyle: { color: colors.text, fontWeight: 600, fontSize: 13 }, subtextStyle: { color: colors.text3 } },
  legend: { textStyle: { color: colors.text2, fontSize: 11.5 }, inactiveColor: colors.text4, icon: 'roundRect', itemWidth: 10, itemHeight: 10, itemGap: 14, pageTextStyle: { color: colors.text3 }, pageIconColor: colors.blueHi, pageIconInactiveColor: colors.text4 },
  tooltip: {
    backgroundColor: 'rgba(4,10,24,0.96)', borderColor: colors.line2, borderWidth: 1, padding: [8, 10],
    textStyle: { color: colors.text, fontSize: 12, fontFamily: FONT_UI },
    extraCssText: 'border-radius:8px;box-shadow:0 12px 32px rgba(0,4,20,.7);backdrop-filter:blur(8px);',
    axisPointer: { type: 'line', lineStyle: { color: 'rgba(140,190,255,0.45)', type: 'dashed' }, crossStyle: { color: 'rgba(140,190,255,0.45)' }, shadowStyle: { color: 'rgba(47,125,255,0.1)' } },
  },
  grid: { left: 8, right: 12, top: 28, bottom: 6, outerBoundsMode: 'same', outerBoundsContain: 'all', borderColor: colors.line },
  categoryAxis: { ...axisCommon(), splitLine: { show: false } },
  valueAxis: { ...axisCommon(), axisLine: { show: false } },
  logAxis: { ...axisCommon(), axisLine: { show: false } },
  timeAxis: { ...axisCommon(), splitLine: { show: false } },
  line: { symbol: 'circle', symbolSize: 5, showSymbol: false, smooth: false, lineStyle: { width: 1.8 }, emphasis: { focus: 'none' } },
  bar: { itemStyle: { borderRadius: [2, 2, 0, 0] }, barMaxWidth: 26 },
  scatter: { symbolSize: 5, itemStyle: { opacity: 0.8 } },
  heatmap: { itemStyle: { borderColor: 'rgba(2,6,16,0.6)', borderWidth: 1 }, emphasis: { itemStyle: { borderColor: '#fff', borderWidth: 1 } } },
  pie: { itemStyle: { borderColor: '#050c1c', borderWidth: 2 }, label: { color: colors.text2 } },
  radar: { axisName: { color: colors.text3 }, splitLine: { lineStyle: { color: colors.line } }, splitArea: { show: false }, axisLine: { lineStyle: { color: colors.line } } },
  gauge: { axisLine: { lineStyle: { color: [[1, colors.line2]] } }, detail: { color: colors.text, fontFamily: FONT_MONO }, title: { color: colors.text3 } },
  visualMap: { textStyle: { color: colors.text3, fontSize: 10.5 }, inRange: { color: ramps.heat } },
  dataZoom: {
    borderColor: colors.line, backgroundColor: 'rgba(6,14,32,0.5)', fillerColor: 'rgba(47,125,255,0.16)',
    handleStyle: { color: colors.blueHi, borderColor: colors.blueHi }, moveHandleStyle: { color: colors.line2 },
    dataBackground: { lineStyle: { color: colors.line2 }, areaStyle: { color: colors.line } },
    selectedDataBackground: { lineStyle: { color: colors.blue }, areaStyle: { color: 'rgba(47,125,255,0.3)' } },
    textStyle: { color: colors.text3 }, brushStyle: { color: 'rgba(47,125,255,0.25)' },
  },
  markLine: { lineStyle: { color: colors.text3, type: 'dashed' }, label: { color: colors.text2, fontSize: 10.5 }, symbol: 'none' },
  markArea: { itemStyle: { color: 'rgba(120,165,255,0.07)' }, label: { color: colors.text3, fontSize: 10.5 } },
  angleAxis: { ...axisCommon() },
  radiusAxis: { ...axisCommon(), axisLine: { show: false } },
};

let echartsPromise = null;
let echartsNS = null;

/** Load ECharts (once) and resolve with its namespace. */
export function ready() {
  if (!echartsPromise) {
    echartsPromise = import(ECHARTS_URL).then((ns) => {
      ns.registerTheme('airview', theme);
      echartsNS = ns;
      return ns;
    }).catch((e) => { echartsPromise = null; throw e; });
  }
  return echartsPromise;
}

const hexToRgba = (hex, a) => {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex || '');
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
};

/** Vertical fade gradient for area fills. */
export function fade(color, top = 0.35, bottom = 0) {
  return { type: 'linear', x: 0, y: 0, x2: 0, y2: 1, colorStops: [{ offset: 0, color: hexToRgba(color, top) }, { offset: 1, color: hexToRgba(color, bottom) }] };
}

/**
 * Keep axis names inside the chart box.  ECharts 6 treats the legacy `grid.containLabel: true` as
 * "contain the tick labels only", so axis titles ("K (dB)", "windows", ...) drawn outside the labels
 * were clipped at the card edge, worst on narrow screens.  Every grid that asks for containLabel is
 * rewritten to the ECharts 6 outer-bounds layout that also contains the axis names; grids that set
 * containLabel: false or their own outerBounds* are left alone.  Mutates and returns `option`.
 */
export function fitLabels(option) {
  if (!option || typeof option !== 'object') return option;
  const grids = Array.isArray(option.grid) ? option.grid : option.grid ? [option.grid] : [];
  for (const g of grids) {
    if (!g || g.containLabel !== true || g.outerBoundsMode !== undefined || g.outerBoundsContain !== undefined) continue;
    delete g.containLabel;
    g.outerBoundsMode = 'same';
    g.outerBoundsContain = 'all';
  }
  return option;
}

/**
 * Chart handle returned by charts.create().
 *   .set(option, { replace })   apply an ECharts option (merged; replace: true = notMerge)
 *   .resize()                   usually unnecessary (auto)
 *   .on(event, handler)         ECharts event (click, datazoom, ...) - queued until ready
 *   .loading(bool)              show/hide the loading mask
 *   .dispose()                  free the instance and observers
 *   .instance                   the ECharts instance (null until loaded)   .whenReady  Promise<instance>
 */
class Chart {
  constructor(el, option, opts, onDispose) {
    this.el = el;
    this.instance = null;
    this.disposed = false;
    this._queue = [];
    this._onDispose = onDispose;
    this._ro = null;
    this._raf = 0;
    this.whenReady = ready().then((ec) => {
      if (this.disposed) return null;
      const inst = ec.init(el, opts.theme || 'airview', { renderer: opts.renderer || 'canvas', useDirtyRect: false });
      this.instance = inst;
      for (const job of this._queue) job(inst);
      this._queue = null;
      if (opts.autoResize !== false && typeof ResizeObserver !== 'undefined') {
        this._ro = new ResizeObserver(() => {
          cancelAnimationFrame(this._raf);
          this._raf = requestAnimationFrame(() => { if (!this.disposed && el.clientWidth > 0 && el.clientHeight > 0) inst.resize(); });
        });
        this._ro.observe(el);
      }
      return inst;
    });
    this.whenReady.catch((e) => {
      console.error('[charts] failed to load ECharts', e);
      if (!this.disposed) el.textContent = 'Chart library failed to load';
    });
    if (option) this.set(option);
  }
  _do(job) {
    if (this.disposed) return this;
    if (this.instance) job(this.instance); else this._queue.push(job);
    return this;
  }
  set(option, o = {}) { const opt = fitLabels(option); return this._do((i) => i.setOption(opt, { notMerge: !!o.replace, lazyUpdate: true })); }
  resize() { return this._do((i) => i.resize()); }
  on(event, handler) { return this._do((i) => i.on(event, handler)); }
  loading(on) {
    return this._do((i) => (on
      ? i.showLoading('default', { text: '', color: colors.blueHi, maskColor: 'rgba(4,10,24,0.45)', spinnerRadius: 9, lineWidth: 2 })
      : i.hideLoading()));
  }
  dispose() {
    if (this.disposed) return;
    this.disposed = true;
    cancelAnimationFrame(this._raf);
    if (this._ro) this._ro.disconnect();
    if (this.instance) { this.instance.dispose(); this.instance = null; }
    this._queue = null;
    if (this._onDispose) this._onDispose(this);
  }
}

// ---------------------------------------------------------------------------------------------
// option presets - each returns a plain ECharts option you can tweak before passing to create()/set()
// ---------------------------------------------------------------------------------------------

function xAxisFor(o) {
  const type = o.xType || (o.x ? 'category' : 'time');
  const ax = { type, name: o.xName, nameLocation: 'middle', nameGap: 26, boundaryGap: type === 'category' && !!o._bar };
  if (o.x) ax.data = o.x;
  if (o.xFormat) ax.axisLabel = { formatter: o.xFormat };
  if (o.xMin !== undefined) ax.min = o.xMin;
  if (o.xMax !== undefined) ax.max = o.xMax;
  return ax;
}

function yAxisFor(o) {
  const mk = (name, min, max, fmt, extra) => {
    const ax = { type: o.yType || 'value', name, scale: !!o.yScale, ...extra };
    // the name sits above the axis and grows inwards, so it is never clipped at the card edge
    if (name) ax.nameTextStyle = { align: extra?.position === 'right' ? 'right' : 'left', padding: [0, 0, 2, 0] };
    if (min !== undefined) ax.min = min;
    if (max !== undefined) ax.max = max;
    if (fmt) ax.axisLabel = { formatter: fmt };
    return ax;
  };
  const y = mk(o.yName, o.yMin, o.yMax, o.yFormat);
  if (o.y2Name !== undefined || o.series?.some((s) => s.y2)) {
    return [y, mk(o.y2Name, o.y2Min, o.y2Max, o.y2Format, { splitLine: { show: false }, scale: !!o.y2Scale, position: 'right' })];
  }
  return y;
}

/**
 * Top of the grid box: the legend row (scrollable, 0..18 px) gets a row of its own.  The y-axis names
 * need no reservation: fitLabels() lays the grid out so that axis names are contained in the box.
 */
function gridTop(o, legend) {
  return legend ? 30 : 10;
}

function baseCartesian(o) {
  const many = (o.series || []).length > 1;
  const legend = o.legend ?? many;
  const opt = {
    animation: o.animation ?? true,
    animationDuration: 300,
    animationDurationUpdate: 200,
    grid: { left: 8, right: 14, top: gridTop(o, legend) + (o.title ? 22 : 0), bottom: o.zoom ? 44 : 8, containLabel: true },
    tooltip: { trigger: 'axis', confine: true, ...(o.tooltip ? { formatter: o.tooltip } : {}), ...(o.valueFormat ? { valueFormatter: o.valueFormat } : {}) },
    legend: legend ? { show: true, top: o.title ? 22 : 0, left: 0, right: 0, type: 'scroll' } : { show: false },
    xAxis: xAxisFor(o),
    yAxis: yAxisFor(o),
  };
  if (o.title) opt.title = { text: o.title, left: 0, top: 0 };
  if (o.zoom) {
    opt.dataZoom = [{ type: 'inside', throttle: 50 }, { type: 'slider', height: 18, bottom: 8, showDetail: false }];
  }
  if (o.horizontal) { const t = opt.xAxis; opt.xAxis = opt.yAxis; opt.yAxis = t; }
  return opt;
}

/**
 * Line chart.
 * o: { x: [...categories] (omit for a time axis with series data [[tMs, v], ...]), xType: 'time'|'category'|'value',
 *      series: [{ name, data, color, area, smooth, step, dashed, width, y2, stack, markLine, connectNulls }],
 *      yName, yMin, yMax, yFormat(v), yScale, y2Name, legend, zoom, title, tooltip(params), valueFormat(v) }
 */
export function line(o = {}) {
  const opt = baseCartesian(o);
  opt.series = (o.series || []).map((s, i) => {
    const color = s.color || palette[i % palette.length];
    return {
      type: 'line', name: s.name, data: s.data, color,
      smooth: s.smooth ? 0.25 : false, step: s.step || false, connectNulls: !!s.connectNulls,
      yAxisIndex: s.y2 ? 1 : 0, stack: s.stack, sampling: 'lttb',
      lineStyle: { width: s.width ?? 1.8, type: s.dashed ? 'dashed' : 'solid' },
      areaStyle: s.area || o.area ? { color: fade(color, typeof s.area === 'number' ? s.area : 0.32) } : undefined,
      markLine: s.markLine, markArea: s.markArea, z: s.z,
    };
  });
  return opt;
}

/** Area chart: line() with gradient fills (same options). */
export function area(o = {}) { return line({ ...o, area: true }); }

/**
 * Bar chart.
 * o: { x: [...], series: [{ name, data, color, stack, y2 }], stack: true (stack all), horizontal, yName, legend, zoom, valueFormat }
 */
export function bar(o = {}) {
  const opt = baseCartesian({ ...o, _bar: true });
  opt.tooltip.axisPointer = { type: 'shadow' };
  opt.series = (o.series || []).map((s, i) => {
    const color = s.color || palette[i % palette.length];
    return {
      type: 'bar', name: s.name, data: s.data, color, stack: s.stack || (o.stack ? 'all' : undefined), yAxisIndex: s.y2 ? 1 : 0,
      itemStyle: { borderRadius: o.stack || s.stack ? 0 : o.horizontal ? [0, 2, 2, 0] : [2, 2, 0, 0] },
      emphasis: { focus: 'series' },
    };
  });
  return opt;
}

/**
 * Heatmap on two category axes.
 * o: { x: [...], y: [...], data: [[xIndex, yIndex, value], ...], min, max, ramp: ramps.heat | [...colors],
 *      xName, yName, valueFormat(v), showScale = true }
 */
export function heatmap(o = {}) {
  const vals = (o.data || []).map((d) => d[2]).filter((v) => typeof v === 'number' && Number.isFinite(v));
  const min = o.min ?? (vals.length ? Math.min(...vals) : 0);
  const max = o.max ?? (vals.length ? Math.max(...vals) : 1);
  const showScale = o.showScale !== false;
  return {
    animation: false,
    grid: { left: 8, right: showScale ? 64 : 12, top: 12, bottom: 8, containLabel: true },
    tooltip: {
      confine: true,
      formatter: o.tooltip || ((p) => `${o.y?.[p.value[1]] ?? p.value[1]} · ${o.x?.[p.value[0]] ?? p.value[0]}<br><b>${o.valueFormat ? o.valueFormat(p.value[2]) : p.value[2]}</b>`),
    },
    xAxis: { type: 'category', data: o.x, name: o.xName, nameLocation: 'middle', nameGap: 26, splitArea: { show: false }, axisLine: { show: false } },
    yAxis: { type: 'category', data: o.y, name: o.yName, splitArea: { show: false }, axisLine: { show: false } },
    visualMap: {
      min, max: max === min ? min + 1 : max, calculable: false, show: showScale, orient: 'vertical', right: 0, top: 'middle',
      itemWidth: 10, itemHeight: 120, inRange: { color: o.ramp || ramps.heat },
      formatter: o.valueFormat,
    },
    series: [{ type: 'heatmap', data: o.data, progressive: 2000, emphasis: { itemStyle: { borderColor: '#fff', borderWidth: 1 } } }],
  };
}

const COMPASS16 = ['N', 'NNE', 'NE', 'ENE', 'E', 'ESE', 'SE', 'SSE', 'S', 'SSW', 'SW', 'WSW', 'W', 'WNW', 'NW', 'NNW'];

/**
 * Label of a bearing on a polar angle axis.
 *   mode 'mixed' (default): N / E / S / W on the cardinal points, degrees elsewhere ("30°")
 *   mode 'compass': compass points on multiples of 22.5° (N, NNE, NE ...), degrees elsewhere
 *   mode 'degrees': always degrees, "0°" ... "330°"
 * 360 gives '' (it coincides with 0).
 */
export function bearingLabel(v, mode = 'mixed') {
  const d = ((Number(v) % 360) + 360) % 360;
  if (Number(v) === 360) return '';
  const deg = (Math.round(d * 10) / 10) + '°';
  if (mode === 'degrees') return deg;
  const step = mode === 'compass' ? 22.5 : 90;
  const i = d / step;
  if (Math.abs(i - Math.round(i)) < 1e-6) return COMPASS16[(Math.round(i) * (step / 22.5)) % 16];
  return deg;
}

/**
 * Polar plot with north up and clockwise bearings (coverage, antenna patterns).
 * o: { series: [{ name, data: [[radius, bearingDeg], ...], type: 'line'|'scatter'|'bar', color, area, closed = true, size, width }],
 *      maxR, minR = 0, rName, rFormat(v), bearingStep = 30, legend,
 *      angleFormat: 'mixed' (N/E/S/W + degrees) | 'compass' (N, NE, ...) | 'degrees' | fn(bearingDeg) -> label }
 */
export function polar(o = {}) {
  const many = (o.series || []).length > 1;
  const legend = o.legend ?? many;
  return {
    animation: o.animation ?? false,
    legend: legend ? { show: true, top: 0, left: 0, type: 'scroll' } : { show: false },
    tooltip: { trigger: 'item', confine: true, formatter: o.tooltip || ((p) => `${p.seriesName ? p.seriesName + '<br>' : ''}${Math.round(p.value[1])}° · <b>${o.rFormat ? o.rFormat(p.value[0]) : p.value[0]}</b>`) },
    polar: { radius: legend ? '66%' : '72%', center: ['50%', legend ? '54%' : '50%'] },
    angleAxis: {
      type: 'value', min: 0, max: 360, startAngle: 90, clockwise: true, interval: o.bearingStep || 30,
      axisLabel: { formatter: typeof o.angleFormat === 'function' ? (v) => (v === 360 ? '' : o.angleFormat(v)) : (v) => bearingLabel(v, o.angleFormat || 'mixed') },
      splitLine: { show: true, lineStyle: { color: colors.line } },
      axisLine: { lineStyle: { color: colors.line2 } },
    },
    radiusAxis: {
      type: 'value', min: o.minR ?? 0, max: o.maxR, name: o.rName,
      axisLabel: { formatter: o.rFormat, fontSize: 9.5, color: colors.text4 },
      splitLine: { show: true, lineStyle: { color: colors.line, type: 'solid' } },
      axisLine: { show: false },
    },
    series: (o.series || []).map((s, i) => {
      const color = s.color || palette[i % palette.length];
      const type = s.type || 'line';
      let data = s.data || [];
      if (type === 'line' && s.closed !== false && data.length > 1) data = [...data, data[0]];
      return {
        type, name: s.name, coordinateSystem: 'polar', data, color,
        showSymbol: false, symbolSize: s.size ?? 4,
        lineStyle: type === 'line' ? { width: s.width ?? 1.6 } : undefined,
        areaStyle: s.area ? { color: hexToRgba(color, typeof s.area === 'number' ? s.area : 0.18) } : undefined,
        itemStyle: type === 'scatter' ? { opacity: 0.75 } : undefined,
        large: type === 'scatter', progressive: 4000,
      };
    }),
  };
}

/**
 * Scatter plot.
 * o: { series: [{ name, data: [[x, y, value?], ...], color, size }], xName, yName, xMin, xMax, yMin, yMax, xFormat, yFormat,
 *      visual: { dimension = 2, min, max, ramp, show } (colour points by a third value), legend, zoom, tooltip }
 */
export function scatter(o = {}) {
  const opt = baseCartesian({ ...o, xType: o.xType || 'value' });
  opt.tooltip = { trigger: 'item', confine: true, ...(o.tooltip ? { formatter: o.tooltip } : {}) };
  opt.xAxis.splitLine = { show: true, lineStyle: { color: colors.line, type: [2, 4] } };
  opt.xAxis.scale = !!o.xScale;
  opt.series = (o.series || []).map((s, i) => ({
    type: 'scatter', name: s.name, data: s.data, color: s.color || palette[i % palette.length],
    symbolSize: s.size ?? 4, large: (s.data || []).length > 2000, largeThreshold: 2000, progressive: 5000,
    itemStyle: { opacity: s.opacity ?? 0.75 },
  }));
  if (o.visual) {
    opt.visualMap = {
      dimension: o.visual.dimension ?? 2, min: o.visual.min ?? 0, max: o.visual.max ?? 1, show: o.visual.show ?? true,
      orient: 'vertical', right: 0, top: 'middle', itemWidth: 10, itemHeight: 110, calculable: false,
      inRange: { color: o.visual.ramp || ramps.heat }, formatter: o.visual.format,
    };
    if (opt.visualMap.show) opt.grid.right = 60;
  }
  return opt;
}

/**
 * Donut chart.  o: { data: [{ name, value, color }], centerText, valueFormat(v), legend = true }
 */
export function donut(o = {}) {
  return {
    tooltip: { trigger: 'item', confine: true, valueFormatter: o.valueFormat },
    legend: o.legend === false ? { show: false } : { show: true, orient: 'vertical', right: 0, top: 'middle' },
    title: o.centerText ? { text: o.centerText, left: o.legend === false ? '49%' : '34%', top: 'middle', textAlign: 'center', textStyle: { fontFamily: FONT_MONO, fontSize: 18, fontWeight: 500, color: colors.text } } : undefined,
    series: [{
      type: 'pie', radius: ['58%', '82%'], center: [o.legend === false ? '50%' : '35%', '50%'], avoidLabelOverlap: true,
      label: { show: false }, labelLine: { show: false },
      data: (o.data || []).map((d, i) => ({ name: d.name, value: d.value, itemStyle: { color: d.color || palette[i % palette.length] } })),
    }],
  };
}

/**
 * Sparkline option: axis-less mini line with a soft fill.
 * o: { data: [v, ...] | [[x, v], ...], color, area = true, min, max, width, tooltip: false | (value) => string }
 */
export function sparklineOption(o = {}) {
  const color = o.color || colors.blueHi;
  const pairs = Array.isArray(o.data?.[0]);
  return {
    animation: false,
    grid: { left: 1, right: 1, top: 3, bottom: 2, containLabel: false },
    xAxis: { type: pairs ? 'value' : 'category', show: false, boundaryGap: false, min: 'dataMin', max: 'dataMax' },
    yAxis: { type: 'value', show: false, min: o.min ?? ((v) => v.min - (v.max - v.min) * 0.08), max: o.max ?? ((v) => (v.max === v.min ? v.max + 1 : v.max + (v.max - v.min) * 0.08)) },
    tooltip: o.tooltip === false ? { show: false } : {
      trigger: 'axis', confine: true, padding: [3, 7], textStyle: { fontSize: 11, fontFamily: FONT_MONO },
      axisPointer: { type: 'line', lineStyle: { color: 'rgba(160,200,255,0.5)', type: 'solid', width: 1 } },
      formatter: (ps) => {
        const p = Array.isArray(ps) ? ps[0] : ps;
        const v = Array.isArray(p.value) ? p.value[p.value.length - 1] : p.value;
        if (v === null || v === undefined || v === '-') return '';
        const x = Array.isArray(p.value) ? p.value[0] : p.dataIndex;
        return typeof o.tooltip === 'function' ? o.tooltip(v, x) : String(v);
      },
    },
    series: [{
      type: 'line', data: o.data || [], color, showSymbol: false, symbolSize: 4, connectNulls: false, sampling: 'lttb',
      lineStyle: { width: o.width ?? 1.4 }, areaStyle: o.area === false ? undefined : { color: fade(color, 0.3) },
    }],
  };
}

// ---------------------------------------------------------------------------------------------
// scopes
// ---------------------------------------------------------------------------------------------

function makeScope() {
  const owned = new Set();
  const scope = {
    /** Resolve with the ECharts namespace (for echarts.graphic etc.). */
    ready,
    /**
     * Create a chart in `el`.  opts: { theme = 'airview', renderer = 'canvas'|'svg', autoResize = true }
     * @returns {Chart}
     */
    create(el, option, opts = {}) {
      const c = new Chart(el, option, opts, (x) => owned.delete(x));
      owned.add(c);
      return c;
    },
    /** Create a sparkline chart in `el` (see sparklineOption). Update with chart.set(charts.sparklineOption({...})). */
    sparkline(el, data, o = {}) {
      return scope.create(el, sparklineOption({ ...o, data }), { renderer: o.renderer || 'canvas' });
    },
    /** Dispose every chart created through this scope. */
    disposeAll() { for (const c of [...owned]) c.dispose(); owned.clear(); },
    /** A child scope with its own disposeAll() (app.js gives each mounted page one). */
    scope: makeScope,
    /** Number of live charts in this scope. */
    get count() { return owned.size; },
    line, area, bar, heatmap, polar, scatter, donut, sparklineOption, fade, bearingLabel, fitLabels,
    palette, colors, ramps, theme,
  };
  return scope;
}

/** The root chart scope. Pages should use ctx.charts (a per-page scope) instead. */
export const charts = makeScope();

export default charts;
