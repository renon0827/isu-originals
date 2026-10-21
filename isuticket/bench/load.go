package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 直近 1 秒のリクエスト統計（ファン追加の判断に使う） ----

type Stats struct {
	requests atomic.Int64
	errors   atomic.Int64
	slow     atomic.Int64
	total    atomic.Int64
}

func (s *Stats) record(res *Response, err error) {
	s.total.Add(1)
	s.requests.Add(1)
	if err != nil || (res != nil && res.Status >= 500) {
		s.errors.Add(1)
		return
	}
	if res != nil && !res.FromCache && res.Latency > time.Second {
		s.slow.Add(1)
	}
}

func (s *Stats) swap() (req, errs, slow int64) {
	return s.requests.Swap(0), s.errors.Swap(0), s.slow.Swap(0)
}

// ---- 走行用公演の作成 ----

// prepareEvents は走行用の公演（先着 4、抽選 2）を作成する。時刻はすべて T0 からの相対。
func (st *State) prepareEvents(ctx context.Context, org *Agent) error {
	res, err := org.Do(ctx, http.MethodGet, "/api/admin/venues", nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusOK {
		return fmt.Errorf("GET /api/admin/venues: status %d", res.Status)
	}
	vr, err := decode[struct {
		Venues []VenueJSON `json:"venues"`
	}](res)
	if err != nil || len(vr.Venues) == 0 {
		return fmt.Errorf("GET /api/admin/venues: invalid response")
	}
	// 先着は大きい会場、抽選は中くらいの会場
	sortVenuesBySize(vr.Venues)
	big := vr.Venues[len(vr.Venues)-4:]
	mid := vr.Venues[len(vr.Venues)/2 : len(vr.Venues)/2+2]

	t0 := st.T0
	type plan struct {
		venue    VenueJSON
		saleType string
		start    time.Duration
	}
	var plans []plan
	for i, off := range []time.Duration{3, 8, 14, 20} {
		plans = append(plans, plan{big[i], "first_come", off * time.Second})
	}
	for i := 0; i < 2; i++ {
		plans = append(plans, plan{mid[i], "lottery", 0})
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, p := range plans {
		wg.Add(1)
		go func(i int, p plan) {
			defer wg.Done()
			be := &BenchEvent{
				SaleType:   p.saleType,
				SaleStart:  t0.Add(p.start),
				SaleEnd:    t0.Add(10 * time.Minute),
				GateOpen:   t0.Add(st.cfg.GateOpenOffset),
				EndAt:      t0.Add(10 * time.Minute),
				Prices:     map[string]int{"S": 15000 + 1000*i, "A": 9800, "B": 6500},
				SeatsTotal: map[string]int{},
			}
			if p.saleType == "lottery" {
				be.SaleEnd = t0.Add(st.cfg.LotteryEntryEnd)
				be.AnnounceAt = t0.Add(st.cfg.LotteryAnnounce)
			}
			for _, sec := range p.venue.Layout {
				be.SeatsTotal[sec.Rank] += sec.Rows * sec.Cols
			}
			img := benchImage(i)
			req := map[string]any{
				"venue_id":      p.venue.ID,
				"title":         fmt.Sprintf("ISUCON ベンチマーク記念公演 %d", i+1),
				"artist":        "ベンチマーカーズ",
				"description":   "走行中だけ開催される幻の公演。",
				"sale_type":     p.saleType,
				"sale_start_at": be.SaleStart,
				"sale_end_at":   be.SaleEnd,
				"gate_open_at":  be.GateOpen,
				"end_at":        be.EndAt,
				"prices":        be.Prices,
				"image":         base64.StdEncoding.EncodeToString(img),
			}
			if p.saleType == "lottery" {
				req["announce_at"] = be.AnnounceAt
			}
			res, err := org.Do(ctx, http.MethodPost, "/api/admin/events", req)
			if err == nil && res.Status != http.StatusCreated {
				err = fmt.Errorf("POST /api/admin/events: status %d: %s", res.Status, truncate(res.Body, 200))
			}
			var body struct {
				ID int64 `json:"id"`
			}
			if err == nil {
				body, err = decode[struct {
					ID int64 `json:"id"`
				}](res)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			be.ID = body.ID
			st.events[be.ID] = be
			st.prices.Store(be.ID, be.Prices)
			if p.saleType == "lottery" {
				st.lotEvents = append(st.lotEvents, be)
			} else {
				st.fcEvents = append(st.fcEvents, be)
			}
		}(i, p)
	}
	wg.Wait()
	return firstErr
}

func sortVenuesBySize(vs []VenueJSON) {
	size := func(v VenueJSON) int {
		n := 0
		for _, s := range v.Layout {
			n += s.Rows * s.Cols
		}
		return n
	}
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && size(vs[j]) < size(vs[j-1]); j-- {
			vs[j], vs[j-1] = vs[j-1], vs[j]
		}
	}
}

// ---- 負荷走行 ----

type Load struct {
	st   *State
	gate *Gate
	org  *Agent

	wg     sync.WaitGroup
	active atomic.Int64
	left   atomic.Int64

	soldOut sync.Map // eventID → true

	fansMu sync.Mutex
	fans   []*Fan
}

func (l *Load) spawn(ctx context.Context, n int) { l.spawnRole(ctx, n, -1) }

// spawnRole は role を指定してファンを追加する（-1 ならランダム）。
func (l *Load) spawnRole(ctx context.Context, n int, role FanRole) {
	for i := 0; i < n; i++ {
		f := l.st.newFan()
		if role >= 0 {
			f.role = role
		}
		l.fansMu.Lock()
		l.fans = append(l.fans, f)
		l.fansMu.Unlock()
		l.active.Add(1)
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer l.active.Add(-1)
			l.runFan(ctx, f)
			if f.left {
				l.left.Add(1)
			}
		}()
	}
}

func (l *Load) Run(ctx context.Context) {
	l.spawn(ctx, l.st.cfg.InitialFans)

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.runOrganizer(ctx)
	}()

	// 1 秒ごとに直近の応答状況を見てファンを増やす
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return
		case <-ticker.C:
		}
		if failed, _ := l.st.score.Failed(); failed {
			continue
		}
		req, errs, slow := l.st.stats.swap()
		active := l.active.Load()
		add := 0
		switch {
		case req == 0:
		case errs == 0 && slow*100 <= req:
			add = int(active / 8)
			if add < 4 {
				add = 4
			}
		case errs*100 <= req && slow*20 <= req:
			add = 1
		}
		if int(active)+add > l.st.cfg.MaxFans {
			add = l.st.cfg.MaxFans - int(active)
		}
		if add > 0 {
			l.spawn(ctx, add)
		}
		// 抽選の申込期間中は、応答が良いほど申込者が殺到する
		now := time.Now()
		rush := 0
		if now.Before(l.st.T0.Add(l.st.cfg.LotteryEntryEnd-3*time.Second)) && req > 0 && errs == 0 && slow*100 <= req {
			rush = 2 + int(active/20)
			l.spawnRole(ctx, rush, RoleLottery)
		}
		if l.st.cfg.Verbose {
			l.st.score.Message(fmt.Sprintf("fans=%d (+%d, rush=%d, left=%d) req=%d err=%d slow=%d", active, add, rush, l.left.Load(), req, errs, slow))
		}
	}
}

