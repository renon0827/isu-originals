// ISUAUCTION のフロントエンド（ハッシュルーティングの SPA）
const app = document.getElementById("app");
const yen = (n) => (n ?? 0).toLocaleString("ja-JP") + "円";
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
let me = null;
let categories = [];
// 画面を離れたら止めるタイマー・ポーリング
let cleanups = [];

async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { "Content-Type": "application/json" }, ...opts });
  const body = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) {
    const err = new Error(body?.error ?? res.statusText);
    err.status = res.status;
    throw err;
  }
  return body;
}

function remaining(endAt) {
  const ms = new Date(endAt) - Date.now();
  if (ms <= 0) return "終了";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `残り ${s} 秒`;
  if (s < 3600) return `残り ${Math.floor(s / 60)} 分`;
  if (s < 86400) return `残り ${Math.floor(s / 3600)} 時間`;
  return `残り ${Math.floor(s / 86400)} 日`;
}

function card(it) {
  const soon = new Date(it.end_at) - Date.now() < 60000 && it.status === "open";
  return `<a class="card" href="#/items/${it.id}">
    <img src="${it.thumbnail_url}" alt="" loading="lazy">
    <div class="body">
      <div class="title">${esc(it.title)}</div>
      <div class="price">${yen(it.current_price)}</div>
      <div class="muted">入札 ${it.bid_count} 件 · <span class="${soon ? "hot" : ""}" data-end="${it.end_at}">${it.status === "ended" ? "終了" : remaining(it.end_at)}</span></div>
    </div>
  </a>`;
}

// data-end を持つ要素の残り時間を 1 秒ごとに更新する
function tickRemaining() {
  const t = setInterval(() => {
    document.querySelectorAll("[data-end]").forEach((el) => (el.textContent = remaining(el.dataset.end)));
  }, 1000);
  cleanups.push(() => clearInterval(t));
}

// ---- 一覧・検索 ----

async function renderList(params) {
  const status = params.get("status") || "open";
  const sort = params.get("sort") || "ending_soon";
  const page = Number(params.get("page") || 1);
  const q = params.get("q") || "";
  const cat = params.get("category_id") || "";
  const qs = new URLSearchParams({ status, sort, page, ...(q && { q }), ...(cat && { category_id: cat }) });
  const { items, has_next } = await api(`/api/items?${qs}`);
  const link = (p) => "#/?" + new URLSearchParams({ ...Object.fromEntries(qs), ...p });
  app.innerHTML = `
    <div class="tabs">
      <a href="${link({ status: "open", sort: "ending_soon", page: 1 })}" class="${status === "open" && sort === "ending_soon" ? "active" : ""}">まもなく終了</a>
      <a href="${link({ status: "open", sort: "newest", page: 1 })}" class="${status === "open" && sort === "newest" ? "active" : ""}">新着</a>
      <a href="${link({ status: "ended", sort: "ending_soon", page: 1 })}" class="${status === "ended" ? "active" : ""}">終了した商品</a>
    </div>
    <div class="toolbar">
      ${q ? `<span>「${esc(q)}」の検索結果</span>` : ""}
      <select id="cat">
        <option value="">すべてのカテゴリ</option>
        ${categories.map((c) => `<option value="${c.id}" ${String(c.id) === cat ? "selected" : ""}>${esc(c.name)}</option>`).join("")}
      </select>
    </div>
    ${items.length ? `<div class="grid">${items.map(card).join("")}</div>` : `<p class="muted">商品が見つかりませんでした。</p>`}
    <div class="pager">
      ${page > 1 ? `<a href="${link({ page: page - 1 })}">← 前へ</a>` : ""}
      ${has_next ? `<a href="${link({ page: page + 1 })}">次へ →</a>` : ""}
    </div>`;
  document.getElementById("cat").onchange = (e) => (location.hash = link({ category_id: e.target.value, page: 1 }));
  tickRemaining();
}

// ---- 商品詳細・入札 ----

