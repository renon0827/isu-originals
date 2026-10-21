package main

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

// ファミリーが切り替わるときの段取り替え時間
const setupTime = 600 * time.Second

type Product struct {
	ID          int64  `db:"id" json:"id"`
	Name        string `db:"name" json:"name"`
	Family      string `db:"family" json:"family"`
	MachineType string `db:"machine_type" json:"machine_type"`
	UnitSeconds int    `db:"unit_seconds" json:"unit_seconds"`
	Drawing     []byte `db:"drawing" json:"-"`
}

type Material struct {
	ID    int64  `db:"id" json:"id"`
	Name  string `db:"name" json:"name"`
	Stock int64  `db:"stock" json:"stock"`
}

type BOMItem struct {
	ProductID  int64 `db:"product_id" json:"-"`
	MaterialID int64 `db:"material_id" json:"material_id"`
	Quantity   int   `db:"quantity" json:"quantity"`
}

type Order struct {
	ID        int64     `db:"id" json:"id"`
	ProductID int64     `db:"product_id" json:"product_id"`
	Quantity  int       `db:"quantity" json:"quantity"`
	DueAt     time.Time `db:"due_at" json:"due_at"`
	Status    string    `db:"status" json:"status"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type ProductJSON struct {
	Product
	BOM []BOMItem `json:"bom"`
}

func getProducts(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var products []Product
	if err := db.Select(&products, "SELECT * FROM products ORDER BY id"); err != nil {
		return err
	}
	res := make([]ProductJSON, 0, len(products))
	for _, p := range products {
		var bom []BOMItem
		if err := db.Select(&bom, "SELECT * FROM bom WHERE product_id = ? ORDER BY material_id", p.ID); err != nil {
			return err
		}
		res = append(res, ProductJSON{Product: p, BOM: bom})
	}
	return c.JSON(http.StatusOK, map[string]any{"products": res})
}

func getMaterials(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	var ms []Material
	if err := db.Select(&ms, "SELECT * FROM materials ORDER BY id"); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"materials": ms})
}

type ReceiveRequest struct {
	Quantity int `json:"quantity"`
}

// postMaterialReceive は部材の入荷を記録し、在庫を増やす。
func postMaterialReceive(c echo.Context) error {
	u, err := requirePlanner(c)
	if u == nil {
		return err
	}
	id, ok := paramID(c, "id")
	if !ok {
		return errorJSON(c, http.StatusBadRequest, "invalid id")
	}
	var req ReceiveRequest
	if err := c.Bind(&req); err != nil || req.Quantity <= 0 || req.Quantity > 100000 {
		return errorJSON(c, http.StatusBadRequest, "quantity must be 1 to 100000")
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE materials SET stock = stock + ? WHERE id = ?", req.Quantity, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errorJSON(c, http.StatusNotFound, "material not found")
	}
	if _, err := tx.Exec("INSERT INTO material_receipts (material_id, quantity, created_at) VALUES (?, ?, ?)", id, req.Quantity, time.Now()); err != nil {
		return err
	}
	var m Material
	if err := tx.Get(&m, "SELECT * FROM materials WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, m)
}

type OrderRequest struct {
	ProductID int64     `json:"product_id"`
	Quantity  int       `json:"quantity"`
	DueAt     time.Time `json:"due_at"`
}

func postOrder(c echo.Context) error {
	u, err := requirePlanner(c)
	if u == nil {
		return err
	}
	var req OrderRequest
	if err := c.Bind(&req); err != nil || req.Quantity <= 0 || req.Quantity > 1000 || req.DueAt.IsZero() {
		return errorJSON(c, http.StatusBadRequest, "invalid order")
	}
	var exists int
	if err := db.Get(&exists, "SELECT COUNT(*) FROM products WHERE id = ?", req.ProductID); err != nil {
		return err
	}
	if exists == 0 {
		return errorJSON(c, http.StatusBadRequest, "product not found")
	}
	res, err := db.Exec("INSERT INTO orders (product_id, quantity, due_at, status, created_at) VALUES (?, ?, ?, 'pending', ?)",
		req.ProductID, req.Quantity, req.DueAt, time.Now())
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func getOrders(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	status := c.QueryParam("status")
	if status == "" {
		status = "pending"
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var orders []Order
	if err := db.Select(&orders, "SELECT * FROM orders WHERE status = ? ORDER BY id DESC LIMIT ?", status, limit); err != nil {
		return err
	}
	if orders == nil {
		orders = []Order{}
	}
	return c.JSON(http.StatusOK, map[string]any{"orders": orders})
}

type PlannedJSON struct {
	OrderID   int64     `json:"order_id"`
	MachineID int64     `json:"machine_id"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
}

