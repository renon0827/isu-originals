package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type FanRole int

const (
	RoleHunter  FanRole = iota // 先着販売を狙う
	RoleLottery                // 抽選に申し込む
	RoleCasual                 // 一覧を眺めて販売中の公演を買う
)

type OwnedTicket struct {
	ID      int64
	EventID int64
	SeatID  int64
	Rank    string
	Price   int
	QR      string
	Status  string // valid / listed
	Entered bool
}

type Fan struct {
	st       *State
	agent    *Agent
	Login    string
	Password string
	isNew    bool
	role     FanRole
	rng      *rand.Rand

	mu      sync.Mutex
	tickets map[int64]*OwnedTicket

	entries    map[int64]*LotteryEntry // eventID → entry
	strikes    int
	left       bool
	staticDone bool
	gateTried  map[int64]bool
}

// errLeave はファンが不満を持って帰ったことを表す。
var errLeave = errors.New("fan left")

func (st *State) newFan() *Fan {
	n := st.nextFan.Add(1)
	f := &Fan{
		st:        st,
		agent:     NewAgent(st.cfg.Target, st.cfg.HostHeader, st.cfg.RequestTimeout),
		rng:       rand.New(rand.NewSource(time.Now().UnixNano() + n)),
		tickets:   map[int64]*OwnedTicket{},
		entries:   map[int64]*LotteryEntry{},
		gateTried: map[int64]bool{},
	}
	// 1 割は新規会員、残りは既存会員（初期データ）
	if f.rng.Intn(10) == 0 {
		f.isNew = true
		f.Login = fmt.Sprintf("bench_%d_%d", time.Now().Unix()%100000, n)
		f.Password = "password-" + f.Login
	} else {
		idx := int((n*7919)%numInitialFans) + 1
		f.Login = fanLogin(idx)
		f.Password = fanPassword(f.Login)
	}
	switch x := f.rng.Intn(100); {
	case x < 40:
		f.role = RoleHunter
	case x < 65:
		f.role = RoleLottery
	default:
		f.role = RoleCasual
	}
	return f
}

// ---- リクエストのラッパー ----

// call は API を呼び、通信エラー・タイムアウト・5xx をペナルティとして記録する。
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

func (f *Fan) Login_(ctx context.Context) error {
	if f.isNew {
		res, err := f.call(ctx, http.MethodPost, "/api/register", map[string]string{
			"login_name": f.Login, "display_name": "ベンチ" + f.Login, "password": f.Password,
		})
		if err != nil {
			return err
		}
		if !expectStatus(f.st, res, "POST /api/register", http.StatusCreated) {
			return errLeave
		}
		return nil
	}
	res, err := f.call(ctx, http.MethodPost, "/api/login", map[string]string{"login_name": f.Login, "password": f.Password})
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, "POST /api/login", http.StatusOK) {
		return errLeave
	}
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

func (f *Fan) browse(ctx context.Context) ([]EventJSON, error) {
	f.visitTop(ctx)
	page := 1 + f.rng.Intn(3)
	path := fmt.Sprintf("/api/events?status=on_sale&page=%d", page)
	start := time.Now()
	res, err := f.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, path, http.StatusOK) {
		return nil, nil
	}
	er, err := decode[EventsResponse](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	if res.Latency > 2*time.Second {
		f.strike()
	}
	for i := range er.Events {
		e := &er.Events[i]
		f.st.setPrices(e.ID, e.Ranks)
		f.st.checkFreshness(e, start)
	}
	// 一覧のポスター画像を読み込む（ブラウザキャッシュを尊重）
	for _, e := range er.Events {
		res, err := f.agent.GetStatic(ctx, fmt.Sprintf("/api/events/%d/image", e.ID))
		f.st.stats.record(res, err)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			f.st.score.Penalize("画像の取得失敗", 5)
			continue
		}
		if res.Status != 200 || len(res.Body) < 3 || res.Body[0] != 0xFF || res.Body[1] != 0xD8 {
			f.st.score.Penalize("画像の取得失敗", 5)
		}
	}
	return er.Events, nil
}

// checkFreshness はベンチ作成の先着公演について、残席数が 2 秒以上古くないか検証する。
func (st *State) checkFreshness(e *EventJSON, reqStart time.Time) {
	be, ok := st.events[e.ID]
	if !ok || be.SaleType != "first_come" {
		return
	}
	sold := be.soldBefore(reqStart.Add(-2 * time.Second))
	for _, r := range e.Ranks {
		total := be.SeatsTotal[r.Rank]
		if r.Remaining < 0 || r.Remaining > total {
			st.score.Fail("公演 %d のランク %s の残席数 %d が不正です", e.ID, r.Rank, r.Remaining)
			return
		}
		if r.Remaining > total-sold[r.Rank] {
			st.score.Penalize("残席数が古い", 10)
		}
	}
}

