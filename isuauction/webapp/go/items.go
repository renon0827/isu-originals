package main

import (
	"bytes"
	"database/sql"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

const itemsPerPage = 20

type Item struct {
	ID            int64         `db:"id"`
	SellerID      int64         `db:"seller_id"`
	CategoryID    int64         `db:"category_id"`
	Title         string        `db:"title"`
	Description   string        `db:"description"`
	ImageID       int64         `db:"image_id"`
	StartPrice    int           `db:"start_price"`
	Increment     int           `db:"increment"`
	Status        string        `db:"status"`
	EndAt         time.Time     `db:"end_at"`
	OriginalEndAt time.Time     `db:"original_end_at"`
	WinnerID      sql.NullInt64 `db:"winner_id"`
	FinalPrice    sql.NullInt64 `db:"final_price"`
	CreatedAt     time.Time     `db:"created_at"`
}

type Bid struct {
	ID        int64     `db:"id"`
	ItemID    int64     `db:"item_id"`
	UserID    int64     `db:"user_id"`
	MaxAmount int       `db:"max_amount"`
	CreatedAt time.Time `db:"created_at"`
}

// AuctionState は入札の集合から導かれる現在の状態。
type AuctionState struct {
	Price    int
	LeaderID int64
	BidCount int
	// 入札者ごとの最新の上限額
	MaxByUser map[int64]int
}

// computeState は入札の集合から現在価格と最高入札者を求める。
//
//   - 入札者ごとに最新の上限額だけを使う
//   - 最高入札者 = 上限額が最大の入札者（同額なら先にその額を入札した方）
//   - 現在価格 = 入札者が 1 人なら開始価格、2 人以上なら min(最高額, 2 番目の額 + 入札単位)
func computeState(item *Item, bids []Bid) AuctionState {
	type um struct {
		user int64
		max  int
		at   time.Time
		seq  int64
	}
	latest := map[int64]*um{}
	for _, b := range bids {
		if u, ok := latest[b.UserID]; ok {
			u.max, u.at, u.seq = b.MaxAmount, b.CreatedAt, b.ID
		} else {
			latest[b.UserID] = &um{b.UserID, b.MaxAmount, b.CreatedAt, b.ID}
		}
	}
	st := AuctionState{Price: item.StartPrice, BidCount: len(bids), MaxByUser: map[int64]int{}}
	if len(latest) == 0 {
		return st
	}
	list := make([]*um, 0, len(latest))
	for _, u := range latest {
		list = append(list, u)
		st.MaxByUser[u.user] = u.max
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].max != list[j].max {
			return list[i].max > list[j].max
		}
		if !list[i].at.Equal(list[j].at) {
			return list[i].at.Before(list[j].at)
		}
		return list[i].seq < list[j].seq
	})
	st.LeaderID = list[0].user
	if len(list) >= 2 {
		st.Price = list[1].max + item.Increment
		if st.Price > list[0].max {
			st.Price = list[0].max
		}
	}
	return st
}

// loadState は商品の入札をすべて読み込んで現在の状態を計算する。
func loadState(q queryer, item *Item) (AuctionState, error) {
	var bids []Bid
	if err := q.Select(&bids, "SELECT * FROM bids WHERE item_id = ? ORDER BY id", item.ID); err != nil {
		return AuctionState{}, err
	}
	return computeState(item, bids), nil
}

