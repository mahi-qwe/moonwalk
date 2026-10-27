package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"moonwalk/internal/order"
	"moonwalk/internal/store"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st, err := store.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svc := order.New(st)
	err = svc.AddRestaurant(context.Background(), store.Restaurant{
		ID: "moon", Name: "Moon",
		Stations: []store.Station{{ID: "grill", Name: "Grill", Capacity: 2}, {ID: "fryer", Name: "Fryer", Capacity: 1}},
		Menu: []store.MenuItem{
			{ID: "burger", Name: "Burger", StationID: "grill", PrepSeconds: 480},
			{ID: "fries", Name: "Fries", StationID: "fryer", PrepSeconds: 240},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(svc))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

// call sends a request and decodes the JSON answer into out (if not nil).
func call(t *testing.T, method, url, body string, wantStatus int, out any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != wantStatus {
		var e map[string]any
		json.NewDecoder(res.Body).Decode(&e)
		t.Fatalf("%s %s: status %d, want %d (%v)", method, url, res.StatusCode, wantStatus, e)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

// Full journey: order, countdown, start, ready, log.
func TestOrderJourney(t *testing.T) {
	srv := newServer(t)
	base := srv.URL + "/api/restaurants/moon"

	var placed order.View
	call(t, "POST", base+"/orders",
		`{"table":"7","items":[{"menu_item_id":"burger","qty":1},{"menu_item_id":"fries","qty":1}]}`,
		http.StatusCreated, &placed)
	if placed.RemainingSeconds < 470 || placed.RemainingSeconds > 480 {
		t.Errorf("burger + fries in parallel should be ~480s, got %d", placed.RemainingSeconds)
	}

	var table []order.View
	call(t, "GET", base+"/tables/7/orders", "", http.StatusOK, &table)
	if len(table) != 1 || table[0].ID != placed.ID || table[0].EstimatedReadyAt == nil {
		t.Fatalf("table 7 should see its order with an estimate, got %+v", table)
	}

	orderURL := fmt.Sprintf("%s/orders/%d", base, placed.ID)
	var v struct {
		Status store.Status `json:"status"`
	}
	call(t, "POST", orderURL+"/start", "", http.StatusOK, &v)
	if v.Status != store.StatusCooking {
		t.Errorf("after start: %s", v.Status)
	}
	call(t, "POST", orderURL+"/ready", "", http.StatusOK, &v)
	if v.Status != store.StatusReady {
		t.Errorf("after ready: %s", v.Status)
	}

	var kitchen []order.View
	call(t, "GET", base+"/orders", "", http.StatusOK, &kitchen)
	if len(kitchen) != 0 {
		t.Errorf("kitchen should be empty after ready, got %d orders", len(kitchen))
	}

	var logs []store.EstimateLog
	call(t, "GET", orderURL+"/log", "", http.StatusOK, &logs)
	// placed and started; once ready the order is no longer estimated
	if len(logs) != 2 || logs[0].Trigger != "placed" || logs[1].Trigger != "started" {
		t.Errorf("want placed + started log rows, got %+v", logs)
	}
}

func TestErrorsGetTheRightStatus(t *testing.T) {
	srv := newServer(t)
	base := srv.URL + "/api/restaurants/moon"

	call(t, "GET", srv.URL+"/api/restaurants/nope", "", http.StatusNotFound, nil)
	call(t, "POST", base+"/orders", `{not json`, http.StatusBadRequest, nil)
	call(t, "POST", base+"/orders", `{"table":"1","items":[{"menu_item_id":"pizza","qty":1}]}`, http.StatusBadRequest, nil)
	call(t, "POST", base+"/orders/abc/start", "", http.StatusBadRequest, nil)
	call(t, "POST", base+"/orders/42/start", "", http.StatusNotFound, nil)
	call(t, "PUT", base+"/strategy", `{"strategy":"magic"}`, http.StatusBadRequest, nil)

	var placed order.View
	call(t, "POST", base+"/orders", `{"table":"1","items":[{"menu_item_id":"burger","qty":1}]}`, http.StatusCreated, &placed)
	call(t, "POST", fmt.Sprintf("%s/orders/%d/ready", base, placed.ID), "", http.StatusConflict, nil) // not started yet

	dup := `{"id":"moon","name":"Again","stations":[{"id":"g","name":"G","capacity":1}],"menu":[{"id":"x","name":"X","station_id":"g","prep_seconds":60}]}`
	call(t, "POST", srv.URL+"/api/restaurants", dup, http.StatusConflict, nil)
}

func TestCompareDoesNotChangeTheCountdown(t *testing.T) {
	srv := newServer(t)
	base := srv.URL + "/api/restaurants/moon"

	var placed order.View
	call(t, "POST", base+"/orders",
		`{"table":"1","items":[{"menu_item_id":"burger","qty":1},{"menu_item_id":"fries","qty":1}]}`,
		http.StatusCreated, &placed)

	var simple store.EstimateLog
	call(t, "GET", fmt.Sprintf("%s/orders/%d/estimate?strategy=simple", base, placed.ID), "", http.StatusOK, &simple)
	if simple.Algorithm != "simple" || simple.EstimatedSeconds != 720 {
		t.Errorf("simple should say 720s (8+4 min), got %+v", simple)
	}

	var table []order.View
	call(t, "GET", base+"/tables/1/orders", "", http.StatusOK, &table)
	if table[0].RemainingSeconds > 480 {
		t.Errorf("customer countdown changed by a comparison: %ds", table[0].RemainingSeconds)
	}
}