// ---- 先着購入 ----

func (f *Fan) fetchSeatmap(ctx context.Context, eventID int64) (*SeatmapResponse, error) {
	path := fmt.Sprintf("/api/events/%d/seatmap", eventID)
	res, err := f.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if !expectStatus(f.st, res, path, http.StatusOK) {
		return nil, nil
	}
	if res.Latency > 1500*time.Millisecond {
		f.strike()
	}
	sm, err := decode[SeatmapResponse](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil, nil
	}
	return &sm, nil
}

// pickSeats は希望ランクの空席から quantity 席を選ぶ。良い席（席番号の若い側）ほど選ばれやすい。
func (f *Fan) pickSeats(sm *SeatmapResponse, rank string, quantity int) []int64 {
	var avail []int64
	for _, sec := range sm.Sections {
		if sec.Rank != rank {
			continue
		}
		for _, s := range sec.Seats {
			if s.Status == "available" {
				avail = append(avail, s.ID)
			}
		}
	}
	if len(avail) < quantity {
		return nil
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i] < avail[j] })
	window := len(avail) / 2
	if window < quantity*4 {
		window = len(avail)
	}
	start := f.rng.Intn(window - quantity + 1)
	return append([]int64(nil), avail[start:start+quantity]...)
}

func (f *Fan) preferredRanks() []string {
	switch x := f.rng.Intn(10); {
	case x < 5:
		return []string{"S", "A", "B"}
	case x < 8:
		return []string{"A", "B", "S"}
	default:
		return []string{"B", "A"}
	}
}

func randomQuantity(rng *rand.Rand) int {
	switch x := rng.Intn(10); {
	case x < 3:
		return 1
	case x < 8:
		return 2
	case x < 9:
		return 3
	default:
		return 4
	}
}

// purchase は座席表を見て購入する。売り切れなら soldOut=true を返す。
func (f *Fan) purchase(ctx context.Context, eventID int64) (soldOut bool, err error) {
	sm, err := f.fetchSeatmap(ctx, eventID)
	if err != nil || sm == nil {
		return false, err
	}
	quantity := randomQuantity(f.rng)
	var seats []int64
	var rank string
	for _, r := range f.preferredRanks() {
		if seats = f.pickSeats(sm, r, quantity); seats != nil {
			rank = r
			break
		}
	}
	if seats == nil {
		if seats = f.pickSeats(sm, "S", 1); seats != nil {
			rank = "S"
		} else if seats = f.pickSeats(sm, "A", 1); seats != nil {
			rank = "A"
		} else if seats = f.pickSeats(sm, "B", 1); seats != nil {
			rank = "B"
		} else {
			return true, nil
		}
	}

	path := fmt.Sprintf("/api/events/%d/purchase", eventID)
	res, err := f.call(ctx, http.MethodPost, path, map[string]any{"seat_ids": seats})
	if err != nil {
		// 走行終了で中断した購入も、課金だけ済んでいる可能性がある
		f.st.unknownPurchaseOutcome()
		return false, err
	}
	switch res.Status {
	case http.StatusCreated:
	case http.StatusConflict:
		f.st.score.Inc(&f.st.score.PurchaseConflict)
		f.strike()
		return false, nil
	case http.StatusBadGateway:
		f.st.unknownPurchaseOutcome()
		f.strike()
		return false, nil
	case http.StatusForbidden:
		// 販売開始・終了の境界（時計のずれ）は許容する
		return false, nil
	default:
		if res.Status >= 500 {
			f.st.unknownPurchaseOutcome()
		}
		expectStatus(f.st, res, path, http.StatusCreated)
		return false, nil
	}

	pr, err := decode[PurchaseResponse](res)
	if err != nil || len(pr.Tickets) != len(seats) {
		f.st.score.Fail("購入レスポンスのチケット数が不正です (公演 %d)", eventID)
		return false, nil
	}
	price, ok := f.st.priceOf(eventID, rank)
	if ok && pr.Total != price*len(seats) {
		f.st.score.Fail("購入金額が不正です (公演 %d: %d != %d)", eventID, pr.Total, price*len(seats))
		return false, nil
	}
	if !f.st.checkPayment(pr.PaymentID, pr.Total) {
		return false, nil
	}
	want := map[int64]bool{}
	for _, s := range seats {
		want[s] = true
	}
	sold := map[int64]int64{}
	for _, t := range pr.Tickets {
		if !want[t.SeatID] {
			f.st.score.Fail("購入していない座席 %d のチケットが返されました", t.SeatID)
			return false, nil
		}
		sold[t.SeatID] = t.ID
	}
	f.st.markSeatsSold(eventID, sold)

	f.mu.Lock()
	for _, t := range pr.Tickets {
		f.tickets[t.ID] = &OwnedTicket{ID: t.ID, EventID: eventID, SeatID: t.SeatID, Rank: rank, Price: price, Status: "valid"}
	}
	f.mu.Unlock()

	f.st.score.AddRevenue("first_come", pr.Total)
	f.st.score.Inc(&f.st.score.PurchaseSuccess)
	if be, ok := f.st.events[eventID]; ok {
		be.recordSale(time.Now(), rank, len(seats))
	}
	f.satisfied()
	return false, f.refreshTickets(ctx)
}

