// ISUFACTORY のフロントエンド（ハッシュルーティングの SPA）
const app = document.getElementById("app");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const fmtTime = (t) => (t ? new Date(t).toLocaleString("ja-JP", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "-");
const METRICS = { temperature: ["温度", "℃"], vibration: ["振動", "mm/s"], current: ["電流", "A"] };
const FAMILY_COLORS = { A: "#2563eb", B: "#16a34a", C: "#d97706", D: "#9333ea", E: "#db2777", F: "#0891b2" };
let me = null;

async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  if (res.status === 401) { location.hash = "#/login"; throw new Error("ログインしてください"); }
  const body = res.status === 204 ? null : await res.json();
  if (!res.ok) throw new Error(body?.error ?? res.statusText);
  return body;
}

function metricVals(latest) {
  return Object.entries(METRICS).map(([k, [label, unit]]) =>
    `<span title="${label}">${label} ${latest?.[k] ? latest[k].value.toFixed(1) + unit : "-"}</span>`).join("");
}

async function renderDashboard() {
  const { lines, generated_at } = await api("/api/dashboard");
  app.innerHTML = `<h1>ダッシュボード</h1><p class="muted">更新: ${fmtTime(generated_at)}</p>` + lines.map((l) => `
    <h2>${esc(l.name)}</h2>
    <div class="grid">${l.machines.map((m) => `
      <a class="card machine" href="#/machines/${m.id}">
        <strong>${esc(m.name)} ${m.recent_alerts ? `<span class="badge">異常 ${m.recent_alerts}</span>` : `<span class="badge ok">正常</span>`}</strong>
        <div class="vals">${metricVals(m.latest)}</div>
        <div class="muted">${m.current_job ? `作業中: ${esc(m.current_job.product_name)} ×${m.current_job.quantity}（〜${fmtTime(m.current_job.end_at)}）` : "待機中"}</div>
      </a>`).join("")}</div>`).join("");
}

function lineChart(points, color) {
  if (!points.length) return `<p class="muted">直近のデータがありません</p>`;
  const w = 600, h = 160, pad = 24;
  const xs = points.map((p) => new Date(p.minute).getTime());
  const ys = points.flatMap((p) => [p.avg, p.max]);
  const x0 = Math.min(...xs), x1 = Math.max(...xs) || x0 + 1;
  const y0 = Math.min(...ys) * 0.95, y1 = Math.max(...ys) * 1.05;
  const X = (t) => pad + ((t - x0) / (x1 - x0 || 1)) * (w - pad * 2);
  const Y = (v) => h - pad - ((v - y0) / (y1 - y0 || 1)) * (h - pad * 2);
  const path = (key) => points.map((p, i) => `${i ? "L" : "M"}${X(new Date(p.minute).getTime()).toFixed(1)},${Y(p[key]).toFixed(1)}`).join("");
  return `<svg class="chart" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <path d="${path("max")}" fill="none" stroke="${color}" stroke-opacity=".35" stroke-width="1.5"/>
    <path d="${path("avg")}" fill="none" stroke="${color}" stroke-width="2"/>
    <text x="${pad}" y="14" font-size="11" fill="currentColor">${y1.toFixed(1)}</text>
    <text x="${pad}" y="${h - 6}" font-size="11" fill="currentColor">${y0.toFixed(1)}</text>
  </svg>`;
}

