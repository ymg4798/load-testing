(function () {
  "use strict";
  var D = window.LT_DATA || {}, runs = D.runs || [];
  var charts = {};

  function $(id) { return document.getElementById(id); }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }
  function add(p) { for (var i = 1; i < arguments.length; i++) if (arguments[i]) p.appendChild(arguments[i]); return p; }
  function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }
  function usParts(us) {
    if (us == null || isNaN(us)) return ["–", ""];
    if (us < 1000) return [String(Math.round(us)), "µs"];
    if (us < 1e6) return [(us / 1000).toFixed(us < 10000 ? 2 : 1), "ms"];
    return [(us / 1e6).toFixed(2), "s"];
  }
  function fmtUs(us) { var p = usParts(us); return p[0] + p[1]; }
  function when(r) {
    var d = new Date(r.started);
    return (d.getMonth() + 1) + "/" + d.getDate() + " " + String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }
  function runLabel(i) {
    var r = runs[i];
    return r ? "#" + (i + 1) + "  " + when(r) + (r.label ? "  " + r.label : "") + (r.pinned ? "  ★" : "") : "";
  }
  function result(r) { return r.stopped ? "stopped" : (!r.thresholds ? "–" : (r.passed ? "pass" : "fail")); }

  // ---------- header ----------
  var last = runs[runs.length - 1], base = runs.filter(function (r) { return r.pinned; })[0];
  $("crumb").textContent = D.test || "";
  $("name").textContent = D.test || "lt trend";
  if (runs.length) $("meta").appendChild(el("span", null, when(runs[0]) + " – " + when(last)));
  var h = $("headline");
  if (!runs.length) h.textContent = "No stored runs yet.";
  else {
    var ref = base && base !== last ? base : runs[runs.length - 2];
    add(h, el("b", null, runs.length + " runs"), document.createTextNode(". Latest p95 "), el("b", null, fmtUs(last.p95)));
    if (ref && ref.p95) {
      var d = (last.p95 - ref.p95) / ref.p95 * 100;
      h.appendChild(document.createTextNode(", " + (d >= 0 ? "+" : "") + d.toFixed(1) + "% vs " + (ref === base ? "the baseline" : "the run before") + "."));
    }
  }

  // ---------- latest run tiles ----------
  if (last) {
    var tiles = [
      ["Latest p95", usParts(last.p95), "p50 " + fmtUs(last.p50)],
      ["Latest p99", usParts(last.p99), ""],
      ["Throughput", [last.rps.toFixed(1), "/s"], last.requests.toLocaleString("en-US") + " requests"],
      ["Error rate", [(last.errorRate * 100).toFixed(2), "%"], ""],
      ["Result", [result(last), ""], when(last)]
    ];
    var root = $("kpis");
    root.style.setProperty("--kpi-n", tiles.length);
    tiles.forEach(function (t) {
      var v = el("div", "v" + (t[0] === "Result" && t[1][0] !== "pass" && t[1][0] !== "–" ? " bad" : ""), t[1][0]);
      if (t[1][1]) v.appendChild(el("small", null, t[1][1]));
      add(root, add(el("div", "kpi"), el("div", "k", t[0]), v, el("div", "d", t[2] || " ")));
    });
  }

  // ---------- charts ----------
  function legend(id, series) {
    var root = $(id);
    if (!root) return;
    root.textContent = "";
    series.forEach(function (s) { var i = el("i"); i.style.background = s.color; root.appendChild(add(el("span"), i, document.createTextNode(s.label))); });
  }
  function tooltip(series) {
    var tip;
    return { hooks: {
      init: [function (u) { tip = el("div", "tip"); u.over.appendChild(tip); }],
      setCursor: [function (u) {
        var i = u.cursor.idx;
        if (i == null || u.cursor.left < 0) { tip.style.display = "none"; return; }
        tip.textContent = "";
        tip.appendChild(el("div", "t", runLabel(i)));
        series.forEach(function (s, k) {
          var v = u.data[k + 1][i], row = el("div", "r"), sw = el("i");
          sw.style.background = s.color;
          add(row, sw, el("span", null, s.label), el("b", null, v == null ? "–" : s.fmt(v)));
          tip.appendChild(row);
        });
        tip.style.display = "block";
        var w = tip.offsetWidth, hh = tip.offsetHeight, left = u.cursor.left + 14;
        if (left + w > u.over.clientWidth) left = u.cursor.left - w - 14;
        tip.style.left = left + "px";
        tip.style.top = Math.max(0, Math.min(u.cursor.top - hh / 2, u.over.clientHeight - hh)) + "px";
      }]
    } };
  }
  function axis(fmt, isY) {
    return {
      stroke: cssVar("--muted"), grid: { stroke: cssVar("--grid"), width: 1 }, ticks: { show: false },
      values: function (u, vals) { return vals.map(fmt); }, size: isY ? 50 : 26, gap: 6,
      font: "11px -apple-system, BlinkMacSystemFont, system-ui, sans-serif"
    };
  }
  function build() {
    Object.keys(charts).forEach(function (k) { charts[k].destroy(); });
    charts = {};
    var x = runs.map(function (_, i) { return i; });
    var ms = function (v) { return fmtUs(v * 1000); };
    var specs = {
      "t-lat": { legend: "lg-lat", height: 300,
        series: [{ label: "p50", color: cssVar("--s1"), fmt: ms }, { label: "p95", color: cssVar("--s2"), fmt: ms }, { label: "p99", color: cssVar("--s3"), fmt: ms }],
        data: [x, runs.map(function (r) { return r.p50 / 1000; }), runs.map(function (r) { return r.p95 / 1000; }), runs.map(function (r) { return r.p99 / 1000; })],
        yFmt: function (v) { return v + "ms"; } },
      "t-rps": { height: 130, series: [{ label: "requests/s", color: cssVar("--s1"), fmt: function (v) { return v.toFixed(1) + "/s"; } }],
        data: [x, runs.map(function (r) { return r.rps; })], yFmt: function (v) { return v >= 1000 ? (v / 1000) + "k" : String(v); } },
      "t-err": { height: 110, series: [{ label: "errors", color: cssVar("--critical"), fmt: function (v) { return v.toFixed(2) + "%"; } }],
        data: [x, runs.map(function (r) { return r.errorRate * 100; })], yFmt: function (v) { return v + "%"; } }
    };
    Object.keys(specs).forEach(function (id) {
      var sp = specs[id], root = $(id);
      if (sp.legend) legend(sp.legend, sp.series);
      root.textContent = "";
      charts[id] = new uPlot({
        width: Math.max(root.clientWidth, 200), height: sp.height, padding: [14, 10, 0, 0],
        scales: { x: { time: false }, y: { range: function (u, min, max) { return [0, max > 0 ? max * 1.12 : 1]; } } },
        axes: [axis(function (v) { return Number.isInteger(v) ? "#" + (v + 1) : ""; }), axis(sp.yFmt, true)],
        series: [{ label: "run" }].concat(sp.series.map(function (s) {
          return { label: s.label, stroke: s.color, width: 1.75, points: { show: true, size: 7, width: 2, fill: cssVar("--surface") } };
        })),
        cursor: { sync: { key: "trend" }, y: false, points: { size: 9 } },
        legend: { show: false },
        plugins: [tooltip(sp.series)]
      }, sp.data, root);
    });
  }

  // ---------- table ----------
  var t = el("table"), thead = el("thead"), hr = el("tr");
  ["run", "started", "label", "rps", "p50", "p95", "p99", "errors", "result", "file"].forEach(function (x) { hr.appendChild(el("th", null, x)); });
  t.appendChild(add(thead, hr));
  var tb = el("tbody");
  runs.slice().reverse().forEach(function (r, j) {
    var i = runs.length - 1 - j, tr = el("tr");
    [("#" + (i + 1)) + (r.pinned ? "  ★" : ""), new Date(r.started).toLocaleString(), r.label || "–", r.rps.toFixed(1),
      fmtUs(r.p50), fmtUs(r.p95), fmtUs(r.p99), (r.errorRate * 100).toFixed(2) + "%"].forEach(function (c) { tr.appendChild(el("td", null, c)); });
    var res = result(r), td = el("td");
    td.appendChild(el("span", "chip " + (res === "pass" ? "ok" : (res === "–" ? "dim" : "no")), res));
    tr.appendChild(td);
    tr.appendChild(el("td", "mono dim", r.file));
    tb.appendChild(tr);
  });
  t.appendChild(tb);
  $("runs").appendChild(add(el("div", "scroll"), t));

  build();
  var rt;
  window.addEventListener("resize", function () { clearTimeout(rt); rt = setTimeout(build, 100); });
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    if (mq.addEventListener) mq.addEventListener("change", build);
  }
})();
