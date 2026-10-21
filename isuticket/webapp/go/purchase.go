package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

// ---- 外部決済 ----

var errPaymentFailed = errors.New("payment failed")

var paymentClient = &http.Client{Timeout: 2 * time.Second}

type paymentResponse struct {
	PaymentID string `json:"payment_id"`
}

// charge は決済サービスに課金を依頼する。
// 一時的な失敗は同じ冪等キーで再試行する（キーを変えると二重課金になる）。
func charge(q queryer, amount int) (string, error) {
	var paymentURL string
	if err := q.Get(&paymentURL, "SELECT value FROM settings WHERE name = 'payment_url'"); err != nil {
		return "", err
	}
	key := randomHex(32)
	body, _ := json.Marshal(map[string]int{"amount": amount})

	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		req, err := http.NewRequest(http.MethodPost, paymentURL+"/payments", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		res, err := paymentClient.Do(req)
		if err != nil {
			continue
		}
		var pr paymentResponse
		decodeErr := json.NewDecoder(res.Body).Decode(&pr)
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusOK && decodeErr == nil:
			return pr.PaymentID, nil
		case res.StatusCode >= 500:
			continue
		default:
			return "", fmt.Errorf("%w: status %d", errPaymentFailed, res.StatusCode)
		}
	}
	return "", errPaymentFailed
}

// ---- 先着購入 ----

type PurchaseRequest struct {
	SeatIDs []int64 `json:"seat_ids"`
}

type PurchasedTicket struct {
	ID     int64 `json:"id"`
	SeatID int64 `json:"seat_id"`
}

type PurchaseResponse struct {
	OrderID   int64             `json:"order_id"`
	Total     int               `json:"total"`
	PaymentID string            `json:"payment_id"`
	Tickets   []PurchasedTicket `json:"tickets"`
}

func postPurchase(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	eventID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var req PurchaseRequest
	if err := c.Bind(&req); err != nil || len(req.SeatIDs) == 0 || len(req.SeatIDs) > 4 {
		return errorJSON(c, http.StatusBadRequest, "seat_ids must contain 1 to 4 seats")
	}
	seen := map[int64]bool{}
	for _, id := range req.SeatIDs {
		if seen[id] {
			return errorJSON(c, http.StatusBadRequest, "duplicated seat_id")
		}
		seen[id] = true
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var e Event
	if err := tx.Get(&e, "SELECT "+eventColumns+" FROM events WHERE id = ? FOR UPDATE", eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "event not found")
		}
		return err
	}
	now := time.Now()
	if e.SaleType != "first_come" {
		return errorJSON(c, http.StatusForbidden, "this event is not first-come")
	}
	if now.Before(e.SaleStartAt) || !now.Before(e.SaleEndAt) {
		return errorJSON(c, http.StatusForbidden, "not on sale")
	}

	seats := make([]EventSeat, 0, len(req.SeatIDs))
	total := 0
	for _, sid := range req.SeatIDs {
		var s EventSeat
		if err := tx.Get(&s, "SELECT * FROM event_seats WHERE id = ?", sid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errorJSON(c, http.StatusBadRequest, "seat not found")
			}
			return err
		}
		if s.EventID != eventID {
			return errorJSON(c, http.StatusBadRequest, "seat not found")
		}
		if s.Status != "available" {
			return errorJSON(c, http.StatusConflict, "seat already sold")
		}
		var price int
		if err := tx.Get(&price, "SELECT price FROM event_ranks WHERE event_id = ? AND `rank` = ?", eventID, s.Rank); err != nil {
			return err
		}
		total += price
		seats = append(seats, s)
	}

	paymentID, err := charge(tx, total)
	if err != nil {
		c.Logger().Errorf("payment failed: %v", err)
		return errorJSON(c, http.StatusBadGateway, "payment failed")
	}

	orderID, tickets, err := issueTickets(tx, u.ID, eventID, "first_come", total, sql.NullString{String: paymentID, Valid: true}, seats, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return c.JSON(http.StatusCreated, PurchaseResponse{OrderID: orderID, Total: total, PaymentID: paymentID, Tickets: tickets})
}

// issueTickets は注文を作り、座席ごとにチケットを発行して座席を売約済みにする。
func issueTickets(tx *sqlx.Tx, userID, eventID int64, kind string, total int, paymentID sql.NullString, seats []EventSeat, now time.Time) (int64, []PurchasedTicket, error) {
	res, err := tx.Exec("INSERT INTO orders (user_id, event_id, kind, total, payment_id, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		userID, eventID, kind, total, paymentID, now)
	if err != nil {
		return 0, nil, err
	}
	orderID, _ := res.LastInsertId()

	tickets := make([]PurchasedTicket, 0, len(seats))
	for _, s := range seats {
		res, err := tx.Exec("INSERT INTO tickets (order_id, event_id, event_seat_id, user_id, serial, status, created_at) VALUES (?, ?, ?, ?, ?, 'valid', ?)",
			orderID, eventID, s.ID, userID, randomHex(32), now)
		if err != nil {
			return 0, nil, err
		}
		ticketID, _ := res.LastInsertId()
		if _, err := tx.Exec("UPDATE event_seats SET status = 'sold', ticket_id = ?, updated_at = ? WHERE id = ?", ticketID, now, s.ID); err != nil {
			return 0, nil, err
		}
		tickets = append(tickets, PurchasedTicket{ID: ticketID, SeatID: s.ID})
	}
	return orderID, tickets, nil
}
