package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

func postWatch(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	if _, err := getItemByID(db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "item not found")
		}
		return err
	}
	if _, err := db.Exec("INSERT IGNORE INTO watches (user_id, item_id, created_at) VALUES (?, ?, ?)", u.ID, id, time.Now()); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func deleteWatch(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	if _, err := db.Exec("DELETE FROM watches WHERE user_id = ? AND item_id = ?", u.ID, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// getMyWatchlist はウォッチ中の商品（新しくウォッチした順に最大 50 件）。
func getMyWatchlist(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var ids []int64
	if err := db.Select(&ids, "SELECT item_id FROM watches WHERE user_id = ? ORDER BY created_at DESC LIMIT 50", u.ID); err != nil {
		return err
	}
	res := make([]*ItemJSON, 0, len(ids))
	for _, id := range ids {
		it, err := getItemByID(db, id)
		if err != nil {
			return err
		}
		j, err := buildItemJSON(it, false, nil)
		if err != nil {
			return err
		}
		res = append(res, j)
	}
	return c.JSON(http.StatusOK, map[string]any{"items": res})
}

type MyBidJSON struct {
	Item  *ItemJSON `json:"item"`
	MyMax int       `json:"my_max"`
	// leading / outbid / won / lost
	Result string `json:"result"`
}

// getMyBids は自分が入札した商品（最近入札した順に最大 20 件）。
func getMyBids(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var ids []int64
	if err := db.Select(&ids, "SELECT item_id FROM bids WHERE user_id = ? GROUP BY item_id ORDER BY MAX(id) DESC LIMIT 20", u.ID); err != nil {
		return err
	}
	res := make([]MyBidJSON, 0, len(ids))
	for _, id := range ids {
		it, err := getItemByID(db, id)
		if err != nil {
			return err
		}
		st, err := loadState(db, it)
		if err != nil {
			return err
		}
		j, err := buildItemJSON(it, false, nil)
		if err != nil {
			return err
		}
		result := "outbid"
		switch {
		case it.Status == "ended" && it.WinnerID.Valid && it.WinnerID.Int64 == u.ID:
			result = "won"
		case it.Status == "ended":
			result = "lost"
		case st.LeaderID == u.ID:
			result = "leading"
		}
		res = append(res, MyBidJSON{Item: j, MyMax: st.MaxByUser[u.ID], Result: result})
	}
	return c.JSON(http.StatusOK, map[string]any{"bids": res})
}

// getMyItems は自分の出品（新しい順に最大 50 件）。
func getMyItems(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var items []Item
	if err := db.Select(&items, "SELECT * FROM items WHERE seller_id = ? ORDER BY created_at DESC LIMIT 50", u.ID); err != nil {
		return err
	}
	res := make([]*ItemJSON, 0, len(items))
	for i := range items {
		j, err := buildItemJSON(&items[i], true, nil)
		if err != nil {
			return err
		}
		res = append(res, j)
	}
	return c.JSON(http.StatusOK, map[string]any{"items": res})
}

type Notification struct {
	ID        int64         `db:"id" json:"id"`
	UserID    int64         `db:"user_id" json:"-"`
	Type      string        `db:"type" json:"type"`
	ItemID    int64         `db:"item_id" json:"item_id"`
	Price     sql.NullInt64 `db:"price" json:"-"`
	CreatedAt time.Time     `db:"created_at" json:"created_at"`
}

type NotificationJSON struct {
	Notification
	ItemTitle string `json:"item_title"`
	Price     *int   `json:"price"`
}

// getMyNotifications は after_id より新しい通知（新しい順に最大 50 件）。
func getMyNotifications(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	after, _ := strconv.ParseInt(c.QueryParam("after_id"), 10, 64)
	var ns []Notification
	if err := db.Select(&ns, "SELECT * FROM notifications WHERE user_id = ? AND id > ? ORDER BY id DESC LIMIT 50", u.ID, after); err != nil {
		return err
	}
	res := make([]NotificationJSON, 0, len(ns))
	for _, n := range ns {
		var title string
		if err := db.Get(&title, "SELECT title FROM items WHERE id = ?", n.ItemID); err != nil {
			return err
		}
		nj := NotificationJSON{Notification: n, ItemTitle: title}
		if n.Price.Valid {
			p := int(n.Price.Int64)
			nj.Price = &p
		}
		res = append(res, nj)
	}
	return c.JSON(http.StatusOK, map[string]any{"notifications": res})
}