async function renderItem(id) {
  const it = await api(`/api/items/${id}`);
  const { bids } = await api(`/api/items/${id}/bids`);
  const cat = categories.find((c) => c.id === it.category_id);
  const ended = it.status !== "open";
  const minBid = it.bid_count === 0 ? it.start_price : it.current_price + it.increment;
  app.innerHTML = `
    <div class="detail">
      <div>
        <img src="${it.image_url}" alt="">
        <div class="panel" style="margin-top:14px">
          <div class="muted">${esc(cat?.name)} · 出品者 ${esc(it.seller.nickname)}</div>
          <p>${esc(it.description)}</p>
        </div>
      </div>
      <div>
        <h1 style="margin-top:0">${esc(it.title)}</h1>
        <div class="panel">
          <div class="muted">${it.status === "ended" ? "落札価格" : "現在価格"}</div>
          <div class="big" id="price">${yen(it.current_price)}</div>
          <div class="muted">入札 <span id="bidcount">${it.bid_count}</span> 件 · 開始 ${yen(it.start_price)} · 入札単位 ${yen(it.increment)} · ウォッチ ${it.watch_count} 人</div>
          <div style="margin-top:8px" id="state">${stateLine(it)}</div>
          <div class="muted" style="margin-top:4px">終了 ${new Date(it.end_at).toLocaleString("ja-JP")}（<span id="remain" data-end="${it.end_at}">${ended ? "終了" : remaining(it.end_at)}</span>）</div>
        </div>
        ${ended ? "" : `
        <div class="panel">
          <form id="bid" class="stack">
            <label>上限額（この金額まで自動で入札します）
              <input name="max_amount" type="number" min="${minBid}" step="${it.increment}" value="${minBid}" required>
            </label>
            <div class="muted">${it.my_max ? `あなたの現在の上限額: ${yen(it.my_max)}` : `${yen(minBid)} 以上で入札できます`}</div>
            <button>入札する</button>
          </form>
          <div id="bidmsg"></div>
        </div>`}
        <div class="row">
          <button class="secondary" id="watch">${it.watching ? "★ ウォッチ中" : "☆ ウォッチする"}</button>
        </div>
        <h3>入札履歴</h3>
        <div class="list">${bids.length ? bids.map((b) => `<div class="item"><span>${esc(b.bidder.nickname)}</span><span class="muted">${new Date(b.created_at).toLocaleString("ja-JP")}</span></div>`).join("") : `<p class="muted">まだ入札はありません。</p>`}</div>
      </div>
    </div>`;

  document.getElementById("watch").onclick = async () => {
    if (!me) return (location.hash = "#/login");
    await api(`/api/items/${id}/watch`, { method: it.watching ? "DELETE" : "POST" });
    renderItem(id);
  };
  const form = document.getElementById("bid");
  if (form) {
    form.onsubmit = async (e) => {
      e.preventDefault();
      if (!me) return (location.hash = "#/login");
      const msg = document.getElementById("bidmsg");
      try {
        const r = await api(`/api/items/${id}/bids`, { method: "POST", body: JSON.stringify({ max_amount: Number(new FormData(form).get("max_amount")) }) });
        msg.innerHTML = `<div class="msg">${r.is_leader ? "最高入札者になりました。" : "入札しましたが、他の人の上限額の方が高いようです。"}</div>`;
        renderItem(id);
      } catch (err) {
        msg.innerHTML = `<div class="msg">${esc(err.message)}</div>`;
      }
    };
  }
  tickRemaining();
  if (!ended) watchPrice(id, it);
}

function stateLine(it) {
  if (it.status === "ended") return it.winner ? `<span class="ok">落札者: ${esc(it.winner.nickname)}</span>` : `<span class="muted">入札なしで終了しました</span>`;
  if (it.status === "closing") return `<span class="hot">落札を確定しています…</span>`;
  if (!it.leader) return `<span class="muted">まだ入札はありません</span>`;
  if (me && it.leader.id === me.id) return `<span class="ok">あなたが最高入札者です</span>`;
  return `最高入札者: ${esc(it.leader.nickname)}`;
}

