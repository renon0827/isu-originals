// ISUTICKET の最小フロントエンド（ハッシュルーティングの SPA）
const app = document.getElementById("app");
const yen = (n) => n.toLocaleString("ja-JP") + "円";
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  const body = res.status === 204 ? null : await res.json();
  if (!res.ok) throw new Error(body?.error ?? res.statusText);
  return body;
}

async function renderEvents(status) {
  const { events } = await api(`/api/events?status=${status}`);
  app.innerHTML = `<div class="grid">${events.map((e) => `
    <a class="card" href="#/event/${e.id}" style="color:inherit;text-decoration:none">
      <img src="/api/events/${e.id}/image" alt="" loading="lazy">
      <div class="body">
        <strong>${esc(e.title)}</strong>
        <div class="muted">${esc(e.venue.name)} / ${e.sale_type === "lottery" ? "抽選" : "先着"}</div>
        <div class="ranks">${e.ranks.map((r) => `<span>${r.rank}: ${yen(r.price)}（残 ${r.remaining}）</span>`).join("")}</div>
      </div>
    </a>`).join("")}</div>`;
}

async function renderEvent(id) {
  const [e, map] = await Promise.all([api(`/api/events/${id}`), api(`/api/events/${id}/seatmap`)]);
  app.innerHTML = `
    <h1>${esc(e.title)}</h1>
    <p class="muted">${esc(e.venue.name)} / 開場 ${new Date(e.gate_open_at).toLocaleString("ja-JP")}</p>
    <p>${esc(e.description)}</p>
    ${map.sections.map((s) => `
      <h3>${esc(s.name)}（${s.rank}）</h3>
      <div class="seatmap" style="grid-template-columns: repeat(${s.cols}, 10px)">
        ${s.seats.map((seat) => `<div class="seat ${seat.status}" title="${seat.row}列${seat.col}番"></div>`).join("")}
      </div>`).join("")}`;
}

async function renderTickets() {
  const { tickets } = await api("/api/me/tickets");
  app.innerHTML = `<h1>マイチケット</h1>` + tickets.map((t) => `
    <div class="card"><div class="body">
      <strong>${esc(t.event_title)}</strong>
      <div class="muted">${esc(t.seat.section)} ${t.seat.row}列${t.seat.col}番（${t.seat.rank}）/ ${t.status}</div>
    </div></div>`).join("");
}

async function renderLottery() {
  const { entries } = await api("/api/me/lottery");
  const label = { pending: "抽選待ち", won: "当選", lost: "落選" };
  app.innerHTML = `<h1>抽選結果</h1>` + entries.map((e) => `
    <div class="card"><div class="body">
      <strong>${esc(e.event_title)}</strong>
      <div class="muted">${e.rank} × ${e.quantity}枚 / ${label[e.status]}</div>
    </div></div>`).join("");
}

function renderLogin() {
  app.innerHTML = `
    <form id="login">
      <input name="login_name" placeholder="ログイン名" required>
      <input name="password" type="password" placeholder="パスワード" required>
      <button>ログイン</button>
    </form>`;
  document.getElementById("login").onsubmit = async (ev) => {
    ev.preventDefault();
    const f = new FormData(ev.target);
    await api("/api/login", { method: "POST", body: JSON.stringify(Object.fromEntries(f)) });
    location.hash = "#/me/tickets";
  };
}

async function route() {
  const h = location.hash || "#/events/on_sale";
  try {
    let m;
    if ((m = h.match(/^#\/events\/(\w+)/))) await renderEvents(m[1]);
    else if ((m = h.match(/^#\/event\/(\d+)/))) await renderEvent(m[1]);
    else if (h === "#/me/tickets") await renderTickets();
    else if (h === "#/me/lottery") await renderLottery();
    else if (h === "#/login") renderLogin();
  } catch (err) {
    app.innerHTML = `<p>${esc(err.message)}</p>`;
  }
}
addEventListener("hashchange", route);
route();
