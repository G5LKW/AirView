// Software update page of airview-feeder (cmd/airview-feeder/updateapi.go).
// Loopback only; every POST/PUT carries the X-AirView-Feeder header (CSRF).
(function () {
  "use strict";
  var H = { "X-AirView-Feeder": "1" };
  var $ = function (id) { return document.getElementById(id); };
  var state = null, settings = null, polling = null;

  function text(id, s) { $(id).textContent = s; }
  function show(id, on) { $(id).hidden = !on; }
  function when(iso) {
    if (!iso) return "never";
    var d = new Date(iso);
    if (isNaN(d.getTime()) || d.getFullYear() < 2000) return "never";
    return d.toLocaleString();
  }
  function lastUpdate(j) {
    if (!j || !j.phase || j.phase === "idle") return "none";
    var s = j.phase;
    if (j.to) s += " " + (j.from ? j.from + " → " : "") + j.to;
    if (j.result) s += " — " + j.result;
    if (j.finished && j.finished.slice(0, 4) > "2000") s += " (" + when(j.finished) + ")";
    return s;
  }
  function err(msg) { text("u-error", msg || ""); show("u-error", !!msg); }

  function render(s) {
    state = s;
    text("u-version", s.version || "?");
    text("u-target", s.target || "?");
    text("u-checked", when(s.lastCheck) + (s.lastError ? " (failed: " + s.lastError + ")" : ""));
    text("u-available", s.available ? s.available + " (" + (s.channel || "stable") + ")"
      : (s.lastCheck && !s.lastError ? "you have the newest version" : "not known yet"));
    text("u-last", lastUpdate(s.last));
    var note = s.installNote || "";
    if (!s.configured && !note) note = "Updates are not set up on this feeder.";
    text("u-note", note); show("u-note", !!note);
    $("b-check").disabled = !s.configured || s.busy;
    show("b-install", !!(s.available && s.canInstall));
    $("b-install").disabled = !!s.busy;
    show("u-busy", !!s.busy);
    err(s.actionError || "");
    if (s.busy && !polling) polling = setInterval(load, 2000);
    if (!s.busy && polling) { clearInterval(polling); polling = null; }
  }

  function req(method, path, body) {
    var o = { method: method, headers: {}, credentials: "same-origin", cache: "no-store" };
    if (method !== "GET") o.headers = Object.assign({}, H);
    if (body !== undefined) { o.headers["Content-Type"] = "application/json"; o.body = JSON.stringify(body); }
    return fetch(path, o).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
        return j;
      });
    });
  }

  function load() {
    return req("GET", "/api/update").then(render, function (e) {
      // The feeder restarts during an install: keep polling quietly.
      if (!polling) err(e.message);
    });
  }

  function loadSettings() {
    return req("GET", "/api/settings").then(function (doc) {
      settings = doc;
      var c = doc.config || {}, u = c.update || {};
      $("s-auto").checked = !!c.autoUpdate;
      var ch = u.channel || "stable";
      var sel = $("s-channel");
      if (![].some.call(sel.options, function (o) { return o.value === ch; })) {
        var o = document.createElement("option"); o.value = o.textContent = ch; sel.appendChild(o);
      }
      sel.value = ch;
      var locked = doc.locked || [];
      $("s-auto").disabled = doc.readOnly || locked.indexOf("autoUpdate") >= 0;
      sel.disabled = doc.readOnly || locked.indexOf("update.channel") >= 0;
      $("b-save").disabled = !!doc.readOnly;
      if (doc.readOnly) text("s-msg", "Read-only: no writable configuration file.");
      else if (locked.indexOf("autoUpdate") >= 0 || locked.indexOf("update.channel") >= 0) text("s-msg", "Some settings are locked by the environment or the command line.");
    }, function (e) { text("s-msg", "Settings unavailable: " + e.message); $("b-save").disabled = true; });
  }

  $("b-check").addEventListener("click", function () {
    $("b-check").disabled = true; show("u-busy", true); err("");
    req("POST", "/api/update/check").then(render, function (e) { err(e.message); load(); });
  });
  $("b-install").addEventListener("click", function () {
    if (!state || !state.available) return;
    if (!confirm("Install " + state.available + " now? The feeder restarts; it goes back to " + state.version + " by itself if the new version does not start cleanly.")) return;
    $("b-install").disabled = true; err("");
    req("POST", "/api/update/install").then(function (s) {
      render(s);
      if (!polling) polling = setInterval(load, 2000);
    }, function (e) { err(e.message); load(); });
  });
  $("b-save").addEventListener("click", function () {
    if (!settings) return;
    var c = JSON.parse(JSON.stringify(settings.config || {}));
    c.autoUpdate = $("s-auto").checked;
    c.update = c.update || {};
    c.update.channel = $("s-channel").value;
    $("b-save").disabled = true; text("s-msg", "Saving…");
    req("PUT", "/api/settings", c).then(function (r) {
      var notes = (r && r.notes) || [];
      text("s-msg", notes.length ? "Saved. " + notes.join(" ") : "Saved.");
      return Promise.all([loadSettings(), load()]);
    }, function (e) { text("s-msg", "Not saved: " + e.message); }).then(function () {
      if (settings && !settings.readOnly) $("b-save").disabled = false;
    });
  });

  load(); loadSettings();
})();
