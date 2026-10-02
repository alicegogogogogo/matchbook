package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// AddSymbol registers a tradable symbol. It returns the created symbol.
func (e *Engine) AddSymbol(name string, priceScale int64) (*Symbol, error) {
	if err := validateSymbolName(name); err != nil {
		return nil, err
	}
	if priceScale < 0 || priceScale > 8 {
		return nil, Invalidf("price_scale must be an integer between 0 and 8")
	}
	if _, exists := e.state.Symbols[name]; exists {
		return nil, Conflictf("symbol %s already exists", name)
	}
	if _, err := e.emit(EventSymbolAdded, map[string]any{
		"symbol":      name,
		"price_scale": priceScale,
	}); err != nil {
		return nil, err
	}
	return e.state.Symbols[name], nil
}

// PlaceOrder validates and matches an order. limitPrice is ignored for market
// orders. explicitID may be empty, in which case a sequence-derived id is used.
func (e *Engine) PlaceOrder(explicitID, symbol, side, orderType string, quantity, limitPrice int64) (OrderView, error) {
	bookSymbol, ok := e.state.Symbols[symbol]
	if !ok {
		return OrderView{}, NotFoundf("symbol %s was not found", symbol)
	}
	if side != SideBuy && side != SideSell {
		return OrderView{}, Invalidf("side must be buy or sell")
	}
	if orderType != TypeLimit && orderType != TypeMarket && orderType != TypeIOC && orderType != TypeFOK {
		return OrderView{}, Invalidf("type must be limit, market, ioc, or fok")
	}
	if quantity <= 0 {
		return OrderView{}, Invalidf("quantity must be a positive integer")
	}
	if quantity > MaxQuantity {
		return OrderView{}, Invalidf("quantity must not exceed %d", MaxQuantity)
	}
	switch orderType {
	case TypeMarket:
		if limitPrice != 0 {
			return OrderView{}, Invalidf("limit_price must be omitted for a market order")
		}
	default:
		if limitPrice <= 0 {
			return OrderView{}, Invalidf("limit_price must be a positive integer for %s orders", orderType)
		}
		if limitPrice > MaxQuantity {
			return OrderView{}, Invalidf("limit_price must not exceed %d ticks", MaxQuantity)
		}
	}
	id := explicitID
	if id == "" {
		id = fmt.Sprintf("ord-%d", len(e.state.Events)+1)
	}
	if err := validateIdentifier(id, "id"); err != nil {
		return OrderView{}, err
	}
	if _, exists := e.state.Orders[id]; exists {
		return OrderView{}, Conflictf("order %s already exists", id)
	}
	data := map[string]any{
		"order_id": id,
		"symbol":   symbol,
		"side":     side,
		"type":     orderType,
		"quantity": quantity,
	}
	if orderType != TypeMarket {
		data["limit_price"] = limitPrice
	}
	if _, err := e.emit(EventOrderAccepted, data); err != nil {
		return OrderView{}, err
	}
	order := e.state.Orders[id]
	e.match(order)
	return orderView(order, bookSymbol, "created"), nil
}

// CancelOrder cancels a live order. Terminal orders are a conflict.
func (e *Engine) CancelOrder(id string) (OrderView, error) {
	order, ok := e.state.Orders[id]
	if !ok {
		return OrderView{}, NotFoundf("order %s was not found", id)
	}
	if order.Status != StatusOpen {
		return OrderView{}, Conflictf("order %s is %s and can no longer be cancelled", id, order.Status)
	}
	if _, err := e.emit(EventOrderCancelled, map[string]any{
		"order_id":        id,
		"filled_quantity": order.Filled,
	}); err != nil {
		return OrderView{}, err
	}
	return orderView(e.state.Orders[id], e.state.Symbols[order.Symbol], "cancelled"), nil
}

// GetOrder returns the public projection of one order.
func (e *Engine) GetOrder(id string) (OrderView, error) {
	order, ok := e.state.Orders[id]
	if !ok {
		return OrderView{}, NotFoundf("order %s was not found", id)
	}
	return orderView(order, e.state.Symbols[order.Symbol], "updated"), nil
}

