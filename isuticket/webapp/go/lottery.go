package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

type LotteryEntry struct {
	ID        int64     `db:"id"`
	EventID   int64     `db:"event_id"`
	UserID    int64     `db:"user_id"`
	Rank      string    `db:"rank"`
	Quantity  int       `db:"quantity"`
	Status    string    `db:"status"`
	CreatedAt time.Time `db:"created_at"`
}

type LotteryEntryRequest struct {
	Rank     string `json:"rank"`
	Quantity int    `json:"quantity"`
}

func postLotteryEntry(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	eventID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var req LotteryEntryRequest
	if err := c.Bind(&req); err != nil || req.Quantity < 1 || req.Quantity > 4 {
		return errorJSON(c, http.StatusBadRequest, "quantity must be 1 to 4")
	}

	e, err := getEventByID(db, eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return errorJSON(c, http.StatusNotFound, "event not found")
	} else if err != nil {
		return err
	}
	now := time.Now()
	if e.SaleType != "lottery" {
		return errorJSON(c, http.StatusForbidden, "this event is not lottery")
	}
	if now.Before(e.SaleStartAt) || !now.Before(e.SaleEndAt) {
		return errorJSON(c, http.StatusForbidden, "entry is not open")
	}
	var rankCount int
	if err := db.Get(&rankCount, "SELECT COUNT(*) FROM event_ranks WHERE event_id = ? AND `rank` = ?", eventID, req.Rank); err != nil {
		return err
	}
	if rankCount == 0 {
		return errorJSON(c, http.StatusBadRequest, "invalid rank")
	}

	var entered int
	if err := db.Get(&entered, "SELECT COUNT(*) FROM lottery_entries WHERE event_id = ? AND user_id = ?", eventID, u.ID); err != nil {
		return err
	}
	if entered > 0 {
		return errorJSON(c, http.StatusConflict, "already entered")
	}

	res, err := db.Exec("INSERT INTO lottery_entries (event_id, user_id, `rank`, quantity, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)",
		eventID, u.ID, req.Rank, req.Quantity, now)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

// ensureDrawn は発表時刻を過ぎていて未抽選の公演の抽選を行う。
func ensureDrawn(eventID int64) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var e Event
	if err := tx.Get(&e, "SELECT "+eventColumns+" FROM events WHERE id = ? FOR UPDATE", eventID); err != nil {
		return err
	}
	if e.LotteryDrawn || !e.AnnounceAt.Valid || time.Now().Before(e.AnnounceAt.Time) {
		return nil
	}

	var entries []LotteryEntry
	if err := tx.Select(&entries, "SELECT * FROM lottery_entries WHERE event_id = ? AND status = 'pending'", eventID); err != nil {
		return err
	}
	rand.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

	// ランクごとの空席を席番号順に用意しておき、当選者に先頭から割り当てる
	available := map[string][]EventSeat{}
	prices := map[string]int{}
	for _, rank := range []string{"S", "A", "B"} {
		var seats []EventSeat
		if err := tx.Select(&seats,
			"SELECT * FROM event_seats WHERE event_id = ? AND `rank` = ? AND status = 'available' ORDER BY id",
			eventID, rank); err != nil {
			return err
		}
		available[rank] = seats
		var price int
		if err := tx.Get(&price, "SELECT price FROM event_ranks WHERE event_id = ? AND `rank` = ?", eventID, rank); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		prices[rank] = price
	}

	now := time.Now()
	for _, entry := range entries {
		seats := available[entry.Rank]
		if len(seats) < entry.Quantity {
			if _, err := tx.Exec("UPDATE lottery_entries SET status = 'lost' WHERE id = ?", entry.ID); err != nil {
				return err
			}
			continue
		}
		won := seats[:entry.Quantity]
		available[entry.Rank] = seats[entry.Quantity:]
		for _, s := range won {
			var status string
			if err := tx.Get(&status, "SELECT status FROM event_seats WHERE id = ? FOR UPDATE", s.ID); err != nil {
				return err
			}
			if status != "available" {
				return fmt.Errorf("seat %d is not available", s.ID)
			}
		}
		if _, _, err := issueTickets(tx, entry.UserID, eventID, "lottery", prices[entry.Rank]*entry.Quantity, sql.NullString{}, won, now); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE lottery_entries SET status = 'won' WHERE id = ?", entry.ID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec("UPDATE events SET lottery_drawn = 1 WHERE id = ?", eventID); err != nil {
		return err
	}
	return tx.Commit()
}

type MyLotteryEntryJSON struct {
	ID         int64     `json:"id"`
	EventID    int64     `json:"event_id"`
	EventTitle string    `json:"event_title"`
	Rank       string    `json:"rank"`
	Quantity   int       `json:"quantity"`
	Status     string    `json:"status"`
	AnnounceAt time.Time `json:"announce_at"`
}

func getMyLottery(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}

	var entries []LotteryEntry
	if err := db.Select(&entries, "SELECT * FROM lottery_entries WHERE user_id = ? ORDER BY id DESC", u.ID); err != nil {
		return err
	}

	now := time.Now()
	res := make([]MyLotteryEntryJSON, 0, len(entries))
	for i := range entries {
		entry := &entries[i]
		e, err := getEventByID(db, entry.EventID)
		if err != nil {
			return err
		}
		if entry.Status == "pending" && e.AnnounceAt.Valid && !now.Before(e.AnnounceAt.Time) {
			if err := ensureDrawn(e.ID); err != nil {
				return err
			}
			if err := db.Get(&entry.Status, "SELECT status FROM lottery_entries WHERE id = ?", entry.ID); err != nil {
				return err
			}
		}
		res = append(res, MyLotteryEntryJSON{
			ID:         entry.ID,
			EventID:    e.ID,
			EventTitle: e.Title,
			Rank:       entry.Rank,
			Quantity:   entry.Quantity,
			Status:     entry.Status,
			AnnounceAt: e.AnnounceAt.Time,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": res})
}