func (l *Load) runFan(ctx context.Context, f *Fan) {
	if err := f.Login_(ctx); err != nil {
		return
	}
	for ctx.Err() == nil && !f.left {
		if failed, _ := l.st.score.Failed(); failed {
			return
		}
		if err := l.step(ctx, f); err != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(20+f.rng.Intn(80)) * time.Millisecond):
		}
	}
}

// step はファンの次の行動を 1 つ実行する。
func (l *Load) step(ctx context.Context, f *Fan) error {
	st := l.st
	now := time.Now()

	// 1. 抽選結果の確認（発表後は最優先）
	if f.hasPendingAnnounced(now) {
		err := f.checkLottery(ctx)
		if err == nil && f.hasPendingAnnounced(time.Now()) {
			time.Sleep(500 * time.Millisecond)
		}
		return err
	}

	// 2. 入場
	if f.hasGateTickets(now) {
		return f.goToGate(ctx, l.gate, now)
	}

	// 3. リセール（抽選発表後〜入場開始まで）
	resaleOpen := !now.Before(st.T0.Add(st.cfg.LotteryAnnounce)) && now.Before(st.T0.Add(st.cfg.GateOpenOffset))
	if resaleOpen {
		switch x := f.rng.Intn(10); {
		case x < 2:
			return f.listForResale(ctx, now)
		case x < 5:
			all := append(append([]*BenchEvent{}, st.fcEvents...), st.lotEvents...)
			return f.buyResale(ctx, all[f.rng.Intn(len(all))].ID)
		}
	}
	// 入場開始後は、譲渡済みチケットの古い QR が無効になっているか確認する
	if !now.Before(st.T0.Add(st.cfg.GateOpenOffset)) && f.rng.Intn(30) == 0 {
		if lst := l.randomSoldListing(f.rng); lst != nil {
			return l.gate.VerifyOldQR(ctx, lst)
		}
	}

	// 4. 抽選申込
	if f.role == RoleLottery {
		for _, be := range st.lotEvents {
			if _, done := f.entries[be.ID]; !done && !now.Before(be.SaleStart) && now.Before(be.SaleEnd.Add(-500*time.Millisecond)) {
				if _, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/events/%d", be.ID), nil); err != nil {
					return err
				}
				return f.enterLottery(ctx, be)
			}
		}
	}

	// 5. 先着販売
	if f.role == RoleHunter {
		for _, be := range st.fcEvents {
			if _, out := l.soldOut.Load(be.ID); out {
				continue
			}
			untilStart := be.SaleStart.Sub(now)
			if untilStart > 0 && untilStart < 3*time.Second {
				// 発売直前は公演ページを更新し続ける
				_, err := f.call(ctx, http.MethodGet, fmt.Sprintf("/api/events/%d", be.ID), nil)
				time.Sleep(300 * time.Millisecond)
				return err
			}
			if untilStart <= 0 {
				soldOut, err := f.purchase(ctx, be.ID)
				if soldOut {
					l.soldOut.Store(be.ID, true)
				}
				return err
			}
		}
	}

	// 6. 一覧を見て、販売中の公演を買う
	events, err := f.browse(ctx)
	if err != nil || len(events) == 0 {
		return err
	}
	if f.rng.Intn(3) == 0 {
		return nil
	}
	var cand []EventJSON
	for _, e := range events {
		if e.SaleType != "first_come" {
			continue
		}
		if _, out := l.soldOut.Load(e.ID); out {
			continue
		}
		if be, ok := st.events[e.ID]; ok && now.Before(be.SaleStart) {
			continue
		}
		cand = append(cand, e)
	}
	if len(cand) == 0 {
		return nil
	}
	e := cand[f.rng.Intn(len(cand))]
	soldOut, err := f.purchase(ctx, e.ID)
	if soldOut {
		l.soldOut.Store(e.ID, true)
	}
	return err
}

