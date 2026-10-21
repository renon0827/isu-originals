package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/jpeg"
	"math/rand"
	"net/http"
	"time"
)

type FanRole int

const (
	RoleRaiser  FanRole = iota // 少しずつ競り上げる
	RoleProxy                  // 上限額を一度に入れ、抜かれたら一度だけ上げる
	RoleSniper                 // 終了間際に入札する
	RoleBrowser                // 検索・閲覧・ウォッチ・マイページ
)

type Fan struct {
	st    *State
	agent *Agent
	rng   *rand.Rand
	role  FanRole

	Login    string
	Password string
	isNew    bool
	UserID   int64

	strikes int
	left    bool

	// 入札中の商品と評価額
	target    *BenchItem
	valuation int
	version   int
	raised    bool

	lastNotice    int64
	noticeCheckAt time.Time
	watchedHot    bool
	staticDone    bool
}

var errLeave = errors.New("fan left")

func (st *State) newFan() *Fan {
	n := st.nextFan.Add(1)
	f := &Fan{
		st:    st,
		agent: NewAgent(st.cfg.Target, st.cfg.HostHeader, st.cfg.RequestTimeout),
		rng:   rand.New(rand.NewSource(time.Now().UnixNano() + n)),
	}
	if f.rng.Intn(10) == 0 {
		f.isNew = true
		f.Login = fmt.Sprintf("bench_%d_%d", time.Now().Unix()%100000, n)
		f.Password = "password-" + f.Login
	} else {
		f.Login = userLogin(int((n*7919)%numInitialUsers) + 1)
		f.Password = userPassword(f.Login)
	}
	switch x := f.rng.Intn(100); {
	case x < 40:
		f.role = RoleRaiser
	case x < 60:
		f.role = RoleProxy
	case x < 75:
		f.role = RoleSniper
	default:
		f.role = RoleBrowser
	}
	return f
}

// ---- リクエストのラッパー ----

func (f *Fan) call(ctx context.Context, method, path string, body any) (*Response, error) {
	res, err := f.agent.Do(ctx, method, path, body)
	f.st.stats.record(res, err)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrTimeout) {
			f.st.score.Penalize("タイムアウト", 5)
		} else {
			f.st.score.Penalize("通信エラー", 5)
		}
		f.strike()
		return nil, err
	}
	if res.Status >= 500 {
		f.st.score.Penalize("サーバーエラー (5xx)", 5)
		f.strike()
	}
	return res, nil
}

func (f *Fan) strike() {
	f.strikes++
	if f.strikes >= 3 {
		f.left = true
	}
}

func (f *Fan) satisfied() {
	if f.strikes > 0 {
		f.strikes--
	}
}

