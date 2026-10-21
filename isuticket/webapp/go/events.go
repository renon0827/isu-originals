package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

const eventsPerPage = 20

type Event struct {
	ID           int64        `db:"id"`
	VenueID      int64        `db:"venue_id"`
	Title        string       `db:"title"`
	Artist       string       `db:"artist"`
	Description  string       `db:"description"`
	SaleType     string       `db:"sale_type"`
	SaleStartAt  time.Time    `db:"sale_start_at"`
	SaleEndAt    time.Time    `db:"sale_end_at"`
	AnnounceAt   sql.NullTime `db:"announce_at"`
	LotteryDrawn bool         `db:"lottery_drawn"`
	GateOpenAt   time.Time    `db:"gate_open_at"`
	EndAt        time.Time    `db:"end_at"`
	Image        []byte       `db:"image"`
	CreatedAt    time.Time    `db:"created_at"`
}

type Venue struct {
	ID     int64           `db:"id" json:"id"`
	Name   string          `db:"name" json:"name"`
	Layout json.RawMessage `db:"layout" json:"layout,omitempty"`
}

type Section struct {
	Name string `json:"name"`
	Rank string `json:"rank"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type RankJSON struct {
	Rank      string `json:"rank"`
	Price     int    `json:"price"`
	Remaining int    `json:"remaining"`
}

type EventJSON struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Artist      string     `json:"artist"`
	Description string     `json:"description,omitempty"`
	Venue       Venue      `json:"venue"`
	SaleType    string     `json:"sale_type"`
	SaleStartAt time.Time  `json:"sale_start_at"`
	SaleEndAt   time.Time  `json:"sale_end_at"`
	AnnounceAt  *time.Time `json:"announce_at"`
	GateOpenAt  time.Time  `json:"gate_open_at"`
	EndAt       time.Time  `json:"end_at"`
	Ranks       []RankJSON `json:"ranks"`
}

// eventColumns は画像を除いた events の列。
const eventColumns = "id, venue_id, title, artist, description, sale_type, sale_start_at, sale_end_at, announce_at, lottery_drawn, gate_open_at, end_at, created_at"

func getEventByID(q queryer, id int64) (*Event, error) {
	var e Event
	if err := q.Get(&e, "SELECT "+eventColumns+" FROM events WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &e, nil
}

// fillEventJSON は会場情報とランク別の価格・残席数を付けて EventJSON を組み立てる。
func fillEventJSON(e *Event, withDescription bool) (*EventJSON, error) {
	var v Venue
	if err := db.Get(&v, "SELECT id, name FROM venues WHERE id = ?", e.VenueID); err != nil {
		return nil, err
	}

	var prices []struct {
		Rank  string `db:"rank"`
		Price int    `db:"price"`
	}
	if err := db.Select(&prices, "SELECT `rank`, price FROM event_ranks WHERE event_id = ? ORDER BY `rank` DESC", e.ID); err != nil {
		return nil, err
	}

	ranks := make([]RankJSON, 0, len(prices))
	for _, p := range prices {
		var remaining int
		if err := db.Get(&remaining,
			"SELECT COUNT(*) FROM event_seats WHERE event_id = ? AND `rank` = ? AND status = 'available'",
			e.ID, p.Rank); err != nil {
			return nil, err
		}
		ranks = append(ranks, RankJSON{Rank: p.Rank, Price: p.Price, Remaining: remaining})
	}
	// S, A, B の順に並べる
	order := map[string]int{"S": 0, "A": 1, "B": 2}
	for i := 0; i < len(ranks); i++ {
		for j := i + 1; j < len(ranks); j++ {
			if order[ranks[j].Rank] < order[ranks[i].Rank] {
				ranks[i], ranks[j] = ranks[j], ranks[i]
			}
		}
	}

	ej := &EventJSON{
		ID:          e.ID,
		Title:       e.Title,
		Artist:      e.Artist,
		Venue:       v,
		SaleType:    e.SaleType,
		SaleStartAt: e.SaleStartAt,
		SaleEndAt:   e.SaleEndAt,
		GateOpenAt:  e.GateOpenAt,
		EndAt:       e.EndAt,
		Ranks:       ranks,
	}
	if e.AnnounceAt.Valid {
		t := e.AnnounceAt.Time
		ej.AnnounceAt = &t
	}
	if withDescription {
		ej.Description = e.Description
	}
	return ej, nil
}

func getEvents(c echo.Context) error {
	status := c.QueryParam("status")
	if status == "" {
		status = "on_sale"
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	if page < 1 {
		page = 1
	}
	now := time.Now()

	var events []Event
	var err error
	switch status {
	case "on_sale":
		err = db.Select(&events, "SELECT "+eventColumns+" FROM events WHERE sale_start_at <= ? AND ? < sale_end_at ORDER BY created_at DESC, id DESC", now, now)
	case "upcoming":
		err = db.Select(&events, "SELECT "+eventColumns+" FROM events WHERE ? < sale_start_at ORDER BY sale_start_at ASC, id ASC", now)
	case "ended":
		err = db.Select(&events, "SELECT "+eventColumns+" FROM events WHERE end_at <= ? ORDER BY end_at DESC, id DESC", now)
	default:
		return errorJSON(c, http.StatusBadRequest, "invalid status")
	}
	if err != nil {
		return err
	}

	// ページング
	start := (page - 1) * eventsPerPage
	if start > len(events) {
		start = len(events)
	}
	end := start + eventsPerPage
	if end > len(events) {
		end = len(events)
	}

	res := make([]*EventJSON, 0, eventsPerPage)
	for i := range events[start:end] {
		ej, err := fillEventJSON(&events[start+i], false)
		if err != nil {
			return err
		}
		res = append(res, ej)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"events":   res,
		"page":     page,
		"has_next": end < len(events),
	})
}

func getEvent(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	e, err := getEventByID(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errorJSON(c, http.StatusNotFound, "event not found")
	} else if err != nil {
		return err
	}
	ej, err := fillEventJSON(e, true)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, ej)
}

func getEventImage(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var image []byte
	if err := db.Get(&image, "SELECT image FROM events WHERE id = ?", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "event not found")
		}
		return err
	}
	return c.Blob(http.StatusOK, "image/jpeg", image)
}

type SeatJSON struct {
	ID     int64  `json:"id"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Status string `json:"status"` // available / sold / mine
}

