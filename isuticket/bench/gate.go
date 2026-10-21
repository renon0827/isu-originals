package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// Gate は入場ゲート端末（主催者セッション）のプール。
type Gate struct {
	st     *State
	agents []*Agent
	next   atomic.Int64

	mu      sync.Mutex
	entered map[string]bool // 入場に成功した QR
}

func NewGate(ctx context.Context, st *State, n int) (*Gate, error) {
	g := &Gate{st: st, entered: map[string]bool{}}
	for i := 0; i < n; i++ {
		a := NewAgent(st.cfg.Target, st.cfg.HostHeader, st.cfg.RequestTimeout)
		if err := loginOrganizer(ctx, a); err != nil {
			return nil, err
		}
		g.agents = append(g.agents, a)
	}
	return g, nil
}

func loginOrganizer(ctx context.Context, a *Agent) error {
	res, err := a.Do(ctx, http.MethodPost, "/api/login", map[string]string{"login_name": organizerLogin, "password": organizerPassword})
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return fmt.Errorf("主催者でログインできません (status %d)", res.Status)
	}
	return nil
}

func (g *Gate) agent() *Agent {
	return g.agents[int(g.next.Add(1))%len(g.agents)]
}

func (g *Gate) verify(ctx context.Context, qr string) (*Response, error) {
	res, err := g.agent().Do(ctx, http.MethodPost, "/api/gate/verify", map[string]string{"qr_token": qr})
	g.st.stats.record(res, err)
	if err != nil && ctx.Err() == nil {
		g.st.score.Penalize("タイムアウト", 5)
	}
	return res, err
}

// Verify はファンのチケットで入場し、ときどき不正な QR でも試して拒否されることを確認する。
func (g *Gate) Verify(ctx context.Context, f *Fan, t *OwnedTicket) error {
	// 改ざんした QR は 400 で拒否されること
	if f.rng.Intn(50) == 0 {
		res, err := g.verify(ctx, tamper(t.QR))
		if err != nil {
			return err
		}
		if res.Status == http.StatusOK {
			g.st.score.Fail("改ざんした QR トークンで入場できました")
			return nil
		}
		expectStatus(g.st, res, "POST /api/gate/verify (改ざん)", http.StatusBadRequest)
	}

	res, err := g.verify(ctx, t.QR)
	if err != nil {
		return err
	}
	if res.Status == http.StatusForbidden {
		// 入場開始の境界、またはリセールで売れた直後
		return nil
	}
	if !expectStatus(g.st, res, "POST /api/gate/verify", http.StatusOK) {
		return nil
	}
	vr, err := decode[GateVerifyResponse](res)
	if err != nil || vr.TicketID != t.ID || vr.Seat.ID != t.SeatID {
		g.st.score.Fail("入場結果のチケット・座席が一致しません (ticket %d)", t.ID)
		return nil
	}
	g.mu.Lock()
	dup := g.entered[t.QR]
	g.entered[t.QR] = true
	g.mu.Unlock()
	if dup {
		g.st.score.Fail("同じ QR トークンで二重に入場できました (ticket %d)", t.ID)
		return nil
	}
	f.mu.Lock()
	t.Entered = true
	f.mu.Unlock()
	g.st.score.Inc(&g.st.score.Entries)
	f.satisfied()

	// 同じ QR で再入場しようとしたら 409 で拒否されること
	if f.rng.Intn(20) == 0 {
		res, err := g.verify(ctx, t.QR)
		if err != nil {
			return err
		}
		if res.Status == http.StatusOK {
			g.st.score.Fail("同じ QR トークンで二重に入場できました (ticket %d)", t.ID)
			return nil
		}
		expectStatus(g.st, res, "POST /api/gate/verify (再入場)", http.StatusConflict)
	}
	return nil
}

// VerifyOldQR はリセールで手放したチケットの古い QR が拒否されることを確認する。
func (g *Gate) VerifyOldQR(ctx context.Context, l *Listing) error {
	if l.OldQR == "" {
		return nil
	}
	res, err := g.verify(ctx, l.OldQR)
	if err != nil {
		return err
	}
	if res.Status == http.StatusOK {
		g.st.score.Fail("リセールで譲渡済みのチケットの古い QR で入場できました (ticket %d)", l.TicketID)
		return nil
	}
	expectStatus(g.st, res, "POST /api/gate/verify (譲渡済み)", http.StatusForbidden)
	return nil
}

func tamper(qr string) string {
	b := []byte(qr)
	if len(b) < 4 {
		return qr + "x"
	}
	i := len(b) / 4
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}
