(function () {
  "use strict";

  var V = window.LT_DATA || {};
  var state = {
    meta: V.meta || {},
    summary: V.summary || {},
    thresholds: V.thresholds || [],
    passed: !!V.passed,
    warnings: V.warnings || [],
    insights: V.insights || [],
    stopped: V.stopped || null,
    compare: V.compare || null,
    alerts: V.alerts || [],
    topSql: V.topSql || [],
    apm: V.apm || null,
    logs: V.logs || null,
    watch: !!V.watch,
    history: !!V.history,
    range: "live",
    points: V.points || [],
    live: !!V.live,
    done: !!V.done,
    step: "",   // "" = all requests
    tab: "steps"
  };
  var charts = {};

  // ---------- helpers ----------
  function $(id) { return document.getElementById(id); }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }
  function add(parent) {
    for (var i = 1; i < arguments.length; i++) if (arguments[i]) parent.appendChild(arguments[i]);
    return parent;
  }
  function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }

  // Numbers split into value and unit so the unit can be set smaller.
  function usParts(us) {
    if (us == null || isNaN(us)) return ["–", ""];
    if (us < 1000) return [String(Math.round(us)), "µs"];
    if (us < 1e6) return [(us / 1000).toFixed(us < 10000 ? 2 : 1), "ms"];
    return [(us / 1e6).toFixed(2), "s"];
  }
  function fmtUs(us) { var p = usParts(us); return p[0] + p[1]; }
  function fmtMs(ms) { return ms == null ? "–" : fmtUs(ms * 1000); }
  function fmtInt(n) { return Math.round(n || 0).toLocaleString("en-US"); }
  function fmtRate(n) { return (n || 0).toLocaleString("en-US", { maximumFractionDigits: n < 10 ? 1 : 0 }); }
  function fmtPct(r) { return ((r || 0) * 100).toFixed(2) + "%"; }
  function compact(v) {
    if (v == null) return "";
    var a = Math.abs(v);
    if (a >= 1e6) return (v / 1e6).toFixed(a >= 1e7 ? 0 : 1) + "M";
    if (a >= 1e3) return (v / 1e3).toFixed(a >= 1e4 ? 0 : 1) + "k";
    return a < 10 && v % 1 ? v.toFixed(1) : String(Math.round(v));
  }
  function fmtDur(sec) {
    sec = Math.round(sec || 0);
    var m = Math.floor(sec / 60), s = sec % 60;
    return m ? m + "m" + (s ? " " + s + "s" : "") : s + "s";
  }
  // fmtT shows a point's time: seconds into the test, or the clock in watch mode.
  function fmtT(t) {
    if (!state.watch) return fmtDur(t);
    var d = new Date(t * 1000), P = state.points, span = P.length ? P[P.length - 1].t - P[0].t : 0;
    var hm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
    if (span > 86400) return (d.getMonth() + 1) + "/" + d.getDate() + " " + hm;
    return span > 1800 ? hm : hm + ":" + String(d.getSeconds()).padStart(2, "0");
  }

  function isInternal(key) { return key.split("|").slice(1).join("|").charAt(0) === "_"; }
  function stepLabel(key) {
    var i = key.indexOf("|"), sc = key.slice(0, i), st = key.slice(i + 1);
    if (st === "_iteration") st = "iteration";
    return (state.meta.scenarios || []).length > 1 ? sc + " / " + st : st;
  }
  function loadUnit() {
    return (state.meta.scenarios || []).some(function (s) { return s.executor === "arrival-rate"; }) ? "/s" : " users";
  }
  function verdict() {
    if (state.watch) return "WATCH";
    if (state.live && !state.done) return "LIVE";
    if (state.stopped) return "STOPPED";
    if (state.thresholds.length && !state.passed) return "FAIL";
    if (state.compare && state.compare.regressed) return "REGRESSED";
    if (state.thresholds.length || state.compare) return "PASS";
    return "DONE";
  }

  // ---------- header & hero ----------
  function renderHeader() {
    var m = state.meta, s = state.summary || {}, lat = s.latency || {};
    document.title = (m.name || "lt") + " · lt";
    var crumb = $("crumb");
    crumb.textContent = "";
    add(crumb, document.createTextNode(state.watch ? "Watch  /  " : "Load test  /  "), el("b", null, m.file || m.name || ""));
    var meta = $("meta");
    meta.textContent = "";
    var bits = [];
    if (m.started) bits.push(new Date(m.started).toLocaleString());
    if (m.agents > 1) bits.push(m.agents + " agents");
    if (m.label) bits.push(m.label);
    bits.forEach(function (b) { meta.appendChild(el("span", null, b)); });

    var v = verdict(), st = $("status");
    var cls = { PASS: "pass", LIVE: "live", DONE: "", WATCH: "live" };
    if (v === "WATCH" && firingAlerts().length) cls.WATCH = "fail";
    st.className = "status " + (v in cls ? cls[v] : "fail");
    st.lastChild.textContent = { LIVE: "Running", STOPPED: "Stopped early", FAIL: "Failed", REGRESSED: "Regressed", PASS: "Passed", DONE: "Finished", WATCH: "Watching" }[v];
    $("name").textContent = m.name || "lt";

    var h = $("headline");
    h.textContent = "";
    var dur = s.duration || state.points.length;
    function b(x) { return el("b", null, x); }
    function t(x) { return document.createTextNode(x); }
    if (v === "WATCH") {
      var sv = serverInfo().names, f = firingAlerts();
      var span = { live: "live", "3600": "over the last hour", "21600": "over the last 6 hours", "86400": "over the last 24 hours", "604800": "over the last 7 days" }[state.range];
      add(h, t("Watching "), b(sv.length + " server series"), t(" " + span + ". "),
        f.length ? b(f.length + (f.length === 1 ? " alert firing." : " alerts firing.")) : t("No alerts firing."));
    } else if (v === "LIVE") {
      add(h, t("Running for "), b(fmtDur(dur)), t(m.planned ? " of " + fmtDur(m.planned) + ". " : ". "),
        t(fmtInt(s.requests) + " requests so far, p95 "), b(fmtUs(lat.p95)), t("."));
    } else if (state.stopped) {
      add(h, t("Stopped at " + state.stopped.t + "s when a limit failed. This target held up to about "),
        b(fmtRate(state.stopped.lastGood) + loadUnit()), t("."));
    } else if (v === "FAIL") {
      var failed = state.thresholds.filter(function (x) { return !x.pass; });
      add(h, b(failed.length + " of " + state.thresholds.length + " thresholds failed"),
        t(": " + failed.slice(0, 2).map(function (x) { return x.metric + " " + x.expr + " (was " + x.actual + ")"; }).join(", ") + "."));
    } else if (v === "REGRESSED") {
      var rows = (state.compare.rows || []).filter(function (r) { return (r.regressed || []).length; });
      add(h, b("Slower than the baseline"), t(" on " + rows.map(function (r) { return r.name; }).slice(0, 3).join(", ") + "."));
    } else {
      add(h, b(fmtInt(s.requests) + " requests"), t(" at " + fmtRate(s.rps) + "/s over " + fmtDur(dur) + ", p95 "),
        b(fmtUs(lat.p95)), t(", errors "), b(fmtPct(s.errorRate)),
        t(v === "PASS" ? ". All " + state.thresholds.length + " thresholds passed." : "."));
    }
    var pr = $("progress");
    pr.hidden = !(v === "LIVE" && m.planned);
    if (!pr.hidden) pr.firstChild.style.width = Math.min(100, dur / m.planned * 100) + "%";
  }

  // ---------- KPIs with sparklines ----------
  function spark(values, color) {
    var ns = "http://www.w3.org/2000/svg";
    var svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 100 28");
    svg.setAttribute("preserveAspectRatio", "none");
    var vals = values.filter(function (x) { return x != null && !isNaN(x); });
    if (vals.length < 2) return svg;
    var max = Math.max.apply(null, vals) || 1, n = values.length - 1, pts = [];
    values.forEach(function (x, i) { if (x != null && !isNaN(x)) pts.push((i / n * 100).toFixed(2) + "," + (26 - x / max * 24).toFixed(2)); });
    var area = document.createElementNS(ns, "polygon");
    area.setAttribute("points", "0,28 " + pts.join(" ") + " 100,28");
    area.setAttribute("fill", color); area.setAttribute("opacity", "0.10");
    var line = document.createElementNS(ns, "polyline");
    line.setAttribute("points", pts.join(" "));
    line.setAttribute("fill", "none"); line.setAttribute("stroke", color);
    line.setAttribute("stroke-width", "1.5"); line.setAttribute("vector-effect", "non-scaling-stroke");
    line.setAttribute("stroke-linejoin", "round");
    svg.appendChild(area); svg.appendChild(line);
    return svg;
  }

  function watchKPIs() {
    var sv = serverInfo(), P = state.points, root = $("kpis");
    root.textContent = "";
    var names = sv.names.filter(function (n) { return /apm|cpu|conn|tps|qps|ops/.test(n); }).concat(sv.names).filter(function (n, i, a) { return a.indexOf(n) === i; }).slice(0, 5);
    root.style.setProperty("--kpi-n", Math.max(1, names.length));
    root.hidden = !names.length;
    names.forEach(function (n) {
      var data = P.map(function (p) { var v = (p.server || {})[n]; return v == null ? null : v; }), last = null;
      for (var i = data.length - 1; i >= 0; i--) if (data[i] != null) { last = data[i]; break; }
      add(root, add(el("div", "kpi"), el("div", "k", n), el("div", "v", fmtServer(last, sv.units[n])),
        el("div", "d", "peak " + fmtServer(peakOf(data), sv.units[n])), spark(data, cssVar("--s3"))));
    });
  }

  function renderKPIs() {
    if (state.watch) { watchKPIs(); return; }
    var s = state.summary || {}, lat = s.latency || {}, P = state.points;
    var list = [
      { k: "Requests", v: [fmtInt(s.requests), ""], d: fmtInt(s.iterations) + " iterations", sp: P.map(function (p) { return p.done; }) },
      { k: "Throughput", v: [fmtRate(s.rps), "/s"], d: "average", sp: P.map(function (p) { return p.done; }) },
      { k: "p95 latency", v: usParts(lat.p95), d: "p50 " + fmtUs(lat.p50), sp: P.map(function (p) { return p.done ? p.p95 : null; }) },
      { k: "p99 latency", v: usParts(lat.p99), d: "max " + fmtUs(lat.max), sp: P.map(function (p) { return p.done ? p.p99 : null; }) },
      { k: "Error rate", v: [((s.errorRate || 0) * 100).toFixed(2), "%"], d: fmtInt(s.errors) + " errors", bad: s.errors > 0,
        sp: P.map(function (p) { return p.errors; }), color: "--critical" }
    ];
    if (s.dropped > 0) list.push({ k: "Dropped", v: [fmtInt(s.dropped), ""], d: "arrivals not started", bad: true, sp: P.map(function (p) { return p.dropped; }), color: "--warning" });
    var root = $("kpis");
    root.textContent = "";
    root.style.setProperty("--kpi-n", list.length);
    list.forEach(function (x) {
      var v = el("div", "v" + (x.bad ? " bad" : ""), x.v[0]);
      if (x.v[1]) v.appendChild(el("small", null, x.v[1]));
      add(root, add(el("div", "kpi"), el("div", "k", x.k), v, el("div", "d", x.d), spark(x.sp, cssVar(x.color || "--s1"))));
    });
  }

  // ---------- what happened ----------
  function renderTimeline() {
    var root = $("timeline");
    root.textContent = "";
    var items = state.insights.slice();
    (state.alerts || []).forEach(function (e) {
      items.push({ t: e.t, at: e.at, level: e.state === "resolved" ? "info" : (e.severity === "critical" ? "critical" : "warn"),
        title: "Alert " + e.state + ": " + e.name, detail: e.rule + " (was " + e.value + ")" });
    });
    if (state.watch) items.reverse(); // newest first in the console
    items.sort(function (a, b) { return (a.t || 1e9) - (b.t || 1e9); });
    $("tl-hint").textContent = items.length ? items.length + (items.length === 1 ? " finding" : " findings") : "";
    if (!items.length) {
      var msg = state.watch ? "No alerts yet. Alert events show up here as they happen." :
        state.live && !state.done ? "Findings appear when the test ends." : "Nothing stood out: no latency knee, error burst or saturation.";
      root.appendChild(add(el("li"), el("p", "empty", msg)));
      return;
    }
    var names = { critical: "Critical", warn: "Warning", info: "Note" };
    items.forEach(function (i) {
      var meta = el("div", "meta");
      if (state.watch && i.at) meta.appendChild(el("span", "when", new Date(i.at).toLocaleTimeString()));
      else if (i.t) meta.appendChild(el("span", "when", i.t + "s"));
      meta.appendChild(el("span", "lv", names[i.level] || i.level));
      root.appendChild(add(el("li", i.level), el("span", "dot"), meta, el("div", "ti", i.title), i.detail ? el("div", "de", i.detail) : null));
    });
  }

  function renderChecks() {
    var th = state.thresholds || [];
    $("th-panel").hidden = !th.length;
    var passed = th.filter(function (t) { return t.pass; }).length;
    $("th-hint").textContent = th.length ? passed + " of " + th.length + " passed" : "";
    var root = $("checks");
    root.textContent = "";
    th.forEach(function (t) {
      var what = el("div");
      add(what, el("code", null, t.expr), el("span", "x", t.metric));
      root.appendChild(add(el("li", t.pass ? "ok" : "no"), el("span", "m", t.pass ? "✓" : "✗"), what, el("span", "a", t.actual)));
    });
    var n = $("notes");
    n.textContent = "";
    n.hidden = !state.warnings.length;
    if (state.warnings.length) {
      n.appendChild(el("b", null, "Measurement notes"));
      state.warnings.forEach(function (w) { n.appendChild(el("p", null, w)); });
    }
  }

  // ---------- charts ----------
  function ms(us) { return us == null ? null : us / 1000; }

  function markerTimes() {
    var ts = state.insights.filter(function (i) { return i.t && i.level !== "info"; })
      .map(function (i) { return { t: i.t, level: i.level }; });
    if (state.stopped && !ts.some(function (x) { return x.t === state.stopped.t; })) ts.push({ t: state.stopped.t, level: "critical" });
    return ts;
  }

  // Draws dashed lines at the seconds findings point to, labeled at the top.
  function markers() {
    return { hooks: { draw: [function (u) {
      var ts = markerTimes();
      if (!ts.length) return;
      var c = u.ctx, dpr = devicePixelRatio || 1;
      c.save();
      c.font = "600 " + (10 * dpr) + "px " + getComputedStyle(document.body).fontFamily;
      // Small charts get lines only; big ones get labels that never overlap.
      var labels = u.height >= 180, lastRight = -1;
      ts.sort(function (a, b) { return a.t - b.t; });
      ts.forEach(function (m) {
        var x = Math.round(u.valToPos(m.t, "x", true));
        if (x < u.bbox.left || x > u.bbox.left + u.bbox.width) return;
        var col = cssVar(m.level === "critical" ? "--critical" : "--warning");
        c.strokeStyle = col; c.globalAlpha = 0.85; c.lineWidth = dpr;
        c.setLineDash([3 * dpr, 3 * dpr]);
        c.beginPath(); c.moveTo(x, u.bbox.top); c.lineTo(x, u.bbox.top + u.bbox.height); c.stroke();
        if (!labels) return;
        c.globalAlpha = 1; c.fillStyle = col;
        var label = m.t + "s", w = c.measureText(label).width;
        var lx = x + 4 * dpr + w > u.bbox.left + u.bbox.width ? x - 4 * dpr - w : x + 4 * dpr;
        if (lx < lastRight + 4 * dpr) return;
        c.fillText(label, lx, u.bbox.top + 10 * dpr);
        lastRight = lx + w;
      });
      c.restore();
    }] } };
  }

  // A floating tooltip that lists every series at the cursor.
  function tooltip(specSeries) {
    var tip;
    return { hooks: {
      init: [function (u) { tip = el("div", "tip"); u.over.appendChild(tip); }],
      setCursor: [function (u) {
        var i = u.cursor.idx;
        if (i == null || u.cursor.left < 0) { tip.style.display = "none"; return; }
        tip.textContent = "";
        tip.appendChild(el("div", "t", "at " + fmtT(u.data[0][i])));
        specSeries.forEach(function (s, k) {
          var v = u.data[k + 1][i], row = el("div", "r"), sw = el("i");
          sw.style.background = s.color;
          add(row, sw, el("span", null, s.label), el("b", null, v == null ? "–" : s.fmt(v)));
          tip.appendChild(row);
        });
        tip.style.display = "block";
        var w = tip.offsetWidth, h = tip.offsetHeight, left = u.cursor.left + 14, top = u.cursor.top - h / 2;
        if (left + w > u.over.clientWidth) left = u.cursor.left - w - 14;
        top = Math.max(0, Math.min(top, u.over.clientHeight - h));
        tip.style.left = left + "px"; tip.style.top = top + "px";
      }]
    } };
  }

  function legend(id, series) {
    var root = $(id);
    if (!root) return;
    root.textContent = "";
    series.forEach(function (s) {
      var sw = el("i", s.dash ? "dash" : null);
      if (s.dash) sw.style.borderColor = s.color; else sw.style.background = s.color;
      root.appendChild(add(el("span"), sw, document.createTextNode(s.label)));
    });
  }

  function axis(fmt, isY) {
    return {
      stroke: cssVar("--muted"),
      grid: { stroke: cssVar("--grid"), width: 1 },
      ticks: { show: false },
      values: fmt ? function (u, vals) { return vals.map(fmt); } : undefined,
      font: "11px -apple-system, BlinkMacSystemFont, system-ui, sans-serif",
      size: isY ? 50 : 26, gap: 6
    };
  }

  function alpha(hex, a) {
    var m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex);
    if (!m) return hex;
    return "rgba(" + parseInt(m[1], 16) + "," + parseInt(m[2], 16) + "," + parseInt(m[3], 16) + "," + a + ")";
  }

  function makeChart(id, spec) {
    var root = $(id);
    if (!root) return;
    if (charts[id]) charts[id].destroy();
    root.textContent = "";
    var series = [{ label: "time" }].concat(spec.series.map(function (s) {
      var o = { label: s.label, stroke: s.color, width: s.width || 1.75, points: { show: false }, spanGaps: false };
      if (s.dash) o.dash = [5, 4];
      if (s.fill) o.fill = s.fill;
      return o;
    }));
    var yr = spec.yRange ? function () { return spec.yRange; } : function (u, min, max) { return [0, max > 0 ? max * 1.12 : 1]; };
    charts[id] = new uPlot({
      width: Math.max(root.clientWidth, 200),
      height: spec.height || 200,
      padding: [14, 10, 0, 0],
      scales: { x: { time: false }, y: { range: yr } },
      axes: [axis(function (v) { return fmtT(v); }), axis(spec.yFmt || compact, true)],
      series: series,
      cursor: { sync: { key: "lt" }, drag: { x: true, y: false }, points: { size: 7, width: 2 }, y: false },
      legend: { show: false },
      plugins: [markers(), tooltip(spec.series)]
    }, spec.data, root);
  }

  function mainSpecs() {
    var P = state.points, t = P.map(function (p) { return p.t; });
    if (state.watch) {
      var names = serverInfo().names;
      var pick = function (re) { return names.filter(function (n) { return re.test(n); }); };
      var tps = pick(/apm tps$/), mean = pick(/apm mean$/), cols = [cssVar("--s1"), cssVar("--s2"), cssVar("--s3")];
      var ser = function (list, f) { return list.slice(0, 3).map(function (n, i) { return { label: n.replace(/ ?apm (tps|mean)$/, "") || "all", color: cols[i], fmt: f }; }); };
      var col = function (list) { return list.slice(0, 3).map(function (n) { return P.map(function (p) { var v = (p.server || {})[n]; return v == null ? null : v; }); }); };
      $("c-rps").parentNode.querySelector("h3").textContent = "Server throughput";
      $("c-lat").parentNode.querySelector("h3").textContent = "Server response time";
      return {
        "c-rps": { series: ser(tps, function (v) { return fmtRate(v) + "/s"; }), data: [t].concat(col(tps)), height: 200, legend: "lg-rps" },
        "c-lat": { series: ser(mean, fmtMs), data: [t].concat(col(mean)), height: 200, legend: "lg-lat", yFmt: function (v) { return compact(v) + "ms"; } }
      };
    }
    var hasTarget = P.some(function (p) { return p.target > 0; });
    var num = function (v) { return fmtRate(v) + "/s"; };
    var s1 = cssVar("--s1"), s2 = cssVar("--s2"), s3 = cssVar("--s3"), mu = cssVar("--muted");
    var rpsSeries = [{ label: "requests", color: s1, fmt: num, fill: alpha(s1, 0.08) }];
    var rpsData = [t, P.map(function (p) { return p.done; })];
    if (hasTarget) {
      rpsSeries = [{ label: "target", color: mu, dash: true, fmt: num }, { label: "iterations", color: s2, fmt: num }].concat(rpsSeries);
      rpsData = [t, P.map(function (p) { return p.target; }), P.map(function (p) { return p.iters || 0; }), P.map(function (p) { return p.done; })];
    }
    var lat = P.map(function (p) {
      if (state.step) { var k = p.keys && p.keys[state.step]; return k && k.n ? k : null; }
      return p.done ? p : null;
    });
    return {
      "c-rps": { series: rpsSeries, data: rpsData, height: 210, legend: "lg-rps" },
      "c-lat": {
        series: [{ label: "p50", color: s1, fmt: fmtMs }, { label: "p95", color: s2, fmt: fmtMs }, { label: "p99", color: s3, fmt: fmtMs }],
        data: [t, lat.map(function (k) { return k ? ms(k.p50) : null; }), lat.map(function (k) { return k ? ms(k.p95) : null; }), lat.map(function (k) { return k ? ms(k.p99) : null; })],
        height: 230, legend: "lg-lat", yFmt: function (v) { return compact(v) + "ms"; }
      }
    };
  }

  function peakOf(arr) { return arr.reduce(function (m, x) { return x != null && x > m ? x : m; }, 0); }

  function miniSpecs() {
    var P = state.points, t = P.map(function (p) { return p.t; });
    var num = function (v) { return fmtRate(v); };
    var errs = P.map(function (p) { return p.errors; }), drop = P.map(function (p) { return p.dropped; });
    var conc = P.map(function (p) { return Math.max(p.inFlight, p.users); });
    var cpu = P.map(function (p) { return p.cpu; }), lag = P.map(function (p) { return ms(p.lagP99); });
    var pe = peakOf(errs), pd = peakOf(drop);
    return [
      { id: "m-err", title: "Errors & dropped", now: pe || pd ? "peak " + fmtRate(Math.max(pe, pd)) + "/s" : "none",
        series: [{ label: "errors", color: cssVar("--critical"), fmt: num }, { label: "dropped", color: cssVar("--warning"), fmt: num }], data: [t, errs, drop] },
      { id: "m-conc", title: "In flight", now: "peak " + fmtInt(peakOf(conc)),
        series: [{ label: "in flight", color: cssVar("--s1"), fmt: num, fill: alpha(cssVar("--s1"), 0.08) }], data: [t, conc] },
      { id: "m-cpu", title: "Generator CPU", now: "peak " + Math.round(peakOf(cpu)) + "%",
        series: [{ label: "cpu", color: cssVar("--s1"), fmt: function (v) { return v.toFixed(0) + "%"; } }], data: [t, cpu],
        yRange: [0, 100], yFmt: function (v) { return v + "%"; } },
      { id: "m-act", title: "In flight by age", now: "peak " + fmtInt(peakOf(P.map(function (p) { return p.active ? p.active.slice(1).reduce(function (a, b) { return a + b; }, 0) : 0; }))) + " over 1s",
        series: [{ label: "< 1s", color: cssVar("--s1"), fmt: num }, { label: "1–3s", color: cssVar("--warning"), fmt: num },
          { label: "3–8s", color: cssVar("--s2"), fmt: num }, { label: "8s +", color: cssVar("--critical"), fmt: num }],
        data: [t].concat([0, 1, 2, 3].map(function (i) { return P.map(function (p) { return p.active ? p.active[i] || 0 : null; }); })) },
      { id: "m-lag", title: "Scheduling lag p99", now: "peak " + fmtMs(peakOf(lag)),
        series: [{ label: "lag p99", color: cssVar("--s1"), fmt: fmtMs }], data: [t, lag], yFmt: function (v) { return compact(v) + "ms"; } }
    ];
  }

  function serverInfo() {
    var units = {}, names = [];
    (state.meta.server || []).forEach(function (s) { if (!(s.name in units)) { units[s.name] = s.unit || ""; names.push(s.name); } });
    state.points.forEach(function (p) {
      Object.keys(p.server || {}).forEach(function (n) { if (!(n in units)) { units[n] = ""; names.push(n); } });
    });
    names.sort();
    return { names: names, units: units };
  }
  function fmtServer(v, unit) {
    if (v == null) return "–";
    if (unit === "%") return (v === 0 ? "0" : v.toFixed(v < 10 ? 1 : 0)) + "%";
    if (unit === "bytes") {
      var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
      while (Math.abs(v) >= 1024 && i < u.length - 1) { v /= 1024; i++; }
      return v.toFixed(v < 10 ? 1 : 0) + " " + u[i];
    }
    if (unit === "ms") return fmtMs(v);
    return compact(v) + (unit === "/s" ? "/s" : "");
  }
  function serverSpecs() {
    var sv = serverInfo(), t = state.points.map(function (p) { return p.t; });
    return sv.names.map(function (n, i) {
      var unit = sv.units[n], data = state.points.map(function (p) { var v = (p.server || {})[n]; return v == null ? null : v; });
      return { id: "srv-" + i, title: n, now: "peak " + fmtServer(peakOf(data), unit),
        series: [{ label: n, color: cssVar("--s3"), fmt: function (v) { return fmtServer(v, unit); }, fill: alpha(cssVar("--s3"), 0.08) }],
        data: [t, data], yFmt: function (v) { return fmtServer(v, unit); } };
    });
  }

  function miniPanels(rootId, specs) {
    var root = $(rootId), want = specs.map(function (s) { return s.id; }).join(",");
    if (root.dataset.ids !== want) {
      root.dataset.ids = want;
      root.textContent = "";
      specs.forEach(function (s) {
        var now = el("span", "now"); now.id = s.id + "-now";
        var c = el("div", "chart"); c.id = s.id;
        root.appendChild(add(el("section", "panel"), add(el("div", "chart-h"), el("h3", null, s.title), now), c));
      });
    }
    specs.forEach(function (s) { $(s.id + "-now").textContent = s.now; });
  }

  function buildCharts() {
    renderHitmap();
    renderMap();
    var main = mainSpecs();
    Object.keys(main).forEach(function (id) { legend(main[id].legend, main[id].series); makeChart(id, main[id]); });
    var mini = miniSpecs();
    miniPanels("mini", mini);
    mini.forEach(function (s) { s.height = 130; makeChart(s.id, s); });
    var srv = serverSpecs();
    $("server-block").hidden = !srv.length;
    miniPanels("server", srv);
    srv.forEach(function (s) { s.height = 130; makeChart(s.id, s); });
    updateCharts.shape = shapeOf(main, srv);
  }

  function shapeOf(main, srv) { return main["c-rps"].series.length + "|" + srv.map(function (s) { return s.title; }).join(","); }

  function updateCharts() {
    var main = mainSpecs(), mini = miniSpecs(), srv = serverSpecs();
    if (shapeOf(main, srv) !== updateCharts.shape) { buildCharts(); return; }
    Object.keys(main).forEach(function (id) { charts[id].setData(main[id].data); });
    renderHitmap();
    miniPanels("mini", mini);
    miniPanels("server", srv);
    mini.concat(srv).forEach(function (s) { if (charts[s.id]) charts[s.id].setData(s.data); });
  }

  function resize() {
    renderHitmap();
    Object.keys(charts).forEach(function (id) {
      var root = $(id);
      if (root) charts[id].setSize({ width: Math.max(root.clientWidth, 200), height: charts[id].height });
    });
  }

  function renderStepPicker() {
    var sel = $("step");
    var keys = ((state.summary || {}).keys || []).filter(function (k) { return !isInternal(k.key); }).map(function (k) { return k.key; });
    var want = keys.join("\n");
    if (sel.dataset.keys === want) return;
    sel.dataset.keys = want;
    sel.textContent = "";
    var o = el("option", null, "All requests"); o.value = ""; sel.appendChild(o);
    keys.forEach(function (k) { var x = el("option", null, stepLabel(k)); x.value = k; sel.appendChild(x); });
    sel.value = state.step;
    sel.parentNode.hidden = keys.length < 2;
  }

  // ---------- tables ----------
  function table(head, rows, rowClass) {
    var t = el("table"), thead = el("thead"), tr = el("tr");
    head.forEach(function (h) { tr.appendChild(el("th", null, h)); });
    t.appendChild(add(thead, tr));
    var tb = el("tbody");
    rows.forEach(function (r, i) {
      var row = el("tr", rowClass ? rowClass(r, i) : null);
      r.forEach(function (c) {
        if (c && typeof c === "object") row.appendChild(c.node ? add(el("td", c.cls), c.node) : el("td", c.cls, c.text));
        else row.appendChild(el("td", null, c));
      });
      tb.appendChild(row);
    });
    t.appendChild(tb);
    return add(el("div", "scroll"), t);
  }
  function chip(text, cls) { return el("span", "chip " + cls, text); }

  function stepsTab() {
    var s = state.summary || {}, keys = s.keys || [], rows = [], cls = [];
    function row(name, k) {
      return [name, fmtInt(k.count), fmtRate(k.rps), { text: fmtPct(k.errorRate), cls: k.errorRate > 0 ? "up" : "dim" },
        fmtUs(k.p50), fmtUs(k.p90), fmtUs(k.p95), fmtUs(k.p99), fmtUs(k.p999), fmtUs(k.max)];
    }
    keys.forEach(function (k) { if (!isInternal(k.key)) { rows.push(row(stepLabel(k.key), k)); cls.push(null); } });
    rows.push(row("All requests", Object.assign({ count: s.requests, rps: s.rps, errorRate: s.errorRate }, s.latency || {}))); cls.push("total");
    keys.forEach(function (k) { if (k.step === "_iteration") { rows.push(row(stepLabel(k.key), k)); cls.push("sub"); } });
    return table(["step", "count", "rps", "errors", "p50", "p90", "p95", "p99", "p99.9", "max"], rows, function (_, i) { return cls[i]; });
  }

  function errorsRows() {
    var out = [], total = (state.summary || {}).errors || 0;
    ((state.summary || {}).keys || []).forEach(function (k) {
      if (k.step === "_iteration") return;
      Object.keys(k.errKinds || {}).forEach(function (kind) { out.push([stepLabel(k.key), kind, k.errKinds[kind], total]); });
    });
    out.sort(function (a, b) { return b[2] - a[2]; });
    return out;
  }
  function errorsTab() {
    return table(["step", "kind", "count", "share"], errorsRows().map(function (e) {
      return [e[0], { text: e[1], cls: "mono" }, fmtInt(e[2]), e[3] ? (e[2] / e[3] * 100).toFixed(1) + "%" : "–"];
    }));
  }

  function delta(a, b) {
    if (!a) return { text: b ? "new" : "0%", cls: "dim" };
    var v = (b - a) / a * 100;
    return { text: (v >= 0 ? "▲ " : "▼ ") + Math.abs(v).toFixed(1) + "%", cls: Math.abs(v) < 1 ? "dim" : (v > 0 ? "up" : "down") };
  }
  function baselineTab() {
    var c = state.compare, wrap = el("div");
    wrap.appendChild(el("p", "note", "Baseline " + c.base + " · " + new Date(c.baseStarted).toLocaleString()));
    wrap.appendChild(table(["step", "rps", "p95", "Δ p95", "p99", "Δ p99", "errors", "Δ pp", ""], (c.rows || []).map(function (r) {
      if (!r.base || !r.new) return [r.name, { text: r.new ? "only in this run" : "only in baseline", cls: "dim" }, "", "", "", "", "", "", ""];
      var b = r.base, n = r.new, dpp = (n.errorRate - b.errorRate) * 100;
      return [r.name, fmtRate(n.rps), fmtUs(n.p95), delta(b.p95, n.p95), fmtUs(n.p99), delta(b.p99, n.p99), fmtPct(n.errorRate),
        { text: (dpp >= 0 ? "+" : "") + dpp.toFixed(2), cls: Math.abs(dpp) < 0.01 ? "dim" : (dpp > 0 ? "up" : "down") },
        { node: (r.regressed || []).length ? chip("regressed: " + r.regressed.join(", "), "no") : chip("ok", "ok") }];
    })));
    return wrap;
  }

  function traceRows() {
    var rows = [];
    ((state.summary || {}).keys || []).forEach(function (k) {
      (k.failed || []).forEach(function (e) { rows.push({ k: k, e: e, failed: true }); });
      (k.slowest || []).forEach(function (e) { rows.push({ k: k, e: e, failed: false }); });
    });
    var failed = rows.filter(function (r) { return r.failed; }).sort(function (a, b) { return a.e.t - b.e.t; }).slice(0, 15);
    var slow = rows.filter(function (r) { return !r.failed; }).sort(function (a, b) { return b.e.latency - a.e.latency; }).slice(0, 15);
    return slow.concat(failed);
  }
  function traceCell(id) {
    var wrap = el("span"), url = state.meta.traceUrl, short = id.slice(0, 10) + "…" + id.slice(-4);
    var name = traceButton(id);
    void url; void short;
    var b = el("button", "copy", "copy");
    b.type = "button";
    b.addEventListener("click", function () {
      if (navigator.clipboard) navigator.clipboard.writeText(id).then(function () { b.textContent = "copied"; setTimeout(function () { b.textContent = "copy"; }, 1200); });
    });
    return add(wrap, name, b);
  }
  function tracesTab() {
    var wrap = el("div");
    wrap.appendChild(el("p", "note", "Every request carried a trace ID. Look one up in your tracing backend to see where the time went inside the server."));
    wrap.appendChild(table(["outcome", "step", "latency", "at", "trace ID"], traceRows().map(function (r) {
      return [{ node: r.failed ? chip(r.e.err, "no") : chip("slowest that second", "dim") }, stepLabel(r.k.key), fmtUs(r.e.latency),
        r.e.t + "s", { node: traceCell(r.e.traceId), cls: "mono" }];
    })));
    return wrap;
  }

  function scenariosTab() {
    return table(["scenario", "executor", "start", "stages", "max in flight"], (state.meta.scenarios || []).map(function (sc) {
      var unit = sc.executor === "arrival-rate" ? "/s" : " users";
      var st = (sc.stages || []).map(function (g) { return fmtDur(g.duration) + " → " + fmtRate(g.target) + unit; }).join(",  ");
      return [sc.name, { text: sc.executor, cls: "mono" }, fmtRate(sc.start) + unit, st, sc.maxInFlight ? fmtInt(sc.maxInFlight) : "–"];
    }));
  }

  function renderTabs() {
    var tabs = [{ id: "steps", label: "Steps", render: stepsTab }];
    var nErr = errorsRows().length;
    if (nErr) tabs.push({ id: "errors", label: "Errors", n: nErr, bad: true, render: errorsTab });
    if (state.compare) {
      var reg = (state.compare.rows || []).filter(function (r) { return (r.regressed || []).length; }).length;
      tabs.push({ id: "baseline", label: "vs baseline", n: reg || null, bad: reg > 0, render: baselineTab });
    }
    var nTr = traceRows().length;
    if (nTr) tabs.push({ id: "traces", label: "Traced requests", n: nTr, render: tracesTab });
    var nOps = ((state.apm || {}).operations || []).length;
    if (nOps) tabs.push({ id: "apm", label: "Server operations", n: nOps, render: apmTab });
    var nErrG = ((state.apm || {}).errors || []).length;
    if (nErrG) tabs.push({ id: "errgroups", label: "Error groups", n: nErrG, bad: true, render: errorGroupsTab });
    if ((state.logs && state.logs.total) || state.hasLogs) tabs.push({ id: "logs", label: "Logs", n: state.logs ? state.logs.total : null, render: logsTab });
    if ((state.topSql || []).length) tabs.push({ id: "sql", label: "Top SQL", n: state.topSql.length, render: sqlTab });
    if ((state.alerts || []).length) tabs.push({ id: "alerts", label: "Alerts", n: firingAlerts().length || state.alerts.length, bad: firingAlerts().length > 0, render: alertsTab });
    if (!state.watch) tabs.push({ id: "scenarios", label: "Scenarios", render: scenariosTab });
    else tabs.shift();
    if (!tabs.length) { $("tabs").textContent = ""; $("tabpanels").textContent = ""; return; }
    if (!tabs.some(function (t) { return t.id === state.tab; })) state.tab = tabs[0].id;
    if (!tabs.some(function (t) { return t.id === state.tab; })) state.tab = "steps";

    var nav = $("tabs"), panels = $("tabpanels");
    nav.textContent = ""; panels.textContent = "";
    tabs.forEach(function (t) {
      var b = el("button", null, t.label);
      b.type = "button"; b.setAttribute("role", "tab"); b.setAttribute("aria-selected", t.id === state.tab ? "true" : "false");
      if (t.n) b.appendChild(el("span", "n" + (t.bad ? " bad" : ""), String(t.n)));
      b.addEventListener("click", function () { state.tab = t.id; renderTabs(); });
      nav.appendChild(b);
      if (t.id === state.tab) panels.appendChild(add(el("div", "tabpanel"), t.render()));
    });
  }

  // ---------- hitmap ----------
  var hit = { src: "", sel: null, drag: null, geo: null };

  function hitKey() { return hit.src === "server" ? "apmHits" : "hits"; }
  function hasHits(k) { return state.points.some(function (p) { return p[k] && ((p[k].ok || []).length || (p[k].err || []).length); }); }

  function renderHitSrc() {
    var both = hasHits("hits") && hasHits("apmHits");
    if (!hit.src) hit.src = hasHits("hits") ? "client" : "server";
    if (hit.src === "client" && !hasHits("hits") && hasHits("apmHits")) hit.src = "server";
    var seg = $("hit-src");
    seg.textContent = "";
    seg.hidden = !both;
    [["client", "Requests"], ["server", "Server (APM)"]].forEach(function (o) {
      var b = el("button", null, o[1]);
      b.type = "button"; b.setAttribute("aria-selected", hit.src === o[0] ? "true" : "false");
      b.addEventListener("click", function () { hit.src = o[0]; hit.sel = null; renderHitmap(); renderHitList(); });
      seg.appendChild(b);
    });
    $("hit-hint").textContent = (hit.src === "server" ? "server transactions" : "every request") + " by time and response time · drag to select";
  }

  function renderHitmap() {
    var panel = $("hit-panel");
    panel.hidden = !(hasHits("hits") || hasHits("apmHits"));
    if (panel.hidden) return;
    renderHitSrc();
    legend("lg-hit", [{ label: "ok", color: cssVar("--s1") }, { label: "error", color: cssVar("--critical") }]);
    var box = $("hitmap"), cv = box.querySelector("canvas"), dpr = devicePixelRatio || 1;
    var W = box.clientWidth, H = box.clientHeight;
    cv.width = W * dpr; cv.height = H * dpr;
    var c = cv.getContext("2d");
    c.setTransform(dpr, 0, 0, dpr, 0, 0);
    c.clearRect(0, 0, W, H);
    var P = state.points, k = hitKey(), n = P.length;
    if (!n) return;
    var L = 52, R = 10, T = 10, B = 24, w = W - L - R, h = H - T - B;
    var lo = 59, hi = 0, maxC = 1;
    P.forEach(function (p) {
      var x = p[k];
      if (!x) return;
      [x.ok || [], x.err || []].forEach(function (arr) {
        arr.forEach(function (v, b) { if (v) { lo = Math.min(lo, b); hi = Math.max(hi, b); maxC = Math.max(maxC, v); } });
      });
    });
    if (lo > hi) { lo = 10; hi = 40; }
    lo = Math.max(0, Math.floor((lo - 3) / 10) * 10); hi = Math.min(59, Math.ceil((hi + 3) / 10) * 10);
    var rows = hi - lo + 1, cw = w / n, rh = h / rows;
    hit.geo = { L: L, T: T, w: w, h: h, n: n, lo: lo, hi: hi, cw: cw, rh: rh };
    // grid and labels at decades
    c.font = "11px -apple-system, BlinkMacSystemFont, system-ui, sans-serif";
    c.textAlign = "right"; c.textBaseline = "middle";
    var names = { 0: "100µs", 10: "1ms", 20: "10ms", 30: "100ms", 40: "1s", 50: "10s" };
    for (var b = lo; b <= hi + 1; b += 1) {
      if (b % 10) continue;
      var y = T + h - (b - lo) * rh;
      c.strokeStyle = cssVar("--grid"); c.lineWidth = 1;
      c.beginPath(); c.moveTo(L, Math.round(y) + 0.5); c.lineTo(L + w, Math.round(y) + 0.5); c.stroke();
      c.fillStyle = cssVar("--muted");
      if (names[b]) c.fillText(names[b], L - 8, y);
    }
    c.textAlign = "center"; c.textBaseline = "top";
    var step = Math.max(1, Math.ceil(n / 10 / 5) * 5);
    for (var i = 0; i < n; i++) {
      if (i % Math.max(1, Math.round(n / 8)) === 0) { c.fillStyle = cssVar("--muted"); c.fillText(fmtT(P[i].t), L + (i + 0.5) * cw, T + h + 6); }
    }
    // Cells: one hue, light to dark by count (log scale); errors on top in red.
    // Each cell is drawn as a soft rounded dot cluster so dense areas read as
    // solid bands and lone slow requests stand out as single dots.
    var s1 = cssVar("--s1"), cr = cssVar("--critical"), lm = Math.log(1 + maxC);
    var dot = Math.max(1.5, Math.min(cw, rh) * 0.5);
    function cells(arr, color, floor) {
      arr.forEach(function (v, b) {
        if (!v || b < lo || b > hi) return;
        var a = floor + (1 - floor) * Math.log(1 + v) / lm;
        c.globalAlpha = a;
        c.fillStyle = color;
        var x0 = L + idx * cw, y0 = T + h - (b - lo + 1) * rh;
        if (cw <= 6) { c.fillRect(x0, y0, cw + 0.5, rh + 0.5); return; }
        // spread up to 6 dots across the second, more for busier cells
        var k = Math.min(6, Math.max(1, Math.ceil(Math.log2(1 + v))));
        for (var j = 0; j < k; j++) {
          var cx = x0 + cw * (j + 0.5) / k, cy = y0 + rh / 2 + ((j * 37) % 5 - 2) * rh * 0.12;
          c.beginPath(); c.arc(cx, cy, dot, 0, Math.PI * 2); c.fill();
        }
      });
    }
    for (var idx = 0; idx < n; idx++) {
      var x = P[idx][k];
      if (!x) continue;
      cells(x.ok || [], s1, 0.18);
      cells(x.err || [], cr, 0.55);
    }
    c.globalAlpha = 1;
    // finding markers
    markerTimes().forEach(function (m) {
      var j = P.findIndex(function (p) { return p.t === m.t; });
      if (j < 0) return;
      var xx = Math.round(L + (j + 0.5) * cw) + 0.5;
      c.strokeStyle = cssVar(m.level === "critical" ? "--critical" : "--warning");
      c.setLineDash([3, 3]); c.globalAlpha = 0.8;
      c.beginPath(); c.moveTo(xx, T); c.lineTo(xx, T + h); c.stroke();
      c.setLineDash([]); c.globalAlpha = 1;
    });
    drawSel();
  }

  function cellAt(px, py) {
    var g = hit.geo;
    if (!g) return null;
    var i = Math.floor((px - g.L) / g.cw), b = g.lo + Math.floor((g.T + g.h - py) / g.rh);
    if (i < 0 || i >= g.n || b < g.lo || b > g.hi) return null;
    return { i: i, b: b };
  }

  function drawSel() {
    var sel = $("hitmap").querySelector(".sel"), g = hit.geo, s = hit.sel;
    if (!s || !g) { sel.hidden = true; return; }
    sel.hidden = false;
    sel.style.left = (g.L + s.i0 * g.cw) + "px";
    sel.style.width = Math.max(2, (s.i1 - s.i0 + 1) * g.cw) + "px";
    sel.style.top = (g.T + g.h - (s.b1 - g.lo + 1) * g.rh) + "px";
    sel.style.height = Math.max(2, (s.b1 - s.b0 + 1) * g.rh) + "px";
  }

  function hitSamples() {
    var s = hit.sel, k = hitKey(), out = [];
    if (!s) return out;
    var loUs = metricsFloor(s.b0), hiUs = s.b1 >= 59 ? Infinity : metricsFloor(s.b1 + 1);
    for (var i = s.i0; i <= s.i1; i++) {
      var p = state.points[i], x = p && p[k];
      if (!x) continue;
      (x.s || []).forEach(function (r) {
        var b = r.l <= 100 ? 0 : Math.min(59, Math.floor(Math.log10(r.l / 100) * 10));
        if (b >= s.b0 && b <= s.b1) out.push({ t: p.t - 1 + r.o / 1000, key: r.k, l: r.l, e: r.e, tr: r.tr });
      });
    }
    out.sort(function (a, b) { return b.l - a.l; });
    void loUs; void hiUs;
    return out;
  }
  function metricsFloor(b) { return 100 * Math.pow(10, b / 10); }

  function renderHitList() {
    var box = $("hit-list"), s = hit.sel;
    box.textContent = "";
    box.hidden = !s;
    if (!s) return;
    var rows = hitSamples(), P = state.points;
    var t0 = P[s.i0] ? P[s.i0].t - 1 : 0, t1 = P[s.i1] ? P[s.i1].t : 0;
    var head = el("div", "bar2");
    add(head, el("b", null, rows.length + " sampled " + (hit.src === "server" ? "transactions" : "requests")),
      document.createTextNode("between " + fmtT(t0) + " and " + fmtT(t1) + ", " + fmtUs(metricsFloor(s.b0)) + " – " + (s.b1 >= 59 ? "∞" : fmtUs(metricsFloor(s.b1 + 1)))));
    var clr = el("button", "btn", "Clear"); clr.type = "button";
    clr.addEventListener("click", function () { hit.sel = null; drawSel(); renderHitList(); });
    head.appendChild(clr);
    box.appendChild(head);
    if (!rows.length) { box.appendChild(el("p", "empty", "No samples in this area. Up to " + 60 + " requests are kept per second; try a larger selection.")); return; }
    box.appendChild(table(["at", hit.src === "server" ? "operation" : "step", "latency", "outcome", "trace"], rows.slice(0, 200).map(function (r) {
      return [state.watch ? fmtT(r.t) : r.t.toFixed(2) + "s", r.key.indexOf("|") >= 0 ? (hit.src === "server" ? r.key.replace("|", " · ") : stepLabel(r.key)) : r.key, fmtUs(r.l),
        { node: r.e ? chip(r.e, "no") : chip("ok", "ok") },
        r.tr ? { node: traceButton(r.tr), cls: "mono" } : { text: "–", cls: "dim" }];
    })));
  }

  function hitEvents() {
    var box = $("hitmap"), tip = box.querySelector(".tip");
    function pos(e) { var r = box.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; }
    box.addEventListener("mousedown", function (e) {
      var p = pos(e), cl = cellAt(p.x, p.y);
      if (!cl) return;
      hit.drag = cl; hit.sel = { i0: cl.i, i1: cl.i, b0: cl.b, b1: cl.b }; drawSel();
      e.preventDefault();
    });
    window.addEventListener("mousemove", function (e) {
      var p = pos(e), cl = cellAt(p.x, p.y);
      if (hit.drag && cl) {
        hit.sel = { i0: Math.min(hit.drag.i, cl.i), i1: Math.max(hit.drag.i, cl.i), b0: Math.min(hit.drag.b, cl.b), b1: Math.max(hit.drag.b, cl.b) };
        drawSel();
      }
      if (!cl || e.target.closest("#hitmap") === null) { tip.style.display = "none"; return; }
      var pt = state.points[cl.i], x = pt && pt[hitKey()];
      var ok = x && x.ok ? x.ok[cl.b] || 0 : 0, er = x && x.err ? x.err[cl.b] || 0 : 0;
      tip.textContent = "";
      tip.appendChild(el("div", "t", "at " + fmtT(pt.t) + " · " + fmtUs(metricsFloor(cl.b)) + " – " + fmtUs(metricsFloor(cl.b + 1))));
      [["ok", ok, "--s1"], ["errors", er, "--critical"]].forEach(function (r) {
        var row = el("div", "r"), sw = el("i"); sw.style.background = cssVar(r[2]);
        tip.appendChild(add(row, sw, el("span", null, r[0]), el("b", null, fmtInt(r[1]))));
      });
      tip.style.display = "block";
      var left = p.x + 14;
      if (left + tip.offsetWidth > box.clientWidth) left = p.x - tip.offsetWidth - 14;
      tip.style.left = left + "px"; tip.style.top = Math.max(0, p.y - tip.offsetHeight / 2) + "px";
    });
    window.addEventListener("mouseup", function () {
      if (!hit.drag) return;
      hit.drag = null;
      renderHitList();
    });
    box.addEventListener("mouseleave", function () { tip.style.display = "none"; });
  }

  // ---------- active transactions (live) ----------
  var activeTimer = null;
  function pollActive() {
    if (activeTimer) return;
    var tick = function () {
      fetch("api/active").then(function (r) { return r.json(); }).then(function (d) { renderActive(d.items || []); }).catch(function () {});
    };
    tick();
    activeTimer = setInterval(function () { if (state.done && !state.watch) { clearInterval(activeTimer); return; } tick(); }, 1500);
  }
  function renderActive(items) {
    $("active-panel").hidden = false;
    var counts = [0, 0, 0, 0];
    items.forEach(function (x) { counts[x.elapsedMs < 1000 ? 0 : x.elapsedMs < 3000 ? 1 : x.elapsedMs < 8000 ? 2 : 3]++; });
    var st = $("astat");
    st.textContent = "";
    ["< 1s", "1–3s", "3–8s", "8s +"].forEach(function (l, i) {
      st.appendChild(add(el("div", "a" + (i + 1)), el("div", "n", fmtInt(counts[i])), el("div", "l", l)));
    });
    $("active-hint").textContent = items.length ? items.length + " in flight" : "idle";
    var list = $("alist");
    list.textContent = "";
    items.slice(0, 40).forEach(function (x) {
      var a = x.elapsedMs < 1000 ? 1 : x.elapsedMs < 3000 ? 2 : x.elapsedMs < 8000 ? 3 : 4;
      var info = el("div"), nm = el("div", "nm");
      add(nm, el("span", "src", x.source === "load" ? "load" : x.source), document.createTextNode(x.name + (x.who ? " · " + x.who : "")));
      add(info, nm, x.detail ? el("div", "dt", x.detail) : null);
      var li = add(el("li", "a" + a), el("div", "el", x.elapsedMs < 1000 ? x.elapsedMs + "ms" : (x.elapsedMs / 1000).toFixed(1) + "s"), info);
      if (x.stack) {
        var sb = el("button", "linkish stackbtn", "stack"); sb.type = "button";
        sb.addEventListener("click", function (e) { e.stopPropagation(); openStack(x); });
        info.appendChild(sb);
      }
      if (x.traceId) { li.style.cursor = "pointer"; li.addEventListener("click", function () { openTrace(x.traceId); }); }
      list.appendChild(li);
    });
    if (!items.length) list.appendChild(add(el("li"), el("span"), el("p", "empty", "Nothing in flight right now.")));
  }

  // ---------- live APM / SQL / alerts ----------
  var liveTimer = null;
  function pollLive() {
    if (liveTimer) return;
    var tick = function () {
      fetch("api/live").then(function (r) { return r.json(); }).then(function (d) {
        if (d.operations) state.apm = Object.assign({}, state.apm || {}, { operations: d.operations });
        if (d.map) state.apm = Object.assign({}, state.apm || {}, { map: d.map });
        if (d.errors) state.apm = Object.assign({}, state.apm || {}, { errors: d.errors });
        if (d.logs) { state.hasLogs = true; state.logs = Object.assign({}, state.logs || {}, d.logs); }
        renderMap();
        if (d.topSql) state.topSql = d.topSql;
        if (d.alerts) state.alerts = d.alerts;
        renderHeader(); renderAlertBar(); renderTimeline();
        if (state.tab !== "logs") renderTabs();
      }).catch(function () {});
    };
    tick();
    liveTimer = setInterval(function () { if (state.done && !state.watch) { clearInterval(liveTimer); return; } tick(); }, 3000);
  }

  function firingAlerts() {
    var last = {};
    (state.alerts || []).forEach(function (e) { last[e.name] = e; });
    return Object.keys(last).map(function (k) { return last[k]; }).filter(function (e) { return e.state === "firing"; });
  }
  function renderAlertBar() {
    var bar = $("alertbar"), f = firingAlerts();
    bar.textContent = "";
    bar.hidden = !f.length || (state.done && !state.watch);
    if (bar.hidden) return;
    bar.appendChild(document.createTextNode("● " + f.length + (f.length === 1 ? " alert firing" : " alerts firing")));
    f.forEach(function (e) { bar.appendChild(el("span", null, e.name + " (" + e.value + ")")); });
  }

  // ---------- trace waterfall ----------
  function traceButton(id) {
    var b = el("button", "linkish", id.slice(0, 10) + "…" + id.slice(-4));
    b.type = "button"; b.title = id;
    b.addEventListener("click", function () { openTrace(id); });
    return b;
  }
  function findTrace(id) {
    var ts = (state.apm && state.apm.traces) || [];
    for (var i = 0; i < ts.length; i++) if (ts[i].traceId === id) return ts[i];
    return null;
  }
  function openTrace(id) {
    var m = $("modal");
    m.hidden = false;
    $("m-title").textContent = "Transaction";
    $("m-sub").textContent = id;
    var body = $("m-body");
    body.textContent = "";
    var t = findTrace(id);
    if (t) { renderWaterfall(body, t, id); return; }
    if (state.live || state.watch) {
      body.appendChild(el("p", "empty", "Loading…"));
      fetch("api/trace?id=" + encodeURIComponent(id)).then(function (r) { return r.ok ? r.json() : null; })
        .then(function (tr) { body.textContent = ""; if (tr) renderWaterfall(body, tr, id); else noTrace(body, id); })
        .catch(function () { body.textContent = ""; noTrace(body, id); });
      return;
    }
    noTrace(body, id);
  }
  function noTrace(body, id) {
    setTimeout(function () { traceLogs(body, id); }, 0);
    body.appendChild(el("p", "empty", "The server did not report this trace. Start lt with --apm and instrument the server (lt agent or OpenTelemetry) to see SQL and outgoing calls here."));
    if (state.meta.traceUrl) {
      var a = el("a", null, "Open in tracing backend ↗");
      a.href = state.meta.traceUrl.split("{traceId}").join(id); a.target = "_blank"; a.rel = "noopener";
      body.appendChild(add(el("p"), a));
    }
  }
  function renderWaterfall(body, t, id) {
    var spans = (t.spans || []).slice(), byId = {}, kids = {};
    spans.forEach(function (s) { byId[s.spanId] = s; });
    spans.forEach(function (s) { var p = byId[s.parentId] ? s.parentId : ""; (kids[p] = kids[p] || []).push(s); });
    Object.keys(kids).forEach(function (k) { kids[k].sort(function (a, b) { return a.startUs - b.startUs; }); });
    var order = [];
    (function walk(p, d) { (kids[p] || []).forEach(function (s) { order.push({ s: s, d: d }); walk(s.spanId, d + 1); }); })("", 0);
    var end = spans.reduce(function (m, s) { return Math.max(m, s.startUs + s.durUs); }, 1);
    var byKind = {};
    spans.forEach(function (s) { if (s.kind !== "server") byKind[s.kind] = (byKind[s.kind] || 0) + s.durUs; });
    var root = order[0] && order[0].s;
    $("m-title").textContent = root ? root.name : "Transaction";
    var sum = el("div", "wf-sum");
    add(sum, add(el("span"), document.createTextNode("total "), el("b", null, fmtUs(end))),
      add(el("span"), document.createTextNode("spans "), el("b", null, String(spans.length))));
    [["db", "--s2", "SQL"], ["http", "--s3", "outgoing calls"]].forEach(function (k) {
      if (!byKind[k[0]]) return;
      var sw = el("i"); sw.style.background = cssVar(k[1]);
      sum.appendChild(add(el("span"), sw, document.createTextNode(k[2] + " "), el("b", null, fmtUs(byKind[k[0]]) + " (" + Math.round(byKind[k[0]] / end * 100) + "%)")));
    });
    if (state.meta.traceUrl) {
      var a = el("a", null, "open in tracing backend ↗");
      a.href = state.meta.traceUrl.split("{traceId}").join(id); a.target = "_blank"; a.rel = "noopener";
      sum.appendChild(a);
    }
    body.appendChild(sum);
    var wf = el("div", "wf");
    order.forEach(function (o) {
      var s = o.s, row = el("div", "wf-row"), name = el("div", "wf-name");
      name.style.paddingLeft = (o.d * 14) + "px";
      add(name, el("span", "k", s.kind), document.createTextNode(s.name));
      name.title = s.service + " · " + s.name;
      var track = el("div", "wf-track"), bar = el("div", "wf-bar " + s.kind + (s.error ? " err" : ""));
      bar.style.left = (s.startUs / end * 100) + "%"; bar.style.width = Math.max(0.3, s.durUs / end * 100) + "%";
      track.appendChild(bar);
      add(row, name, track, el("div", "wf-dur", fmtUs(s.durUs)));
      var attrs = Object.keys(s.attrs || {});
      if (attrs.length) {
        row.style.cursor = "pointer";
        var det = el("div", "wf-attr", attrs.map(function (k) { return k + ": " + s.attrs[k]; }).join("\n"));
        det.hidden = !(s.kind === "db");
        row.addEventListener("click", function () { det.hidden = !det.hidden; });
        wf.appendChild(row); wf.appendChild(det);
      } else wf.appendChild(row);
    });
    body.appendChild(wf);
    traceLogs(body, id);
  }
  $("m-close").addEventListener("click", function () { $("modal").hidden = true; });
  $("modal").addEventListener("click", function (e) { if (e.target.id === "modal") $("modal").hidden = true; });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") $("modal").hidden = true; });

  // ---------- APM, SQL, alerts tables ----------
  function apmTab() {
    var ops = ((state.apm || {}).operations || []).slice(0, 100), total = ops.reduce(function (m, o) { return m + o.totalMs; }, 0) || 1;
    var wrap = el("div");
    wrap.appendChild(el("p", "note", "What the server spent time on, from its own tracing: endpoints, SQL and outgoing calls. " +
      ((state.apm || {}).spans ? fmtInt(state.apm.spans) + " spans from " + (state.apm.services || []).join(", ") + "." : "")));
    wrap.appendChild(table(["kind", "operation", "service", "count", "errors", "mean", "p95", "max", "total time"], ops.map(function (o) {
      var share = el("span"), bar = el("span", "share");
      bar.style.width = Math.max(2, o.totalMs / total * 80) + "px";
      add(share, bar, document.createTextNode(fmtMs(o.totalMs)));
      return [{ node: chip(o.kind, o.kind === "db" ? "dim" : "dim") }, { text: o.name, cls: o.kind === "db" ? "sql" : "" }, o.service, fmtInt(o.count),
        { text: fmtInt(o.errors), cls: o.errors ? "up" : "dim" }, fmtMs(o.meanMs), fmtMs(o.p95Ms), fmtMs(o.maxMs), { node: share }];
    })));
    return wrap;
  }
  function sqlTab() {
    var q = (state.topSql || []).slice(0, 60), total = q.reduce(function (m, x) { return m + x.totalMs; }, 0) || 1;
    var wrap = el("div");
    wrap.appendChild(el("p", "note", "Busiest statements by total time, from the databases' own statistics during the test."));
    wrap.appendChild(table(["source", "statement", "calls", "mean", "total time", "rows"], q.map(function (x) {
      var share = el("span"), bar = el("span", "share");
      bar.style.width = Math.max(2, x.totalMs / total * 80) + "px";
      add(share, bar, document.createTextNode(fmtMs(x.totalMs)));
      return [x.source, { text: x.query, cls: "sql" }, fmtInt(x.calls), fmtMs(x.meanMs), { node: share }, x.rows ? fmtInt(x.rows) : { text: "–", cls: "dim" }];
    })));
    return wrap;
  }
  function alertsTab() {
    return table(["at", "alert", "severity", "state", "value", "rule"], (state.alerts || []).map(function (e) {
      return [state.watch ? new Date(e.at).toLocaleTimeString() : e.t + "s", e.name, { node: chip(e.severity, e.severity === "critical" ? "no" : "dim") },
        { node: chip(e.state, e.state === "firing" ? "no" : "ok") }, e.value, { text: e.rule, cls: "mono" }];
    }));
  }

  // ---------- service map ----------
  function renderMap() {
    var m = state.apm && state.apm.map, box = $("map-block");
    box.hidden = !(m && m.nodes && m.nodes.length > 1);
    if (box.hidden) return;
    var root = $("svcmap");
    root.textContent = "";
    // Columns: client, then services by call depth, then databases and outside hosts.
    var depth = {}, out = {};
    var self = {};
    m.edges.forEach(function (e) {
      if (e.from === e.to) { self[e.from] = e; return; } // a service calling itself: shown on its box
      (out[e.from] = out[e.from] || []).push(e.to);
    });
    m.nodes.forEach(function (n) { if (n.kind === "client") depth[n.id] = 0; });
    var queue = Object.keys(depth);
    while (queue.length) { // breadth-first: each node at its shortest call depth
      var id = queue.shift();
      (out[id] || []).forEach(function (to) {
        if (depth[to] === undefined) { depth[to] = depth[id] + 1; queue.push(to); }
      });
    }
    var maxSvc = 1;
    m.nodes.forEach(function (n) { if (n.kind === "service") { if (depth[n.id] === undefined) depth[n.id] = 1; maxSvc = Math.max(maxSvc, depth[n.id]); } });
    m.nodes.forEach(function (n) { if (n.kind === "db" || n.kind === "http") depth[n.id] = maxSvc + 1; if (depth[n.id] === undefined) depth[n.id] = 0; });
    var cols = {};
    m.nodes.forEach(function (n) { (cols[depth[n.id]] = cols[depth[n.id]] || []).push(n); });
    var ncol = maxSvc + 2, W = Math.max(root.clientWidth, 640), nodeW = 200, nodeH = 58;
    var rowsMax = Math.max.apply(null, Object.keys(cols).map(function (k) { return cols[k].length; }));
    var H = Math.max(180, rowsMax * (nodeH + 26) + 30), pos = {};
    Object.keys(cols).forEach(function (k) {
      var list = cols[k].sort(function (a, b) { return b.count - a.count; });
      var x = 20 + (W - 40 - nodeW) * (ncol === 1 ? 0 : +k / (ncol - 1));
      list.forEach(function (n, i) { pos[n.id] = { x: x, y: (H - list.length * (nodeH + 26) + 26) / 2 + i * (nodeH + 26) }; });
    });
    var ns = "http://www.w3.org/2000/svg", svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 " + W + " " + H); svg.setAttribute("width", "100%"); svg.setAttribute("height", H);
    var maxC = Math.max.apply(null, m.edges.map(function (e) { return e.count; }).concat([1]));
    function mk(tag, attrs, text) { var e = document.createElementNS(ns, tag); Object.keys(attrs).forEach(function (k) { e.setAttribute(k, attrs[k]); }); if (text != null) e.textContent = text; return e; }
    m.edges.forEach(function (e) {
      var a = pos[e.from], b = pos[e.to];
      if (!a || !b || e.from === e.to) return;
      var x1 = a.x + nodeW, y1 = a.y + nodeH / 2, x2 = b.x, y2 = b.y + nodeH / 2, mx = (x1 + x2) / 2;
      var bad = e.errors > 0 && e.errors / e.count > 0.01;
      var col = bad ? cssVar("--critical") : cssVar("--faint");
      svg.appendChild(mk("path", { d: "M" + x1 + "," + y1 + " C" + mx + "," + y1 + " " + mx + "," + y2 + " " + x2 + "," + y2,
        fill: "none", stroke: col, "stroke-width": 1 + 3 * Math.sqrt(e.count / maxC), opacity: 0.9 }));
      var label = mk("text", { x: mx, y: (y1 + y2) / 2 - 6, "text-anchor": "middle", "font-size": 11, fill: bad ? cssVar("--critical") : cssVar("--muted") },
        compact(e.count) + " · " + fmtMs(e.meanMs) + (e.errors ? " · " + compact(e.errors) + " err" : ""));
      svg.appendChild(label);
    });
    var kindCol = { client: "--muted", service: "--s1", db: "--s2", http: "--s3" };
    m.nodes.forEach(function (n) {
      var p = pos[n.id], g = mk("g", {});
      var errRate = n.count ? n.errors / n.count : 0;
      g.appendChild(mk("rect", { x: p.x, y: p.y, width: nodeW, height: nodeH, rx: 10, fill: cssVar("--surface"),
        stroke: errRate > 0.01 ? cssVar("--critical") : cssVar("--line-2"), "stroke-width": errRate > 0.01 ? 2 : 1 }));
      g.appendChild(mk("rect", { x: p.x, y: p.y + 10, width: 4, height: nodeH - 20, rx: 2, fill: cssVar(kindCol[n.kind] || "--muted") }));
      g.appendChild(mk("text", { x: p.x + 14, y: p.y + 21, "font-size": 10, "font-weight": 700, fill: cssVar("--muted"), "letter-spacing": ".06em" }, n.kind.toUpperCase()));
      var name = n.name.length > 24 ? n.name.slice(0, 23) + "…" : n.name;
      g.appendChild(mk("text", { x: p.x + 14, y: p.y + 37, "font-size": 13, "font-weight": 650, fill: cssVar("--ink") }, name));
      g.appendChild(mk("text", { x: p.x + 14, y: p.y + 51, "font-size": 11, fill: cssVar("--ink-2") },
        compact(n.count) + " calls" + (n.meanMs ? " · " + fmtMs(n.meanMs) : "") + (n.errors ? " · " + (errRate * 100).toFixed(1) + "% err" : "")));
      if (self[n.id]) g.appendChild(mk("text", { x: p.x + nodeW - 10, y: p.y + 21, "font-size": 10, "text-anchor": "end", fill: cssVar("--muted") }, "↻ " + compact(self[n.id].count)));
      var title = mk("title", {}, n.id); g.appendChild(title);
      svg.appendChild(g);
    });
    root.appendChild(svg);
  }

  // ---------- logs ----------
  var logState = { q: "", lines: null, timer: null };
  function levelChip(l) {
    var lv = (l || "").toLowerCase();
    return chip(lv || "log", lv === "error" || lv === "fatal" ? "no" : (lv === "warn" || lv === "warning" ? "warn" : "dim"));
  }
  function localLogs(q, limit) {
    var all = (state.logs && state.logs.lines) || [], out = [], ql = q.toLowerCase().trim(), tr = /trace[:=]([0-9a-f]+)/.exec(ql), lv = /level[:=](\w+)/.exec(ql);
    var text = ql.replace(/trace[:=][0-9a-f]+/, "").replace(/level[:=]\w+/, "").trim();
    for (var i = all.length - 1; i >= 0 && out.length < limit; i--) {
      var l = all[i];
      if (tr && (l.traceId || "").toLowerCase() !== tr[1]) continue;
      if (lv && (l.level || "").toLowerCase() !== lv[1]) continue;
      if (text && (l.message || "").toLowerCase().indexOf(text) < 0) continue;
      out.push(l);
    }
    return out;
  }
  function fetchLogs(q, limit, cb) {
    if ((state.live && !state.done) || state.watch) {
      fetch("api/logs?limit=" + limit + "&q=" + encodeURIComponent(q)).then(function (r) { return r.json(); })
        .then(function (d) { cb(d.lines || []); }).catch(function () { cb([]); });
    } else cb(localLogs(q, limit));
  }
  function logTable(lines) {
    return table(["time", "level", "source", "message", "trace"], lines.map(function (l) {
      var msg = el("div", "logmsg", l.message);
      return [{ text: new Date(l.time).toLocaleTimeString([], { hour12: false }) + "." + String(new Date(l.time).getMilliseconds()).padStart(3, "0"), cls: "mono" },
        { node: levelChip(l.level) }, l.source || "", { node: msg, cls: "logcell" },
        l.traceId ? { node: traceButton(l.traceId), cls: "mono" } : { text: "–", cls: "dim" }];
    }));
  }
  function logsTab() {
    var wrap = el("div"), bar = el("div", "logbar"), input = el("input");
    input.type = "search"; input.placeholder = "Search: text, /regex/, level:error, trace:<id>, source:<name>"; input.value = logState.q;
    var res = el("div");
    var run = function () {
      logState.q = input.value;
      fetchLogs(logState.q, 300, function (lines) {
        res.textContent = "";
        res.appendChild(el("p", "note", lines.length ? lines.length + " lines, newest first" : "No matching lines."));
        if (lines.length) res.appendChild(logTable(lines));
      });
    };
    input.addEventListener("keydown", function (e) { if (e.key === "Enter") run(); });
    ["level:error", "level:warn", ""].forEach(function (q) {
      var b = el("button", "btn", q || "all"); b.type = "button";
      b.addEventListener("click", function () { input.value = q; run(); });
      bar.appendChild(b);
    });
    bar.insertBefore(input, bar.firstChild);
    add(wrap, bar, res);
    run();
    return wrap;
  }

  // ---------- error groups ----------
  function errorGroupsTab() {
    var groups = ((state.apm || {}).errors || []).slice().sort(function (a, b) { return b.count - a.count; });
    var wrap = el("div");
    wrap.appendChild(el("p", "note", "The same failure seen many times, grouped by service and message (from traces and logs)."));
    wrap.appendChild(table(["count", "source", "service", "message", "last seen", "example"], groups.map(function (g) {
      var msg = el("div", "logmsg", g.message);
      if (g.stack) {
        var d = el("details"), s = el("summary", null, "stack"), pre = el("pre", "stack", g.stack);
        add(d, s, pre); msg.appendChild(d);
      }
      return [{ text: fmtInt(g.count), cls: "up" }, { node: chip(g.source, g.source === "panic" ? "no" : "dim") }, g.service || "–",
        { node: msg, cls: "logcell" }, new Date(g.last).toLocaleTimeString(), g.traceId ? { node: traceButton(g.traceId), cls: "mono" } : { text: "–", cls: "dim" }];
    })));
    return wrap;
  }

  function openStack(x) {
    $("modal").hidden = false;
    $("m-title").textContent = "Active stack";
    $("m-sub").textContent = x.name + " · " + (x.elapsedMs / 1000).toFixed(1) + "s";
    var body = $("m-body");
    body.textContent = "";
    body.appendChild(el("p", "note", "Where " + (x.source || "the server") + " was when it reported this transaction" + (x.detail ? " (" + x.detail + ")" : "") + "."));
    body.appendChild(el("pre", "stack big", x.stack));
    if (x.traceId) { var b = traceButton(x.traceId); b.textContent = "Open the transaction"; body.appendChild(add(el("p"), b)); }
  }

  function traceLogs(body, id) {
    fetchLogs("trace:" + id, 200, function (lines) {
      if (!lines.length) return;
      body.appendChild(el("h3", "sub-h", "Logs of this transaction"));
      body.appendChild(logTable(lines.slice().reverse()));
    });
  }

  // ---------- history range (watch mode) ----------
  var liveBuffer = null;
  function renderRange() {
    var box = $("range");
    box.hidden = !(state.watch && state.history);
    if (box.hidden) return;
    box.textContent = "";
    [["live", "Live"], ["3600", "1h"], ["21600", "6h"], ["86400", "24h"], ["604800", "7d"]].forEach(function (o) {
      var b = el("button", null, o[1]);
      b.type = "button"; b.setAttribute("aria-selected", state.range === o[0] ? "true" : "false");
      b.addEventListener("click", function () { pickRange(o[0]); });
      box.appendChild(b);
    });
  }
  function pickRange(r) {
    if (r === state.range) return;
    if (state.range === "live") liveBuffer = state.points;
    state.range = r;
    hit.sel = null; renderHitList();
    renderRange();
    if (r === "live") {
      state.points = liveBuffer || [];
      liveBuffer = null;
      renderAll(); buildCharts();
      return;
    }
    var to = Math.floor(Date.now() / 1000), from = to - (+r);
    fetch("api/range?from=" + from + "&to=" + to + "&points=600").then(function (x) { return x.json(); }).then(function (d) {
      state.points = d.points || [];
      renderAll(); buildCharts();
    });
  }

  // ---------- wiring ----------
  function renderAll() {
    document.body.classList.toggle("watching", state.watch);
    renderRange();
    $("signals-block").hidden = state.watch;
    $("timeline").closest(".panel").querySelector("h2").textContent = state.watch ? "Alert history" : "What happened";
    renderHeader();
    renderAlertBar();
    renderKPIs();
    renderTimeline();
    renderChecks();
    renderStepPicker();
    renderTabs();
  }

  $("step").addEventListener("change", function (e) {
    state.step = e.target.value;
    charts["c-lat"].setData(mainSpecs()["c-lat"].data);
  });
  var rt;
  window.addEventListener("resize", function () { clearTimeout(rt); rt = setTimeout(resize, 100); });
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    if (mq.addEventListener) mq.addEventListener("change", function () { renderKPIs(); buildCharts(); });
  }

  hitEvents();
  renderAll();
  buildCharts();
  if ((state.live && !state.done) || state.watch) { pollActive(); pollLive(); }
  var hm = /[#&]trace=([0-9a-fA-F]+)/.exec(location.hash);
  if (hm) openTrace(hm[1].toLowerCase());
  else if (location.hash === "#slowest") {
    var tr = traceRows().filter(function (r) { return findTrace(r.e.traceId); })[0];
    if (tr) openTrace(tr.e.traceId);
  }

  if (state.live && !state.done && window.EventSource) {
    var es = new EventSource("events"), pending = false;
    var schedule = function () {
      if (pending) return;
      pending = true;
      requestAnimationFrame(function () { pending = false; renderAll(); updateCharts(); });
    };
    var take = function (v) {
      if (v.meta) state.meta = v.meta;
      state.summary = v.summary || {};
      state.thresholds = v.thresholds || [];
      state.warnings = v.warnings || [];
      state.insights = v.insights || [];
      state.stopped = v.stopped || null;
      state.compare = v.compare || null;
      if (v.alerts) state.alerts = v.alerts;
      if (v.topSql) state.topSql = v.topSql;
      if (v.apm) state.apm = v.apm;
      if (v.logs) state.logs = v.logs;
      if (v.watch !== undefined) state.watch = !!v.watch;
      state.passed = !!v.passed;
    };
    es.addEventListener("init", function (ev) {
      var v = JSON.parse(ev.data);
      take(v);
      state.points = v.points || [];
      state.done = !!v.done;
      renderAll();
      buildCharts();
      if (state.done) es.close();
    });
    es.addEventListener("meta", function (ev) {
      state.meta = JSON.parse(ev.data);
      updateCharts.shape = "";
      schedule();
    });
    es.addEventListener("point", function (ev) {
      var m = JSON.parse(ev.data);
      var into = state.range === "live" ? state.points : liveBuffer;
      if (into) {
        into.push(m.point);
        if (state.watch && into.length > 3600) into.splice(0, into.length - 3600);
      }
      if (state.range !== "live") return;
      state.summary = m.summary;
      schedule();
    });
    es.addEventListener("done", function (ev) {
      take(JSON.parse(ev.data));
      state.done = true;
      es.close();
      $("active-panel").hidden = true;
      renderAll();
      buildCharts();
    });
  }
})();
