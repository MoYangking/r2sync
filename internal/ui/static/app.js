const $ = (s) => document.querySelector(s);

async function api(path, options = {}) {
  const init = { ...options };
  if (init.body && typeof init.body !== "string") {
    init.headers = { "Content-Type": "application/json", ...(init.headers || {}) };
    init.body = JSON.stringify(init.body);
  }
  const res = await fetch(path, init);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.message || res.statusText);
  return data;
}

function humanBytes(value) {
  if (!Number.isFinite(value)) return "-";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let n = value;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 2 : 0)} ${units[i]}`;
}

function setText(id, value) {
  const el = $(id);
  if (el) el.textContent = value || "-";
}

async function loadStatus() {
  const data = await api("/api/status");
  $("#loginPanel").hidden = true;
  $("#app").hidden = false;
  const ready = data.status?.ready;
  $("#ready").textContent = ready ? "就绪" : "未就绪";
  $("#ready").className = ready ? "pill ok" : "pill warn";
  setText("#stage", data.status?.stage);
  setText("#progress", `${data.status?.progress ?? 0}%`);
  setText("#bytes", humanBytes(data.current_bytes || 0));
  setText("#classA", String(data.counters?.class_a ?? 0));
  setText("#classB", String(data.counters?.class_b ?? 0));
  setText("#nextSync", data.status?.next_scheduled_at || "-");

  const cfg = data.config || {};
  for (const name of ["bucket_name", "account_id", "object_prefix", "listen_addr", "sync_interval", "storage_cap_bytes"]) {
    const input = $(`[name="${name}"]`);
    if (input) input.value = cfg[name] ?? "";
  }
  $('[name="disable_cost_guards"]').checked = Boolean(cfg.disable_cost_guards);
  $('[name="strict_verify"]').checked = Boolean(cfg.strict_verify);
  $("#targets").value = (cfg.targets || []).join("\n");
  $("#excludes").value = (cfg.excludes || []).join("\n");
  $("#events").textContent = (data.recent_events || []).map((ev) =>
    `[${ev.time}] ${ev.level}: ${ev.target ? ev.target + ": " : ""}${ev.message}`
  ).join("\n") || "暂无事件";
}

$("#loginForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/api/login", { method: "POST", body: { password: $("#password").value } });
    await loadStatus();
  } catch (err) {
    $("#loginLog").textContent = err.message;
  }
});

$("#configForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  const body = Object.fromEntries(form.entries());
  body.storage_cap_bytes = Number(body.storage_cap_bytes || 0);
  body.disable_cost_guards = $('[name="disable_cost_guards"]').checked;
  body.strict_verify = $('[name="strict_verify"]').checked;
  if (!body.cloudflare_token) delete body.cloudflare_token;
  await api("/api/config", { method: "PUT", body });
  await loadStatus();
});

$("#saveTargets").addEventListener("click", async () => {
  const targets = $("#targets").value.split(/\r?\n/).map((x) => x.trim()).filter(Boolean);
  const excludes = $("#excludes").value.split(/\r?\n/).map((x) => x.trim()).filter(Boolean);
  await api("/api/targets", { method: "PUT", body: { targets, excludes } });
  await loadStatus();
});

$("#syncNow").addEventListener("click", async () => {
  await api("/api/sync", { method: "POST" });
  await loadStatus();
});

$("#verifyNow").addEventListener("click", async () => {
  await api("/api/verify", { method: "POST" });
  await loadStatus();
});

$("#refresh").addEventListener("click", loadStatus);

$("#deleteRemote").addEventListener("click", async () => {
  await api("/api/objects/delete", {
    method: "POST",
    body: { target: $("#deleteTarget").value, confirm: $("#deleteConfirm").value }
  });
  await loadStatus();
});

loadStatus().catch(() => {
  $("#loginPanel").hidden = false;
  $("#app").hidden = true;
});
