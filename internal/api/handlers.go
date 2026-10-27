// Package api is the HTTP layer: requests in, JSON out. No business rules here.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"moonwalk/internal/estimator"
	"moonwalk/internal/order"
	"moonwalk/internal/store"
	"moonwalk/web"
)

const (
	requestTimeout = 5 * time.Second // per request
	maxBodyBytes   = 1 << 20         // 1 MB
)

// New builds the router.
func New(svc *order.Service) *gin.Engine {
	r := gin.New()
	r.Use(recoverJSON, logRequests, limits)

	h := handlers{svc: svc}
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/api/strategies", func(c *gin.Context) { c.JSON(http.StatusOK, estimator.Names()) })

	r.POST("/api/restaurants", h.addRestaurant)
	rest := r.Group("/api/restaurants/:rid")
	{
		rest.GET("", h.restaurant)
		rest.PUT("/strategy", h.setStrategy)

		rest.POST("/orders", h.place)
		rest.GET("/orders", h.kitchen)
		rest.POST("/orders/:oid/start", h.move(svc.Start))
		rest.POST("/orders/:oid/ready", h.move(svc.Ready))
		rest.POST("/orders/:oid/cancel", h.move(svc.Cancel))
		rest.GET("/orders/:oid/estimate", h.compare)
		rest.GET("/orders/:oid/log", h.log)

		rest.GET("/tables/:table/orders", h.table)
	}

	// Pages read the restaurant and table from their URL.
	r.StaticFS("/static", http.FS(web.Static))
	r.GET("/r/:rid/table/:table", page(web.Customer))
	r.GET("/r/:rid/kitchen", page(web.Kitchen))
	return r
}

func page(html []byte) gin.HandlerFunc {
	return func(c *gin.Context) { c.Data(http.StatusOK, "text/html; charset=utf-8", html) }
}

type handlers struct {
	svc *order.Service
}

// POST /api/restaurants (same shape as restaurants.json)
func (h handlers) addRestaurant(c *gin.Context) {
	var r store.Restaurant
	if !bind(c, &r) {
		return
	}
	if err := h.svc.AddRestaurant(c.Request.Context(), r); err != nil {
		fail(c, err)
		return
	}
	h.restaurantByID(c, r.ID, http.StatusCreated)
}

// GET /api/restaurants/:rid
func (h handlers) restaurant(c *gin.Context) {
	h.restaurantByID(c, c.Param("rid"), http.StatusOK)
}

func (h handlers) restaurantByID(c *gin.Context, id string, status int) {
	r, err := h.svc.Restaurant(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(status, r)
}

// PUT /api/restaurants/:rid/strategy {"strategy": "queue"}
func (h handlers) setStrategy(c *gin.Context) {
	var body struct {
		Strategy string `json:"strategy"`
	}
	if !bind(c, &body) {
		return
	}
	if err := h.svc.SetStrategy(c.Request.Context(), c.Param("rid"), body.Strategy); err != nil {
		fail(c, err)
		return
	}
	h.restaurantByID(c, c.Param("rid"), http.StatusOK)
}

// POST /api/restaurants/:rid/orders {table, items: [{menu_item_id, qty}]}
func (h handlers) place(c *gin.Context) {
	var body struct {
		Table string       `json:"table"`
		Items []order.Item `json:"items"`
	}
	if !bind(c, &body) {
		return
	}
	v, err := h.svc.Place(c.Request.Context(), c.Param("rid"), body.Table, body.Items)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}

// GET /api/restaurants/:rid/orders (kitchen screen)
func (h handlers) kitchen(c *gin.Context) {
	views, err := h.svc.KitchenOrders(c.Request.Context(), c.Param("rid"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, views)
}

// GET /api/restaurants/:rid/tables/:table/orders (customer screen)
func (h handlers) table(c *gin.Context) {
	views, err := h.svc.TableOrders(c.Request.Context(), c.Param("rid"), c.Param("table"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, views)
}

// move handles the start/ready/cancel buttons.
func (h handlers) move(action func(context.Context, string, int64) (order.View, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := orderID(c)
		if !ok {
			return
		}
		v, err := action(c.Request.Context(), c.Param("rid"), id)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(http.StatusOK, v)
	}
}

// GET .../orders/:oid/estimate?strategy=simple: another strategy's answer, countdown untouched.
func (h handlers) compare(c *gin.Context) {
	id, ok := orderID(c)
	if !ok {
		return
	}
	name := c.DefaultQuery("strategy", estimator.Default)
	row, err := h.svc.Compare(c.Request.Context(), c.Param("rid"), id, name)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// GET /api/restaurants/:rid/orders/:oid/log
func (h handlers) log(c *gin.Context) {
	id, ok := orderID(c)
	if !ok {
		return
	}
	logs, err := h.svc.Log(c.Request.Context(), c.Param("rid"), id)
	if err != nil {
		fail(c, err)
		return
	}
	if logs == nil {
		logs = []store.EstimateLog{} // [] reads better than null
	}
	c.JSON(http.StatusOK, logs)
}

func orderID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("oid"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order id must be a positive number"})
		return 0, false
	}
	return id, true
}

func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// limits caps each request's time and body size.
func limits(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	c.Next()
}

// logRequests logs only changes and errors; the pages poll too often to log every GET.
func logRequests(c *gin.Context) {
	start := time.Now()
	c.Next()
	status := c.Writer.Status()
	if c.Request.Method == http.MethodGet && status < 400 {
		return
	}
	slog.Info("request", "method", c.Request.Method, "path", c.Request.URL.Path,
		"status", status, "took", time.Since(start))
}
