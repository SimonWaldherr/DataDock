// dashboard.js: the BI foundation's dashboard list + canvas pages
// (templates/dashboards.html, templates/dashboard.html). Kept separate from
// app.js (which every other page shares) since this is a large, self-
// contained new feature area; loaded only on the two dashboard templates.
// Charts are rendered client-side with vega-embed (loaded globally in
// base.html) against row data fetched from the existing /api/query — no new
// SQL execution surface, just the same gated path the SQL editor already
// uses.

// ─── Dashboards list page (templates/dashboards.html) ──────────────────────

function createDashboard(event) {
  event.preventDefault();
  var name = document.getElementById('newDashboardName').value.trim();
  var description = document.getElementById('newDashboardDescription').value.trim();
  var status = document.getElementById('newDashboardStatus');
  if (!name) return;
  fetch('/api/bi/dashboards', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({name: name, description: description, layout: []})
  })
    .then(function (resp) { return resp.json().then(function (body) { return {ok: resp.ok, body: body}; }); })
    .then(function (res) {
      if (res.ok && res.body && res.body.id) {
        window.location.href = '/dashboards/' + encodeURIComponent(res.body.id);
        return;
      }
      var detail = (res.body && (res.body.detail || res.body.title)) || 'Could not create dashboard.';
      status.innerHTML = '<span class="text-danger">' + escHtml(detail) + '</span>';
    })
    .catch(function (err) {
      status.innerHTML = '<span class="text-danger">' + escHtml(String(err)) + '</span>';
    });
}

// ─── Dashboard canvas page (templates/dashboard.html) ──────────────────────

// dashboardState holds the one dashboard being edited plus every saved chart
// (for resolving a tile's chart_id to its name/SQL/spec). Reassigned wholesale
// by loadDashboardState, not mutated field-by-field, so a failed reload never
// leaves stale and fresh data mixed together.
var dashboardState = {dashboard: null, chartsByID: {}};

function initDashboardCanvasPage() {
  var canvas = document.getElementById('dashboardCanvas');
  if (!canvas) return;
  loadDashboardState(canvas.dataset.dashboardId);
}

function loadDashboardState(dashboardID) {
  Promise.all([
    fetch('/api/bi/dashboards?id=' + encodeURIComponent(dashboardID)).then(function (r) { return r.json(); }),
    fetch('/api/bi/charts').then(function (r) { return r.json(); })
  ]).then(function (results) {
    var dashboard = results[0];
    var charts = (results[1] && results[1].charts) || [];
    var chartsByID = {};
    charts.forEach(function (c) { chartsByID[c.id] = c; });
    dashboardState = {dashboard: dashboard, chartsByID: chartsByID};
    if (!dashboard.layout) dashboard.layout = [];
    renderDashboardCanvas();
  }).catch(function (err) {
    var status = document.getElementById('dashboardStatus');
    if (status) status.innerHTML = '<span class="text-danger">Could not load dashboard: ' + escHtml(String(err)) + '</span>';
  });
}

function renderDashboardCanvas() {
  var canvas = document.getElementById('dashboardCanvas');
  if (!canvas || !dashboardState.dashboard) return;
  canvas.innerHTML = '';
  dashboardState.dashboard.layout.forEach(function (tile, idx) {
    canvas.appendChild(buildDashboardTileElement(tile, idx));
  });
}

function buildDashboardTileElement(tile, idx) {
  var chart = dashboardState.chartsByID[tile.chart_id];
  var el = document.createElement('div');
  el.className = 'card';
  el.style.gridColumn = (tile.x + 1) + ' / span ' + tile.w;
  el.style.gridRow = (tile.y + 1) + ' / span ' + tile.h;
  el.style.overflow = 'hidden';

  var title = chart ? escHtml(chart.name) : '<span class="text-danger">Missing chart</span>';
  el.innerHTML =
    '<div class="card-body p-2 d-flex flex-column h-100">' +
    '<div class="d-flex justify-content-between align-items-start mb-1 gap-1">' +
    '<span class="small fw-semibold text-truncate">' + title + '</span>' +
    '<div class="btn-group btn-group-sm" role="group">' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Move left" onclick="moveDashboardTile(' + idx + ', -1, 0)"><i class="bi bi-arrow-left"></i></button>' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Move right" onclick="moveDashboardTile(' + idx + ', 1, 0)"><i class="bi bi-arrow-right"></i></button>' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Move up" onclick="moveDashboardTile(' + idx + ', 0, -1)"><i class="bi bi-arrow-up"></i></button>' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Move down" onclick="moveDashboardTile(' + idx + ', 0, 1)"><i class="bi bi-arrow-down"></i></button>' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Shrink" onclick="resizeDashboardTile(' + idx + ', -1, 0)"><i class="bi bi-dash-lg"></i></button>' +
    '<button type="button" class="btn btn-outline-secondary btn-sm py-0 px-1" title="Grow" onclick="resizeDashboardTile(' + idx + ', 1, 0)"><i class="bi bi-plus-lg"></i></button>' +
    '<button type="button" class="btn btn-outline-danger btn-sm py-0 px-1" title="Remove" onclick="removeDashboardTile(' + idx + ')"><i class="bi bi-x-lg"></i></button>' +
    '</div></div>' +
    '<div class="flex-grow-1" id="dashboardTileChart' + idx + '" style="min-height:0"></div>' +
    '</div>';

  if (chart) {
    renderChartIntoTile('dashboardTileChart' + idx, chart);
  }
  return el;
}