func getItemByID(q queryer, id int64) (*Item, error) {
	var it Item
	if err := q.Get(&it, "SELECT * FROM items WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &it, nil
}

// displayStatus は終了時刻を過ぎて落札確定前の商品を closing と表示する。
func displayStatus(it *Item, now time.Time) string {
	if it.Status == "open" && !now.Before(it.EndAt) {
		return "closing"
	}
	return it.Status
}

type UserSummary struct {
	ID       int64  `json:"id" db:"id"`
	Nickname string `json:"nickname" db:"nickname"`
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
	Description  string       `json:"description,omitempty"`
	Leader       *UserSummary `json:"leader,omitempty"`
	Winner       *UserSummary `json:"winner,omitempty"`
	FinalPrice   *int         `json:"final_price,omitempty"`
	WatchCount   *int         `json:"watch_count,omitempty"`
	Watching     *bool        `json:"watching,omitempty"`
	MyMax        *int         `json:"my_max,omitempty"`
}

func getUserSummary(id int64) (*UserSummary, error) {
	var u UserSummary
	if err := db.Get(&u, "SELECT id, nickname FROM users WHERE id = ?", id); err != nil {
		return nil, err
	}
	return &u, nil
}

// buildItemJSON は一覧・詳細用の JSON を組み立てる。detail=true なら説明・最高入札者などを含める。
func buildItemJSON(it *Item, detail bool, viewer *User) (*ItemJSON, error) {
	st, err := loadState(db, it)
	if err != nil {
		return nil, err
	}
	seller, err := getUserSummary(it.SellerID)
	if err != nil {
		return nil, err
	}
	j := &ItemJSON{
		ID:           it.ID,
		Title:        it.Title,
		CategoryID:   it.CategoryID,
		Seller:       *seller,
		StartPrice:   it.StartPrice,
		Increment:    it.Increment,
		CurrentPrice: st.Price,
		BidCount:     st.BidCount,
		Status:       displayStatus(it, time.Now()),
		EndAt:        it.EndAt,
		ImageURL:     "/api/items/" + strconv.FormatInt(it.ID, 10) + "/image",
		ThumbnailURL: "/api/items/" + strconv.FormatInt(it.ID, 10) + "/image?size=thumb",
	}
	if it.Status == "ended" && it.FinalPrice.Valid {
		p := int(it.FinalPrice.Int64)
		j.FinalPrice = &p
		j.CurrentPrice = p
	}
	if !detail {
		return j, nil
	}
	j.Description = it.Description
	if st.LeaderID != 0 {
		if j.Leader, err = getUserSummary(st.LeaderID); err != nil {
			return nil, err
		}
	}
	if it.Status == "ended" && it.WinnerID.Valid {
		if j.Winner, err = getUserSummary(it.WinnerID.Int64); err != nil {
			return nil, err
		}
	}
	var wc int
	if err := db.Get(&wc, "SELECT COUNT(*) FROM watches WHERE item_id = ?", it.ID); err != nil {
		return nil, err
	}
	j.WatchCount = &wc
	if viewer != nil {
		var w int
		if err := db.Get(&w, "SELECT COUNT(*) FROM watches WHERE user_id = ? AND item_id = ?", viewer.ID, it.ID); err != nil {
			return nil, err
		}
		watching := w > 0
		j.Watching = &watching
		if m, ok := st.MaxByUser[viewer.ID]; ok {
			j.MyMax = &m
		}
	}
	return j, nil
}

type Category struct {
	ID   int64  `db:"id" json:"id"`
	Name string `db:"name" json:"name"`
}

func getCategories(c echo.Context) error {
	var cs []Category
	if err := db.Select(&cs, "SELECT * FROM categories ORDER BY id"); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"categories": cs})
}