// BookView is the public order book snapshot.
type BookView struct {
	Symbol     string  `json:"symbol"`
	PriceScale int     `json:"price_scale"`
	Sequence   int64   `json:"sequence"`
	Depth      int     `json:"depth"`
	BestBid    *string `json:"best_bid"`
	BestAsk    *string `json:"best_ask"`
	LastTrade  *string `json:"last_trade_price"`
	BidCount   int     `json:"bid_level_count"`
	AskCount   int     `json:"ask_level_count"`
	OpenOrders int     `json:"open_order_count"`
	Crossed    bool    `json:"crossed"`
	Bids       []Level `json:"bids"`
	Asks       []Level `json:"asks"`
}

// Book returns a depth-limited snapshot for one symbol.
func (e *Engine) Book(symbol string, depth int) (BookView, error) {
	bookSymbol, ok := e.state.Symbols[symbol]
	if !ok {
		return BookView{}, NotFoundf("symbol %s was not found", symbol)
	}
	if depth <= 0 || depth > 100 {
		return BookView{}, Invalidf("depth must be an integer between 1 and 100")
	}
	book, ok := e.state.Books[symbol]
	if !ok {
		return BookView{}, NotFoundf("symbol %s has no book", symbol)
	}
	bids, asks := book.Snapshot(depth)
	view := BookView{
		Symbol:     symbol,
		PriceScale: bookSymbol.PriceScale,
		Sequence:   int64(len(e.state.Events)),
		Depth:      depth,
		BidCount:   len(book.Bids),
		AskCount:   len(book.Asks),
		OpenOrders: len(book.Index),
		Crossed:    bookCrossed(book),
		Bids:       levelViews(bids),
		Asks:       levelViews(asks),
	}
	if bid := book.bestBid(); bid != nil {
		price := ScalePrice(bid.Price, bookSymbol.PriceScale)
		view.BestBid = &price
	}
	if ask := book.bestAsk(); ask != nil {
		price := ScalePrice(ask.Price, bookSymbol.PriceScale)
		view.BestAsk = &price
	}
	for i := len(e.state.Trades) - 1; i >= 0; i-- {
		if e.state.Trades[i].Symbol == symbol {
			price := ScalePrice(e.state.Trades[i].Price, bookSymbol.PriceScale)
			view.LastTrade = &price
			break
		}
	}
	return view, nil
}

// TradePage is one page of the trade tape.
type TradePage struct {
	Symbol     string      `json:"symbol"`
	PriceScale int         `json:"price_scale"`
	Total      int         `json:"total"`
	Offset     int         `json:"offset"`
	Limit      int         `json:"limit"`
	Trades     []TradeView `json:"trades"`
}