// 価格の更新を確認する
function watchPrice(id, it) {
  let version = it.bid_count;
  let stopped = false;
  cleanups.push(() => (stopped = true));
  (async function loop() {
    while (!stopped) {
      const started = Date.now();
      try {
        const p = await api(`/api/items/${id}/price?after=${version}`);
        if (stopped) return;
        if (p.version !== version || p.status !== "open") {
          version = p.version;
          document.getElementById("price").textContent = yen(p.current_price);
          document.getElementById("bidcount").textContent = p.bid_count;
          document.getElementById("remain").dataset.end = p.end_at;
          if (p.status !== "open") {
            await new Promise((r) => setTimeout(r, 1000));
            if (!stopped) renderItem(id);
            return;
          }
        }
      } catch {
        await new Promise((r) => setTimeout(r, 2000));
      }
      if (Date.now() - started < 1000) await new Promise((r) => setTimeout(r, 1000));
    }
  })();
}

// ---- マイページ ----

async function renderMe(tab) {
  if (!me) return (location.hash = "#/login");
  tab = tab || "bids";
  const tabs = [["bids", "入札中・落札"], ["watchlist", "ウォッチリスト"], ["items", "出品"], ["notifications", "お知らせ"]];
  let body = "";
  if (tab === "bids") {
    const { bids } = await api("/api/me/bids");
    const label = { leading: ["最高入札者", "ok"], outbid: ["他の人に抜かれました", "hot"], won: ["落札しました", "ok"], lost: ["落札できませんでした", "muted"] };
    body = bids.map((b) => `<a class="item" href="#/items/${b.item.id}"><img src="${b.item.thumbnail_url}" alt="">
      <div><div>${esc(b.item.title)}</div><div class="muted">現在 ${yen(b.item.current_price)} · あなたの上限 ${yen(b.my_max)}</div>
      <div class="${label[b.result][1]}">${label[b.result][0]}</div></div></a>`).join("");
  } else if (tab === "watchlist") {
    const { items } = await api("/api/me/watchlist");
    body = items.map((it) => `<a class="item" href="#/items/${it.id}"><img src="${it.thumbnail_url}" alt="">
      <div><div>${esc(it.title)}</div><div class="muted">${yen(it.current_price)} · <span data-end="${it.end_at}">${it.status === "ended" ? "終了" : remaining(it.end_at)}</span></div></div></a>`).join("");
  } else if (tab === "items") {
    const { items } = await api("/api/me/items");
    body = items.map((it) => `<a class="item" href="#/items/${it.id}"><img src="${it.thumbnail_url}" alt="">
      <div><div>${esc(it.title)}</div><div class="muted">${it.status === "ended" ? (it.winner ? `落札 ${yen(it.final_price)}（${esc(it.winner.nickname)}）` : "入札なしで終了") : `${yen(it.current_price)} · 入札 ${it.bid_count} 件`}</div></div></a>`).join("");
  } else {
    const { notifications } = await api("/api/me/notifications");
    const text = { outbid: "他の人に最高額を抜かれました", won: "落札しました", sold: "出品した商品が落札されました", ended: "オークションが終了しました" };
    body = notifications.map((n) => `<a class="item" href="#/items/${n.item_id}"><div>
      <div>${text[n.type] ?? n.type}${n.price ? `（${yen(n.price)}）` : ""}</div>
      <div class="muted">${esc(n.item_title)} · ${new Date(n.created_at).toLocaleString("ja-JP")}</div></div></a>`).join("");
  }
  app.innerHTML = `<h1>マイページ <span class="muted">${esc(me.nickname)}</span></h1>
    <div class="tabs">${tabs.map(([k, v]) => `<a href="#/me/${k}" class="${k === tab ? "active" : ""}">${v}</a>`).join("")}</div>
    <div class="list">${body || `<p class="muted">まだありません。</p>`}</div>`;
  tickRemaining();
}

// ---- 出品 ----

