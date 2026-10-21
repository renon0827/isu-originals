package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// finalCheck は走行後の整合性を検証する。
func finalCheck(ctx context.Context, st *State, load *Load) {
	// 出品者が落札確定を見届けるのを待つ（走行終了から最大 15 秒）
	done := make(chan struct{})
	go func() {
		load.settleWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
	if failed, _ := st.score.Failed(); failed {
		return
	}

	checked, skipped, unsettled := 0, 0, 0
	var winners []*BenchItem
	for _, it := range st.allItems() {
		it.mu.Lock()
		settled := it.settled
		unknown := it.unknown
		bids := append([]AcceptedBid(nil), it.bids...)
		finalPrice, winner, finalEnd := it.finalPrice, it.winnerID, it.finalEnd
		it.mu.Unlock()

		if !settled {
			if it.end().Before(st.T0.Add(st.cfg.Duration)) {
				unsettled++
			}
			continue
		}
		// 受理された入札が終了時刻の延長規則と矛盾しないか
		end, ok := expectedEnd(it.OriginalEnd, bids)
		if !ok {
			st.score.Fail("商品 %d: 終了時刻を過ぎた入札を受け付けています", it.ID)
			return
		}
		if unknown > 0 {
			// 結果の分からない入札がある商品は価格・落札者の厳密な検証を省く
			skipped++
			continue
		}
		checked++
		if diff := finalEnd.Sub(end); diff > time.Millisecond || diff < -time.Millisecond {
			st.score.Fail("商品 %d: 終了時刻が延長規則と一致しません (期待 %s / 実際 %s)", it.ID, end.Format(time.RFC3339Nano), finalEnd.Format(time.RFC3339Nano))
			return
		}
		price, leader := expectedState(it.StartPrice, it.Increment, bids)
		if leader == 0 {
			if winner != 0 || finalPrice != nil {
				st.score.Fail("商品 %d: 入札が無いのに落札者がいます", it.ID)
				return
			}
			continue
		}
		if winner != leader || finalPrice == nil || *finalPrice != price {
			got := -1
			if finalPrice != nil {
				got = *finalPrice
			}
			st.score.Fail("商品 %d: 落札結果が不正です (期待: ユーザー %d / %d円、実際: ユーザー %d / %d円)", it.ID, leader, price, winner, got)
			return
		}
		winners = append(winners, it)
	}
	if unsettled > 0 {
		st.score.Penalize("落札が確定しない商品", 0)
		st.score.Message(fmt.Sprintf("走行中に終了したのに落札確定を確認できない商品が %d 件あります", unsettled))
	}
	st.score.Message(fmt.Sprintf("落札結果の検証: %d 件（結果不明の入札があり省略: %d 件）", checked, skipped))
	st.score.Message(settleDelaySummary(st))

	checkWonNotifications(ctx, st, load, winners)
}

// checkWonNotifications は落札者に「落札しました」の通知が届いているか抜き取りで確認する。
func checkWonNotifications(ctx context.Context, st *State, load *Load, winners []*BenchItem) {
	byUser := map[int64]*Fan{}
	load.fansMu.Lock()
	for _, f := range load.fans {
		if f.UserID != 0 {
			byUser[f.UserID] = f
		}
	}
	load.fansMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sampled := 0
	for _, it := range winners {
		if sampled >= 20 {
			break
		}
		f, ok := byUser[it.winnerID]
		if !ok {
			continue
		}
		sampled++
		res, err := f.agent.Do(ctx, http.MethodGet, "/api/me/notifications", nil)
		if err != nil || res.Status != http.StatusOK {
			st.score.Penalize("落札通知の確認に失敗", 10)
			continue
		}
		body, err := decode[struct {
			Notifications []NotificationJSON `json:"notifications"`
		}](res)
		if err != nil {
			st.score.Penalize("不正なレスポンス", 10)
			continue
		}
		found := false
		for _, n := range body.Notifications {
			if n.Type == "won" && n.ItemID == it.ID {
				found = true
			}
		}
		// 50 件より古い可能性があるときは判定しない
		if !found && len(body.Notifications) < 50 {
			st.score.Penalize("落札通知が届いていない", 50)
		}
	}
}

// settleDelaySummary は終了から落札確定を確認するまでの遅れの分布。
func settleDelaySummary(st *State) string {
	var ds []time.Duration
	for _, it := range st.allItems() {
		it.mu.Lock()
		if it.settled {
			ds = append(ds, it.settledAt.Sub(it.finalEnd))
		}
		it.mu.Unlock()
	}
	if len(ds) == 0 {
		return "落札確定までの遅れ: 確認できた商品なし"
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	q := func(p float64) time.Duration { return ds[int(float64(len(ds)-1)*p)].Round(10 * time.Millisecond) }
	return fmt.Sprintf("落札確定までの遅れ: 中央値 %v / 90%% %v / 最大 %v（%d 件）", q(0.5), q(0.9), q(1), len(ds))
}
