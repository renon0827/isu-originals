package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// validate は負荷走行の前に、API が仕様どおりに動くことを少量のリクエストで確認する。
func validate(ctx context.Context, st *State) error {
	newAgent := func() *Agent { return NewAgent(st.cfg.Target, st.cfg.HostHeader, 30*time.Second) }
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
	login := func(i int) (*Agent, int64, error) {
		a := newAgent()
		l := userLogin(i)
		res, err := call(a, http.MethodPost, "/api/login", map[string]string{"login_name": l, "password": userPassword(l)}, http.StatusOK)
		if err != nil {
			return nil, 0, err
		}
		u, err := decode[UserJSON](res)
		if err != nil || u.LoginName != l {
			return nil, 0, fmt.Errorf("POST /api/login: レスポンスが不正です")
		}
		return a, u.ID, nil
	}

	// ---- 静的ファイル ----
	top := newAgent()
	res, err := call(top, http.MethodGet, "/", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !strings.Contains(string(res.Body), "ISUAUCTION") {
		return fmt.Errorf("GET /: トップページの内容が不正です")
	}
	for _, p := range []string{"/assets/app.js", "/assets/app.css"} {
		if _, err := call(top, http.MethodGet, p, nil, http.StatusOK); err != nil {
			return err
		}
	}

	// ---- 会員登録・ログイン ----
	name := fmt.Sprintf("validate_%d", time.Now().UnixNano()%1_000_000_000)
	reg := newAgent()
	res, err = call(reg, http.MethodPost, "/api/register", map[string]string{"login_name": name, "nickname": "検証ユーザー", "password": "validate-password"}, http.StatusCreated)
	if err != nil {
		return err
	}
	if u, err := decode[UserJSON](res); err != nil || u.LoginName != name || u.ID == 0 {
		return fmt.Errorf("POST /api/register: レスポンスが不正です")
	}
	if _, err := call(newAgent(), http.MethodPost, "/api/register", map[string]string{"login_name": name, "nickname": "x", "password": "validate-password"}, http.StatusConflict); err != nil {
		return err
	}
	if _, err := call(newAgent(), http.MethodPost, "/api/login", map[string]string{"login_name": userLogin(1), "password": "wrong-password"}, http.StatusUnauthorized); err != nil {
		return err
	}
	if _, err := call(reg, http.MethodPost, "/api/logout", nil, http.StatusNoContent); err != nil {
		return err
	}
	if _, err := call(reg, http.MethodGet, "/api/me", nil, http.StatusUnauthorized); err != nil {
		return err
	}

	// ---- カテゴリ・一覧・検索 ----
	res, err = call(top, http.MethodGet, "/api/categories", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if cs, err := decode[struct {
		Categories []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"categories"`
	}](res); err != nil || len(cs.Categories) != numCategories {
		return fmt.Errorf("GET /api/categories: レスポンスが不正です")
	}

	list := func(path string) (*ItemsResponse, error) {
		res, err := call(top, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		ir, err := decode[ItemsResponse](res)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		return &ir, nil
	}
	open, err := list("/api/items?sort=ending_soon")
	if err != nil {
		return err
	}
	if len(open.Items) != 20 || !open.HasNext {
		return fmt.Errorf("GET /api/items: 1 ページ目は 20 件・次ページありのはずです (got %d)", len(open.Items))
	}
	for i, it := range open.Items {
		if it.Status != "open" || it.ID <= numEndedItems {
			return fmt.Errorf("GET /api/items: 出品中でない商品 %d が含まれています", it.ID)
		}
		if i > 0 && it.EndAt.Before(open.Items[i-1].EndAt) {
			return fmt.Errorf("GET /api/items?sort=ending_soon: 並び順が不正です")
		}
	}
	ended, err := list("/api/items?status=ended")
	if err != nil {
		return err
	}
	for i, it := range ended.Items {
		if it.Status != "ended" || i > 0 && it.EndAt.After(ended.Items[i-1].EndAt) {
			return fmt.Errorf("GET /api/items?status=ended: 内容または並び順が不正です")
		}
	}
	newest, err := list("/api/items?sort=newest")
	if err != nil || len(newest.Items) == 0 {
		return fmt.Errorf("GET /api/items?sort=newest: レスポンスが不正です")
	}
	word := searchWords[rng.Intn(len(searchWords))]
	found, err := list("/api/items?q=" + url.QueryEscape(word))
	if err != nil {
		return err
	}
	for _, it := range found.Items {
		if it.Status != "open" {
			return fmt.Errorf("GET /api/items?q=: 出品中でない商品が含まれています")
		}
	}
	cat := 1 + rng.Intn(numCategories)
	byCat, err := list(fmt.Sprintf("/api/items?category_id=%d", cat))
	if err != nil {
		return err
	}
	for _, it := range byCat.Items {
		if it.CategoryID != int64(cat) {
			return fmt.Errorf("GET /api/items?category_id=: 別カテゴリの商品が含まれています")
		}
	}

	// ---- 商品詳細・価格・画像 ----
	target := open.Items[rng.Intn(len(open.Items))]
	res, err = call(top, http.MethodGet, fmt.Sprintf("/api/items/%d", target.ID), nil, http.StatusOK)
	if err != nil {
		return err
	}
	detail, err := decode[ItemJSON](res)
	if err != nil || detail.ID != target.ID || detail.Description == "" || detail.CurrentPrice != target.CurrentPrice || detail.WatchCount == nil {
		return fmt.Errorf("GET /api/items/%d: レスポンスが不正です", target.ID)
	}
	res, err = call(top, http.MethodGet, fmt.Sprintf("/api/items/%d/price?after=%d", target.ID, detail.BidCount+1), nil, http.StatusOK)
	if err != nil {
		return err
	}
	if p, err := decode[PriceJSON](res); err != nil || p.Version != detail.BidCount || p.CurrentPrice != detail.CurrentPrice {
		return fmt.Errorf("GET /api/items/%d/price: レスポンスが不正です", target.ID)
	}
	res, err = call(top, http.MethodGet, detail.ImageURL, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !isJPEGSize(res.Body, 480, 360) {
		return fmt.Errorf("GET %s: 480x360 の JPEG ではありません", detail.ImageURL)
	}
	res, err = call(top, http.MethodGet, detail.ThumbnailURL, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !isJPEGSize(res.Body, 120, 90) {
		return fmt.Errorf("GET %s: 120x90 の JPEG ではありません", detail.ThumbnailURL)
	}
	if _, err := call(top, http.MethodGet, "/api/items/99999999", nil, http.StatusNotFound); err != nil {
		return err
	}

	// ---- 出品・入札 ----
	base := 1 + rng.Intn(numInitialUsers-3)
	seller, _, err := login(base)
	if err != nil {
		return err
	}
	alice, aliceID, err := login(base + 1)
	if err != nil {
		return err
	}
	bob, bobID, err := login(base + 2)
	if err != nil {
		return err
	}
	res, err = call(seller, http.MethodPost, "/api/items", map[string]any{
		"title": "事前チェック用の椅子", "description": "事前チェック", "category_id": 1,
		"start_price": 1000, "increment": 100, "duration_sec": 10, "image": encodeImage(benchImage(rng.Int63())),
	}, http.StatusCreated)
	if err != nil {
		return err
	}
	created, err := decode[struct {
		ID    int64     `json:"id"`
		EndAt time.Time `json:"end_at"`
	}](res)
	if err != nil || created.ID == 0 || created.EndAt.Sub(time.Now()) > 11*time.Second || created.EndAt.Sub(time.Now()) < 8*time.Second {
		return fmt.Errorf("POST /api/items: レスポンスが不正です")
	}
	if _, err := call(seller, http.MethodPost, "/api/items", map[string]any{
		"title": "期間が短すぎる", "category_id": 1, "start_price": 1000, "increment": 100, "duration_sec": 5, "image": encodeImage(benchImage(1)),
	}, http.StatusBadRequest); err != nil {
		return err
	}
	id := created.ID
	bidPath := fmt.Sprintf("/api/items/%d/bids", id)
	bid := func(a *Agent, max, want int) (*BidResponse, error) {
		res, err := call(a, http.MethodPost, bidPath, map[string]int{"max_amount": max}, want)
		if err != nil || want != http.StatusCreated {
			return nil, err
		}
		r, err := decode[BidResponse](res)
		return &r, err
	}
	if _, err := bid(alice, 900, http.StatusConflict); err != nil {
		return err
	}
	if _, err := bid(seller, 1500, http.StatusForbidden); err != nil {
		return err
	}
	if _, err := bid(newAgent(), 1500, http.StatusUnauthorized); err != nil {
		return err
	}
	expect := func(r *BidResponse, price int, leader bool, version int) error {
		if r == nil || r.CurrentPrice != price || r.IsLeader != leader || r.Version != version {
			return fmt.Errorf("POST %s: 入札結果が不正です (期待: %d円 / 最高入札者 %v / version %d)", bidPath, price, leader, version)
		}
		return nil
	}
	r, err := bid(alice, 1500, http.StatusCreated)
	if err != nil {
		return err
	}
	if err := expect(r, 1000, true, 1); err != nil {
		return err
	}
	if _, err := bid(bob, 1000, http.StatusConflict); err != nil {
		return err
	}
	if r, err = bid(bob, 1300, http.StatusCreated); err != nil {
		return err
	}
	if err := expect(r, 1400, false, 2); err != nil {
		return err
	}
	if r, err = bid(bob, 2000, http.StatusCreated); err != nil {
		return err
	}
	if err := expect(r, 1600, true, 3); err != nil {
		return err
	}
	// 同額なら先に入札した方が優先される
	if r, err = bid(alice, 2000, http.StatusCreated); err != nil {
		return err
	}
	if err := expect(r, 2000, false, 4); err != nil {
		return err
	}

	res, err = call(alice, http.MethodGet, fmt.Sprintf("/api/items/%d", id), nil, http.StatusOK)
	if err != nil {
		return err
	}
	d, err := decode[ItemJSON](res)
	if err != nil || d.BidCount != 4 || d.CurrentPrice != 2000 || d.Leader == nil || d.Leader.ID != bobID || d.MyMax == nil || *d.MyMax != 2000 {
		return fmt.Errorf("GET /api/items/%d: 入札後の内容が不正です", id)
	}
	res, err = call(alice, http.MethodGet, fmt.Sprintf("/api/items/%d/bids", id), nil, http.StatusOK)
	if err != nil {
		return err
	}
	if h, err := decode[struct {
		Bids []struct {
			Bidder UserSummary `json:"bidder"`
		} `json:"bids"`
	}](res); err != nil || len(h.Bids) != 4 || h.Bids[0].Bidder.ID != aliceID {
		return fmt.Errorf("GET /api/items/%d/bids: 入札履歴が不正です", id)
	}

	// ---- ウォッチ・マイページ ----
	if _, err := call(alice, http.MethodPost, fmt.Sprintf("/api/items/%d/watch", id), nil, http.StatusNoContent); err != nil {
		return err
	}
	res, err = call(alice, http.MethodGet, "/api/me/watchlist", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if wl, err := decode[ItemsResponse](res); err != nil || len(wl.Items) == 0 || wl.Items[0].ID != id {
		return fmt.Errorf("GET /api/me/watchlist: ウォッチした商品がありません")
	}
	res, err = call(alice, http.MethodGet, "/api/me/bids", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if mb, err := decode[struct {
		Bids []MyBidJSON `json:"bids"`
	}](res); err != nil || len(mb.Bids) == 0 || mb.Bids[0].Item.ID != id || mb.Bids[0].Result != "outbid" || mb.Bids[0].MyMax != 2000 {
		return fmt.Errorf("GET /api/me/bids: 内容が不正です")
	}
	res, err = call(seller, http.MethodGet, "/api/me/items", nil, http.StatusOK)
	if err != nil {
		return err
	}
	if mi, err := decode[ItemsResponse](res); err != nil || len(mi.Items) == 0 || mi.Items[0].ID != id {
		return fmt.Errorf("GET /api/me/items: 出品した商品がありません")
	}

	// ---- 終了・落札確定 ----
	var ended2 ItemJSON
	deadline := created.EndAt.Add(5 * time.Second)
	for {
		time.Sleep(300 * time.Millisecond)
		res, err = call(top, http.MethodGet, fmt.Sprintf("/api/items/%d", id), nil, http.StatusOK)
		if err != nil {
			return err
		}
		if ended2, err = decode[ItemJSON](res); err != nil {
			return err
		}
		if ended2.Status == "ended" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("商品 %d: 終了から 5 秒経っても落札が確定しません", id)
		}
	}
	if ended2.Winner == nil || ended2.Winner.ID != bobID || ended2.FinalPrice == nil || *ended2.FinalPrice != 2000 {
		return fmt.Errorf("商品 %d: 落札結果が不正です", id)
	}
	if _, err := bid(alice, 5000, http.StatusForbidden); err != nil {
		return err
	}
	notices := func(a *Agent) (map[string]bool, error) {
		res, err := call(a, http.MethodGet, "/api/me/notifications", nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		body, err := decode[struct {
			Notifications []NotificationJSON `json:"notifications"`
		}](res)
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, n := range body.Notifications {
			if n.ItemID == id {
				m[n.Type] = true
			}
		}
		return m, nil
	}
	for _, c := range []struct {
		a    *Agent
		who  string
		want []string
	}{{bob, "落札者", []string{"won"}}, {seller, "出品者", []string{"sold"}}, {alice, "入札者", []string{"outbid", "ended"}}} {
		m, err := notices(c.a)
		if err != nil {
			return err
		}
		for _, w := range c.want {
			if !m[w] {
				return fmt.Errorf("GET /api/me/notifications: %sに %s の通知がありません", c.who, w)
			}
		}
	}
	return nil
}

func encodeImage(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
