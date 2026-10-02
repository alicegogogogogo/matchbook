package engine_test

import (
	"errors"
	"testing"

	"matchbook/internal/engine"
)

// TestIOCFillsWhatIsAvailableAndCancelsRemainder covers the core ioc contract:
// it walks the book inside its limit at maker prices, never rests, and reports
// cancelled with the unfilled quantity preserved.
func TestIOCFillsWhatIsAvailableAndCancelsRemainder(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 103)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 15, 103)
	if view.Status != engine.StatusCancelled {
		t.Fatalf("a partially filled ioc order must be cancelled: %+v", view)
	}
	if view.FilledQty != 10 || view.RemainingQty != 5 {
		t.Fatalf("filled and remaining must be reported truthfully: %+v", view)
	}
	if view.FilledQty+view.RemainingQty != view.Quantity {
		t.Fatalf("quantity conservation broken: %+v", view)
	}
	if len(view.TradeIDs) != 2 {
		t.Fatalf("ioc order must sweep every reachable level: %+v", view.TradeIDs)
	}

	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 || page.Trades[0].PriceTicks != 100 || page.Trades[1].PriceTicks != 103 {
		t.Fatalf("trades must walk the book in price order at maker prices: %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 0 || book.BidCount != 0 || book.AskCount != 0 || book.Crossed {
		t.Fatalf("an ioc remainder must never enter the book: %+v", book)
	}
	if _, err := matching.CancelOrder("ioc-1"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a cancelled ioc order is terminal, got %v", err)
	}
}

// TestIOCRespectsItsLimitAndLeavesTheRestResting checks that quotes past the
// limit are untouched and the ioc taker still ends cancelled.
func TestIOCRespectsItsLimitAndLeavesTheRestResting(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 5, 1000)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 5, 1060)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 20, 1050)
	if view.Status != engine.StatusCancelled || view.FilledQty != 5 || view.RemainingQty != 15 {
		t.Fatalf("ioc must only trade inside its limit: %+v", view)
	}
	if view.LimitPrice == nil || *view.LimitPrice != "10.50" || view.LimitTicks == nil || *view.LimitTicks != 1050 {
		t.Fatalf("ioc orders must project their limit price: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 1 || page.Trades[0].MakerOrder != "ask-1" {
		t.Fatalf("only the level inside the limit may trade: %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.AskCount != 1 || book.Asks[0].Price != 1060 || book.Asks[0].Quantity != 5 {
		t.Fatalf("the quote past the limit must remain resting untouched: %+v", book.Asks)
	}
}

// TestIOCPartialFillAtOneLevelProvesFIFO makes sure an ioc taker that exhausts
// one maker and taps the next at the same price follows time priority.
func TestIOCPartialFillAtOneLevelProvesFIFO(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 3, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 10, 100)

	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 5, 100)
	if view.Status != engine.StatusFilled || view.FilledQty != 5 || view.RemainingQty != 0 {
		t.Fatalf("enough same-price liquidity means the ioc order fills fully: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 {
		t.Fatalf("expected two fills, got %+v", page.Trades)
	}
	if page.Trades[0].MakerOrder != "ask-1" || page.Trades[0].Quantity != 3 {
		t.Fatalf("oldest same-price order must trade first: %+v", page.Trades[0])
	}
	if page.Trades[1].MakerOrder != "ask-2" || page.Trades[1].Quantity != 2 {
		t.Fatalf("the next order supplies the remainder: %+v", page.Trades[1])
	}
	book := bookOf(t, matching, "ACME")
	if book.Asks[0].Quantity != 8 || book.Asks[0].OrderCount != 1 {
		t.Fatalf("ask-2 must keep its unfilled quantity: %+v", book.Asks)
	}
}

// TestIOCWithNoLiquidityIsCancelledWithZeroFills covers an ioc order that
// crosses nothing: it still terminates as cancelled, produces no trade, and
// leaves the book exactly as it was.
func TestIOCWithNoLiquidityIsCancelledWithZeroFills(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 5, 105)
	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 5, 104)
	if view.Status != engine.StatusCancelled || view.FilledQty != 0 || view.RemainingQty != 5 {
		t.Fatalf("a zero-fill ioc order must be cancelled with quantities intact: %+v", view)
	}
	if len(view.TradeIDs) != 0 {
		t.Fatalf("a non-crossing ioc order must not trade: %+v", view.TradeIDs)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("no trades expected, got %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.AskCount != 1 || book.Asks[0].Quantity != 5 || book.BidCount != 0 {
		t.Fatalf("the resting quote must be untouched: %+v", book)
	}
}