function renderSell() {
  if (!me) return (location.hash = "#/login");
  app.innerHTML = `<h1>出品する</h1>
    <form id="sell" class="stack">
      <input name="title" placeholder="商品名" required>
      <textarea name="description" rows="4" placeholder="商品の説明"></textarea>
      <select name="category_id">${categories.map((c) => `<option value="${c.id}">${esc(c.name)}</option>`).join("")}</select>
      <label>開始価格 <input name="start_price" type="number" min="1" value="1000" required></label>
      <label>入札単位 <input name="increment" type="number" min="1" value="100" required></label>
      <label>期間
        <select name="duration_sec">
          <option value="60">1 分</option><option value="600">10 分</option><option value="3600" selected>1 時間</option>
          <option value="86400">1 日</option><option value="604800">7 日</option>
        </select>
      </label>
      <label>画像（JPEG） <input name="image" type="file" accept="image/jpeg" required></label>
      <button>出品する</button>
      <div id="sellmsg"></div>
    </form>`;
  document.getElementById("sell").onsubmit = async (e) => {
    e.preventDefault();
    const f = new FormData(e.target);
    const file = f.get("image");
    const b64 = await new Promise((resolve) => {
      const r = new FileReader();
      r.onload = () => resolve(String(r.result).split(",")[1]);
      r.readAsDataURL(file);
    });
    try {
      const r = await api("/api/items", {
        method: "POST",
        body: JSON.stringify({
          title: f.get("title"), description: f.get("description"), category_id: Number(f.get("category_id")),
          start_price: Number(f.get("start_price")), increment: Number(f.get("increment")),
          duration_sec: Number(f.get("duration_sec")), image: b64,
        }),
      });
      location.hash = `#/items/${r.id}`;
    } catch (err) {
      document.getElementById("sellmsg").innerHTML = `<div class="msg">${esc(err.message)}</div>`;
    }
  };
}

// ---- ログイン・会員登録 ----

function renderLogin() {
  app.innerHTML = `<h1>ログイン</h1>
    <form id="login" class="stack">
      <input name="login_name" placeholder="ログイン名" required>
      <input name="password" type="password" placeholder="パスワード" required>
      <button>ログイン</button>
      <div id="loginmsg"></div>
    </form>
    <h2>はじめての方</h2>
    <form id="register" class="stack">
      <input name="login_name" placeholder="ログイン名（英小文字・数字・_）" required>
      <input name="nickname" placeholder="ニックネーム" required>
      <input name="password" type="password" placeholder="パスワード（8 文字以上）" required>
      <button>会員登録</button>
      <div id="regmsg"></div>
    </form>`;
  const submit = (formId, path, msgId) => {
    document.getElementById(formId).onsubmit = async (e) => {
      e.preventDefault();
      try {
        me = await api(path, { method: "POST", body: JSON.stringify(Object.fromEntries(new FormData(e.target))) });
        updateAuthLink();
        location.hash = "#/";
      } catch (err) {
        document.getElementById(msgId).innerHTML = `<div class="msg">${esc(err.message)}</div>`;
      }
    };
  };
  submit("login", "/api/login", "loginmsg");
  submit("register", "/api/register", "regmsg");
}

function updateAuthLink() {
  const a = document.getElementById("auth-link");
  if (me) {
    a.textContent = "ログアウト";
    a.href = "#/";
    a.onclick = async () => {
      await api("/api/logout", { method: "POST" });
      me = null;
      updateAuthLink();
    };
  } else {
    a.textContent = "ログイン";
    a.href = "#/login";
    a.onclick = null;
  }
}

// ---- ルーティング ----

document.getElementById("search").onsubmit = (e) => {
  e.preventDefault();
  const q = new FormData(e.target).get("q");
  location.hash = "#/?" + new URLSearchParams({ q, status: "open", sort: "ending_soon" });
};

async function route() {
  cleanups.forEach((f) => f());
  cleanups = [];
  const h = location.hash || "#/";
  try {
    let m;
    if ((m = h.match(/^#\/items\/(\d+)/))) await renderItem(m[1]);
    else if ((m = h.match(/^#\/me\/?(\w*)/))) await renderMe(m[1]);
    else if (h === "#/sell") renderSell();
    else if (h === "#/login") renderLogin();
    else await renderList(new URLSearchParams(h.split("?")[1] || ""));
  } catch (err) {
    app.innerHTML = `<div class="msg">${esc(err.message)}</div>`;
  }
}

(async () => {
  try { me = await api("/api/me"); } catch { me = null; }
  try { categories = (await api("/api/categories")).categories; } catch { categories = []; }
  updateAuthLink();
  addEventListener("hashchange", route);
  route();
})();