async function renderMachine(id) {
  const [m, sched, alerts] = await Promise.all([
    api(`/api/machines/${id}`), api(`/api/machines/${id}/schedule?hours=12`), api(`/api/alerts?machine_id=${id}&limit=10`),
  ]);
  const charts = await Promise.all(Object.keys(METRICS).map((k) => api(`/api/machines/${id}/metrics?metric=${k}&minutes=60`)));
  const job = m.current_job;
  app.innerHTML = `
    <h1>${esc(m.name)}</h1>
    <div class="vals">${metricVals(m.latest)}</div>
    <div class="row">
      <div>${charts.map((c, i) => `<h2>${METRICS[c.metric][0]}（直近 60 分・1 分平均と最大）</h2>${lineChart(c.points, ["#2563eb", "#d97706", "#16a34a"][i])}`).join("")}</div>
      <div>
        <h2>作業中のジョブ</h2>
        ${job ? `<p>${esc(job.product_name)} ×${job.quantity}<br><span class="muted">${fmtTime(job.start_at)} 〜 ${fmtTime(job.end_at)}（納期 ${fmtTime(job.due_at)}）</span></p>
          <img class="drawing" src="/api/products/${job.product_id}/drawing" alt="図面">` : `<p class="muted">待機中</p>`}
        <h2>今後の作業予定</h2>
        <div class="table-wrap"><table><tr><th>製品</th><th>開始</th><th>終了</th><th>納期</th></tr>
          ${sched.jobs.map((j) => `<tr><td>${esc(j.product_name)} ×${j.quantity}</td><td>${fmtTime(j.start_at)}</td><td>${fmtTime(j.end_at)}</td>
            <td class="${new Date(j.end_at) > new Date(j.due_at) ? "late" : ""}">${fmtTime(j.due_at)}</td></tr>`).join("") || `<tr><td colspan="4" class="muted">予定なし</td></tr>`}
        </table></div>
        <h2>直近のアラート</h2>
        ${alertTable(alerts.alerts)}
      </div>
    </div>`;
}

function alertTable(alerts) {
  return `<div class="table-wrap"><table><tr><th>時刻</th><th>設備</th><th>指標</th><th class="num">値</th><th class="num">基準</th></tr>
    ${alerts.map((a) => `<tr><td>${fmtTime(a.measured_at)}</td><td><a href="#/machines/${a.machine_id}">#${a.machine_id}</a></td>
      <td>${METRICS[a.metric]?.[0] ?? a.metric}</td><td class="num">${a.value.toFixed(2)}</td><td class="num">${a.baseline.toFixed(2)}</td></tr>`).join("") ||
      `<tr><td colspan="5" class="muted">アラートはありません</td></tr>`}</table></div>`;
}

async function renderAlerts() {
  const { alerts } = await api("/api/alerts?limit=100");
  app.innerHTML = `<h1>アラート</h1>` + alertTable(alerts);
}

async function renderMaterials() {
  const { materials } = await api("/api/materials");
  app.innerHTML = `<h1>部材在庫</h1>
    <div class="table-wrap"><table><tr><th>部材</th><th class="num">引当可能な在庫</th><th></th></tr>
    ${materials.map((m) => `<tr><td>${esc(m.name)}</td><td class="num">${m.stock.toLocaleString()}</td>
      <td><form class="inline receive" data-id="${m.id}"><input type="number" name="quantity" min="1" value="1000" style="width:100px"><button>入荷</button></form></td></tr>`).join("")}
    </table></div><p id="msg" class="muted"></p>`;
  document.querySelectorAll("form.receive").forEach((f) => (f.onsubmit = async (ev) => {
    ev.preventDefault();
    try {
      await api(`/api/materials/${f.dataset.id}/receive`, { method: "POST", body: JSON.stringify({ quantity: Number(f.quantity.value) }) });
      renderMaterials();
    } catch (e) { document.getElementById("msg").textContent = e.message; }
  }));
}

