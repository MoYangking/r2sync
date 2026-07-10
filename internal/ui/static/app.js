/* r2sync 管理控制台 — 交互层
   无依赖单文件；所有动态内容用 DOM API 构建（CSP: script-src 'self'）。 */
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  const STATUS_POLL_MS = 8000;
  const PROGRESS_POLL_MS = 1000;
  const RUNNING_STAGES = new Set(["initial", "scheduled", "manual", "verify"]);

  const state = {
    authed: false,
    config: null,
    method: "r2",
    syncingLocal: false,
    nextSyncAt: null,
    lastSuccessAt: null,
    statusTimer: 0,
    progressTimer: 0,
    clockTimer: 0,
  };

  /* ---------------- 工具 ---------------- */

  const opsFmt = new Intl.NumberFormat("zh-CN");
  const opsCompactFmt = new Intl.NumberFormat("zh-CN", { notation: "compact" });

  function humanBytes(n) {
    if (!Number.isFinite(n) || n < 0) return "—";
    if (n === 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let v = n;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    const text = i === 0 || v >= 100 ? Math.round(v).toString() : v.toFixed(v >= 10 ? 1 : 2);
    return `${text} ${units[i]}`;
  }

  function bytesToGib(bytes) {
    if (!Number.isFinite(bytes) || bytes <= 0) return "";
    return String(Math.round((bytes / 2 ** 30) * 100) / 100);
  }

  function parseTime(value) {
    if (!value) return null;
    const d = new Date(value);
    if (Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1) return null;
    return d;
  }

  function absTime(d) {
    return d ? d.toLocaleString("zh-CN", { hour12: false }) : "";
  }

  function relTime(d) {
    if (!d) return "—";
    const s = (Date.now() - d.getTime()) / 1000;
    if (s < 0) return absTime(d);
    if (s < 45) return "刚刚";
    if (s < 3600) return `${Math.round(s / 60)} 分钟前`;
    if (s < 86400) return `${Math.round(s / 3600)} 小时前`;
    return `${Math.round(s / 86400)} 天前`;
  }

  function pad2(n) {
    return String(n).padStart(2, "0");
  }

  function countdownText(d) {
    const diff = Math.floor((d.getTime() - Date.now()) / 1000);
    if (diff <= 0) return "即将执行";
    const h = Math.floor(diff / 3600);
    const m = Math.floor((diff % 3600) / 60);
    const s = diff % 60;
    return h > 0 ? `${h}:${pad2(m)}:${pad2(s)} 后` : `${m}:${pad2(s)} 后`;
  }

  function setText(el, text) {
    if (el) el.textContent = text;
  }

  /* ---------------- API ---------------- */

  async function api(path, options = {}) {
    const init = { ...options };
    delete init.auth;
    if (init.body && typeof init.body !== "string") {
      init.headers = { "Content-Type": "application/json", ...(init.headers || {}) };
      init.body = JSON.stringify(init.body);
    }
    const res = await fetch(path, init);
    let data = {};
    try {
      data = await res.json();
    } catch {
      /* 空响应体 */
    }
    if (!res.ok) {
      const err = new Error(data.message || res.statusText || "请求失败");
      err.status = res.status;
      err.code = data.code || "";
      if (res.status === 401 && options.auth !== false && state.authed) {
        forceLogin("登录已过期，请重新登录");
      }
      throw err;
    }
    return data;
  }

  /* ---------------- Toast ---------------- */

  function toast(message, kind = "info", ms = 3800) {
    const root = $("toast-root");
    while (root.children.length >= 4) root.firstChild.remove();
    const el = document.createElement("div");
    el.className = `toast glass ${kind}`;
    el.textContent = message;
    root.append(el);
    setTimeout(() => {
      el.classList.add("leaving");
      setTimeout(() => el.remove(), 220);
    }, ms);
  }

  /* ---------------- 模态 ---------------- */

  function confirmModal({ title, text, requireText = "", okLabel = "确认", danger = true }) {
    return new Promise((resolve) => {
      const root = $("modal-root");
      const input = $("modal-confirm-input");
      const ok = $("modal-ok");
      const cancel = $("modal-cancel");
      const field = $("modal-confirm-field");

      setText($("modal-title"), title);
      setText($("modal-text"), text);
      ok.textContent = okLabel;
      ok.className = danger ? "btn btn-danger" : "btn btn-primary";
      field.hidden = !requireText;
      if (requireText) {
        setText($("modal-confirm-label"), `输入 ${requireText} 以确认`);
        input.value = "";
        ok.disabled = true;
      } else {
        ok.disabled = false;
      }

      const close = (result) => {
        root.hidden = true;
        input.removeEventListener("input", onInput);
        ok.removeEventListener("click", onOk);
        cancel.removeEventListener("click", onCancel);
        $("modal-backdrop").removeEventListener("click", onCancel);
        document.removeEventListener("keydown", onKey);
        resolve(result);
      };
      const onInput = () => {
        ok.disabled = input.value.trim() !== requireText;
      };
      const onOk = () => close(true);
      const onCancel = () => close(false);
      const onKey = (e) => {
        if (e.key === "Escape") close(false);
      };

      input.addEventListener("input", onInput);
      ok.addEventListener("click", onOk);
      cancel.addEventListener("click", onCancel);
      $("modal-backdrop").addEventListener("click", onCancel);
      document.addEventListener("keydown", onKey);

      root.hidden = false;
      if (requireText) input.focus();
    });
  }

  /* ---------------- 按钮忙碌态 ---------------- */

  function setBtnBusy(btn, busy, busyLabel = "进行中…") {
    if (busy) {
      if (!btn.dataset.label) btn.dataset.label = btn.textContent;
      btn.disabled = true;
      btn.textContent = "";
      const spin = document.createElement("span");
      spin.className = "spin";
      btn.append(spin, document.createTextNode(busyLabel));
    } else {
      btn.disabled = false;
      if (btn.dataset.label) {
        btn.textContent = btn.dataset.label;
        delete btn.dataset.label;
      }
    }
  }

  /* ---------------- 视图切换 ---------------- */

  function showLogin() {
    stopTimers();
    state.authed = false;
    $("view-app").hidden = true;
    $("view-login").hidden = false;
    $("login-password").value = "";
    $("login-password").focus();
  }

  function showApp() {
    $("view-login").hidden = true;
    $("view-app").hidden = false;
  }

  function forceLogin(message) {
    showLogin();
    if (message) toast(message, "warn");
  }

  function stopTimers() {
    clearInterval(state.statusTimer);
    clearInterval(state.progressTimer);
    clearInterval(state.clockTimer);
    state.statusTimer = state.progressTimer = state.clockTimer = 0;
  }

  /* ---------------- 渲染：状态 ---------------- */

  const STAGE_LABELS = {
    initial: ["初始同步", "badge-cyan"],
    scheduled: ["周期同步", "badge-cyan"],
    manual: ["手动同步", "badge-cyan"],
    verify: ["严格校验", "badge-violet"],
    complete: ["空闲", "badge-green"],
    error: ["异常", "badge-rose"],
  };

  function renderStage(status) {
    const stage = status.stage || "";
    const [label, cls] = STAGE_LABELS[stage] || ["待机", "badge-neutral"];
    const badge = $("stage-badge");
    badge.textContent = label;
    badge.className = `badge ${cls}`;

    const running = RUNNING_STAGES.has(stage);
    setText($("current-target"), running && status.current_target ? status.current_target : "");

    const track = $("progress-track");
    if (running || state.syncingLocal) {
      track.hidden = false;
      const pct = Math.max(2, Math.min(100, status.progress ?? 0));
      $("progress-fill").style.width = `${pct}%`;
    } else {
      track.hidden = true;
    }

    const errEl = $("status-error");
    if (stage === "error" && status.last_error) {
      errEl.textContent = status.last_error;
      errEl.hidden = false;
    } else {
      errEl.hidden = true;
    }

    const pill = $("ready-pill");
    if (running || state.syncingLocal) {
      pill.textContent = "同步中";
      pill.className = "pill pill-busy";
    } else if (status.ready) {
      pill.textContent = "已就绪";
      pill.className = "pill pill-ok";
    } else if (stage === "error") {
      pill.textContent = "异常";
      pill.className = "pill pill-bad";
    } else {
      pill.textContent = "未就绪";
      pill.className = "pill pill-warn";
    }
    return running;
  }

  function renderClock() {
    const next = $("next-sync");
    if (state.nextSyncAt) {
      next.textContent = countdownText(state.nextSyncAt);
      next.title = absTime(state.nextSyncAt);
    } else {
      next.textContent = "—";
      next.title = "";
    }
    const last = $("last-success");
    if (state.lastSuccessAt) {
      last.textContent = relTime(state.lastSuccessAt);
      last.title = absTime(state.lastSuccessAt);
    } else {
      last.textContent = "—";
      last.title = "";
    }
  }

  /* ---------------- 渲染：用量 ---------------- */

  function applyMeter(fillEl, ratio, warnAt, blockAt) {
    const pct = Math.max(0, Math.min(100, ratio * 100));
    fillEl.style.width = `${pct}%`;
    fillEl.classList.remove("warn", "crit");
    if (blockAt > 0 && ratio >= blockAt) fillEl.classList.add("crit");
    else if (warnAt > 0 && ratio >= warnAt) fillEl.classList.add("warn");
  }

  function renderMetrics(data) {
    const cfg = data.config || {};
    const counters = data.counters || {};
    const used = data.current_bytes || 0;
    const cap = cfg.storage_cap_bytes || 0;
    const isGitHub = cfg.sync_method === "github";

    $("card-classa").hidden = isGitHub;
    $("card-classb").hidden = isGitHub;
    $("card-transfer").hidden = isGitHub;
    $("card-repo").hidden = !isGitHub;
    if (isGitHub) {
      setText($("m-repo"), cfg.github_repo || "未配置");
      setText($("m-repo-branch"), cfg.github_branch || "main");
    }

    setText($("m-storage"), humanBytes(used));
    setText($("m-storage-cap"), cap > 0 ? `/ ${humanBytes(cap)}` : "");
    applyMeter($("meter-storage"), cap > 0 ? used / cap : 0, 0.8, 1);

    const freeA = data.free_tier_class_a || 0;
    const freeB = data.free_tier_class_b || 0;

    setText($("m-classa"), opsFmt.format(counters.class_a ?? 0));
    setText($("m-classa-limit"), freeA > 0 ? `/ ${opsCompactFmt.format(freeA)} 免费额度` : "");
    applyMeter(
      $("meter-classa"),
      freeA > 0 ? (counters.class_a ?? 0) / freeA : 0,
      cfg.class_a_warn_ratio || 0.8,
      cfg.class_a_block_ratio || 0.95,
    );
    $("tick-a-warn").style.left = `${(cfg.class_a_warn_ratio || 0.8) * 100}%`;
    $("tick-a-block").style.left = `${(cfg.class_a_block_ratio || 0.95) * 100}%`;

    setText($("m-classb"), opsFmt.format(counters.class_b ?? 0));
    setText($("m-classb-limit"), freeB > 0 ? `/ ${opsCompactFmt.format(freeB)} 免费额度` : "");
    applyMeter(
      $("meter-classb"),
      freeB > 0 ? (counters.class_b ?? 0) / freeB : 0,
      cfg.class_b_warn_ratio || 0.8,
      cfg.class_b_block_ratio || 0.95,
    );
    $("tick-b-warn").style.left = `${(cfg.class_b_warn_ratio || 0.8) * 100}%`;
    $("tick-b-block").style.left = `${(cfg.class_b_block_ratio || 0.95) * 100}%`;

    setText($("m-up"), humanBytes(counters.uploaded_bytes ?? 0));
    setText($("m-down"), humanBytes(counters.downloaded_bytes ?? 0));
  }

  /* ---------------- 渲染：目标表 ---------------- */

  const ACTION_LABELS = {
    uploaded: ["已上传", "badge-cyan"],
    restored: ["已恢复", "badge-violet"],
    verified: ["已校验", "badge-green"],
    metadata_refreshed: ["元数据刷新", "badge-green"],
    missing: ["缺失", "badge-amber"],
    remote_deleted: ["远端已删除", "badge-rose"],
  };

  function sideCell(exists, size, timeValue, missingLabel) {
    const td = document.createElement("td");
    if (!exists) {
      const span = document.createElement("span");
      span.className = "muted";
      span.textContent = missingLabel;
      td.append(span);
      return td;
    }
    const strong = document.createElement("span");
    strong.className = "num";
    strong.textContent = humanBytes(size);
    td.append(strong);
    const t = parseTime(timeValue);
    if (t) {
      const sub = document.createElement("span");
      sub.className = "cell-sub";
      sub.textContent = relTime(t);
      sub.title = absTime(t);
      td.append(sub);
    }
    return td;
  }

  function renderTargets(data) {
    const body = $("targets-body");
    body.textContent = "";
    const records = Object.values(data.targets || {}).sort((a, b) =>
      (a.target || "").localeCompare(b.target || ""),
    );
    $("targets-empty").hidden = records.length > 0;
    setText($("targets-count"), records.length ? `${records.length} 个文件` : "");

    for (const rec of records) {
      const tr = document.createElement("tr");

      const tdTarget = document.createElement("td");
      const cell = document.createElement("div");
      cell.className = "target-cell";
      const path = document.createElement("span");
      path.className = "path";
      path.textContent = rec.target || "";
      cell.append(path);
      if (rec.object_key && rec.object_key !== rec.target) {
        const key = document.createElement("span");
        key.className = "key";
        key.textContent = rec.object_key;
        cell.append(key);
      }
      if (rec.last_error) {
        const err = document.createElement("span");
        err.className = "err";
        err.textContent = rec.last_error;
        cell.append(err);
      }
      tdTarget.append(cell);
      tr.append(tdTarget);

      tr.append(sideCell(rec.local?.exists, rec.local?.size ?? 0, rec.local?.mtime, "缺失"));
      tr.append(sideCell(rec.remote?.exists, rec.remote?.size ?? 0, rec.remote?.last_modified, "不存在"));

      const tdAction = document.createElement("td");
      const [label, cls] = ACTION_LABELS[rec.last_action] || ["—", "badge-neutral"];
      const badge = document.createElement("span");
      badge.className = `badge ${cls}`;
      badge.textContent = label;
      tdAction.append(badge);
      tr.append(tdAction);

      const tdTime = document.createElement("td");
      const updated = parseTime(rec.updated_at);
      const span = document.createElement("span");
      span.className = "num muted";
      span.textContent = updated ? relTime(updated) : "—";
      if (updated) span.title = absTime(updated);
      tdTime.append(span);
      tr.append(tdTime);

      const tdOps = document.createElement("td");
      if (rec.remote?.exists) {
        const del = document.createElement("button");
        del.type = "button";
        del.className = "btn btn-danger btn-xs";
        del.textContent = "删除远端";
        del.addEventListener("click", () => deleteRemote(rec.target));
        tdOps.append(del);
      }
      tr.append(tdOps);

      body.append(tr);
    }
  }

  /* ---------------- 渲染：事件 ---------------- */

  function renderEvents(data) {
    const list = $("events-list");
    list.textContent = "";
    const events = (data.recent_events || []).slice(-60).reverse();
    $("events-empty").hidden = events.length > 0;

    for (const ev of events) {
      const li = document.createElement("li");
      li.className = "event-item";

      const dot = document.createElement("span");
      dot.className = `event-dot ${ev.level === "error" ? "error" : ev.level === "warn" ? "warn" : "info"}`;
      li.append(dot);

      const bodyEl = document.createElement("div");
      bodyEl.className = "event-body";
      const msg = document.createElement("span");
      msg.className = "event-msg";
      msg.textContent = ev.message || "";
      bodyEl.append(msg);
      if (ev.target) {
        const tgt = document.createElement("span");
        tgt.className = "event-target";
        tgt.textContent = ev.target;
        bodyEl.append(tgt);
      }
      li.append(bodyEl);

      const t = parseTime(ev.time);
      const timeEl = document.createElement("span");
      timeEl.className = "event-time";
      timeEl.textContent = t ? relTime(t) : "";
      if (t) timeEl.title = absTime(t);
      li.append(timeEl);

      list.append(li);
    }
  }

  /* ---------------- 渲染：整页 ---------------- */

  function renderAll(data) {
    state.config = data.config || state.config;
    state.nextSyncAt = parseTime(data.status?.next_scheduled_at);
    state.lastSuccessAt = parseTime(data.status?.last_successful_sync_at);

    const versionText = data.version && data.version !== "dev" ? data.version : data.version || "";
    const chip = $("version-chip");
    if (versionText) {
      chip.textContent = versionText;
      chip.hidden = false;
    }
    setText($("version-footer"), versionText);

    const cfg = data.config || {};
    const configured = cfg.sync_method === "github"
      ? Boolean(cfg.github_configured && cfg.github_repo)
      : Boolean(cfg.cloudflare_configured && cfg.bucket_name);
    $("setup-hint").hidden = configured;

    const running = renderStage(data.status || {});
    renderClock();
    renderMetrics(data);
    renderTargets(data);
    renderEvents(data);

    if (running && !state.progressTimer) startProgressPolling();
  }

  function setMethod(method) {
    state.method = method === "github" ? "github" : "r2";
    for (const btn of document.querySelectorAll("#method-seg .seg-btn")) {
      btn.classList.toggle("active", btn.dataset.method === state.method);
    }
    $("group-r2").hidden = state.method !== "r2";
    $("group-github").hidden = state.method !== "github";
  }

  function fillForms(cfg) {
    if (!cfg) return;
    setMethod(cfg.sync_method || "r2");
    $("f-bucket").value = cfg.bucket_name || "";
    $("f-account").value = cfg.account_id || "";
    $("f-token").value = "";
    $("f-prefix").value = cfg.object_prefix || "";
    $("f-gh-repo").value = cfg.github_repo || "";
    $("f-gh-token").value = "";
    $("f-gh-branch").value = cfg.github_branch || "main";
    $("f-interval").value = cfg.sync_interval || "";
    $("f-cap-gib").value = bytesToGib(cfg.storage_cap_bytes);
    $("f-listen").value = cfg.listen_addr || "";
    $("f-strict").checked = Boolean(cfg.strict_verify);
    $("f-guards-off").checked = Boolean(cfg.disable_cost_guards);
    $("input-targets").value = (cfg.targets || []).join("\n");
    $("input-excludes").value = (cfg.excludes || []).join("\n");
  }

  /* ---------------- 轮询 ---------------- */

  async function refresh() {
    try {
      const data = await api("/api/status");
      renderAll(data);
    } catch (err) {
      if (err.status !== 401) {
        const pill = $("ready-pill");
        pill.textContent = "连接中断";
        pill.className = "pill pill-bad";
      }
    }
  }

  function startPolling() {
    stopTimers();
    state.statusTimer = setInterval(refresh, STATUS_POLL_MS);
    state.clockTimer = setInterval(renderClock, 1000);
  }

  function startProgressPolling() {
    if (state.progressTimer) return;
    state.progressTimer = setInterval(async () => {
      try {
        const status = await api("/api/progress");
        const running = renderStage(status);
        if (!running && !state.syncingLocal) {
          clearInterval(state.progressTimer);
          state.progressTimer = 0;
          refresh();
        }
      } catch {
        clearInterval(state.progressTimer);
        state.progressTimer = 0;
      }
    }, PROGRESS_POLL_MS);
  }

  /* ---------------- 动作 ---------------- */

  async function triggerSync(kind) {
    if (state.syncingLocal) return;
    state.syncingLocal = true;
    const btn = kind === "verify" ? $("btn-verify") : $("btn-sync");
    setBtnBusy(btn, true, kind === "verify" ? "校验中…" : "同步中…");
    startProgressPolling();
    try {
      const res = await api(kind === "verify" ? "/api/verify" : "/api/sync", { method: "POST" });
      toast(
        `${kind === "verify" ? "校验" : "同步"}完成 · 上传 ${res.uploaded ?? 0} · 恢复 ${res.restored ?? 0} · 跳过 ${res.skipped ?? 0}`,
        "success",
      );
    } catch (err) {
      if (err.status === 409) toast("已有同步在进行中，请稍候", "warn");
      else if (err.code === "not_configured") toast("尚未完成同步连接配置，请先保存连接凭据", "warn");
      else if (err.status !== 401) toast(err.message, "error");
    } finally {
      state.syncingLocal = false;
      setBtnBusy(btn, false);
      refresh();
    }
  }

  async function deleteRemote(target) {
    const ok = await confirmModal({
      title: "删除远端对象",
      text: `将删除 ${target} 在远端的唯一副本，本地文件不受影响。此操作不可撤销。`,
      requireText: "DELETE",
      okLabel: "删除远端对象",
    });
    if (!ok) return;
    try {
      await api("/api/objects/delete", {
        method: "POST",
        body: { target, confirm: "DELETE" },
      });
      toast("远端对象已删除", "success");
    } catch (err) {
      if (err.status !== 401) toast(err.message, "error");
    }
    refresh();
  }

  /* ---------------- 表单提交 ---------------- */

  function onSubmit(formId, handler) {
    $(formId).addEventListener("submit", async (event) => {
      event.preventDefault();
      const btn = event.target.querySelector('button[type="submit"]');
      setBtnBusy(btn, true, "保存中…");
      try {
        await handler(event);
      } catch (err) {
        if (err.status !== 401) toast(err.message, "error");
      } finally {
        setBtnBusy(btn, false);
      }
    });
  }

  function bindForms() {
    onSubmit("form-conn", async () => {
      const body = { sync_method: state.method };
      if (state.method === "github") {
        body.github_repo = $("f-gh-repo").value.trim();
        body.github_branch = $("f-gh-branch").value.trim() || "main";
        const ghToken = $("f-gh-token").value.trim();
        if (ghToken) body.github_token = ghToken;
      } else {
        body.bucket_name = $("f-bucket").value.trim();
        body.account_id = $("f-account").value.trim();
        body.object_prefix = $("f-prefix").value.trim();
        const token = $("f-token").value.trim();
        if (token) body.cloudflare_token = token;
      }
      const cfg = await api("/api/config", { method: "PUT", body });
      state.config = cfg;
      fillForms(cfg);
      toast("连接配置已保存，同步服务已应用新配置", "success");
      refresh();
    });

    onSubmit("form-behavior", async () => {
      const gib = parseFloat($("f-cap-gib").value);
      if (!Number.isFinite(gib) || gib <= 0) {
        toast("存储上限必须是正数（GiB）", "warn");
        return;
      }
      const capBytes = Math.round(gib * 2 ** 30);
      const guardsOff = $("f-guards-off").checked;
      const body = {
        sync_interval: $("f-interval").value.trim(),
        listen_addr: $("f-listen").value.trim(),
        storage_cap_bytes: capBytes,
        strict_verify: $("f-strict").checked,
        disable_cost_guards: guardsOff,
      };

      const reasons = [];
      if (capBytes > (state.config?.storage_cap_bytes ?? 0)) reasons.push("提高存储保护上限");
      if (guardsOff && !state.config?.disable_cost_guards) reasons.push("关闭成本保护");
      if (reasons.length) {
        const ok = await confirmModal({
          title: "高风险操作确认",
          text: `你正在${reasons.join("、")}，超出免费额度可能产生 Cloudflare 费用。`,
          requireText: "CONFIRM",
          okLabel: "确认修改",
        });
        if (!ok) return;
        body.confirm_risk = "CONFIRM";
      }

      const cfg = await api("/api/config", { method: "PUT", body });
      state.config = cfg;
      fillForms(cfg);
      toast("同步行为已保存", "success");
      refresh();
    });

    onSubmit("form-targets", async () => {
      const split = (v) =>
        v
          .split(/\r?\n/)
          .map((x) => x.trim())
          .filter(Boolean);
      const res = await api("/api/targets", {
        method: "PUT",
        body: {
          targets: split($("input-targets").value),
          excludes: split($("input-excludes").value),
        },
      });
      $("input-targets").value = (res.targets || []).join("\n");
      $("input-excludes").value = (res.excludes || []).join("\n");
      toast("目标路径已保存，下次同步生效", "success");
      refresh();
    });

    onSubmit("form-password", async () => {
      const current = $("f-pw-current").value;
      const next = $("f-pw-new").value;
      const confirm = $("f-pw-confirm").value;
      if (next.length < 8) {
        toast("新密码至少需要 8 位", "warn");
        return;
      }
      if (next !== confirm) {
        toast("两次输入的新密码不一致", "warn");
        return;
      }
      await api("/api/password", {
        method: "POST",
        body: { current_password: current, new_password: next },
      });
      $("f-pw-current").value = "";
      $("f-pw-new").value = "";
      $("f-pw-confirm").value = "";
      toast("管理密码已修改，其它会话已被吊销", "success");
    });
  }

  /* ---------------- 登录 / 登出 ---------------- */

  async function enterApp() {
    const data = await api("/api/status");
    state.authed = true;
    showApp();
    fillForms(data.config);
    renderAll(data);
    startPolling();
  }

  function bindAuth() {
    $("login-form").addEventListener("submit", async (event) => {
      event.preventDefault();
      const errEl = $("login-error");
      errEl.hidden = true;
      setBtnBusy($("login-submit"), true, "登录中…");
      try {
        await api("/api/login", {
          method: "POST",
          auth: false,
          body: { password: $("login-password").value },
        });
        await enterApp();
      } catch (err) {
        errEl.textContent =
          err.status === 429 ? "失败次数过多，请稍后再试" : err.message || "登录失败";
        errEl.hidden = false;
      } finally {
        setBtnBusy($("login-submit"), false);
      }
    });

    $("btn-logout").addEventListener("click", async () => {
      try {
        await api("/api/logout", { method: "POST" });
      } catch {
        /* 忽略登出错误 */
      }
      showLogin();
    });
  }

  function bindActions() {
    $("btn-sync").addEventListener("click", () => triggerSync("sync"));
    $("btn-verify").addEventListener("click", () => triggerSync("verify"));
    for (const preset of document.querySelectorAll(".preset")) {
      preset.addEventListener("click", () => {
        $("f-interval").value = preset.dataset.val || "";
      });
    }
    for (const btn of document.querySelectorAll("#method-seg .seg-btn")) {
      btn.addEventListener("click", () => setMethod(btn.dataset.method));
    }
  }

  /* ---------------- 启动 ---------------- */

  async function boot() {
    bindAuth();
    bindActions();
    bindForms();
    try {
      await enterApp();
    } catch {
      showLogin();
    }
  }

  boot();
})();
