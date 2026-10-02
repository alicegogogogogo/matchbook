package server_test

import (
	"net/http"
	"testing"

	"matchbook/internal/engine"
)

// placeLimit is a small helper that rests a limit order over HTTP.
func placeLimit(t *testing.T, handler http.Handler, body string) {
	t.Helper()
	expectStatus(t, send(t, handler, http.MethodPost, "/orders", body, nil), http.StatusCreated)
}

func TestIOCOrderOverHTTPCancelsRemainder(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")
	placeLimit(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":1000}`)
	placeLimit(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":1030}`)

	response := send(t, handler, http.MethodPost, "/orders",
		`{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":15,"limit_price":1030}`,
		map[string]string{"Idempotency-Key": "ioc-1"})
	expectStatus(t, response, http.StatusCreated)
	view := decode[engine.OrderView](t, response)
	if view.Type != "ioc" || view.Status != "cancelled" || view.FilledQty != 10 || view.RemainingQty != 5 {
		t.Fatalf("unexpected ioc projection: %+v", view)
	}
	if view.LimitPrice == nil || *view.LimitPrice != "10.30" || view.LimitTicks == nil || *view.LimitTicks != 1030 {
		t.Fatalf("ioc must expose its scaled limit price: %+v", view)
	}
	if len(view.TradeIDs) != 2 {
		t.Fatalf("ioc must sweep two levels: %+v", view.TradeIDs)
	}

	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 || len(book.Asks) != 0 || len(book.Bids) != 0 {
		t.Fatalf("the ioc remainder must never enter the book: %+v", book)
	}
	page := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if page.Total != 2 || page.Trades[0].PriceTicks != 1000 || page.Trades[1].PriceTicks != 1030 {
		t.Fatalf("ioc trades must be visible at maker prices in order: %+v", page.Trades)
	}
}

func TestFOKOrderOverHTTPFillsOrKills(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	placeLimit(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":100}`)
	placeLimit(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":103}`)

	killed := send(t, handler, http.MethodPost, "/orders",
		`{"id":"fok-kill","symbol":"ACME","side":"buy","type":"fok","quantity":11,"limit_price":103}`,
		map[string]string{"Idempotency-Key": "fok-k"})
	expectStatus(t, killed, http.StatusCreated)
	killedView := decode[engine.OrderView](t, killed)
	if killedView.Type != "fok" || killedView.Status != "rejected" || killedView.FilledQty != 0 || killedView.RemainingQty != 11 {
		t.Fatalf("an unfillable fok order must reject with zero fills: %+v", killedView)
	}
	if killedView.LimitPrice == nil || *killedView.LimitPrice != "103" {
		t.Fatalf("fok must expose its limit price: %+v", killedView)
	}

	// Nothing must have traded on the kill.
	if page := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil)); page.Total != 0 {
		t.Fatalf("a fok kill must leave no trades: %+v", page.Trades)
	}

	filled := send(t, handler, http.MethodPost, "/orders",
		`{"id":"fok-fill","symbol":"ACME","side":"buy","type":"fok","quantity":10,"limit_price":103}`,
		map[string]string{"Idempotency-Key": "fok-f"})
	expectStatus(t, filled, http.StatusCreated)
	filledView := decode[engine.OrderView](t, filled)
	if filledView.Status != "filled" || filledView.FilledQty != 10 || filledView.RemainingQty != 0 || len(filledView.TradeIDs) != 2 {
		t.Fatalf("the available fok order must fill across levels: %+v", filledView)
	}
	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 || book.Crossed {
		t.Fatalf("filled fok must leave a flat, uncrossed book: %+v", book)
	}
}

func TestIOCFOKValidationOverHTTP(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"unknown type", `{"symbol":"ACME","side":"buy","type":"stop","quantity":1,"limit_price":10}`, http.StatusBadRequest, "validation_error"},
		{"ioc missing price", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1}`, http.StatusBadRequest, "validation_error"},
		{"fok missing price", `{"symbol":"ACME","side":"buy","type":"fok","quantity":1}`, http.StatusBadRequest, "validation_error"},
		{"ioc fractional price", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1,"limit_price":1.5}`, http.StatusBadRequest, "validation_error"},
		{"fok string price", `{"symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":"10"}`, http.StatusBadRequest, "validation_error"},
		{"ioc unknown field", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1,"limit_price":10,"tif":"ioc"}`, http.StatusBadRequest, "validation_error"},
		{"ioc unknown symbol", `{"symbol":"NOPE","side":"buy","type":"ioc","quantity":1,"limit_price":10}`, http.StatusNotFound, "not_found"},
		{"fok unknown symbol", `{"symbol":"NOPE","side":"sell","type":"fok","quantity":1,"limit_price":10}`, http.StatusNotFound, "not_found"},
		{"ioc missing quantity", `{"symbol":"ACME","side":"buy","type":"ioc","limit_price":10}`, http.StatusBadRequest, "validation_error"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			expectError(t, send(t, handler, http.MethodPost, "/orders", testCase.body, nil), testCase.status, testCase.code)
		})
	}
	// Market orders still must not carry a price, and the new types stay priced.
	expectError(t, send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"market","quantity":1,"limit_price":10}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"ioc","quantity":1,"limit_price":0}`, nil), http.StatusBadRequest, "validation_error")
	expectError(t, send(t, handler, http.MethodPost, "/orders", `{"symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":-3}`, nil), http.StatusBadRequest, "validation_error")
}

