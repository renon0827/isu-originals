package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

type GateVerifyRequest struct {
	QRToken string `json:"qr_token"`
}

type GateVerifyResponse struct {
	TicketID  int64        `json:"ticket_id"`
	EventID   int64        `json:"event_id"`
	Seat      SeatInfoJSON `json:"seat"`
	EnteredAt time.Time    `json:"entered_at"`
}

func verifyQR(token string) (*QRPayload, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	key, err := loadPublicKey()
	if err != nil {
		return nil, false
	}
	digest := sha256.Sum256([]byte(parts[0]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var p QRPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false
	}
	return &p, true
}

func postGateVerify(c echo.Context) error {
	u, err := requireOrganizer(c)
	if u == nil {
		return err
	}
	var req GateVerifyRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}
	p, ok := verifyQR(req.QRToken)
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid qr token")
	}

	var t Ticket
	if err := db.Get(&t, "SELECT * FROM tickets WHERE serial = ?", p.Serial); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// リセールで所有者が変わった古い QR など
			return errorJSON(c, http.StatusForbidden, "ticket is no longer valid")
		}
		return err
	}
	if t.EventID != p.EventID {
		return errorJSON(c, http.StatusForbidden, "ticket is no longer valid")
	}
	if t.Status != "valid" {
		return errorJSON(c, http.StatusForbidden, "ticket is listed for resale")
	}
	e, err := getEventByID(db, t.EventID)
	if err != nil {
		return err
	}
	now := time.Now()
	if now.Before(e.GateOpenAt) || !now.Before(e.EndAt) {
		return errorJSON(c, http.StatusForbidden, "gate is not open")
	}

	res, err := db.Exec("UPDATE tickets SET used_at = ? WHERE id = ? AND used_at IS NULL", now, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errorJSON(c, http.StatusConflict, "already entered")
	}

	var s EventSeat
	if err := db.Get(&s, "SELECT * FROM event_seats WHERE id = ?", t.EventSeatID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, GateVerifyResponse{
		TicketID:  t.ID,
		EventID:   t.EventID,
		Seat:      SeatInfoJSON{ID: s.ID, Section: s.Section, Row: s.RowNo, Col: s.ColNo, Rank: s.Rank},
		EnteredAt: now,
	})
}
