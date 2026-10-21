package main

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// minJob は最も短い作業の長さ（数量 10 × 1 個 10 秒）。
const minJob = 100 * time.Second

// finalCheck は走行後の整合性（設備の重複割当・段取り替え・部材在庫）を検証する。
func finalCheck(ctx context.Context, st *State) {
	if failed, _ := st.score.Failed(); failed {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 受注作成のレスポンスより先に計画に現れた割当を照合する
	st.orderMu.Lock()
	early := st.earlyPlanned
	st.earlyPlanned = nil
	st.orderMu.Unlock()
	l := &Load{st: st}
	for i := range early {
		p := early[i]
		st.orderMu.Lock()
		o, ok := st.orders[p.OrderID]
		dup := ok && o.Planned != nil
		if ok && !dup {
			o.Planned = &p
			st.assignments = append(st.assignments, &p)
		}
		st.orderMu.Unlock()
		if !ok {
			// 作成レスポンスを受け取れなかった受注（タイムアウト等）の可能性があるため採点だけしない
			continue
		}
		if dup {
			st.score.Fail("受注 %d が二重に計画されました", p.OrderID)
			return
		}
		if !l.checkAssignment(o, &p, st.T0) {
			return
		}
	}

	// 設備ごとに時刻順に並べ、重複と段取り替えを検証する
	st.orderMu.Lock()
	byMachine := map[int64][]*OrderState{}
	for _, o := range st.orders {
		if o.Planned != nil {
			byMachine[o.Planned.MachineID] = append(byMachine[o.Planned.MachineID], o)
		}
	}
	st.orderMu.Unlock()
	for mid, os := range byMachine {
		sort.Slice(os, func(i, j int) bool { return os[i].Planned.StartAt.Before(os[j].Planned.StartAt) })
		for i := 1; i < len(os); i++ {
			prev, cur := os[i-1], os[i]
			// 時刻は DB でマイクロ秒に丸められるため、1 ミリ秒未満のずれは許容する
			if cur.Planned.StartAt.Before(prev.Planned.EndAt.Add(-time.Millisecond)) {
				st.score.Fail("設備 %d で受注 %d と %d の作業時間が重なっています", mid, prev.ID, cur.ID)
				return
			}
			// 計画 API の応答を受け取れなかった割当が間に挟まっている可能性があるため、
			// 最短の作業（10 個 × 10 秒）も入らない隙間のときだけ段取り替えを検証する
			if prev.Product.Family != cur.Product.Family && cur.Planned.StartAt.Sub(prev.Planned.EndAt) < minJob {
				need := time.Duration(cur.Quantity*cur.Product.UnitSeconds)*time.Second + setupTime
				if cur.Planned.EndAt.Sub(cur.Planned.StartAt) < need-time.Millisecond {
					st.score.Fail("設備 %d の受注 %d に段取り替えの時間が含まれていません", mid, cur.ID)
					return
				}
			}
		}
	}

	// 部材在庫: 初期在庫 + 入荷 - 計画での引当 と一致すること
	a := st.newAgent()
	if err := login(ctx, a, plannerLogin(9)); err != nil {
		st.score.Fail("最終チェックでログインできません: %v", err)
		return
	}
	res, err := a.Do(ctx, http.MethodGet, "/api/materials", nil)
	if err != nil || res.Status != http.StatusOK {
		st.score.Fail("最終チェックで部材在庫を取得できません")
		return
	}
	ms, err := decode[struct {
		Materials []MaterialJSON `json:"materials"`
	}](res)
	if err != nil {
		st.score.Fail("GET /api/materials: 不正なレスポンス")
		return
	}
	st.matMu.Lock()
	defer st.matMu.Unlock()
	for _, m := range ms.Materials {
		upper := st.initialStock[m.ID] + st.receivedSent[m.ID] - st.consumed[m.ID]
		if m.Stock > upper {
			st.score.Fail("部材 %d の在庫 (%d) が、入荷と引当から計算した値 (%d) より多くなっています", m.ID, m.Stock, upper)
			return
		}
		if m.Stock < 0 {
			st.score.Fail("部材 %d の在庫が負になっています", m.ID)
			return
		}
	}
}
