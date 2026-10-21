package main

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

type ResaleListing struct {
	ID        int64         `db:"id"`
	TicketID  int64         `db:"ticket_id"`
	EventID   int64         `db:"event_id"`
	SellerID  int64         `db:"seller_id"`
	Price     int           `db:"price"`
	Status    string        `db:"status"`
	BuyerID   sql.NullInt64 `db:"buyer_id"`
	CreatedAt time.Time     `db:"created_at"`
	SoldAt    sql.NullTime  `db:"sold_at"`
}

type ResaleRequest struct {
	Price int `json:"price"`
}

const minResalePrice = 1000

// lockResale は settings の専用行をロックする。
func lockResale(tx *sqlx.Tx) error {
	var v string
	return tx.Get(&v, "SELECT value FROM settings WHERE name = 'resale_lock' FOR UPDATE")
}

func postResale(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	ticketID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var req ResaleRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var t Ticket
	if err := tx.Get(&t, "SELECT * FROM tickets WHERE id = ? FOR UPDATE", ticketID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "ticket not found")
		}
		return err
	}
	if t.UserID != u.ID {
		return errorJSON(c, http.StatusNotFound, "ticket not found")
	}
	if t.Status != "valid" || t.UsedAt.Valid {
		return errorJSON(c, http.StatusConflict, "ticket cannot be listed")
	}
	e, err := getEventByID(tx, t.EventID)
	if err != nil {
		return err
	}
	now := time.Now()
	if !now.Before(e.GateOpenAt) {
		return errorJSON(c, http.StatusForbidden, "resale is closed")
	}
	var face int
	if err := tx.Get(&face,
		"SELECT r.price FROM event_seats s JOIN event_ranks r ON r.event_id = s.event_id AND r.`rank` = s.`rank` WHERE s.id = ?",
		t.EventSeatID); err != nil {
		return err
	}
	if req.Price < minResalePrice || req.Price > face {
		return errorJSON(c, http.StatusBadRequest, "price must be between 1000 and the face value")
	}

	res, err := tx.Exec("INSERT INTO resale_listings (ticket_id, event_id, seller_id, price, status, created_at) VALUES (?, ?, ?, ?, 'listed', ?)",
		t.ID, t.EventID, u.ID, req.Price, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE tickets SET status = 'listed' WHERE id = ?", t.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func deleteResale(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	listingID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockResale(tx); err != nil {
		return err
	}

	var l ResaleListing
	if err := tx.Get(&l, "SELECT * FROM resale_listings WHERE id = ?", listingID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "listing not found")
		}
		return err
	}
	if l.SellerID != u.ID {
		return errorJSON(c, http.StatusNotFound, "listing not found")
	}
	if l.Status != "listed" {
		return errorJSON(c, http.StatusConflict, "listing is not active")
	}
	if _, err := tx.Exec("UPDATE resale_listings SET status = 'cancelled' WHERE id = ?", l.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE tickets SET status = 'valid' WHERE id = ?", l.TicketID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

type ResaleListingJSON struct {
	ID        int64        `json:"id"`
	Price     int          `json:"price"`
	Seat      SeatInfoJSON `json:"seat"`
	CreatedAt time.Time    `json:"created_at"`
}

func getResales(c echo.Context) error {
	eventID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var listings []ResaleListing
	if err := db.Select(&listings, "SELECT * FROM resale_listings WHERE event_id = ? AND status = 'listed' ORDER BY price ASC, id ASC", eventID); err != nil {
		return err
	}
	res := make([]ResaleListingJSON, 0, len(listings))
	for _, l := range listings {
		var t Ticket
		if err := db.Get(&t, "SELECT * FROM tickets WHERE id = ?", l.TicketID); err != nil {
			return err
		}
		var s EventSeat
		if err := db.Get(&s, "SELECT * FROM event_seats WHERE id = ?", t.EventSeatID); err != nil {
			return err
		}
		res = append(res, ResaleListingJSON{
			ID:        l.ID,
			Price:     l.Price,
			Seat:      SeatInfoJSON{ID: s.ID, Section: s.Section, Row: s.RowNo, Col: s.ColNo, Rank: s.Rank},
			CreatedAt: l.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"listings": res})
}

type ResalePurchaseResponse struct {
	OrderID   int64  `json:"order_id"`
	TicketID  int64  `json:"ticket_id"`
	Total     int    `json:"total"`
	PaymentID string `json:"payment_id"`
}

func postResalePurchase(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	listingID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockResale(tx); err != nil {
		return err
	}

	var l ResaleListing
	if err := tx.Get(&l, "SELECT * FROM resale_listings WHERE id = ?", listingID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "listing not found")
		}
		return err
	}
	if l.Status != "listed" {
		return errorJSON(c, http.StatusConflict, "listing is not available")
	}
	if l.SellerID == u.ID {
		return errorJSON(c, http.StatusBadRequest, "cannot buy your own listing")
	}

	paymentID, err := charge(tx, l.Price)
	if err != nil {
		c.Logger().Errorf("payment failed: %v", err)
		return errorJSON(c, http.StatusBadGateway, "payment failed")
	}

	now := time.Now()
	if _, err := tx.Exec("UPDATE resale_listings SET status = 'sold', buyer_id = ?, sold_at = ? WHERE id = ?", u.ID, now, l.ID); err != nil {
		return err
	}
	// 所有者が変わるのでシリアルを振り直し、旧所有者の QR を無効にする
	if _, err := tx.Exec("UPDATE tickets SET user_id = ?, serial = ?, status = 'valid' WHERE id = ?", u.ID, randomHex(32), l.TicketID); err != nil {
		return err
	}
	res, err := tx.Exec("INSERT INTO orders (user_id, event_id, kind, total, payment_id, created_at) VALUES (?, ?, 'resale', ?, ?, ?)",
		u.ID, l.EventID, l.Price, paymentID, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	orderID, _ := res.LastInsertId()
	return c.JSON(http.StatusCreated, ResalePurchaseResponse{OrderID: orderID, TicketID: l.TicketID, Total: l.Price, PaymentID: paymentID})
}