func (l *Load) randomSoldListing(rng *rand.Rand) *Listing {
	l.st.listingMu.Lock()
	defer l.st.listingMu.Unlock()
	for _, lst := range l.st.listings {
		if lst.SoldTo != nil && lst.OldQR != "" {
			if rng.Intn(3) == 0 {
				return lst
			}
		}
	}
	return nil
}

// runOrganizer は主催者として売上を定期的に確認する。
func (l *Load) runOrganizer(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, be := range l.st.fcEvents {
			reqStart := time.Now()
			path := fmt.Sprintf("/api/admin/events/%d/sales", be.ID)
			res, err := l.org.Do(ctx, http.MethodGet, path, nil)
			l.st.stats.record(res, err)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				l.st.score.Penalize("タイムアウト", 5)
				continue
			}
			if !expectStatus(l.st, res, path, http.StatusOK) {
				continue
			}
			sales, err := decode[SalesJSON](res)
			if err != nil {
				l.st.score.Penalize("不正なレスポンス", 10)
				continue
			}
			sold := be.soldBefore(reqStart.Add(-2 * time.Second))
			for rank, n := range sold {
				if sales.SoldSeats[rank] < n {
					l.st.score.Penalize("売上集計が古い", 10)
					break
				}
			}
		}
	}
}

// benchImage は走行用公演のポスター（小さな JPEG）。
func benchImage(i int) []byte {
	return makeJPEG(int64(1000 + i))
}