// ---- マイチケット ----

// refreshTickets は /api/me/tickets を取得し、所持チケットと QR を検証・更新する。
func (f *Fan) refreshTickets(ctx context.Context) error {
	res, err := f.call(ctx, http.MethodGet, "/api/me/tickets", nil)
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, "GET /api/me/tickets", http.StatusOK) {
		return nil
	}
	if res.Latency > 2*time.Second {
		f.strike()
	}
	body, err := decode[struct {
		Tickets []TicketJSON `json:"tickets"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	got := map[int64]*TicketJSON{}
	for i := range body.Tickets {
		t := &body.Tickets[i]
		got[t.ID] = t
		if _, ok := verifyQR(f.st.pubKey, t.QRToken, t.EventID); !ok {
			f.st.score.Fail("チケット %d の QR トークンが公開鍵で検証できません", t.ID)
			return nil
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for id, ot := range f.tickets {
		t, ok := got[id]
		if !ok {
			// 直後にリセールで売れた場合などは除外されるので、listed のものは見逃す
			if ot.Status == "valid" {
				f.st.score.Penalize("購入したチケットが表示されない", 50)
				f.st.score.Message(fmt.Sprintf("%s: チケット %d (公演 %d, 座席 %d) が /api/me/tickets にありません", f.Login, id, ot.EventID, ot.SeatID))
			}
			continue
		}
		if t.Seat.ID != ot.SeatID || t.EventID != ot.EventID {
			f.st.score.Fail("チケット %d の座席・公演が購入時と異なります", id)
			return nil
		}
		ot.QR = t.QRToken
		ot.Entered = t.UsedAt != nil
	}
	return nil
}

func verifyQR(pub *rsa.PublicKey, token string, eventID int64) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256([]byte(parts[0]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return "", false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var p struct {
		EventID int64  `json:"eid"`
		Serial  string `json:"sn"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.EventID != eventID || p.Serial == "" {
		return "", false
	}
	return p.Serial, true
}

// ---- 抽選 ----

func (f *Fan) enterLottery(ctx context.Context, be *BenchEvent) error {
	if _, ok := f.entries[be.ID]; ok {
		return nil
	}
	ranks := f.preferredRanks()
	entry := &LotteryEntry{Fan: f, EventID: be.ID, Rank: ranks[0], Quantity: randomQuantity(f.rng), Status: "pending"}
	path := fmt.Sprintf("/api/events/%d/lottery", be.ID)
	res, err := f.call(ctx, http.MethodPost, path, map[string]any{"rank": entry.Rank, "quantity": entry.Quantity})
	if err != nil {
		return err
	}
	switch res.Status {
	case http.StatusCreated:
		entry.EnteredAt = time.Now()
		f.entries[be.ID] = entry
		be.mu.Lock()
		be.entries = append(be.entries, entry)
		be.mu.Unlock()
		f.satisfied()
	case http.StatusForbidden:
		// 受付終了の境界
		f.entries[be.ID] = &LotteryEntry{Status: "rejected"}
	default:
		expectStatus(f.st, res, path, http.StatusCreated)
	}
	return nil
}

func (f *Fan) hasPendingAnnounced(now time.Time) bool {
	for id, e := range f.entries {
		if e.Status == "pending" && !now.Before(f.st.events[id].AnnounceAt) {
			return true
		}
	}
	return false
}

// checkLottery は抽選結果を確認する。final=true は最終チェックからの呼び出し。
func (f *Fan) checkLottery(ctx context.Context) error {
	reqStart := time.Now()
	res, err := f.call(ctx, http.MethodGet, "/api/me/lottery", nil)
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, "GET /api/me/lottery", http.StatusOK) {
		return nil
	}
	body, err := decode[struct {
		Entries []LotteryEntryJSON `json:"entries"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	wonAny := false
	for _, ej := range body.Entries {
		entry, ok := f.entries[ej.EventID]
		if !ok || entry.Status != "pending" {
			continue
		}
		be := f.st.events[ej.EventID]
		if ej.Rank != entry.Rank || ej.Quantity != entry.Quantity {
			f.st.score.Fail("抽選申込の内容が一致しません (公演 %d)", ej.EventID)
			return nil
		}
		switch ej.Status {
		case "pending":
			if reqStart.After(be.AnnounceAt.Add(f.st.cfg.LotteryDeadline)) {
				f.strike()
			}
		case "won", "lost":
			entry.Status = ej.Status
			seenAt := time.Now()
			be.mu.Lock()
			if be.resultSeenAt.IsZero() {
				be.resultSeenAt = seenAt
			}
			be.mu.Unlock()
			late := seenAt.After(be.AnnounceAt.Add(f.st.cfg.LotteryDeadline))
			if ej.Status == "won" {
				wonAny = true
				if late {
					// 発表から期限内に結果を確認できなかった当選者は辞退する
					f.st.score.Penalize("当選辞退（結果確認の遅れ）", 0)
				} else {
					f.st.score.AddRevenue("lottery", be.Prices[entry.Rank]*entry.Quantity)
				}
			}
			if !late {
				f.satisfied()
			}
		default:
			f.st.score.Fail("抽選ステータス %q は不正です", ej.Status)
			return nil
		}
	}
	if wonAny {
		return f.collectLotteryTickets(ctx)
	}
	return nil
}

// collectLotteryTickets は当選したチケットを /api/me/tickets から取り込み、座席の重複を検証する。
func (f *Fan) collectLotteryTickets(ctx context.Context) error {
	res, err := f.call(ctx, http.MethodGet, "/api/me/tickets", nil)
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, "GET /api/me/tickets", http.StatusOK) {
		return nil
	}
	body, err := decode[struct {
		Tickets []TicketJSON `json:"tickets"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	perEvent := map[int64][]TicketJSON{}
	for _, t := range body.Tickets {
		perEvent[t.EventID] = append(perEvent[t.EventID], t)
	}
	for eventID, entry := range f.entries {
		if entry.Status != "won" || entry.Collected {
			continue
		}
		entry.Collected = true
		ts := perEvent[eventID]
		if len(ts) != entry.Quantity {
			f.st.score.Fail("当選したチケットの枚数が不正です (公演 %d: %d != %d)", eventID, len(ts), entry.Quantity)
			return nil
		}
		sold := map[int64]int64{}
		f.mu.Lock()
		for _, t := range ts {
			if t.Seat.Rank != entry.Rank {
				f.mu.Unlock()
				f.st.score.Fail("当選したチケットのランクが申込と異なります (公演 %d)", eventID)
				return nil
			}
			if _, ok := verifyQR(f.st.pubKey, t.QRToken, eventID); !ok {
				f.mu.Unlock()
				f.st.score.Fail("チケット %d の QR トークンが公開鍵で検証できません", t.ID)
				return nil
			}
			sold[t.Seat.ID] = t.ID
			f.tickets[t.ID] = &OwnedTicket{ID: t.ID, EventID: eventID, SeatID: t.Seat.ID, Rank: t.Seat.Rank, Price: t.Price, QR: t.QRToken, Status: "valid"}
		}
		f.mu.Unlock()
		f.st.markSeatsSold(eventID, sold)
	}
	return nil
}

// ---- リセール ----

func (f *Fan) listForResale(ctx context.Context, now time.Time) error {
	f.mu.Lock()
	var cand []*OwnedTicket
	for _, t := range f.tickets {
		be, ok := f.st.events[t.EventID]
		if ok && t.Status == "valid" && !t.Entered && t.QR != "" && now.Before(be.GateOpen.Add(-time.Second)) {
			cand = append(cand, t)
		}
	}
	f.mu.Unlock()
	if len(cand) == 0 {
		return nil
	}
	t := cand[f.rng.Intn(len(cand))]
	price := t.Price - f.rng.Intn(3)*500
	if price < 1000 {
		price = 1000
	}
	path := fmt.Sprintf("/api/tickets/%d/resale", t.ID)
	res, err := f.call(ctx, http.MethodPost, path, map[string]int{"price": price})
	if err != nil {
		return err
	}
	if res.Status == http.StatusForbidden {
		return nil // 入場開始の境界
	}
	if !expectStatus(f.st, res, path, http.StatusCreated) {
		return nil
	}
	body, err := decode[struct {
		ID int64 `json:"id"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	f.mu.Lock()
	t.Status = "listed"
	qr := t.QR
	f.mu.Unlock()
	f.st.listingMu.Lock()
	f.st.listings[body.ID] = &Listing{ID: body.ID, EventID: t.EventID, TicketID: t.ID, Seller: f, Price: price, OldQR: qr}
	f.st.listingMu.Unlock()
	return nil
}

func (f *Fan) buyResale(ctx context.Context, eventID int64) error {
	path := fmt.Sprintf("/api/events/%d/resales", eventID)
	res, err := f.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if !expectStatus(f.st, res, path, http.StatusOK) {
		return nil
	}
	body, err := decode[struct {
		Listings []ResaleListingJSON `json:"listings"`
	}](res)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	var cand []ResaleListingJSON
	f.st.listingMu.Lock()
	for _, l := range body.Listings {
		if bl, ok := f.st.listings[l.ID]; ok && bl.Seller == f {
			continue
		}
		cand = append(cand, l)
		if len(cand) >= 5 {
			break
		}
	}
	f.st.listingMu.Unlock()
	if len(cand) == 0 {
		return nil
	}
	l := cand[f.rng.Intn(len(cand))]
	ppath := fmt.Sprintf("/api/resales/%d/purchase", l.ID)
	pres, err := f.call(ctx, http.MethodPost, ppath, nil)
	if err != nil {
		f.st.unknownPurchaseOutcome()
		return err
	}
	switch pres.Status {
	case http.StatusCreated:
	case http.StatusConflict, http.StatusNotFound:
		return nil
	case http.StatusBadGateway:
		f.st.unknownPurchaseOutcome()
		return nil
	default:
		if pres.Status >= 500 {
			f.st.unknownPurchaseOutcome()
		}
		expectStatus(f.st, pres, ppath, http.StatusCreated)
		return nil
	}
	pr, err := decode[ResalePurchaseResponse](pres)
	if err != nil {
		f.st.score.Penalize("不正なレスポンス", 10)
		return nil
	}
	if pr.Total != l.Price {
		f.st.score.Fail("リセール購入金額が出品価格と異なります (%d != %d)", pr.Total, l.Price)
		return nil
	}
	if !f.st.checkPayment(pr.PaymentID, pr.Total) {
		return nil
	}

	f.st.listingMu.Lock()
	bl, known := f.st.listings[l.ID]
	if !known {
		bl = &Listing{ID: l.ID, EventID: eventID, TicketID: pr.TicketID, Price: l.Price}
		f.st.listings[l.ID] = bl
	}
	if bl.SoldTo != nil {
		f.st.listingMu.Unlock()
		f.st.score.Fail("リセール出品 %d が二重に販売されました", l.ID)
		return nil
	}
	bl.SoldTo = f
	seller := bl.Seller
	f.st.listingMu.Unlock()

	if seller != nil {
		seller.mu.Lock()
		delete(seller.tickets, bl.TicketID)
		seller.mu.Unlock()
	}
	f.mu.Lock()
	f.tickets[pr.TicketID] = &OwnedTicket{ID: pr.TicketID, EventID: eventID, SeatID: l.Seat.ID, Rank: l.Seat.Rank, Price: l.Price, Status: "valid"}
	f.mu.Unlock()

	f.st.score.AddRevenue("resale_fee", l.Price*10/100)
	f.st.score.Inc(&f.st.score.ResaleSuccess)
	f.satisfied()
	return f.refreshTickets(ctx)
}

// ---- 入場 ----

func (f *Fan) goToGate(ctx context.Context, gate *Gate, now time.Time) error {
	if err := f.refreshTickets(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	var cand []*OwnedTicket
	for _, t := range f.tickets {
		be, ok := f.st.events[t.EventID]
		if ok && t.Status == "valid" && !t.Entered && t.QR != "" && !now.Before(be.GateOpen) && !f.gateTried[t.ID] {
			cand = append(cand, t)
		}
	}
	f.mu.Unlock()
	for _, t := range cand {
		f.gateTried[t.ID] = true
		if err := gate.Verify(ctx, f, t); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fan) hasGateTickets(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tickets {
		be, ok := f.st.events[t.EventID]
		if ok && t.Status == "valid" && !t.Entered && !now.Before(be.GateOpen) && !f.gateTried[t.ID] {
			return true
		}
	}
	return false
}
