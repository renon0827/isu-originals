package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"time"
)

func newGzipReader(r io.Reader) (*gzip.Reader, error) { return gzip.NewReader(r) }

// ---- API の JSON ----

type UserJSON struct {
	ID        int64  `json:"id"`
	LoginName string `json:"login_name"`
	Nickname  string `json:"nickname"`
}

type UserSummary struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
}

type ItemJSON struct {
	ID           int64        `json:"id"`
	Title        string       `json:"title"`
	CategoryID   int64        `json:"category_id"`
	Seller       UserSummary  `json:"seller"`
	StartPrice   int          `json:"start_price"`
	Increment    int          `json:"increment"`
	CurrentPrice int          `json:"current_price"`
	BidCount     int          `json:"bid_count"`
	Status       string       `json:"status"`
	EndAt        time.Time    `json:"end_at"`
	ImageURL     string       `json:"image_url"`
	ThumbnailURL string       `json:"thumbnail_url"`
	Description  string       `json:"description"`
	Leader       *UserSummary `json:"leader"`
	Winner       *UserSummary `json:"winner"`
	FinalPrice   *int         `json:"final_price"`
	WatchCount   *int         `json:"watch_count"`
	Watching     *bool        `json:"watching"`
	MyMax        *int         `json:"my_max"`
}

type ItemsResponse struct {
	Items   []ItemJSON `json:"items"`
	Page    int        `json:"page"`
	HasNext bool       `json:"has_next"`
}

type PriceJSON struct {
	Version      int       `json:"version"`
	CurrentPrice int       `json:"current_price"`
	BidCount     int       `json:"bid_count"`
	LeaderID     int64     `json:"leader_id"`
	Status       string    `json:"status"`
	EndAt        time.Time `json:"end_at"`
}

type BidResponse struct {
	BidID        int64     `json:"bid_id"`
	AcceptedAt   time.Time `json:"accepted_at"`
	CurrentPrice int       `json:"current_price"`
	IsLeader     bool      `json:"is_leader"`
	EndAt        time.Time `json:"end_at"`
	Version      int       `json:"version"`
}

type NotificationJSON struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	ItemID    int64     `json:"item_id"`
	ItemTitle string    `json:"item_title"`
	Price     *int      `json:"price"`
	CreatedAt time.Time `json:"created_at"`
}

type MyBidJSON struct {
	Item   ItemJSON `json:"item"`
	MyMax  int      `json:"my_max"`
	Result string   `json:"result"`
}

// ---- 初期データの前提（tools/datagen と一致させる） ----

const (
	numInitialUsers = 30000
	numEndedItems   = 120000
	numOpenItems    = 3000
	numCategories   = 16
)

func userLogin(i int) string           { return fmt.Sprintf("user%05d", i) }
func userPassword(login string) string { return "isu-" + login }

var searchWords = []string{"北欧", "ヴィンテージ", "美品", "レトロ", "チェア", "スツール", "ソファ", "オーク材", "革張り", "籐"}

// ---- 価格のルール ----

// AcceptedBid は受理された入札。
type AcceptedBid struct {
	UserID int64
	Max    int
	At     time.Time
	BidID  int64
}

// expectedState は入札の集合から現在価格と最高入札者を求める。
//
//   - 入札者ごとに最新の上限額だけを使う
//   - 最高入札者 = 上限額が最大の入札者（同額なら先にその額を入札した方）
//   - 現在価格 = 入札者が 1 人なら開始価格、2 人以上なら min(最高額, 2 番目の額 + 入札単位)
func expectedState(start, inc int, bids []AcceptedBid) (price int, leader int64) {
	latest := map[int64]AcceptedBid{}
	for _, b := range bids {
		if cur, ok := latest[b.UserID]; !ok || b.At.After(cur.At) || (b.At.Equal(cur.At) && b.BidID > cur.BidID) {
			latest[b.UserID] = b
		}
	}
	if len(latest) == 0 {
		return start, 0
	}
	list := make([]AcceptedBid, 0, len(latest))
	for _, b := range latest {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Max != list[j].Max {
			return list[i].Max > list[j].Max
		}
		if !list[i].At.Equal(list[j].At) {
			return list[i].At.Before(list[j].At)
		}
		return list[i].BidID < list[j].BidID
	})
	if len(list) == 1 {
		return start, list[0].UserID
	}
	p := list[1].Max + inc
	if p > list[0].Max {
		p = list[0].Max
	}
	return p, list[0].UserID
}

// 終了間際の入札による延長幅
const extendWindow = 3 * time.Second

// expectedEnd は受理された入札から最終的な終了時刻を求める。終了後に受理された入札があれば ok=false。
func expectedEnd(originalEnd time.Time, bids []AcceptedBid) (end time.Time, ok bool) {
	sorted := append([]AcceptedBid(nil), bids...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].At.Before(sorted[j].At)
		}
		return sorted[i].BidID < sorted[j].BidID
	})
	end = originalEnd
	for _, b := range sorted {
		if !b.At.Before(end) {
			return end, false
		}
		if end.Sub(b.At) < extendWindow {
			// DB には µs 精度で保存されるので合わせる
			end = b.At.Add(extendWindow).Truncate(time.Microsecond)
		}
	}
	return end, true
}