// TestIOCOnSellSideAndFullyFilled exercises the mirrored sell path and the
// fully-filled terminal state.
func TestIOCOnSellSideAndFullyFilled(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 7, 99)
	view := mustOrder(t, matching, "ioc-1", "ACME", engine.SideSell, engine.TypeIOC, 7, 99)
	if view.Status != engine.StatusFilled || view.FilledQty != 7 || view.RemainingQty != 0 {
		t.Fatalf("an ioc order that finds its whole quantity is filled: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 1 || page.Trades[0].PriceTicks != 99 || page.Trades[0].MakerOrder != "bid-1" {
		t.Fatalf("the sell must print at the resting bid price: %+v", page.Trades)
	}
}

// TestFOKFillsAcrossLevelsInOneShot requires the full quantity to be available
// across several price levels and then consumes them all at maker prices.
func TestFOKFillsAcrossLevelsInOneShot(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 103)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 10, 103)
	if view.Status != engine.StatusFilled || view.FilledQty != 10 || view.RemainingQty != 0 {
		t.Fatalf("a fully available fok order must fill in one shot: %+v", view)
	}
	if len(view.TradeIDs) != 2 {
		t.Fatalf("the fok order must span both levels: %+v", view.TradeIDs)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 || page.Trades[0].PriceTicks != 100 || page.Trades[0].Quantity != 6 ||
		page.Trades[1].PriceTicks != 103 || page.Trades[1].Quantity != 4 {
		t.Fatalf("fok must sweep best price then next, at maker prices: %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 0 || book.Crossed {
		t.Fatalf("filled fok must leave no orders behind: %+v", book)
	}
}

// TestFOKRejectsWhenTotalQuantityIsInsufficient is the kill branch: not one
// trade may happen and every resting order stays exactly as it was.
func TestFOKRejectsWhenTotalQuantityIsInsufficient(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 103)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 11, 103)
	if view.Status != engine.StatusRejected {
		t.Fatalf("an unfillable fok order must be rejected: %+v", view)
	}
	if view.FilledQty != 0 || view.RemainingQty != 11 {
		t.Fatalf("a killed fok order must report zero fills and full remainder: %+v", view)
	}
	if len(view.TradeIDs) != 0 {
		t.Fatalf("a killed fok order must produce no trades: %+v", view.TradeIDs)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("no trade may exist after a fok kill: %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 2 || len(book.Asks) != 2 {
		t.Fatalf("resting liquidity must be completely untouched: %+v", book)
	}
	if book.Asks[0].Quantity != 6 || book.Asks[1].Quantity != 4 {
		t.Fatalf("resting quantities must be unchanged: %+v", book.Asks)
	}
	if _, err := matching.CancelOrder("fok-1"); !errors.Is(err, engine.ErrConflict) {
		t.Fatalf("a rejected fok order is terminal, got %v", err)
	}
}

// TestFOKCountsOnlyLiquidityInsideItsLimit makes sure volume past the limit
// cannot rescue a fok order.
func TestFOKCountsOnlyLiquidityInsideItsLimit(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 100)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 10, 106)

	view := mustOrder(t, matching, "fok-1", "ACME", engine.SideBuy, engine.TypeFOK, 10, 105)
	if view.Status != engine.StatusRejected || view.FilledQty != 0 || view.RemainingQty != 10 {
		t.Fatalf("volume past the limit must not count toward a fok fill: %+v", view)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 0 {
		t.Fatalf("a fok kill trades nothing: %+v", page.Trades)
	}
	book := bookOf(t, matching, "ACME")
	if book.OpenOrders != 2 {
		t.Fatalf("both resting orders must survive a fok kill: %+v", book)
	}

	// Once the limit reaches the far level the same quantity fills completely.
	filled := mustOrder(t, matching, "fok-2", "ACME", engine.SideBuy, engine.TypeFOK, 10, 106)
	if filled.Status != engine.StatusFilled || filled.FilledQty != 10 {
		t.Fatalf("raising the limit must allow the full fill: %+v", filled)
	}
}