func expectStatus(st *State, res *Response, path string, want ...int) bool {
	for _, w := range want {
		if res.Status == w {
			return true
		}
	}
	if res.Status < 500 {
		st.score.Penalize("想定外のステータスコード", 10)
		st.score.Message(fmt.Sprintf("%s: 想定外のステータス %d: %s", path, res.Status, truncate(res.Body, 200)))
	}
	return false
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ---- ログイン ----

func (f *Fan) login(ctx context.Context) error {
	var res *Response
	var err error
	if f.isNew {
		res, err = f.call(ctx, http.MethodPost, "/api/register", map[string]string{
			"login_name": f.Login, "nickname": "ベンチ" + f.Login, "password": f.Password,
		})
		if err != nil {
			return err
		}
		if !expectStatus(f.st, res, "POST /api/register", http.StatusCreated) {
			return errLeave
		}
	} else {
		res, err = f.call(ctx, http.MethodPost, "/api/login", map[string]string{"login_name": f.Login, "password": f.Password})
		if err != nil {
			return err
		}
		if !expectStatus(f.st, res, "POST /api/login", http.StatusOK) {
			return errLeave
		}
	}
	u, err := decode[UserJSON](res)
	if err != nil || u.ID == 0 {
		f.st.score.Penalize("不正なレスポンス", 10)
		return errLeave
	}
	f.UserID = u.ID
	return nil
}

// ---- 閲覧 ----

func (f *Fan) visitTop(ctx context.Context) {
	if f.staticDone {
		return
	}
	for _, p := range []string{"/", "/assets/app.js", "/assets/app.css"} {
		res, err := f.agent.GetStatic(ctx, p)
		f.st.stats.record(res, err)
		if err != nil || res.Status != 200 {
			f.st.score.Penalize("静的ファイルの取得失敗", 5)
			return
		}
	}
	f.staticDone = true
}

// listItems は一覧を取得し、サムネイルを読み込む。
func (f *Fan) listItems(ctx context.Context, path string) ([]ItemJSON, error) {
	reqStart := time.Now()
	res, err := f.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, path, http.StatusOK) {
		return nil, nil
	}
	if res.Latency > 2*time.Second {
		f.strike()
	}
	ir, err := decode[ItemsResponse](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	for _, it := range ir.Items {
		f.st.checkFreshness(it.ID, it.BidCount, reqStart)
		if bi, ok := f.st.item(it.ID); ok && it.Status == "open" {
			bi.seeEnd(it.EndAt)
		}
	}
	for _, it := range ir.Items {
		res, err := f.agent.GetStatic(ctx, it.ThumbnailURL)
		f.st.stats.record(res, err)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			f.st.score.Penalize("画像の取得失敗", 5)
			continue
		}
		if !isJPEGSize(res.Body, 120, 90) {
			f.st.score.Penalize("画像の取得失敗", 5)
		}
	}
	return ir.Items, nil
}

func isJPEGSize(b []byte, w, h int) bool {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	return err == nil && cfg.Width == w && cfg.Height == h
}

func (f *Fan) browse(ctx context.Context) error {
	f.visitTop(ctx)
	var path string
	switch x := f.rng.Intn(10); {
	case x < 4:
		path = fmt.Sprintf("/api/items?sort=ending_soon&page=%d", 1+f.rng.Intn(3))
	case x < 6:
		path = "/api/items?sort=newest"
	case x < 8:
		path = "/api/items?q=" + urlEscape(searchWords[f.rng.Intn(len(searchWords))])
	case x < 9:
		path = fmt.Sprintf("/api/items?category_id=%d", 1+f.rng.Intn(numCategories))
	default:
		path = "/api/items?status=ended"
	}
	items, err := f.listItems(ctx, path)
	if err != nil || len(items) == 0 {
		return err
	}
	it := items[f.rng.Intn(len(items))]
	if _, err := f.detail(ctx, it.ID); err != nil {
		return err
	}
	switch f.rng.Intn(20) {
	case 0, 1, 2:
		res, err := f.call(ctx, http.MethodPost, fmt.Sprintf("/api/items/%d/watch", it.ID), nil)
		if err != nil {
			return err
		}
		expectStatus(f.st, res, "POST /api/items/:id/watch", http.StatusNoContent)
	case 3, 4:
		res, err := f.call(ctx, http.MethodGet, "/api/me/watchlist", nil)
		if err != nil {
			return err
		}
		expectStatus(f.st, res, "GET /api/me/watchlist", http.StatusOK)
	case 5:
		res, err := f.call(ctx, http.MethodGet, "/api/me/bids", nil)
		if err != nil {
			return err
		}
		expectStatus(f.st, res, "GET /api/me/bids", http.StatusOK)
	}
	f.satisfied()
	return nil
}

func (f *Fan) detail(ctx context.Context, id int64) (*ItemJSON, error) {
	reqStart := time.Now()
	path := fmt.Sprintf("/api/items/%d", id)
	res, err := f.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, "GET /api/items/:id", http.StatusOK) {
		return nil, nil
	}
	it, err := decode[ItemJSON](res)
	if err != nil || it.ID != id {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	f.st.checkFreshness(id, it.BidCount, reqStart)
	if bi, ok := f.st.item(id); ok && it.Status == "open" {
		bi.seeEnd(it.EndAt)
	}
	return &it, nil
}