async function renderPlans() {
  const [{ products }, { orders }] = await Promise.all([api("/api/products"), api("/api/orders?status=pending&limit=200")]);
  const pmap = Object.fromEntries(products.map((p) => [p.id, p]));
  const due = new Date(Date.now() + 3 * 3600 * 1000);
  due.setSeconds(0, 0);
  const local = new Date(due.getTime() - due.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  app.innerHTML = `<h1>生産計画</h1>
    <h2>受注の登録</h2>
    <form class="inline" id="order">
      <label>製品<select name="product_id">${products.map((p) => `<option value="${p.id}">${esc(p.name)}（${p.machine_type}・${p.unit_seconds}秒/個）</option>`).join("")}</select></label>
      <label>数量<input type="number" name="quantity" min="1" max="1000" value="50" style="width:90px"></label>
      <label>納期<input type="datetime-local" name="due_at" value="${local}"></label>
      <button>登録</button>
    </form>
    <h2>未計画の受注（${orders.length} 件）</h2>
    <form class="inline" id="plan"><button>計画を作成</button><span id="plan-msg" class="muted"></span></form>
    <div class="table-wrap"><table><tr><th>受注</th><th>製品</th><th class="num">数量</th><th>納期</th></tr>
      ${orders.map((o) => `<tr><td>#${o.id}</td><td>${esc(pmap[o.product_id]?.name)}</td><td class="num">${o.quantity}</td><td>${fmtTime(o.due_at)}</td></tr>`).join("") || `<tr><td colspan="4" class="muted">なし</td></tr>`}
    </table></div>
    <h2>設備ごとの計画（今後 12 時間）</h2><div id="gantt" class="muted">読み込み中…</div>`;
  document.getElementById("order").onsubmit = async (ev) => {
    ev.preventDefault();
    const f = ev.target;
    await api("/api/orders", { method: "POST", body: JSON.stringify({
      product_id: Number(f.product_id.value), quantity: Number(f.quantity.value), due_at: new Date(f.due_at.value).toISOString() }) });
    renderPlans();
  };
  document.getElementById("plan").onsubmit = async (ev) => {
    ev.preventDefault();
    const msg = document.getElementById("plan-msg");
    msg.textContent = "計画中…";
    try {
      const r = await api("/api/plans", { method: "POST" });
      msg.textContent = `${r.assignments.length} 件を計画しました（部材不足で保留 ${r.skipped.length} 件）`;
      setTimeout(renderPlans, 1200);
    } catch (e) { msg.textContent = e.message; }
  };
  renderGantt();
}

async function renderGantt() {
  const { lines } = await api("/api/dashboard");
  const machines = lines.flatMap((l) => l.machines);
  const from = Date.now() - 3600 * 1000, to = Date.now() + 12 * 3600 * 1000;
  const scheds = await Promise.all(machines.map((m) => api(`/api/machines/${m.id}/schedule?hours=12`)));
  const pct = (t) => Math.max(0, Math.min(100, ((new Date(t).getTime() - from) / (to - from)) * 100));
  document.getElementById("gantt").innerHTML = machines.map((m, i) => `
    <div class="muted">${esc(m.name)}</div>
    <div class="gantt">${scheds[i].jobs.map((j) => `<div class="job" title="${esc(j.product_name)} ×${j.quantity} / 納期 ${fmtTime(j.due_at)}"
      style="left:${pct(j.start_at)}%;width:${Math.max(0.3, pct(j.end_at) - pct(j.start_at))}%;background:${FAMILY_COLORS[j.family] ?? "#64748b"};
      ${new Date(j.end_at) > new Date(j.due_at) ? "outline:2px solid var(--warn)" : ""}">${esc(j.family)}</div>`).join("")}</div>`).join("");
}

function renderLogin() {
  app.innerHTML = `<h1>ログイン</h1>
    <form class="inline" id="login">
      <label>ログイン名<input name="login_name" placeholder="operator01" required></label>
      <label>パスワード<input name="password" type="password" required></label>
      <button>ログイン</button>
    </form><p id="msg" class="muted"></p>`;
  document.getElementById("login").onsubmit = async (ev) => {
    ev.preventDefault();
    try {
      me = await api("/api/login", { method: "POST", body: JSON.stringify(Object.fromEntries(new FormData(ev.target))) });
      showMe();
      location.hash = "#/";
    } catch (e) { document.getElementById("msg").textContent = e.message; }
  };
}

function showMe() {
  document.getElementById("me").innerHTML = me ? `${esc(me.display_name)}（${me.role === "planner" ? "計画担当" : "現場担当"}） <a href="#/logout">ログアウト</a>` : "";
}

async function route() {
  const h = location.hash || "#/";
  try {
    let m;
    if (h === "#/login") return renderLogin();
    if (h === "#/logout") { await api("/api/logout", { method: "POST" }); me = null; showMe(); location.hash = "#/login"; return; }
    if (!me) { me = await api("/api/me"); showMe(); }
    if ((m = h.match(/^#\/machines\/(\d+)/))) await renderMachine(m[1]);
    else if (h === "#/plans") await renderPlans();
    else if (h === "#/materials") await renderMaterials();
    else if (h === "#/alerts") await renderAlerts();
    else await renderDashboard();
  } catch (err) {
    if (location.hash !== "#/login") app.innerHTML = `<p>${esc(err.message)}</p>`;
  }
}
addEventListener("hashchange", route);
route();