// TestFOKSellSideKillAndFill mirrors the buy-side checks for a selling taker.
func TestFOKSellSideKillAndFill(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 0)

	mustOrder(t, matching, "bid-1", "ACME", engine.SideBuy, engine.TypeLimit, 4, 97)
	mustOrder(t, matching, "bid-2", "ACME", engine.SideBuy, engine.TypeLimit, 4, 95)

	killed := mustOrder(t, matching, "fok-1", "ACME", engine.SideSell, engine.TypeFOK, 9, 95)
	if killed.Status != engine.StatusRejected || killed.RemainingQty != 9 || killed.FilledQty != 0 {
		t.Fatalf("insufficient bid liquidity must kill the sell fok order: %+v", killed)
	}
	filled := mustOrder(t, matching, "fok-2", "ACME", engine.SideSell, engine.TypeFOK, 8, 95)
	if filled.Status != engine.StatusFilled || filled.FilledQty != 8 {
		t.Fatalf("bids at and above the limit must fill the sell fok order: %+v", filled)
	}
	page, _ := matching.Trades("ACME", 0, 100)
	if page.Total != 2 || page.Trades[0].PriceTicks != 97 || page.Trades[1].PriceTicks != 95 {
		t.Fatalf("the sell walks bids best first at maker prices: %+v", page.Trades)
	}
}

// TestIOCFOKPriceValidation checks every contradictory or missing price case
// and that the new types only accept positive integer limits.
func TestIOCFOKPriceValidation(t *testing.T) {
	matching := newEngine(t)
	mustSymbol(t, matching, "ACME", 2)

	cases := []struct {
		name      string
		orderType string
		price     int64
	}{
		{"ioc without a price", engine.TypeIOC, 0},
		{"fok without a price", engine.TypeFOK, 0},
		{"ioc with a negative price", engine.TypeIOC, -10},
		{"fok with a negative price", engine.TypeFOK, -10},
		{"ioc with an oversized price", engine.TypeIOC, engine.MaxQuantity + 1},
		{"fok with an oversized price", engine.TypeFOK, engine.MaxQuantity + 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := matching.PlaceOrder("", "ACME", engine.SideBuy, testCase.orderType, 1, testCase.price)
			if !errors.Is(err, engine.ErrValidation) {
				t.Fatalf("expected validation_error, got %v", err)
			}
		})
	}
	if _, err := matching.PlaceOrder("", "ACME", engine.SideBuy, "stop-limit", 1, 100); !errors.Is(err, engine.ErrValidation) {
		t.Fatalf("unknown type must be a validation_error, got %v", err)
	}
	if _, err := matching.PlaceOrder("", "NOPE", engine.SideBuy, engine.TypeIOC, 1, 100); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("unknown symbol must stay not_found for ioc, got %v", err)
	}
}