// postPlan は未計画の受注をすべて設備に割り当てる。
// 部材が足りない受注は計画せず skipped として返す（次回以降に再挑戦される）。
func postPlan(c echo.Context) error {
	u, err := requirePlanner(c)
	if u == nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var lock string
	if err := tx.Get(&lock, "SELECT value FROM settings WHERE name = 'plan_lock' FOR UPDATE"); err != nil {
		return err
	}

	var pending []Order
	if err := tx.Select(&pending, "SELECT * FROM orders WHERE status = 'pending' ORDER BY id"); err != nil {
		return err
	}

	now := time.Now()
	planned := []PlannedJSON{}
	skipped := []int64{}
	for _, o := range pending {
		p, err := planOrder(tx, &o, now)
		if err != nil {
			return err
		}
		if p == nil {
			skipped = append(skipped, o.ID)
			continue
		}
		planned = append(planned, *p)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"assignments": planned, "skipped": skipped})
}

// planOrder は 1 件の受注について部材を引き当て、最も早く終わる設備の末尾に割り当てる。
// 部材が足りなければ nil を返す。
func planOrder(tx *sqlx.Tx, o *Order, now time.Time) (*PlannedJSON, error) {
	var p Product
	if err := tx.Get(&p, "SELECT * FROM products WHERE id = ?", o.ProductID); err != nil {
		return nil, err
	}
	var bom []BOMItem
	if err := tx.Select(&bom, "SELECT * FROM bom WHERE product_id = ?", p.ID); err != nil {
		return nil, err
	}
	for _, b := range bom {
		var stock int64
		if err := tx.Get(&stock, "SELECT stock FROM materials WHERE id = ? FOR UPDATE", b.MaterialID); err != nil {
			return nil, err
		}
		if stock < int64(b.Quantity*o.Quantity) {
			return nil, nil
		}
	}

	var machines []Machine
	if err := tx.Select(&machines, "SELECT * FROM machines WHERE machine_type = ? ORDER BY id", p.MachineType); err != nil {
		return nil, err
	}
	var best *PlannedJSON
	work := time.Duration(o.Quantity*p.UnitSeconds) * time.Second
	for _, m := range machines {
		var as []Assignment
		if err := tx.Select(&as, "SELECT * FROM assignments WHERE machine_id = ?", m.ID); err != nil {
			return nil, err
		}
		ready := now
		lastFamily := ""
		var lastEnd time.Time
		for _, a := range as {
			if a.EndAt.After(lastEnd) {
				lastEnd = a.EndAt
				lastFamily = a.Family
			}
		}
		if lastEnd.After(ready) {
			ready = lastEnd
		}
		dur := work
		if lastFamily != "" && lastFamily != p.Family {
			dur += setupTime
		}
		end := ready.Add(dur)
		if best == nil || end.Before(best.EndAt) {
			best = &PlannedJSON{OrderID: o.ID, MachineID: m.ID, StartAt: ready, EndAt: end}
		}
	}
	if best == nil {
		return nil, errors.New("no machine for the product")
	}

	for _, b := range bom {
		if _, err := tx.Exec("UPDATE materials SET stock = stock - ? WHERE id = ?", b.Quantity*o.Quantity, b.MaterialID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec("INSERT INTO assignments (order_id, machine_id, family, start_at, end_at, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		o.ID, best.MachineID, p.Family, best.StartAt, best.EndAt, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE orders SET status = 'planned' WHERE id = ?", o.ID); err != nil {
		return nil, err
	}
	return best, nil
}
