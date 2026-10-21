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
	"regexp"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const sessionCookieName = "isuticket_session"

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
	cfg.Addr = getEnv("ISUTICKET_DB_HOST", "127.0.0.1") + ":" + getEnv("ISUTICKET_DB_PORT", "3306")
	cfg.User = getEnv("ISUTICKET_DB_USER", "isucon")
	cfg.Passwd = getEnv("ISUTICKET_DB_PASSWORD", "isucon")
	cfg.DBName = getEnv("ISUTICKET_DB_NAME", "isuticket")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.InterpolateParams = false
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
	e.Logger.SetLevel(0)
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/api/initialize", postInitialize)
	e.GET("/api/public-key", getPublicKey)

	e.POST("/api/register", postRegister)
	e.POST("/api/login", postLogin)
	e.POST("/api/logout", postLogout)
	e.GET("/api/me", getMe)

	e.GET("/api/events", getEvents)
	e.GET("/api/events/:id", getEvent)
	e.GET("/api/events/:id/image", getEventImage)
	e.GET("/api/events/:id/seatmap", getSeatmap)
	e.POST("/api/events/:id/purchase", postPurchase)
	e.POST("/api/events/:id/lottery", postLotteryEntry)
	e.GET("/api/events/:id/resales", getResales)

	e.GET("/api/me/lottery", getMyLottery)
	e.GET("/api/me/tickets", getMyTickets)

	e.POST("/api/tickets/:id/resale", postResale)
	e.DELETE("/api/resales/:id", deleteResale)
	e.POST("/api/resales/:id/purchase", postResalePurchase)

	e.GET("/api/admin/venues", getAdminVenues)
	e.POST("/api/admin/events", postAdminEvent)
	e.GET("/api/admin/events/:id/sales", getAdminSales)
	e.POST("/api/gate/verify", postGateVerify)

	e.Static("/", getEnv("ISUTICKET_PUBLIC_DIR", "../public"))

	addr := ":" + getEnv("ISUTICKET_APP_PORT", "8080")
	e.Logger.Fatal(e.Start(addr))
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

// ---- initialize ----

type InitializeRequest struct {
	PaymentURL string `json:"payment_url"`
}

func postInitialize(c echo.Context) error {
	var req InitializeRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}

	out, err := exec.Command("../sql/init.sh").CombinedOutput()
	if err != nil {
		c.Logger().Errorf("init.sh failed: %s", out)
		return errorJSON(c, http.StatusInternalServerError, "failed to initialize")
	}

	if _, err := db.Exec("INSERT INTO settings (name, value) VALUES ('payment_url', ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", req.PaymentURL); err != nil {
		return err
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
	IsOrganizer  bool      `db:"is_organizer" json:"is_organizer"`
	CreatedAt    time.Time `db:"created_at" json:"-"`
}

var loginNamePattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

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

// requireUser は未ログインなら 401 を返す。戻り値が nil のときは既にレスポンス済み。
func requireUser(c echo.Context) (*User, error) {
	u, err := currentUser(c)
	if errors.Is(err, errUnauthorized) {
		return nil, errorJSON(c, http.StatusUnauthorized, "login required")
	}
	return u, err
}

func requireOrganizer(c echo.Context) (*User, error) {
	u, err := requireUser(c)
	if u == nil {
		return nil, err
	}
	if !u.IsOrganizer {
		return nil, errorJSON(c, http.StatusForbidden, "organizer only")
	}
	return u, nil
}

func startSession(c echo.Context, userID int64) error {
	sid := randomHex(64)
	if _, err := db.Exec("INSERT INTO sessions (id, user_id, created_at) VALUES (?, ?, ?)", sid, userID, time.Now()); err != nil {
		return err
	}
	c.SetCookie(&http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

type RegisterRequest struct {
	LoginName   string `json:"login_name"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

func postRegister(c echo.Context) error {
	var req RegisterRequest
	if err := c.Bind(&req); err != nil {
		return errorJSON(c, http.StatusBadRequest, "invalid request")
	}
	if !loginNamePattern.MatchString(req.LoginName) || req.DisplayName == "" || len(req.Password) < 8 {
		return errorJSON(c, http.StatusBadRequest, "invalid parameters")
	}

	var exists int
	if err := db.Get(&exists, "SELECT COUNT(*) FROM users WHERE login_name = ?", req.LoginName); err != nil {
		return err
	}
	if exists > 0 {
		return errorJSON(c, http.StatusConflict, "login_name already taken")
	}

	salt := randomHex(16)
	res, err := db.Exec(
		"INSERT INTO users (login_name, display_name, password_hash, salt, is_organizer, created_at) VALUES (?, ?, ?, ?, 0, ?)",
		req.LoginName, req.DisplayName, hashPassword(salt, req.Password), salt, time.Now(),
	)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return errorJSON(c, http.StatusConflict, "login_name already taken")
		}
		return err
	}
	id, _ := res.LastInsertId()
	if err := startSession(c, id); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, User{ID: id, LoginName: req.LoginName, DisplayName: req.DisplayName})
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
	if err := startSession(c, u.ID); err != nil {
		return err
	}
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

func paramID(c echo.Context, name string) (int64, bool) {
	var id int64
	if _, err := fmt.Sscan(c.Param(name), &id); err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