// TestIOCFOKReplayAndReload rebuilds a mixed workload from the event log and
// after a full restart, proving ioc cancellations and fok zero-trade rejects
// are reconstructed exactly, including trade order, quantities and levels.
func TestIOCFOKReplayAndReload(t *testing.T) {
	directory := t.TempDir()
	matching, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	mustSymbol(t, matching, "ACME", 2)

	mustOrder(t, matching, "ask-1", "ACME", engine.SideSell, engine.TypeLimit, 6, 1000)
	mustOrder(t, matching, "ask-2", "ACME", engine.SideSell, engine.TypeLimit, 4, 1030)
	mustOrder(t, matching, "ask-3", "ACME", engine.SideSell, engine.TypeLimit, 8, 1060)

	// ioc: takes 10 across the 1000 and 1030 levels, cancels 5.
	iocView := mustOrder(t, matching, "ioc-1", "ACME", engine.SideBuy, engine.TypeIOC, 15, 1030)
	// fok kill: after the ioc sweep, only the 8 @1060 ask remains, and it is
	// outside limit 1050, so nothing is reachable and the order must reject.
	fokKilled := mustOrder(t, matching, "fok-kill", "ACME", engine.SideBuy, engine.TypeFOK, 10, 1050)
	// fok fill: the 8 @1060 ask is exactly available at limit 1060.
	fokFilled := mustOrder(t, matching, "fok-fill", "ACME", engine.SideBuy, engine.TypeFOK, 8, 1060)

	if iocView.Status != engine.StatusCancelled || fokKilled.Status != engine.StatusRejected || fokFilled.Status != engine.StatusFilled {
		t.Fatalf("unexpected terminal states: ioc=%s fok=%s/%s", iocView.Status, fokKilled.Status, fokFilled.Status)
	}

	live := matching.Events()
	sawIOCCancel, sawFOKReject := false, false
	for _, event := range live {
		switch event.Type {
		case engine.EventOrderCancelled:
			if event.Data["order_id"] == "ioc-1" {
				sawIOCCancel = true
				if filled, _ := event.Data["filled_quantity"].(int64); filled != 10 {
					t.Fatalf("ioc cancel event must carry its partial fill: %+v", event.Data)
				}
			}
		case engine.EventOrderRejected:
			if event.Data["order_id"] == "fok-kill" {
				sawFOKReject = true
				if filled, _ := event.Data["filled_quantity"].(int64); filled != 0 {
					t.Fatalf("fok reject event must record zero fills: %+v", event.Data)
				}
				if unfilled, _ := event.Data["unfilled_quantity"].(int64); unfilled != 10 {
					t.Fatalf("fok reject event must keep the full remainder: %+v", event.Data)
				}
			}
		}
	}
	if !sawIOCCancel || !sawFOKReject {
		t.Fatalf("event log must record both ioc cancel and fok reject: cancel=%v reject=%v", sawIOCCancel, sawFOKReject)
	}

	rebuilt, err := matching.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(live) != len(rebuilt.Events) {
		t.Fatalf("event counts differ: live %d rebuilt %d", len(live), len(rebuilt.Events))
	}
	for i := range live {
		if live[i].Type != rebuilt.Events[i].Type || live[i].Sequence != rebuilt.Events[i].Sequence {
			t.Fatalf("event %d differs after rebuild", i)
		}
	}
	assertRebuiltOrder := func(id string, status string, filled, remaining int64) {
		t.Helper()
		order, ok := rebuilt.Orders[id]
		if !ok {
			t.Fatalf("rebuilt state is missing order %s", id)
		}
		if order.Status != status || order.Filled != filled || order.Remaining != remaining {
			t.Fatalf("rebuilt order %s = %+v, want status=%s filled=%d remaining=%d", id, order, status, filled, remaining)
		}
	}
	assertRebuiltOrder("ioc-1", engine.StatusCancelled, 10, 5)
	assertRebuiltOrder("fok-kill", engine.StatusRejected, 0, 10)
	assertRebuiltOrder("fok-fill", engine.StatusFilled, 8, 0)
	if len(rebuilt.Books["ACME"].Index) != 0 {
		t.Fatalf("every taker above is terminal; rebuilt book must be flat, got %d resting", len(rebuilt.Books["ACME"].Index))
	}
	livePage, _ := matching.Trades("ACME", 0, 1000)
	if livePage.Total != len(rebuilt.Trades) {
		t.Fatalf("trade count differs: live %d rebuilt %d", livePage.Total, len(rebuilt.Trades))
	}
	for i, trade := range rebuilt.Trades {
		liveTrade := livePage.Trades[i]
		if trade.ID != liveTrade.ID || trade.Price != liveTrade.PriceTicks || trade.Quantity != liveTrade.Quantity ||
			trade.MakerID != liveTrade.MakerOrder || trade.TakerID != liveTrade.TakerOrder {
			t.Fatalf("rebuilt trade %d differs: %+v vs %+v", i, trade, liveTrade)
		}
	}

	if err := matching.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reloaded, err := engine.NewEngine(directory, fixedClock())
	if err != nil {
		t.Fatalf("reload NewEngine: %v", err)
	}
	defer reloaded.Close()
	for _, expected := range []struct {
		id             string
		status         string
		filled, remain int64
	}{
		{"ioc-1", engine.StatusCancelled, 10, 5},
		{"fok-kill", engine.StatusRejected, 0, 10},
		{"fok-fill", engine.StatusFilled, 8, 0},
	} {
		view, err := reloaded.GetOrder(expected.id)
		if err != nil {
			t.Fatalf("GetOrder(%s) after reload: %v", expected.id, err)
		}
		if view.Status != expected.status || view.FilledQty != expected.filled || view.RemainingQty != expected.remain {
			t.Fatalf("reloaded order %s = %+v, want status=%s filled=%d remaining=%d", expected.id, view, expected.status, expected.filled, expected.remain)
		}
	}
	reloadedPage, _ := reloaded.Trades("ACME", 0, 1000)
	if reloadedPage.Total != livePage.Total {
		t.Fatalf("trade tape differs after reload: %d vs %d", reloadedPage.Total, livePage.Total)
	}
	if reloaded.CountOpenOrders("ACME") != 0 {
		t.Fatalf("reloaded book must be flat")
	}
}