// renderChartIntoTile runs a chart's stored SQL through the same gated
// /api/query path the SQL editor uses (no new SQL execution surface), then
// hands the rows to vega-embed alongside the chart's stored spec.
function renderChartIntoTile(targetID, chart) {
  fetch('/api/query', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({sql: chart.sql_text, limit: 5000})
  })
    .then(function (resp) { return resp.json(); })
    .then(function (result) {
      var target = document.getElementById(targetID);
      if (!target) return;
      if (result.error) {
        target.innerHTML = '<div class="small text-danger p-2">' + escHtml(result.error) + '</div>';
        return;
      }
      var values = queryResultToVegaValues(result);
      var spec = Object.assign({}, chart.vega_lite_spec, {data: {values: values}, width: 'container', height: 'container'});
      // ast/expr opt into vega-interpreter's no-eval expression evaluator —
      // see the vega-interpreter <script type="module"> in base.html for why
      // this is required under this app's CSP (no unsafe-eval).
      window.vegaEmbed(target, spec, {actions: false, ast: true, expr: window.vegaExpressionInterpreter}).catch(function (err) {
        target.innerHTML = '<div class="small text-danger p-2">' + escHtml(String(err && err.message || err)) + '</div>';
      });
    })
    .catch(function (err) {
      var target = document.getElementById(targetID);
      if (target) target.innerHTML = '<div class="small text-danger p-2">' + escHtml(String(err)) + '</div>';
    });
}

// queryResultToVegaValues turns the /api/query {columns, rows} shape (every
// cell arrives as a string) into the array-of-objects Vega-Lite expects,
// coercing cells that parse cleanly as finite numbers — mirroring the same
// numeric-detection app.js's Quick Chart already uses (profileQuickChartColumns)
// — so a "quantitative" encoding gets real numbers instead of numeric strings.
function queryResultToVegaValues(result) {
  var columns = result.columns || [];
  var rows = result.rows || [];
  return rows.map(function (row) {
    var obj = {};
    columns.forEach(function (col, i) {
      var raw = row[i];
      if (raw !== '' && raw !== null && raw !== undefined && !isNaN(Number(raw))) {
        obj[col] = Number(raw);
      } else {
        obj[col] = raw;
      }
    });
    return obj;
  });
}

function addChartToDashboard() {
  var select = document.getElementById('dashboardAddChartSelect');
  var chartID = select.value;
  if (!chartID || !dashboardState.dashboard) return;
  var layout = dashboardState.dashboard.layout;
  var nextY = layout.reduce(function (max, t) { return Math.max(max, t.y + t.h); }, 0);
  layout.push({chart_id: chartID, x: 0, y: nextY, w: 6, h: 3});
  select.value = '';
  renderDashboardCanvas();
}

function removeDashboardTile(idx) {
  dashboardState.dashboard.layout.splice(idx, 1);
  renderDashboardCanvas();
}

function moveDashboardTile(idx, dx, dy) {
  var tile = dashboardState.dashboard.layout[idx];
  if (!tile) return;
  tile.x = Math.max(0, Math.min(12 - tile.w, tile.x + dx));
  tile.y = Math.max(0, tile.y + dy);
  renderDashboardCanvas();
}

function resizeDashboardTile(idx, dw, dh) {
  var tile = dashboardState.dashboard.layout[idx];
  if (!tile) return;
  tile.w = Math.max(1, Math.min(12, tile.w + dw));
  tile.h = Math.max(1, tile.h + dh);
  if (tile.x + tile.w > 12) tile.x = 12 - tile.w;
  renderDashboardCanvas();
}

function saveDashboardLayout() {
  var status = document.getElementById('dashboardStatus');
  if (!dashboardState.dashboard) return;
  fetch('/api/bi/dashboards', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(dashboardState.dashboard)
  })
    .then(function (resp) { return resp.json().then(function (body) { return {ok: resp.ok, body: body}; }); })
    .then(function (res) {
      if (res.ok) {
        dashboardState.dashboard = res.body;
        if (status) status.innerHTML = '<span class="text-success"><i class="bi bi-check-circle me-1"></i>Layout saved.</span>';
      } else {
        var detail = (res.body && (res.body.detail || res.body.title)) || 'Could not save layout.';
        if (status) status.innerHTML = '<span class="text-danger">' + escHtml(detail) + '</span>';
      }
    })
    .catch(function (err) {
      if (status) status.innerHTML = '<span class="text-danger">' + escHtml(String(err)) + '</span>';
    });
}
