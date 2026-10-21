package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/labstack/echo/v4"
)

// 鍵はプロビジョニング時に生成される。
func keyDir() string { return getEnv("ISUTICKET_KEY_DIR", "/home/isucon/keys") }

func loadPrivateKey() (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(keyDir() + "/qr_private.pem")
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("invalid private key pem")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

func loadPublicKey() (*rsa.PublicKey, error) {
	b, err := os.ReadFile(keyDir() + "/qr_public.pem")
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("invalid public key pem")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return pk, nil
}

func getPublicKey(c echo.Context) error {
	b, err := os.ReadFile(keyDir() + "/qr_public.pem")
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/x-pem-file", b)
}

// QRPayload は QR トークンに埋め込む内容。
// トークン形式: base64url(payload JSON) + "." + base64url(RSA-PKCS1v15-SHA256 署名)
type QRPayload struct {
	EventID int64  `json:"eid"`
	Serial  string `json:"sn"`
}

func signQR(p QRPayload) (string, error) {
	key, err := loadPrivateKey()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(encoded))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return encoded + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

type Ticket struct {
	ID          int64        `db:"id"`
	OrderID     int64        `db:"order_id"`
	EventID     int64        `db:"event_id"`
	EventSeatID int64        `db:"event_seat_id"`
	UserID      int64        `db:"user_id"`
	Serial      string       `db:"serial"`
	Status      string       `db:"status"`
	UsedAt      sql.NullTime `db:"used_at"`
	CreatedAt   time.Time    `db:"created_at"`
}

type SeatInfoJSON struct {
	ID      int64  `json:"id"`
	Section string `json:"section"`
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	Rank    string `json:"rank"`
}

type MyTicketJSON struct {
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

// getMyTickets は終演前の公演のチケットを返す。
func getMyTickets(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}

	var tickets []Ticket
	if err := db.Select(&tickets, "SELECT * FROM tickets WHERE user_id = ? ORDER BY id", u.ID); err != nil {
		return err
	}

	now := time.Now()
	res := make([]MyTicketJSON, 0, len(tickets))
	for _, t := range tickets {
		e, err := getEventByID(db, t.EventID)
		if err != nil {
			return err
		}
		if !now.Before(e.EndAt) {
			continue
		}
		var s EventSeat
		if err := db.Get(&s, "SELECT * FROM event_seats WHERE id = ?", t.EventSeatID); err != nil {
			return err
		}
		var price int
		if err := db.Get(&price, "SELECT price FROM event_ranks WHERE event_id = ? AND `rank` = ?", t.EventID, s.Rank); err != nil {
			return err
		}
		token, err := signQR(QRPayload{EventID: t.EventID, Serial: t.Serial})
		if err != nil {
			return fmt.Errorf("sign qr: %w", err)
		}
		tj := MyTicketJSON{
			ID:         t.ID,
			EventID:    e.ID,
			EventTitle: e.Title,
			GateOpenAt: e.GateOpenAt,
			EndAt:      e.EndAt,
			Seat:       SeatInfoJSON{ID: s.ID, Section: s.Section, Row: s.RowNo, Col: s.ColNo, Rank: s.Rank},
			Price:      price,
			Status:     t.Status,
			QRToken:    token,
		}
		if t.UsedAt.Valid {
			ut := t.UsedAt.Time
			tj.UsedAt = &ut
		}
		res = append(res, tj)
	}
	return c.JSON(http.StatusOK, map[string]any{"tickets": res})
}
