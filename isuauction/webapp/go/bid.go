package main

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// 終了間際の入札で終了時刻を延長する（残り extendWindow 未満なら、入札時刻 + extendWindow まで延ばす）。
const extendWindow = 3 * time.Second

type BidResponse struct {
	BidID        int64     `json:"bid_id"`
	AcceptedAt   time.Time `json:"accepted_at"`
	CurrentPrice int       `json:"current_price"`
	IsLeader     bool      `json:"is_leader"`
	EndAt        time.Time `json:"end_at"`
	Version      int       `json:"version"`
}

// postBid は入札（自動入札の上限額の設定）。
//
// 最高入札者以外: 上限額は「入札が無ければ開始価格、あれば現在価格 + 入札単位」以上であること。
// 最高入札者自身: 上限額の引き上げのみ可能。
func postBid(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	itemID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var req struct {
		MaxAmount int `json:"max_amount"`
	}
	if err := c.Bind(&req); err != nil || req.MaxAmount <= 0 {
		return errorJSON(c, http.StatusBadRequest, "max_amount is required")
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var it Item
	if err := tx.Get(&it, "SELECT * FROM items WHERE id = ? FOR UPDATE", itemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "item not found")
		}
		return err
	}
	now := time.Now()
	if it.Status != "open" || !now.Before(it.EndAt) {
		return errorJSON(c, http.StatusForbidden, "auction has ended")
	}
	if it.SellerID == u.ID {
		return errorJSON(c, http.StatusForbidden, "cannot bid on your own item")
	}

	before, err := loadState(tx, &it)
	if err != nil {
		return err
	}
	if before.LeaderID == u.ID {
		if req.MaxAmount <= before.MaxByUser[u.ID] {
			return errorJSON(c, http.StatusConflict, "max_amount must be greater than your current max")
		}
	} else {
		min := it.StartPrice
		if before.BidCount > 0 {
			min = before.Price + it.Increment
		}
		if req.MaxAmount < min {
			return errorJSON(c, http.StatusConflict, "max_amount is too low")
		}
	}

	res, err := tx.Exec("INSERT INTO bids (item_id, user_id, max_amount, created_at) VALUES (?, ?, ?, ?)", it.ID, u.ID, req.MaxAmount, now)
	if err != nil {
		return err
	}
	bidID, _ := res.LastInsertId()

	if it.EndAt.Sub(now) < extendWindow {
		it.EndAt = now.Add(extendWindow)
		if _, err := tx.Exec("UPDATE items SET end_at = ? WHERE id = ?", it.EndAt, it.ID); err != nil {
			return err
		}
	}

	after, err := loadState(tx, &it)
	if err != nil {
		return err
	}
	// 最高入札者が入れ替わったら、前の最高入札者に通知する
	if before.LeaderID != 0 && after.LeaderID != before.LeaderID {
		if _, err := tx.Exec("INSERT INTO notifications (user_id, type, item_id, price, created_at) VALUES (?, 'outbid', ?, ?, ?)",
			before.LeaderID, it.ID, after.Price, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return c.JSON(http.StatusCreated, BidResponse{
		BidID:        bidID,
		AcceptedAt:   now,
		CurrentPrice: after.Price,
		IsLeader:     after.LeaderID == u.ID,
		EndAt:        it.EndAt,
		Version:      after.BidCount,
	})
}

// ---- 出品 ----

const (
	minDuration = 10 * time.Second
	maxDuration = 7 * 24 * time.Hour
)

func postItem(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		CategoryID  int64  `json:"category_id"`
		StartPrice  int    `json:"start_price"`
		Increment   int    `json:"increment"`
		DurationSec int    `json:"duration_sec"`
		Image       string `json:"image"` // base64 エンコードした JPEG
	}
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}
	dur := time.Duration(req.DurationSec) * time.Second
	if req.Title == "" || req.StartPrice < 1 || req.Increment < 1 || dur < minDuration || dur > maxDuration {
		return errorJSON(c, http.StatusBadRequest, "invalid parameters")
	}
	var cat int
	if err := db.Get(&cat, "SELECT COUNT(*) FROM categories WHERE id = ?", req.CategoryID); err != nil {
		return err
	}
	if cat == 0 {
		return errorJSON(c, http.StatusBadRequest, "invalid category")
	}
	img, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil || len(img) < 3 || img[0] != 0xFF || img[1] != 0xD8 {
		return errorJSON(c, http.StatusBadRequest, "image must be a base64 encoded JPEG")
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO images (data) VALUES (?)", img)
	if err != nil {
		return err
	}
	imageID, _ := res.LastInsertId()
	now := time.Now()
	end := now.Add(dur)
	res, err = tx.Exec(`INSERT INTO items (seller_id, category_id, title, description, image_id, start_price, increment, status, end_at, original_end_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		u.ID, req.CategoryID, req.Title, req.Description, imageID, req.StartPrice, req.Increment, end, end, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return c.JSON(http.StatusCreated, map[string]any{"id": id, "end_at": end})
}

// ---- 落札確定 ----

// settleLoop は 1 秒ごとに終了時刻を過ぎた商品を探して落札を確定する。
func settleLoop() {
	for range time.Tick(time.Second) {
		var ids []int64
		if err := db.Select(&ids, "SELECT id FROM items WHERE status = 'open' AND end_at <= ?", time.Now()); err != nil {
			log.Printf("settle: %v", err)
			continue
		}
		for _, id := range ids {
			if err := settleItem(id); err != nil {
				log.Printf("settle item %d: %v", id, err)
			}
		}
	}
}

func settleItem(id int64) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var it Item
	if err := tx.Get(&it, "SELECT * FROM items WHERE id = ? FOR UPDATE", id); err != nil {
		return err
	}
	now := time.Now()
	if it.Status != "open" || now.Before(it.EndAt) {
		return nil
	}
	var bids []Bid
	if err := tx.Select(&bids, "SELECT * FROM bids WHERE item_id = ? ORDER BY id", it.ID); err != nil {
		return err
	}
	st := computeState(&it, bids)

	notify := func(userID int64, typ string, price any) error {
		_, err := tx.Exec("INSERT INTO notifications (user_id, type, item_id, price, created_at) VALUES (?, ?, ?, ?, ?)", userID, typ, it.ID, price, now)
		return err
	}
	if st.LeaderID != 0 {
		if _, err := tx.Exec("UPDATE items SET status = 'ended', winner_id = ?, final_price = ? WHERE id = ?", st.LeaderID, st.Price, it.ID); err != nil {
			return err
		}
		if err := notify(st.LeaderID, "won", st.Price); err != nil {
			return err
		}
		if err := notify(it.SellerID, "sold", st.Price); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec("UPDATE items SET status = 'ended' WHERE id = ?", it.ID); err != nil {
			return err
		}
	}

	// ウォッチしていた人と、落札できなかった入札者に終了を知らせる
	var watchers []int64
	if err := tx.Select(&watchers, "SELECT user_id FROM watches WHERE item_id = ?", it.ID); err != nil {
		return err
	}
	notified := map[int64]bool{st.LeaderID: true, it.SellerID: true}
	targets := append(watchers, keys(st.MaxByUser)...)
	for _, uid := range targets {
		if notified[uid] {
			continue
		}
		notified[uid] = true
		if err := notify(uid, "ended", nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func keys(m map[int64]int) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