// Trades returns the executions of one symbol in sequence order.
func (e *Engine) Trades(symbol string, offset, limit int) (TradePage, error) {
	bookSymbol, ok := e.state.Symbols[symbol]
	if !ok {
		return TradePage{}, NotFoundf("symbol %s was not found", symbol)
	}
	if offset < 0 {
		return TradePage{}, Invalidf("offset must not be negative")
	}
	if limit <= 0 || limit > 1000 {
		return TradePage{}, Invalidf("limit must be an integer between 1 and 1000")
	}
	all := []TradeView{}
	for i := range e.state.Trades {
		if e.state.Trades[i].Symbol != symbol {
			continue
		}
		all = append(all, tradeView(&e.state.Trades[i], bookSymbol))
	}
	page := TradePage{
		Symbol:     symbol,
		PriceScale: bookSymbol.PriceScale,
		Total:      len(all),
		Offset:     offset,
		Limit:      limit,
		Trades:     []TradeView{},
	}
	if offset >= len(all) {
		return page, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page.Trades = all[offset:end]
	return page, nil
}

// Symbols returns registered symbols in the order they were added.
func (e *Engine) Symbols() []Symbol {
	names := make([]string, 0, len(e.state.Symbols))
	for name := range e.state.Symbols {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Symbol, 0, len(names))
	for _, name := range names {
		out = append(out, *e.state.Symbols[name])
	}
	return out
}

// Events returns a copy of the recorded event log.
func (e *Engine) Events() []Event {
	return append([]Event{}, e.state.Events...)
}

// Rebuild replays the event log from the very beginning and returns the state
// it produces. It reads no live structure and no clock, so the result depends
// only on the bytes of the log.
func (e *Engine) Rebuild() (*State, error) {
	return rebuildFromFile(e.path)
}

// ValidateIdempotencyKey normalises an optional Idempotency-Key header value.
func ValidateIdempotencyKey(key string) (string, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > 200 {
		return "", Invalidf("Idempotency-Key must be at most 200 characters")
	}
	return trimmed, nil
}

// Remember returns the response stored for an idempotency key, or stores the
// response produced by action when the key is new.
func Remember[T any](e *Engine, key, operation string, action func() (T, error)) (T, error) {
	var zero T
	if key == "" {
		return action()
	}
	if existing, ok := e.keys[key]; ok {
		if existing.Operation != operation {
			return zero, Conflictf("idempotency key was already used for another operation")
		}
		var cached T
		if err := json.Unmarshal(existing.Response, &cached); err != nil {
			return zero, err
		}
		return cached, nil
	}
	response, err := action()
	if err != nil {
		return zero, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return zero, err
	}
	e.keys[key] = idempotent{Operation: operation, Response: encoded}
	if err := e.saveLedger(); err != nil {
		return zero, err
	}
	return response, nil
}

// saveLedger rewrites the ledger file with sorted keys so the file contents are
// stable across restarts. The ledger is a convenience cache: losing it only
// costs a replayed duplicate request, never engine state.
func (e *Engine) saveLedger() error {
	if e.ledgerPath == "" {
		return nil
	}
	document, err := json.Marshal(e.keys)
	if err != nil {
		return err
	}
	return os.WriteFile(e.ledgerPath, document, 0o644)
}

// emit stamps an event with the injected clock and the next sequence number,
// appends it to the log, and applies it to the live state.
func (e *Engine) emit(eventType string, data map[string]any) (Event, error) {
	event := Event{
		Sequence: int64(len(e.state.Events) + 1),
		At:       e.clock(),
		Type:     eventType,
		Data:     data,
	}
	line, err := json.Marshal(logEntry{
		Sequence: event.Sequence,
		At:       event.At,
		Type:     event.Type,
		Data:     event.Data,
	})
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	if e.file != nil {
		if _, err := e.file.Write(line); err != nil {
			return Event{}, err
		}
		if err := e.file.Sync(); err != nil {
			return Event{}, err
		}
	}
	if err := e.state.Apply(rawEvent{At: event.At, Type: event.Type, Data: event.Data}); err != nil {
		return Event{}, err
	}
	e.state.Events = append(e.state.Events, event)
	return event, nil
}

// match executes an incoming order against the opposite side of the book using
// price-time priority. Limit orders that cannot be filled completely rest with
// their remainder; market orders that cannot be filled completely are rejected.
// An ioc order fills everything the book offers within its limit and is
// cancelled with the unfilled remainder; a fok order trades only when its whole
// quantity is available within its limit, otherwise it is rejected without a
// single trade.
func (e *Engine) match(taker *Order) {
	book := e.state.Books[taker.Symbol]
	if book == nil {
		return
	}
	if taker.Type == TypeFOK && !fokFillable(book, taker) {
		e.rejectOrder(taker, "insufficient_liquidity")
		return
	}
	for taker.Remaining > 0 {
		levels := &book.Asks
		if taker.Side == SideSell {
			levels = &book.Bids
		}
		if len(*levels) == 0 {
			break
		}
		level := (*levels)[0]
		if taker.Type != TypeMarket {
			if taker.Side == SideBuy && level.Price > taker.LimitPrice {
				break
			}
			if taker.Side == SideSell && level.Price < taker.LimitPrice {
				break
			}
		}
		maker := level.Orders[0]
		quantity := taker.Remaining
		if maker.Remaining < quantity {
			quantity = maker.Remaining
		}
		if quantity <= 0 {
			// Defensive: a level holding an empty order would otherwise loop.
			book.clearLevel(level.Price, maker.Side)
			continue
		}
		if err := e.execute(taker, maker, level.Price, quantity); err != nil {
			return
		}
	}
	switch {
	case taker.Remaining == 0:
		return
	case taker.Type == TypeLimit:
		// Drop any stale level entry first: an order that partially traded has
		// already been touched by Apply and must rest exactly once.
		book.remove(taker)
		book.add(taker)
		return
	case taker.Type == TypeIOC:
		// An ioc order never rests: its unfilled remainder is cancelled, while
		// whatever already traded stays traded and visible on the tape.
		e.cancelRemainder(taker)
	default:
		// A market order never rests: any unexecuted remainder is rejected.
		e.rejectOrder(taker, "insufficient_liquidity")
	}
}

// fokFillable reports whether the opposite side can absorb the whole taker
// quantity at prices within its limit, walking the levels in price-time order.
// It mutates nothing, so a negative answer costs not a single trade.
func fokFillable(book *Book, taker *Order) bool {
	levels := book.Asks
	if taker.Side == SideSell {
		levels = book.Bids
	}
	available := int64(0)
	for _, level := range levels {
		if taker.Side == SideBuy && level.Price > taker.LimitPrice {
			break
		}
		if taker.Side == SideSell && level.Price < taker.LimitPrice {
			break
		}
		for _, maker := range level.Orders {
			available += maker.Remaining
			if available >= taker.Remaining {
				return true
			}
		}
	}
	return false
}

// rejectOrder terminates an order as rejected, keeping the quantities it held
// at the end of matching.
func (e *Engine) rejectOrder(order *Order, reason string) {
	_, _ = e.emit(EventOrderRejected, map[string]any{
		"order_id":           order.ID,
		"reason":             reason,
		"unfilled_quantity":  order.Remaining,
		"filled_quantity":    order.Filled,
		"requested_quantity": order.Quantity,
	})
}

// cancelRemainder terminates an ioc order as cancelled with its unfilled
// remainder left in place.
func (e *Engine) cancelRemainder(order *Order) {
	_, _ = e.emit(EventOrderCancelled, map[string]any{
		"order_id":        order.ID,
		"filled_quantity": order.Filled,
	})
}

// execute records one fill between a resting maker and an incoming taker.
//
// The quantities are computed from the state observed before the fill, then
// written as events and applied by State.Apply. The engine never mutates an
// order directly, so the live book and a book rebuilt from the log are the same
// data structure advanced by the same code.
func (e *Engine) execute(taker, maker *Order, price, quantity int64) error {
	tradeID := fmt.Sprintf("trade-%d", len(e.state.Trades)+1)
	if _, err := e.emit(EventTradeExecuted, map[string]any{
		"trade_id":       tradeID,
		"maker_order_id": maker.ID,
		"taker_order_id": taker.ID,
		"price":          price,
		"quantity":       quantity,
	}); err != nil {
		return err
	}
	makerFilled := maker.Filled + quantity
	makerRemaining := maker.Remaining - quantity
	if err := e.emitOrderProgress(maker, makerFilled, makerRemaining, tradeID); err != nil {
		return err
	}
	takerFilled := taker.Filled + quantity
	takerRemaining := taker.Remaining - quantity
	return e.emitOrderProgress(taker, takerFilled, takerRemaining, tradeID)
}

// emitOrderProgress records a fill or, when the order is exhausted, its
// terminal filled state.
func (e *Engine) emitOrderProgress(order *Order, filled, remaining int64, tradeID string) error {
	if remaining > 0 {
		_, err := e.emit(EventOrderUpdated, map[string]any{
			"order_id":           order.ID,
			"filled_quantity":    filled,
			"remaining_quantity": remaining,
			"trade_id":           tradeID,
		})
		return err
	}
	_, err := e.emit(EventOrderFilled, map[string]any{
		"order_id":        order.ID,
		"filled_quantity": filled,
		"trade_id":        tradeID,
	})
	return err
}

// bookCrossed reports whether the best bid is at or above the best ask, which
// the engine treats as an invariant violation.
func bookCrossed(book *Book) bool {
	return BookCrossed(book)
}

// BookCrossed reports whether a book's best bid is at or above its best ask.
// A well formed book never is: it would mean an executable pair was left
// unmatched.
func BookCrossed(book *Book) bool {
	bid, ask := book.bestBid(), book.bestAsk()
	if bid == nil || ask == nil {
		return false
	}
	return bid.Price >= ask.Price
}

// CountOpenOrders counts resting orders for one symbol.
func (e *Engine) CountOpenOrders(symbol string) int {
	book, ok := e.state.Books[symbol]
	if !ok {
		return 0
	}
	return len(book.Index)
}

// Close releases the event log file handle.
func (e *Engine) Close() error {
	if e.file == nil {
		return nil
	}
	err := e.file.Close()
	e.file = nil
	return err
}

// OpenLedger loads the idempotency ledger from disk, if it exists.
func (e *Engine) OpenLedger(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			e.keys = map[string]idempotent{}
			e.ledgerPath = path
			return nil
		}
		return err
	}
	keys := map[string]idempotent{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &keys); err != nil {
			return fmt.Errorf("idempotency ledger is not valid JSON: %w", err)
		}
	}
	e.keys = keys
	e.ledgerPath = path
	return nil
}
