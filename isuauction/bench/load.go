package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 直近 1 秒のリクエスト統計（ユーザー追加の判断に使う） ----

type Stats struct {
	requests atomic.Int64
	errors   atomic.Int64
	slow     atomic.Int64
}

func (s *Stats) record(res *Response, err error) {
	s.requests.Add(1)
	if err != nil || (res != nil && res.Status >= 500) {
		s.errors.Add(1)
		return
	}
	// 価格確認は待たされてよい API なので遅延として数えない
	if res != nil && !res.FromCache && res.Latency > time.Second && res.Latency < 5*time.Second && !res.LongPoll {
		s.slow.Add(1)
	}
}

func (s *Stats) swap() (req, errs, slow int64) {
	return s.requests.Swap(0), s.errors.Swap(0), s.slow.Swap(0)
}

// ---- 負荷走行 ----

type Load struct {
	st *State

	wg     sync.WaitGroup
	active atomic.Int64
	left   atomic.Int64

	sellers  atomic.Int64
	settleWG sync.WaitGroup
	fansMu   sync.Mutex
	fans     []*Fan
}

func (l *Load) spawn(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		f := l.st.newFan()
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

// spawnSeller は出品者を 1 人追加する。出品者は帰らない。
func (l *Load) spawnSeller(ctx context.Context, settleCtx context.Context) {
	l.sellers.Add(1)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		f := l.st.newFan()
		f.role = RoleBrowser
		if err := f.login(ctx); err != nil {
			return
		}
		for ctx.Err() == nil {
			// 走行終了までに終わるよう、終了時刻は走行終了の 5 秒前まで
			remain := time.Until(l.st.T0.Add(l.st.cfg.Duration)) - 5*time.Second
			if remain < 12*time.Second {
				return
			}
			dur := time.Duration(12+f.rng.Intn(14)) * time.Second
			if dur > remain {
				dur = remain
			}
			start := (2 + f.rng.Intn(9)) * 500
			it, err := f.sell(ctx, dur, start, 100, false)
			if err == nil && it != nil {
				l.settleWG.Add(1)
				go func() {
					defer l.settleWG.Done()
					f.watchSettlement(settleCtx, it)
				}()
			}
			sleepCtx(ctx, time.Duration(1500+f.rng.Intn(1500))*time.Millisecond)
		}
	}()
}

func (l *Load) Run(ctx context.Context, settleCtx context.Context) {
	l.spawn(ctx, l.st.cfg.InitialFans)
	l.spawnSeller(ctx, settleCtx)

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
		// 利用者が増えると出品者も増える（25 人に 1 人）
		for int64(active)/25+1 > l.sellers.Load() {
			l.spawnSeller(ctx, settleCtx)
		}
		if l.st.cfg.Verbose {
			l.st.score.Message(fmt.Sprintf("fans=%d (+%d, left=%d) sellers=%d req=%d err=%d slow=%d", active, add, l.left.Load(), l.sellers.Load(), req, errs, slow))
		}
	}
}

func (l *Load) runFan(ctx context.Context, f *Fan) {
	if err := f.login(ctx); err != nil {
		return
	}
	for ctx.Err() == nil && !f.left {
		if failed, _ := l.st.score.Failed(); failed {
			return
		}
		l.step(ctx, f)
		sleepCtx(ctx, time.Duration(20+f.rng.Intn(60))*time.Millisecond)
	}
}

func (l *Load) step(ctx context.Context, f *Fan) {
	// 目玉商品は終了前に一度ウォッチしておく（落札確定時の通知の宛先になる）
	if !f.watchedHot {
		f.watchedHot = true
		for _, it := range l.st.openItems(time.Now()) {
			if it.Hot {
				res, err := f.call(ctx, http.MethodPost, fmt.Sprintf("/api/items/%d/watch", it.ID), nil)
				if err != nil {
					return
				}
				expectStatus(l.st, res, "POST /api/items/:id/watch", http.StatusNoContent)
			}
		}
	}
	if err := f.checkNotifications(ctx); err != nil {
		return
	}
	if f.role == RoleBrowser {
		f.browse(ctx)
		return
	}
	f.stepBidder(ctx)
}

// prepareHotItems は走行前に目玉商品（T0+30 秒に一斉に終了）を出品する。
func (l *Load) prepareHotItems(ctx context.Context, settleCtx context.Context) error {
	f := l.st.newFan()
	f.isNew = false
	f.Login = userLogin(numInitialUsers)
	f.Password = userPassword(f.Login)
	f.role = RoleBrowser
	if err := f.login(ctx); err != nil {
		return fmt.Errorf("目玉商品の出品者がログインできません: %w", err)
	}
	for i := 0; i < 3; i++ {
		dur := time.Until(l.st.T0.Add(l.st.cfg.HotEndOffset))
		it, err := f.sell(ctx, dur.Truncate(time.Second), 10000, 500, true)
		if err != nil {
			return err
		}
		if it == nil {
			return fmt.Errorf("目玉商品を出品できません")
		}
		l.settleWG.Add(1)
		go func() {
			defer l.settleWG.Done()
			f.watchSettlement(settleCtx, it)
		}()
	}
	return nil
}

// benchImage は出品用の小さな JPEG（480x360）。
func benchImage(seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, 480, 360))
	c := color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}
	for y := 0; y < 360; y++ {
		for x := 0; x < 480; x++ {
			img.Set(x, y, color.RGBA{c.R ^ uint8(x/30), c.G ^ uint8(y/30), c.B, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}