// price は価格の更新を確認する（after 付き。サーバは最大 5 秒待って応答してよい）。
func (f *Fan) price(ctx context.Context, id int64, after int) (*PriceJSON, error) {
	reqStart := time.Now()
	res, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/items/%d/price?after=%d", id, after), nil)
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, "GET /api/items/:id/price", http.StatusOK) {
		return nil, nil
	}
	if res.Latency > 6*time.Second {
		f.st.score.Penalize("価格確認の応答が 5 秒を超えた", 5)
	}
	p, err := decode[PriceJSON](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	f.st.checkFreshness(id, p.Version, reqStart)
	if bi, ok := f.st.item(id); ok && p.Status == "open" {
		bi.seeEnd(p.EndAt)
	}
	// サーバがすぐ応答した場合は、ブラウザと同様に少し間をあけて再確認する
	if p.Version == after && p.Status == "open" && res.Latency < time.Second {
		sleepCtx(ctx, 300*time.Millisecond)
	}
	return &p, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---- 入札 ----

// bid は上限額 max で入札する。受理されたら true。
func (f *Fan) bid(ctx context.Context, it *BenchItem, max int) (bool, error) {
	path := fmt.Sprintf("/api/items/%d/bids", it.ID)
	res, err := f.call(ctx, http.MethodPost, path, map[string]int{"max_amount": max})
	if err != nil {
		it.mu.Lock()
		it.unknown++
		it.mu.Unlock()
		return false, err
	}
	switch res.Status {
	case http.StatusCreated:
	case http.StatusConflict, http.StatusForbidden:
		f.st.score.Add(&f.st.score.BidRejected, 1)
		return false, nil
	default:
		if res.Status >= 500 {
			it.mu.Lock()
			it.unknown++
			it.mu.Unlock()
		}
		expectStatus(f.st, res, "POST /api/items/:id/bids", http.StatusCreated)
		return false, nil
	}
	if res.Latency > 2*time.Second {
		f.strike()
	}
	r, err := decode[BidResponse](res)
	if err != nil || r.BidID == 0 {
		f.st.score.Fail("入札レスポンスが不正です (商品 %d)", it.ID)
		return false, nil
	}
	if !r.AcceptedAt.Before(r.EndAt) {
		f.st.score.Fail("終了時刻 (%s) 以降の入札 (%s) を受け付けました (商品 %d)", r.EndAt.Format(time.RFC3339Nano), r.AcceptedAt.Format(time.RFC3339Nano), it.ID)
		return false, nil
	}
	// 最高入札者なら現在価格は自分の上限額以下、そうでなければ自分が 2 番手なので
	// 「自分の上限額」以上「自分の上限額 + 入札単位」以下になる
	if r.IsLeader && (r.CurrentPrice > max || r.CurrentPrice < it.StartPrice) ||
		!r.IsLeader && (r.CurrentPrice < max || r.CurrentPrice > max+it.Increment) {
		f.st.score.Fail("入札後の現在価格が不正です (商品 %d: 上限額 %d / 現在価格 %d / 最高入札者 %v)", it.ID, max, r.CurrentPrice, r.IsLeader)
		return false, nil
	}
	it.recordBid(f.UserID, max, r)
	f.version = r.Version
	f.st.score.Add(&f.st.score.Bids, 1)
	f.satisfied()
	return true, nil
}

// pickTarget は入札する商品を選ぶ。目玉商品が開いていれば半分の確率でそれを選ぶ。
func (f *Fan) pickTarget(now time.Time, minRemain time.Duration) *BenchItem {
	open := f.st.openItems(now)
	var cand, hot []*BenchItem
	for _, it := range open {
		if it.Seller == f || it.end().Sub(now) < minRemain {
			continue
		}
		cand = append(cand, it)
		if it.Hot {
			hot = append(hot, it)
		}
	}
	if len(hot) > 0 && f.rng.Intn(2) == 0 {
		return hot[f.rng.Intn(len(hot))]
	}
	if len(cand) == 0 {
		return nil
	}
	// 終了の近いものほど選ばれやすい
	i := f.rng.Intn(len(cand))
	if j := f.rng.Intn(len(cand)); j < i {
		i = j
	}
	return cand[i]
}

func (f *Fan) setTarget(it *BenchItem) {
	f.target = it
	f.version = 0
	f.raised = false
	// 評価額は開始価格の 3〜15 倍
	f.valuation = it.StartPrice * (3 + f.rng.Intn(13))
	f.valuation -= f.valuation % it.Increment
}

// stepBidder はせり上げ型・自動入札型・スナイパーの 1 ステップ。
func (f *Fan) stepBidder(ctx context.Context) error {
	now := time.Now()
	if f.target == nil {
		minRemain := time.Second
		if f.role == RoleSniper {
			minRemain = 3 * time.Second
		}
		it := f.pickTarget(now, minRemain)
		if it == nil {
			return f.browse(ctx)
		}
		f.setTarget(it)
		if _, err := f.detail(ctx, it.ID); err != nil {
			return err
		}
	}
	it := f.target

	p, err := f.price(ctx, it.ID, f.version)
	if err != nil || p == nil {
		return err
	}
	if p.Status != "open" {
		f.target = nil
		return nil
	}
	f.version = p.Version
	minBid := it.StartPrice
	if p.BidCount > 0 {
		minBid = p.CurrentPrice + it.Increment
	}
	leading := p.LeaderID == f.UserID

	switch f.role {
	case RoleRaiser:
		if leading || minBid > f.valuation {
			if minBid > f.valuation {
				f.target = nil // 予算オーバーなので諦める
			}
			return nil
		}
		max := minBid + it.Increment*f.rng.Intn(5)
		if max > f.valuation {
			max = f.valuation
		}
		_, err := f.bid(ctx, it, max)
		return err
	case RoleProxy:
		if leading {
			return nil
		}
		if f.version > 0 && f.raised {
			f.target = nil
			return nil
		}
		max := f.valuation
		if f.version > 0 && p.BidCount > 0 && f.rng.Intn(2) == 0 {
			// 抜かれたら一度だけ評価額を 3 割上げる
			max = f.valuation * 13 / 10
			max -= max % it.Increment
			f.raised = true
		}
		if max < minBid {
			f.target = nil
			return nil
		}
		_, err := f.bid(ctx, it, max)
		return err
	case RoleSniper:
		remain := time.Until(p.EndAt)
		if remain > 1500*time.Millisecond || leading {
			return nil
		}
		max := f.valuation
		if max < minBid {
			f.target = nil
			return nil
		}
		ok, err := f.bid(ctx, it, max)
		if ok || err != nil {
			f.target = nil
		}
		return err
	}
	return nil
}

// checkNotifications は新着通知を確認する（2 秒に 1 回程度）。
func (f *Fan) checkNotifications(ctx context.Context) error {
	if time.Since(f.noticeCheckAt) < 2*time.Second {
		return nil
	}
	f.noticeCheckAt = time.Now()
	res, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/me/notifications?after_id=%d", f.lastNotice), nil)
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, "GET /api/me/notifications", http.StatusOK) {
		return nil
	}
	body, err := decode[struct {
		Notifications []NotificationJSON `json:"notifications"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	after := f.lastNotice
	for _, n := range body.Notifications {
		if n.ID <= after {
			f.st.score.Penalize("不正なレスポンス", 10)
			continue
		}
		if n.ID > f.lastNotice {
			f.lastNotice = n.ID
		}
	}
	return nil
}

// ---- 出品 ----

var sellTitles = []string{"ベンチマーク記念チェア", "走行限定スツール", "スナイプ専用ソファ", "延長戦ロッキングチェア", "落札確定ベンチ"}

func (f *Fan) sell(ctx context.Context, dur time.Duration, start, inc int, hot bool) (*BenchItem, error) {
	title := sellTitles[f.rng.Intn(len(sellTitles))]
	if hot {
		title = "【目玉】" + title
	}
	res, err := f.call(ctx, http.MethodPost, "/api/items", map[string]any{
		"title":        fmt.Sprintf("%s No.%d", title, f.rng.Intn(100000)),
		"description":  "ベンチマーク走行中に出品された商品です。",
		"category_id":  1 + f.rng.Intn(numCategories),
		"start_price":  start,
		"increment":    inc,
		"duration_sec": int(dur / time.Second),
		"image":        base64.StdEncoding.EncodeToString(benchImage(f.rng.Int63())),
	})
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, "POST /api/items", http.StatusCreated) {
		return nil, nil
	}
	body, err := decode[struct {
		ID    int64     `json:"id"`
		EndAt time.Time `json:"end_at"`
	}](res)
	if err != nil || body.ID == 0 {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	it := &BenchItem{ID: body.ID, Seller: f, StartPrice: start, Increment: inc, OriginalEnd: body.EndAt, lastEnd: body.EndAt, Hot: hot}
	f.st.addItem(it)
	f.st.score.Add(&f.st.score.Listed, 1)
	return it, nil
}

// watchSettlement は出品者として、終了後に落札が確定するのを見届ける。
// 終了から settleDeadline 以内に確定を確認できれば手数料を計上する。
func (f *Fan) watchSettlement(ctx context.Context, it *BenchItem) {
	for ctx.Err() == nil {
		end := it.end()
		if wait := time.Until(end); wait > 0 {
			sleepCtx(ctx, wait)
			continue
		}
		// 終了したかは軽い価格確認 API で見て、終了していたら詳細を 1 回だけ取る
		res, err := f.agent.Do(ctx, http.MethodGet, fmt.Sprintf("/api/items/%d/price?after=-1", it.ID), nil)
		f.st.stats.record(res, err)
		if err != nil || res.Status != http.StatusOK {
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		p, err := decode[PriceJSON](res)
		if err != nil {
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		if p.Status != "ended" {
			if p.EndAt.After(end) {
				it.seeEnd(p.EndAt)
			}
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		seenAt := time.Now()
		res, err = f.agent.Do(ctx, http.MethodGet, fmt.Sprintf("/api/items/%d", it.ID), nil)
		f.st.stats.record(res, err)
		if err != nil || res.Status != http.StatusOK {
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		d, err := decode[ItemJSON](res)
		if err != nil || d.Status != "ended" {
			f.st.score.Penalize("不正なレスポンス", 10)
			return
		}
		f.st.recordSettled(it, &d, seenAt)
		return
	}
}

func (st *State) recordSettled(it *BenchItem, d *ItemJSON, seenAt time.Time) {
	it.mu.Lock()
	if it.settled {
		it.mu.Unlock()
		return
	}
	it.settled = true
	it.settledAt = seenAt
	it.finalPrice = d.FinalPrice
	it.finalEnd = d.EndAt
	if d.Winner != nil {
		it.winnerID = d.Winner.ID
	}
	onTime := !seenAt.After(d.EndAt.Add(st.cfg.SettleDeadline))
	fee := 0
	if d.FinalPrice != nil {
		fee = *d.FinalPrice / 10
	}
	it.feeCounted = onTime && fee > 0
	it.mu.Unlock()

	if onTime {
		st.score.Add(&st.score.Settled, 1)
		st.score.Add(&st.score.Fee, int64(fee))
	} else {
		st.score.Add(&st.score.LateSettled, 1)
	}
}

func urlEscape(s string) string {
	var b bytes.Buffer
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
