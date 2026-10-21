package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// finalCheck は走行後の整合性を検証する。
func finalCheck(ctx context.Context, st *State, load *Load) {
	if failed, _ := st.score.Failed(); failed {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 抽選結果が未確認の申込者に、結果を確認させる
	var pending []*Fan
	seen := map[*Fan]bool{}
	for _, be := range st.lotEvents {
		be.mu.Lock()
		for _, e := range be.entries {
			if e.Status == "pending" && !seen[e.Fan] {
				seen[e.Fan] = true
				pending = append(pending, e.Fan)
			}
		}
		be.mu.Unlock()
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, f := range pending {
		wg.Add(1)
		sem <- struct{}{}
		go func(f *Fan) {
			defer wg.Done()
			defer func() { <-sem }()
			f.left = false
			f.checkLottery(ctx)
		}(f)
	}
	wg.Wait()

	for _, be := range st.lotEvents {
		be.mu.Lock()
		delay := "未確認"
		if !be.resultSeenAt.IsZero() {
			delay = be.resultSeenAt.Sub(be.AnnounceAt).Round(10 * time.Millisecond).String()
		}
		st.score.Message(fmt.Sprintf("抽選公演 %d: 申込 %d 件 / 結果確認まで %s", be.ID, len(be.entries), delay))
		if len(be.entries) > 0 && (be.resultSeenAt.IsZero() || be.resultSeenAt.After(be.AnnounceAt.Add(st.cfg.LotteryDeadline))) {
			st.score.Penalize("抽選結果の発表遅延", 1000)
		}
		be.mu.Unlock()
		checkLotteryResult(st, be)
	}
	checkOvercharge(st)
}

func checkLotteryResult(st *State, be *BenchEvent) {
	be.mu.Lock()
	entries := append([]*LotteryEntry(nil), be.entries...)
	be.mu.Unlock()

	type agg struct {
		requested, won, pending int
		list                    []*LotteryEntry
	}
	byRank := map[string]*agg{}
	for _, e := range entries {
		a, ok := byRank[e.Rank]
		if !ok {
			a = &agg{}
			byRank[e.Rank] = a
		}
		a.requested += e.Quantity
		a.list = append(a.list, e)
		switch e.Status {
		case "won":
			a.won += e.Quantity
		case "pending":
			a.pending++
		}
	}

	for rank, a := range byRank {
		seats := be.SeatsTotal[rank]
		if a.won > seats {
			st.score.Fail("公演 %d ランク %s: 当選枚数 %d が座席数 %d を超えています", be.ID, rank, a.won, seats)
			return
		}
		if a.pending > 0 {
			st.score.Penalize("抽選結果が確定しない申込", 0)
			continue
		}
		// 席が余っているのに落選させていないか（4 枚申込で端数が残るケースは許容）
		if a.requested >= seats && a.won < seats*9/10 {
			st.score.Fail("公演 %d ランク %s: 申込が座席数を上回るのに当選が少なすぎます (%d / %d 席)", be.ID, rank, a.won, seats)
			return
		}
		if a.requested < seats && a.won != a.requested {
			st.score.Fail("公演 %d ランク %s: 全員当選できるはずなのに落選者がいます", be.ID, rank)
			return
		}
		checkFairness(st, be, rank, a.list, seats, a.requested)
	}
}

// checkFairness は申込順で当選しやすさに偏りがないか検証する。
func checkFairness(st *State, be *BenchEvent, rank string, list []*LotteryEntry, seats, requested int) {
	if requested < seats*3/2 || len(list) < 100 {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].EnteredAt.Before(list[j].EnteredAt) })
	half := len(list) / 2
	rate := func(es []*LotteryEntry) float64 {
		won := 0
		for _, e := range es {
			if e.Status == "won" {
				won++
			}
		}
		return float64(won) / float64(len(es))
	}
	early, late := rate(list[:half]), rate(list[half:])
	if early > late*2+0.1 || late > early*2+0.1 {
		st.score.Fail("公演 %d ランク %s: 申込順で当選率に偏りがあります (前半 %.2f / 後半 %.2f)", be.ID, rank, early, late)
	}
}

// checkOvercharge は課金されたのに注文が成立していない決済（二重課金）を数える。
func checkOvercharge(st *State) {
	all := st.payment.All()
	st.payMu.Lock()
	matched := len(st.matchedPayment)
	unknown := st.unknownOutcome
	st.payMu.Unlock()
	over := len(all) - matched - unknown
	if over > 0 {
		st.score.Penalize("二重課金", 1000*over)
		st.score.Message(fmt.Sprintf("注文に紐づかない課金が %d 件あります", over))
	}
}
