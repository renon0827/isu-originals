package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// validate は負荷走行の前に、API が仕様どおりに動くことを少量のリクエストで確認する。
func validate(ctx context.Context, st *State) error {
	timeout := 30 * time.Second
	newAgent := func() *Agent { return NewAgent(st.cfg.Target, st.cfg.HostHeader, timeout) }
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	call := func(a *Agent, method, path string, body any, want int) (*Response, error) {
		res, err := a.Do(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		if res.Status != want {
			return nil, fmt.Errorf("%s %s: ステータス %d を期待しましたが %d でした: %s", method, path, want, res.Status, truncate(res.Body, 200))
		}
		return res, nil
	}

	// ---- 静的ファイル ----
	top := newAgent()
	res, err := call(top, http.MethodGet, "/", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !strings.Contains(string(res.Body), "ISUTICKET") {
		return fmt.Errorf("GET /: トップページの内容が不正です")
	}
	for _, p := range []string{"/assets/app.js", "/assets/app.css"} {
		if _, err := call(top, http.MethodGet, p, nil, http.StatusOK); err != nil {
			return err
		}
	}

	// ---- 会員登録・ログイン ----
	login := fmt.Sprintf("validate_%d", time.Now().UnixNano()%1_000_000_000)
	alice := newAgent()
	res, err = call(alice, http.MethodPost, "/api/register", map[string]string{"login_name": login, "display_name": "検証ユーザー", "password": "validate-password"}, http.StatusCreated)
	if err != nil {
		return err
	}
	if u, err := decode[UserJSON](res); err != nil || u.LoginName != login || u.ID == 0 {
		return fmt.Errorf("POST /api/register: レスポンスが不正です")
	}
	if _, err := call(newAgent(), http.MethodPost, "/api/register", map[string]string{"login_name": login, "display_name": "x", "password": "validate-password"}, http.StatusConflict); err != nil {
		return err
	}
	if _, err := call(newAgent(), http.MethodPost, "/api/login", map[string]string{"login_name": fanLogin(1), "password": "wrong-password"}, http.StatusUnauthorized); err != nil {
		return err
	}
	tmp := newAgent()
	if _, err := call(tmp, http.MethodPost, "/api/login", map[string]string{"login_name": login, "password": "validate-password"}, http.StatusOK); err != nil {
		return err
	}
	if _, err := call(tmp, http.MethodPost, "/api/logout", nil, http.StatusNoContent); err != nil {
		return err
	}
	if _, err := call(tmp, http.MethodGet, "/api/me", nil, http.StatusUnauthorized); err != nil {
		return err
	}

	bob := newAgent()
	bobLogin := fanLogin(1 + rng.Intn(numInitialFans))
	if _, err := call(bob, http.MethodPost, "/api/login", map[string]string{"login_name": bobLogin, "password": fanPassword(bobLogin)}, http.StatusOK); err != nil {
		return err
	}
	res, err = call(bob, http.MethodGet, "/api/me", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if u, err := decode[UserJSON](res); err != nil || u.LoginName != bobLogin {
		return fmt.Errorf("GET /api/me: レスポンスが不正です")
	}

	org := newAgent()
	if err := loginOrganizer(ctx, org); err != nil {
		return err
	}

	// ---- 公演一覧 ----
	res, err = call(alice, http.MethodGet, "/api/events?status=on_sale", nil, http.StatusOK)
	if err != nil {
		return err
	}
	onSale, err := decode[EventsResponse](res)
	if err != nil {
		return fmt.Errorf("GET /api/events: %w", err)
	}
	if len(onSale.Events) != 20 || !onSale.HasNext {
		return fmt.Errorf("GET /api/events?status=on_sale: 1 ページ目は 20 件・次ページありのはずです (got %d)", len(onSale.Events))
	}
	for i, e := range onSale.Events {
		if i > 0 && e.ID >= onSale.Events[i-1].ID {
			return fmt.Errorf("GET /api/events?status=on_sale: 並び順が不正です")
		}
		if e.ID < initialOnSaleFirst || e.ID > initialOnSaleLast {
			return fmt.Errorf("GET /api/events?status=on_sale: 販売中ではない公演 %d が含まれています", e.ID)
		}
		if len(e.Ranks) != 3 || e.Ranks[0].Rank != "S" || e.Ranks[1].Rank != "A" || e.Ranks[2].Rank != "B" {
			return fmt.Errorf("GET /api/events: 公演 %d のランクが不正です", e.ID)
		}
		st.setPrices(e.ID, e.Ranks)
	}
	res, err = call(alice, http.MethodGet, "/api/events?status=on_sale&page=4", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if p4, err := decode[EventsResponse](res); err != nil || len(p4.Events) != 10 || p4.HasNext {
		return fmt.Errorf("GET /api/events?status=on_sale&page=4: 10 件・次ページなしのはずです")
	}
	for _, status := range []string{"upcoming", "ended"} {
		res, err = call(alice, http.MethodGet, "/api/events?status="+status, nil, http.StatusOK)
		if err != nil {
			return err
		}
		er, err := decode[EventsResponse](res)
		if err != nil || len(er.Events) == 0 {
			return fmt.Errorf("GET /api/events?status=%s: レスポンスが不正です", status)
		}
		for i := 1; i < len(er.Events); i++ {
			a, b := er.Events[i-1], er.Events[i]
			if status == "upcoming" && b.SaleStartAt.Before(a.SaleStartAt) || status == "ended" && b.EndAt.After(a.EndAt) {
				return fmt.Errorf("GET /api/events?status=%s: 並び順が不正です", status)
			}
		}
	}

	// ---- 公演詳細・画像・座席表 ----
	target := onSale.Events[rng.Intn(len(onSale.Events))]
	res, err = call(alice, http.MethodGet, fmt.Sprintf("/api/events/%d", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	detail, err := decode[EventJSON](res)
	if err != nil || detail.ID != target.ID || detail.Description == "" || detail.Venue.Name != target.Venue.Name {
		return fmt.Errorf("GET /api/events/%d: レスポンスが不正です", target.ID)
	}
	res, err = call(alice, http.MethodGet, fmt.Sprintf("/api/events/%d/image", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	if len(res.Body) < 3 || res.Body[0] != 0xFF || res.Body[1] != 0xD8 {
		return fmt.Errorf("GET /api/events/%d/image: JPEG ではありません", target.ID)
	}
	if _, err := call(alice, http.MethodGet, "/api/events/999999", nil, http.StatusNotFound); err != nil {
		return err
	}

	seatmap := func(a *Agent) (*SeatmapResponse, error) {
		res, err := call(a, http.MethodGet, fmt.Sprintf("/api/events/%d/seatmap", target.ID), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		sm, err := decode[SeatmapResponse](res)
		return &sm, err
	}
	sm, err := seatmap(alice)
	if err != nil {
		return err
	}
	availByRank := map[string][]int64{}
	for _, sec := range sm.Sections {
		if len(sec.Seats) != sec.Rows*sec.Cols {
			return fmt.Errorf("GET /api/events/%d/seatmap: 区画 %s の座席数が不正です", target.ID, sec.Name)
		}
		for _, s := range sec.Seats {
			if s.Status == "available" {
				availByRank[sec.Rank] = append(availByRank[sec.Rank], s.ID)
			}
		}
	}
	for _, r := range detail.Ranks {
		if len(availByRank[r.Rank]) != r.Remaining {
			return fmt.Errorf("公演 %d: 座席表の空席数と残席数 (%s) が一致しません", target.ID, r.Rank)
		}
	}

	// ---- 先着購入 ----
	rank := "A"
	if len(availByRank[rank]) < 2 {
		rank = "B"
	}
	seats := availByRank[rank]
	if len(seats) < 2 {
		return fmt.Errorf("公演 %d に空席がありません", target.ID)
	}
	k := rng.Intn(len(seats) - 1)
	buy := []int64{seats[k], seats[k+1]}
	res, err = call(alice, http.MethodPost, fmt.Sprintf("/api/events/%d/purchase", target.ID), map[string]any{"seat_ids": buy}, http.StatusCreated)
	if err != nil {
		return err
	}
	pr, err := decode[PurchaseResponse](res)
	price, _ := st.priceOf(target.ID, rank)
	if err != nil || len(pr.Tickets) != 2 || pr.Total != price*2 {
		return fmt.Errorf("POST /api/events/%d/purchase: レスポンスが不正です", target.ID)
	}
	if p, ok := st.payment.Get(pr.PaymentID); !ok || p.Amount != pr.Total {
		return fmt.Errorf("POST /api/events/%d/purchase: 決済が記録されていません", target.ID)
	}
	if _, err := call(bob, http.MethodPost, fmt.Sprintf("/api/events/%d/purchase", target.ID), map[string]any{"seat_ids": []int64{buy[0]}}, http.StatusConflict); err != nil {
		return err
	}
	if _, err := call(bob, http.MethodPost, fmt.Sprintf("/api/events/%d/purchase", target.ID), map[string]any{"seat_ids": []int64{1, 2, 3, 4, 5}}, http.StatusBadRequest); err != nil {
		return err
	}
	if _, err := call(newAgent(), http.MethodPost, fmt.Sprintf("/api/events/%d/purchase", target.ID), map[string]any{"seat_ids": []int64{buy[0]}}, http.StatusUnauthorized); err != nil {
		return err
	}
	for _, check := range []struct {
		a    *Agent
		want string
	}{{alice, "mine"}, {bob, "sold"}} {
		sm, err := seatmap(check.a)
		if err != nil {
			return err
		}
		for _, sec := range sm.Sections {
			for _, s := range sec.Seats {
				if (s.ID == buy[0] || s.ID == buy[1]) && s.Status != check.want {
					return fmt.Errorf("GET /api/events/%d/seatmap: 購入済みの座席は %q のはずです (got %q)", target.ID, check.want, s.Status)
				}
			}
		}
	}
	res, err = call(alice, http.MethodGet, fmt.Sprintf("/api/events/%d", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	after, _ := decode[EventJSON](res)
	for _, r := range after.Ranks {
		if r.Rank == rank && r.Remaining != len(availByRank[rank])-2 {
			return fmt.Errorf("GET /api/events/%d: 購入後の残席数が不正です", target.ID)
		}
	}

	// ---- マイチケット ----
	myTickets := func(a *Agent) ([]TicketJSON, error) {
		res, err := call(a, http.MethodGet, "/api/me/tickets", nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		body, err := decode[struct {
			Tickets []TicketJSON `json:"tickets"`
		}](res)
		return body.Tickets, err
	}
	ts, err := myTickets(alice)
	if err != nil {
		return err
	}
	if len(ts) != 2 {
		return fmt.Errorf("GET /api/me/tickets: 購入した 2 枚が返されません (got %d)", len(ts))
	}
	for _, t := range ts {
		if _, ok := verifyQR(st.pubKey, t.QRToken, target.ID); !ok || t.Price != price || t.Status != "valid" {
			return fmt.Errorf("GET /api/me/tickets: チケット %d の内容が不正です", t.ID)
		}
	}
	ticket := ts[0]

	// ---- リセール ----
	if _, err := call(alice, http.MethodPost, fmt.Sprintf("/api/tickets/%d/resale", ticket.ID), map[string]int{"price": price + 1}, http.StatusBadRequest); err != nil {
		return err
	}
	if _, err := call(bob, http.MethodPost, fmt.Sprintf("/api/tickets/%d/resale", ticket.ID), map[string]int{"price": price}, http.StatusNotFound); err != nil {
		return err
	}
	res, err = call(alice, http.MethodPost, fmt.Sprintf("/api/tickets/%d/resale", ticket.ID), map[string]int{"price": price}, http.StatusCreated)
	if err != nil {
		return err
	}
	listing, _ := decode[struct {
		ID int64 `json:"id"`
	}](res)
	res, err = call(bob, http.MethodGet, fmt.Sprintf("/api/events/%d/resales", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	ls, _ := decode[struct {
		Listings []ResaleListingJSON `json:"listings"`
	}](res)
	found := false
	for _, l := range ls.Listings {
		if l.ID == listing.ID && l.Price == price && l.Seat.ID == ticket.Seat.ID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("GET /api/events/%d/resales: 出品が表示されません", target.ID)
	}
	if _, err := call(alice, http.MethodPost, fmt.Sprintf("/api/resales/%d/purchase", listing.ID), nil, http.StatusBadRequest); err != nil {
		return err
	}
	res, err = call(bob, http.MethodPost, fmt.Sprintf("/api/resales/%d/purchase", listing.ID), nil, http.StatusCreated)
	if err != nil {
		return err
	}
	rp, _ := decode[ResalePurchaseResponse](res)
	if rp.TicketID != ticket.ID || rp.Total != price {
		return fmt.Errorf("POST /api/resales/%d/purchase: レスポンスが不正です", listing.ID)
	}
	carol := newAgent()
	carolLogin := fanLogin(1 + rng.Intn(numInitialFans))
	if _, err := call(carol, http.MethodPost, "/api/login", map[string]string{"login_name": carolLogin, "password": fanPassword(carolLogin)}, http.StatusOK); err != nil {
		return err
	}
	if carolLogin != bobLogin {
		if _, err := call(carol, http.MethodPost, fmt.Sprintf("/api/resales/%d/purchase", listing.ID), nil, http.StatusConflict); err != nil {
			return err
		}
	}
	ts, err = myTickets(alice)
	if err != nil {
		return err
	}
	if len(ts) != 1 {
		return fmt.Errorf("GET /api/me/tickets: リセールで売れたチケットが残っています")
	}
	bobTickets, err := myTickets(bob)
	if err != nil {
		return err
	}
	var transferred *TicketJSON
	for i := range bobTickets {
		if bobTickets[i].ID == ticket.ID {
			transferred = &bobTickets[i]
		}
	}
	if transferred == nil {
		return fmt.Errorf("GET /api/me/tickets: リセールで購入したチケットが表示されません")
	}
	if transferred.QRToken == ticket.QRToken {
		return fmt.Errorf("リセール後も QR トークンが変わっていません")
	}

	// ---- 入場・主催者 ----
	if _, err := call(bob, http.MethodPost, "/api/gate/verify", map[string]string{"qr_token": transferred.QRToken}, http.StatusForbidden); err != nil {
		return err
	}
	if _, err := call(org, http.MethodPost, "/api/gate/verify", map[string]string{"qr_token": tamper(transferred.QRToken)}, http.StatusBadRequest); err != nil {
		return err
	}
	// 入場開始前の公演なので、正しい QR でも 403
	if _, err := call(org, http.MethodPost, "/api/gate/verify", map[string]string{"qr_token": transferred.QRToken}, http.StatusForbidden); err != nil {
		return err
	}
	if _, err := call(bob, http.MethodGet, fmt.Sprintf("/api/admin/events/%d/sales", target.ID), nil, http.StatusForbidden); err != nil {
		return err
	}
	res, err = call(org, http.MethodGet, fmt.Sprintf("/api/admin/events/%d/sales", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	if sales, err := decode[SalesJSON](res); err != nil || sales.ResaleCount < 1 || sales.Revenue <= 0 {
		return fmt.Errorf("GET /api/admin/events/%d/sales: レスポンスが不正です", target.ID)
	}

	// ---- 抽選 ----
	res, err = call(alice, http.MethodGet, "/api/events?status=upcoming", nil, http.StatusOK)
	if err != nil {
		return err
	}
	up, _ := decode[EventsResponse](res)
	for _, e := range up.Events {
		if e.SaleType == "lottery" {
			if _, err := call(alice, http.MethodPost, fmt.Sprintf("/api/events/%d/lottery", e.ID), map[string]any{"rank": "S", "quantity": 1}, http.StatusForbidden); err != nil {
				return err
			}
			break
		}
	}
	res, err = call(bob, http.MethodGet, "/api/me/lottery", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if _, err := decode[struct {
		Entries []LotteryEntryJSON `json:"entries"`
	}](res); err != nil {
		return fmt.Errorf("GET /api/me/lottery: %w", err)
	}
	return nil
}