func TestIOCFOKIdempotencyReplaysFirstResult(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":0}`, "")
	placeLimit(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":3,"limit_price":100}`)

	first := send(t, handler, http.MethodPost, "/orders",
		`{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":10,"limit_price":100}`,
		map[string]string{"Idempotency-Key": "ioc-key"})
	expectStatus(t, first, http.StatusCreated)
	firstView := decode[engine.OrderView](t, first)
	if firstView.Status != "cancelled" || firstView.FilledQty != 3 || firstView.RemainingQty != 7 {
		t.Fatalf("unexpected first ioc result: %+v", firstView)
	}

	// Resubmitting the same key, even with a different body, returns the stored
	// projection and must not match again (the book now has zero asks).
	repeat := send(t, handler, http.MethodPost, "/orders",
		`{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":10,"limit_price":100}`,
		map[string]string{"Idempotency-Key": "ioc-key"})
	expectStatus(t, repeat, http.StatusCreated)
	if first.Body.String() != repeat.Body.String() {
		t.Fatalf("repeated key must return the first response:\n%s\n%s", first.Body.String(), repeat.Body.String())
	}
	book := decode[engine.BookView](t, send(t, handler, http.MethodGet, "/book?symbol=ACME", "", nil))
	if book.OpenOrders != 0 || len(book.Asks) != 0 {
		t.Fatalf("the replay must not match a second time: %+v", book)
	}
	page := decode[engine.TradePage](t, send(t, handler, http.MethodGet, "/trades?symbol=ACME", "", nil))
	if page.Total != 1 {
		t.Fatalf("exactly one trade must exist after an idempotent retry: %+v", page.Trades)
	}

	// A fok kill cached under its own key replays as the same rejection.
	killed := send(t, handler, http.MethodPost, "/orders",
		`{"id":"fok-1","symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":100}`,
		map[string]string{"Idempotency-Key": "fok-key"})
	expectStatus(t, killed, http.StatusCreated)
	killedAgain := send(t, handler, http.MethodPost, "/orders",
		`{"id":"fok-1","symbol":"ACME","side":"buy","type":"fok","quantity":1,"limit_price":100}`,
		map[string]string{"Idempotency-Key": "fok-key"})
	if killed.Body.String() != killedAgain.Body.String() {
		t.Fatalf("fok kill must be idempotent:\n%s\n%s", killed.Body.String(), killedAgain.Body.String())
	}
	if view := decode[engine.OrderView](t, killedAgain); view.Status != "rejected" || view.FilledQty != 0 {
		t.Fatalf("cached fok result must stay rejected with zero fills: %+v", view)
	}
}

func TestReplayConsistencyAfterIOCAndFOK(t *testing.T) {
	handler, _ := newAPI(t)
	createSymbol(t, handler, `{"symbol":"ACME","price_scale":2}`, "")
	placeLimit(t, handler, `{"id":"ask-1","symbol":"ACME","side":"sell","type":"limit","quantity":6,"limit_price":1000}`)
	placeLimit(t, handler, `{"id":"ask-2","symbol":"ACME","side":"sell","type":"limit","quantity":4,"limit_price":1030}`)
	placeLimit(t, handler, `{"id":"ask-3","symbol":"ACME","side":"sell","type":"limit","quantity":8,"limit_price":1060}`)

	ioc := send(t, handler, http.MethodPost, "/orders", `{"id":"ioc-1","symbol":"ACME","side":"buy","type":"ioc","quantity":15,"limit_price":1030}`, nil)
	expectStatus(t, ioc, http.StatusCreated)
	killed := send(t, handler, http.MethodPost, "/orders", `{"id":"fok-kill","symbol":"ACME","side":"buy","type":"fok","quantity":10,"limit_price":1050}`, nil)
	expectStatus(t, killed, http.StatusCreated)
	filled := send(t, handler, http.MethodPost, "/orders", `{"id":"fok-fill","symbol":"ACME","side":"buy","type":"fok","quantity":8,"limit_price":1060}`, nil)
	expectStatus(t, filled, http.StatusCreated)
	// A resting limit keeps the book non-empty so open-order rebuild is checked.
	placeLimit(t, handler, `{"id":"ask-rest","symbol":"ACME","side":"sell","type":"limit","quantity":2,"limit_price":1090}`)

	// The terminal ioc/fok orders are observable through the POST projection.
	if view := decode[engine.OrderView](t, ioc); view.Status != "cancelled" || view.FilledQty != 10 || view.RemainingQty != 5 {
		t.Fatalf("ioc projection mismatch: %+v", view)
	}
	if view := decode[engine.OrderView](t, killed); view.Status != "rejected" || view.FilledQty != 0 || view.RemainingQty != 10 {
		t.Fatalf("fok kill projection mismatch: %+v", view)
	}
	if view := decode[engine.OrderView](t, filled); view.Status != "filled" || view.FilledQty != 8 || view.RemainingQty != 0 {
		t.Fatalf("fok fill projection mismatch: %+v", view)
	}

	type replayReport struct {
		Consistent      bool `json:"consistent"`
		LiveSequence    int  `json:"live_sequence"`
		RebuiltSequence int  `json:"rebuilt_sequence"`
		Books           []struct {
			Consistent            bool `json:"consistent"`
			BookMatched           bool `json:"book_matched"`
			OrdersMatched         bool `json:"orders_matched"`
			TradesMatched         bool `json:"trades_matched"`
			OpenOrderCount        int  `json:"open_order_count"`
			RebuiltOpenOrderCount int  `json:"rebuilt_open_order_count"`
			TradeCount            int  `json:"trade_count"`
			RebuiltTradeCount     int  `json:"rebuilt_trade_count"`
		} `json:"books"`
	}

	report := decode[replayReport](t, send(t, handler, http.MethodPost, "/replay", ``, nil))
	if !report.Consistent || report.LiveSequence != report.RebuiltSequence || report.LiveSequence == 0 {
		t.Fatalf("replay must be consistent after ioc/fok traffic: %+v", report)
	}
	for _, book := range report.Books {
		if !book.Consistent || !book.BookMatched || !book.OrdersMatched || !book.TradesMatched {
			t.Fatalf("rebuilt book must match live state: %+v", book)
		}
		if book.OpenOrderCount != 1 || book.TradeCount != 3 {
			t.Fatalf("only the resting limit survives, from three fills (ioc x2 + fok x1): %+v", book)
		}
	}
}
