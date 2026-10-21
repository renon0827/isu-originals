package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"time"
)

func newGzipReader(r io.Reader) (*gzip.Reader, error) { return gzip.NewReader(r) }

// ---- API の JSON ----

type UserJSON struct {
	ID          int64  `json:"id"`
	LoginName   string `json:"login_name"`
	DisplayName string `json:"display_name"`
	IsOrganizer bool   `json:"is_organizer"`
}

type RankJSON struct {
	Rank      string `json:"rank"`
	Price     int    `json:"price"`
	Remaining int    `json:"remaining"`
}

type VenueJSON struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Layout []SectionSpec `json:"layout,omitempty"`
}

type SectionSpec struct {
	Name string `json:"name"`
	Rank string `json:"rank"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type EventJSON struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Artist      string     `json:"artist"`
	Description string     `json:"description"`
	Venue       VenueJSON  `json:"venue"`
	SaleType    string     `json:"sale_type"`
	SaleStartAt time.Time  `json:"sale_start_at"`
	SaleEndAt   time.Time  `json:"sale_end_at"`
	AnnounceAt  *time.Time `json:"announce_at"`
	GateOpenAt  time.Time  `json:"gate_open_at"`
	EndAt       time.Time  `json:"end_at"`
	Ranks       []RankJSON `json:"ranks"`
}

type EventsResponse struct {
	Events  []EventJSON `json:"events"`
	Page    int         `json:"page"`
	HasNext bool        `json:"has_next"`
}

type SeatJSON struct {
	ID     int64  `json:"id"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Status string `json:"status"`
}

type SectionJSON struct {
	Name  string     `json:"name"`
	Rank  string     `json:"rank"`
	Rows  int        `json:"rows"`
	Cols  int        `json:"cols"`
	Seats []SeatJSON `json:"seats"`
}

type SeatmapResponse struct {
	EventID  int64         `json:"event_id"`
	Sections []SectionJSON `json:"sections"`
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

type LotteryEntryJSON struct {
	ID         int64     `json:"id"`
	EventID    int64     `json:"event_id"`
	EventTitle string    `json:"event_title"`
	Rank       string    `json:"rank"`
	Quantity   int       `json:"quantity"`
	Status     string    `json:"status"`
	AnnounceAt time.Time `json:"announce_at"`
}

type SeatInfoJSON struct {
	ID      int64  `json:"id"`
	Section string `json:"section"`
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	Rank    string `json:"rank"`
}

type TicketJSON struct {
	ID         int64        `json:"id"`
	EventID    int64        `json:"event_id"`
	EventTitle string       `json:"event_title"`
	GateOpenAt time.Time    `json:"gate_open_at"`
	EndAt      time.Time    `json:"end_at"`
	Seat       SeatInfoJSON `json:"seat"`
	Price      int          `json:"price"`
	Status     string       `json:"status"`
	UsedAt     *time.Time   `json:"used_at"`
	QRToken    string       `json:"qr_token"`
}

type ResaleListingJSON struct {
	ID        int64        `json:"id"`
	Price     int          `json:"price"`
	Seat      SeatInfoJSON `json:"seat"`
	CreatedAt time.Time    `json:"created_at"`
}

type ResalePurchaseResponse struct {
	OrderID   int64  `json:"order_id"`
	TicketID  int64  `json:"ticket_id"`
	Total     int    `json:"total"`
	PaymentID string `json:"payment_id"`
}

type GateVerifyResponse struct {
	TicketID  int64        `json:"ticket_id"`
	EventID   int64        `json:"event_id"`
	Seat      SeatInfoJSON `json:"seat"`
	EnteredAt time.Time    `json:"entered_at"`
}

type SalesJSON struct {
	EventID     int64          `json:"event_id"`
	SoldSeats   map[string]int `json:"sold_seats"`
	Revenue     int            `json:"revenue"`
	ResaleCount int            `json:"resale_count"`
	ResaleFee   int            `json:"resale_fee"`
	Entered     int            `json:"entered"`
}

// ---- 初期データの前提（tools/datagen と一致させる） ----

const (
	numInitialFans     = 50000
	initialOnSaleFirst = 401
	initialOnSaleLast  = 470
	organizerLogin     = "organizer"
	organizerPassword  = "organizer-pass"
)

func fanLogin(i int) string           { return fmt.Sprintf("fan%05d", i) }
func fanPassword(login string) string { return "isu-" + login }
