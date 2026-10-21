package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const sessionCookieName = "isufactory_session"

var db *sqlx.DB

// queryer は *sqlx.DB と *sqlx.Tx の両方で使える読み取り用インターフェース。
type queryer interface {
	Get(dest any, query string, args ...any) error
	Select(dest any, query string, args ...any) error
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func connectDB() (*sqlx.DB, error) {
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = getEnv("ISUFACTORY_DB_HOST", "127.0.0.1") + ":" + getEnv("ISUFACTORY_DB_PORT", "3306")
	cfg.User = getEnv("ISUFACTORY_DB_USER", "isucon")
	cfg.Passwd = getEnv("ISUFACTORY_DB_PASSWORD", "isucon")
	cfg.DBName = getEnv("ISUFACTORY_DB_NAME", "isufactory")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	return sqlx.Open("mysql", cfg.FormatDSN())
}

func main() {
	var err error
	db, err = connectDB()
	if err != nil {
		log.Fatalf("failed to connect db: %v", err)
	}
	db.SetMaxOpenConns(10)
	defer db.Close()

	e := echo.New()
	e.Debug = true
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/api/initialize", postInitialize)

	e.POST("/api/login", postLogin)
	e.POST("/api/logout", postLogout)
	e.GET("/api/me", getMe)

	// センサー送信端末
	e.POST("/api/readings", postReadings)

	// 現場担当・計画担当
	e.GET("/api/dashboard", getDashboard)
	e.GET("/api/machines/:id", getMachine)
	e.GET("/api/machines/:id/metrics", getMachineMetrics)
	e.GET("/api/machines/:id/schedule", getMachineSchedule)
	e.GET("/api/alerts", getAlerts)
	e.GET("/api/products", getProducts)
	e.GET("/api/products/:id/drawing", getProductDrawing)
	e.GET("/api/materials", getMaterials)
	e.GET("/api/orders", getOrders)

	// 計画担当のみ
	e.POST("/api/orders", postOrder)
	e.POST("/api/materials/:id/receive", postMaterialReceive)
	e.POST("/api/plans", postPlan)

	e.Static("/", getEnv("ISUFACTORY_PUBLIC_DIR", "../public"))

	e.Logger.Fatal(e.Start(":" + getEnv("ISUFACTORY_APP_PORT", "8080")))
}

// ---- 共通ヘルパー ----

func errorJSON(c echo.Context, status int, msg string) error {
	return c.JSON(status, map[string]string{"error": msg})
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashPassword(salt, password string) string {
	h := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(h[:])
}

func paramID(c echo.Context, name string) (int64, bool) {
	var id int64
	if _, err := fmt.Sscan(c.Param(name), &id); err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ---- initialize ----

func postInitialize(c echo.Context) error {
	out, err := exec.Command("../sql/init.sh").CombinedOutput()
	if err != nil {
		c.Logger().Errorf("init.sh failed: %s", out)
		return errorJSON(c, http.StatusInternalServerError, "failed to initialize")
	}
	return c.JSON(http.StatusOK, map[string]string{"language": "go"})
}

// ---- 認証 ----

type User struct {
	ID           int64     `db:"id" json:"id"`
	LoginName    string    `db:"login_name" json:"login_name"`
	DisplayName  string    `db:"display_name" json:"display_name"`
	PasswordHash string    `db:"password_hash" json:"-"`
	Salt         string    `db:"salt" json:"-"`
	Role         string    `db:"role" json:"role"`
	CreatedAt    time.Time `db:"created_at" json:"-"`
}

var errUnauthorized = errors.New("unauthorized")

func currentUser(c echo.Context) (*User, error) {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errUnauthorized
	}
	var userID int64
	if err := db.Get(&userID, "SELECT user_id FROM sessions WHERE id = ?", cookie.Value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUnauthorized
		}
		return nil, err
	}
	var u User
	if err := db.Get(&u, "SELECT * FROM users WHERE id = ?", userID); err != nil {
		return nil, err
	}
	return &u, nil
}

// requireUser は未ログインなら 401 を返す。戻り値が nil のときは既にレスポンス済み（または err を返す）。
func requireUser(c echo.Context) (*User, error) {
	u, err := currentUser(c)
	if errors.Is(err, errUnauthorized) {
		return nil, errorJSON(c, http.StatusUnauthorized, "login required")
	}
	return u, err
}

func requirePlanner(c echo.Context) (*User, error) {
	u, err := requireUser(c)
	if u == nil {
		return nil, err
	}
	if u.Role != "planner" {
		return nil, errorJSON(c, http.StatusForbidden, "planner only")
	}
	return u, nil
}

type LoginRequest struct {
	LoginName string `json:"login_name"`
	Password  string `json:"password"`
}

func postLogin(c echo.Context) error {
	var req LoginRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}
	var u User
	if err := db.Get(&u, "SELECT * FROM users WHERE login_name = ?", req.LoginName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errorJSON(c, http.StatusUnauthorized, "invalid login_name or password")
		}
		return err
	}
	if hashPassword(u.Salt, req.Password) != u.PasswordHash {
		return errorJSON(c, http.StatusUnauthorized, "invalid login_name or password")
	}
	sid := randomHex(64)
	if _, err := db.Exec("INSERT INTO sessions (id, user_id, created_at) VALUES (?, ?, ?)", sid, u.ID, time.Now()); err != nil {
		return err
	}
	c.SetCookie(&http.Cookie{Name: sessionCookieName, Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return c.JSON(http.StatusOK, u)
}

func postLogout(c echo.Context) error {
	if cookie, err := c.Cookie(sessionCookieName); err == nil {
		if _, err := db.Exec("DELETE FROM sessions WHERE id = ?", cookie.Value); err != nil {
			return err
		}
	}
	c.SetCookie(&http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	return c.NoContent(http.StatusNoContent)
}

func getMe(c echo.Context) error {
	u, err := requireUser(c)
	if u == nil {
		return err
	}
	return c.JSON(http.StatusOK, u)
}