type SectionJSON struct {
	Name  string     `json:"name"`
	Rank  string     `json:"rank"`
	Rows  int        `json:"rows"`
	Cols  int        `json:"cols"`
	Seats []SeatJSON `json:"seats"`
}

type EventSeat struct {
	ID        int64         `db:"id"`
	EventID   int64         `db:"event_id"`
	Section   string        `db:"section"`
	RowNo     int           `db:"row_no"`
	ColNo     int           `db:"col_no"`
	Rank      string        `db:"rank"`
	Status    string        `db:"status"`
	TicketID  sql.NullInt64 `db:"ticket_id"`
	UpdatedAt time.Time     `db:"updated_at"`
}

func getSeatmap(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	e, err := getEventByID(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errorJSON(c, http.StatusNotFound, "event not found")
	} else if err != nil {
		return err
	}
	// ログインしていれば自分の座席を "mine" で返す
	var userID int64
	if u, err := currentUser(c); err == nil {
		userID = u.ID
	}

	var venue Venue
	if err := db.Get(&venue, "SELECT * FROM venues WHERE id = ?", e.VenueID); err != nil {
		return err
	}
	var sections []Section
	if err := json.Unmarshal(venue.Layout, &sections); err != nil {
		return err
	}

	var seats []EventSeat
	if err := db.Select(&seats, "SELECT * FROM event_seats WHERE event_id = ? ORDER BY id", id); err != nil {
		return err
	}

	res := make([]SectionJSON, 0, len(sections))
	for _, sec := range sections {
		sj := SectionJSON{Name: sec.Name, Rank: sec.Rank, Rows: sec.Rows, Cols: sec.Cols, Seats: []SeatJSON{}}
		for _, s := range seats {
			if s.Section != sec.Name {
				continue
			}
			status := s.Status
			if s.Status == "sold" && userID != 0 && s.TicketID.Valid {
				var owner int64
				if err := db.Get(&owner, "SELECT user_id FROM tickets WHERE id = ?", s.TicketID.Int64); err != nil {
					return err
				}
				if owner == userID {
					status = "mine"
				}
			}
			sj.Seats = append(sj.Seats, SeatJSON{ID: s.ID, Row: s.RowNo, Col: s.ColNo, Status: status})
		}
		res = append(res, sj)
	}
	return c.JSON(http.StatusOK, map[string]any{"event_id": id, "sections": res})
}