// getItems は商品の一覧・検索。
//
//	status: open（既定） / ended、sort: ending_soon（既定） / newest、q: キーワード、category_id、page
func getItems(c echo.Context) error {
	status := c.QueryParam("status")
	if status == "" {
		status = "open"
	}
	if status != "open" && status != "ended" {
		return errorJSON(c, http.StatusBadRequest, "invalid status")
	}
	sortKey := c.QueryParam("sort")
	if sortKey == "" {
		sortKey = "ending_soon"
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	if page < 1 {
		page = 1
	}

	now := time.Now()
	where := "status = ?"
	args := []any{status}
	if status == "open" {
		where += " AND end_at > ?"
		args = append(args, now)
	}
	if q := c.QueryParam("q"); q != "" {
		where += " AND (title LIKE ? OR description LIKE ?)"
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if cat, err := strconv.ParseInt(c.QueryParam("category_id"), 10, 64); err == nil && cat > 0 {
		where += " AND category_id = ?"
		args = append(args, cat)
	}
	var order string
	switch sortKey {
	case "ending_soon":
		order = "end_at ASC, id ASC"
		if status == "ended" {
			order = "end_at DESC, id DESC"
		}
	case "newest":
		order = "created_at DESC, id DESC"
	default:
		return errorJSON(c, http.StatusBadRequest, "invalid sort")
	}
	args = append(args, itemsPerPage+1, (page-1)*itemsPerPage)

	var items []Item
	if err := db.Select(&items, "SELECT * FROM items WHERE "+where+" ORDER BY "+order+" LIMIT ? OFFSET ?", args...); err != nil {
		return err
	}
	hasNext := len(items) > itemsPerPage
	if hasNext {
		items = items[:itemsPerPage]
	}
	res := make([]*ItemJSON, 0, len(items))
	for i := range items {
		j, err := buildItemJSON(&items[i], false, nil)
		if err != nil {
			return err
		}
		res = append(res, j)
	}
	return c.JSON(http.StatusOK, map[string]any{"items": res, "page": page, "has_next": hasNext})
}

func getItem(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	it, err := getItemByID(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errorJSON(c, http.StatusNotFound, "item not found")
	} else if err != nil {
		return err
	}
	viewer, _ := currentUser(c)
	j, err := buildItemJSON(it, true, viewer)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, j)
}

type PriceJSON struct {
	Version      int       `json:"version"`
	CurrentPrice int       `json:"current_price"`
	BidCount     int       `json:"bid_count"`
	LeaderID     int64     `json:"leader_id"`
	Status       string    `json:"status"`
	EndAt        time.Time `json:"end_at"`
}

// getItemPrice は価格の更新確認用の軽量 API。
// after に手元の version を渡す。
func getItemPrice(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	it, err := getItemByID(db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return errorJSON(c, http.StatusNotFound, "item not found")
	} else if err != nil {
		return err
	}
	st, err := loadState(db, it)
	if err != nil {
		return err
	}
	p := PriceJSON{
		Version:      st.BidCount,
		CurrentPrice: st.Price,
		BidCount:     st.BidCount,
		LeaderID:     st.LeaderID,
		Status:       displayStatus(it, time.Now()),
		EndAt:        it.EndAt,
	}
	if it.Status == "ended" && it.FinalPrice.Valid {
		p.CurrentPrice = int(it.FinalPrice.Int64)
	}
	return c.JSON(http.StatusOK, p)
}

type BidHistoryJSON struct {
	Bidder    UserSummary `json:"bidder"`
	CreatedAt time.Time   `json:"created_at"`
}

// getItemBids は入札履歴（新しい順に最大 50 件。上限額は公開しない）。
func getItemBids(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var bids []Bid
	if err := db.Select(&bids, "SELECT * FROM bids WHERE item_id = ? ORDER BY id DESC LIMIT 50", id); err != nil {
		return err
	}
	res := make([]BidHistoryJSON, 0, len(bids))
	for _, b := range bids {
		u, err := getUserSummary(b.UserID)
		if err != nil {
			return err
		}
		res = append(res, BidHistoryJSON{Bidder: *u, CreatedAt: b.CreatedAt})
	}
	return c.JSON(http.StatusOK, map[string]any{"bids": res})
}

const thumbW, thumbH = 120, 90

// getItemImage は商品画像。size=thumb なら 120x90 に縮小して返す。
func getItemImage(c echo.Context) error {
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var data []byte
	if err := db.Get(&data, "SELECT im.data FROM items it JOIN images im ON im.id = it.image_id WHERE it.id = ?", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusNotFound, "item not found")
		}
		return err
	}
	if c.QueryParam("size") != "thumb" {
		return c.Blob(http.StatusOK, "image/jpeg", data)
	}
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	dst := image.NewRGBA(image.Rect(0, 0, thumbW, thumbH))
	b := src.Bounds()
	for y := 0; y < thumbH; y++ {
		for x := 0; x < thumbW; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/thumbW, b.Min.Y+y*b.Dy()/thumbH))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75}); err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "image/jpeg", buf.Bytes())
}
