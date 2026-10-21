package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

func getAdminVenues(c echo.Context) error {
	u, err := requireOrganizer(c)
	if u == nil {
		return err
	}
	var venues []Venue
	if err := db.Select(&venues, "SELECT * FROM venues ORDER BY id"); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"venues": venues})
}

type AdminEventRequest struct {
	VenueID     int64          `json:"venue_id"`
	Title       string         `json:"title"`
	Artist      string         `json:"artist"`
	Description string         `json:"description"`
	SaleType    string         `json:"sale_type"`
	SaleStartAt time.Time      `json:"sale_start_at"`
	SaleEndAt   time.Time      `json:"sale_end_at"`
	AnnounceAt  *time.Time     `json:"announce_at"`
	GateOpenAt  time.Time      `json:"gate_open_at"`
	EndAt       time.Time      `json:"end_at"`
	Prices      map[string]int `json:"prices"`
	// base64 エンコードした JPEG
	Image string `json:"image"`
}

func postAdminEvent(c echo.Context) error {
	u, err := requireOrganizer(c)
	if u == nil {
		return err
	}
	var req AdminEventRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}
	if req.Title == "" || req.Artist == "" ||
		(req.SaleType != "first_come" && req.SaleType != "lottery") ||
		!req.SaleStartAt.Before(req.SaleEndAt) || !req.GateOpenAt.Before(req.EndAt) ||
		(req.SaleType == "lottery" && req.AnnounceAt == nil) {
		return errorJSON(c, http.StatusBadRequest, "invalid parameters")
	}
	image, err := base64.StdEncoding.DecodeString(req.Image)
	if err != nil || len(image) == 0 {
		return errorJSON(c, http.StatusBadRequest, "invalid image")
	}

	var venue Venue
	if err := db.Get(&venue, "SELECT * FROM venues WHERE id = ?", req.VenueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusBadRequest, "venue not found")
		}
		return err
	}
	var sections []Section
	if err := json.Unmarshal(venue.Layout, &sections); err != nil {
		return err
	}
	for _, sec := range sections {
		if req.Prices[sec.Rank] <= 0 {
			return errorJSON(c, http.StatusBadRequest, "price is required for every rank")
		}
	}

	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()
	var announce sql.NullTime
	if req.SaleType == "lottery" {
		announce = sql.NullTime{Time: *req.AnnounceAt, Valid: true}
	}
	res, err := tx.Exec(`INSERT INTO events
		(venue_id, title, artist, description, sale_type, sale_start_at, sale_end_at, announce_at, lottery_drawn, gate_open_at, end_at, image, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		venue.ID, req.Title, req.Artist, req.Description, req.SaleType, req.SaleStartAt, req.SaleEndAt, announce,
		req.GateOpenAt, req.EndAt, image, now)
	if err != nil {
		return err
	}
	eventID, _ := res.LastInsertId()

	added := map[string]bool{}
	for _, sec := range sections {
		if added[sec.Rank] {
			continue
		}
		added[sec.Rank] = true
		if _, err := tx.Exec("INSERT INTO event_ranks (event_id, `rank`, price) VALUES (?, ?, ?)", eventID, sec.Rank, req.Prices[sec.Rank]); err != nil {
			return err
		}
	}
	for _, sec := range sections {
		for r := 1; r <= sec.Rows; r++ {
			for col := 1; col <= sec.Cols; col++ {
				if _, err := tx.Exec("INSERT INTO event_seats (event_id, section, row_no, col_no, `rank`, status, updated_at) VALUES (?, ?, ?, ?, ?, 'available', ?)",
					eventID, sec.Name, r, col, sec.Rank, now); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, map[string]any{"id": eventID})
}

type SalesJSON struct {
	EventID     int64          `json:"event_id"`
	SoldSeats   map[string]int `json:"sold_seats"`
	Revenue     int            `json:"revenue"`
	ResaleCount int            `json:"resale_count"`
	ResaleFee   int            `json:"resale_fee"`
	Entered     int            `json:"entered"`
}

const resaleFeeRate = 10 // %

func getAdminSales(c echo.Context) error {
	u, err := requireOrganizer(c)
	if u == nil {
		return err
	}
	eventID, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	if _, err := getEventByID(db, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "event not found")
		}
		return err
	}

	sales := SalesJSON{EventID: eventID, SoldSeats: map[string]int{"S": 0, "A": 0, "B": 0}}

	var tickets []Ticket
	if err := db.Select(&tickets, "SELECT * FROM tickets WHERE event_id = ?", eventID); err != nil {
		return err
	}
	for _, t := range tickets {
		var rank string
		if err := db.Get(&rank, "SELECT `rank` FROM event_seats WHERE id = ?", t.EventSeatID); err != nil {
			return err
		}
		sales.SoldSeats[rank]++
		if t.UsedAt.Valid {
			sales.Entered++
		}
	}

	var orders []struct {
		Kind  string `db:"kind"`
		Total int    `db:"total"`
	}
	if err := db.Select(&orders, "SELECT kind, total FROM orders WHERE event_id = ?", eventID); err != nil {
		return err
	}
	for _, o := range orders {
		if o.Kind == "resale" {
			sales.ResaleCount++
			sales.ResaleFee += o.Total * resaleFeeRate / 100
			continue
		}
		sales.Revenue += o.Total
	}
	return c.JSON(http.StatusOK, sales)
}
