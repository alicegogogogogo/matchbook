// Package server exposes the Matchbook matching engine over HTTP.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"matchbook/internal/engine"
)

const maxBodyBytes = 1 << 20

// API holds the engine behind the HTTP contract.
type API struct {
	engine *engine.Engine
}

// New returns an API over the given engine.
func New(matching *engine.Engine) *API {
	return &API{engine: matching}
}

// Handler returns the HTTP handler for the public contract.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.route)
	return mux
}

// errorBody is the single error envelope used by every endpoint.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type symbolRequest struct {
	Symbol     *string `json:"symbol"`
	PriceScale *int64  `json:"price_scale"`
}

type orderRequest struct {
	ID         *string  `json:"id"`
	Symbol     *string  `json:"symbol"`
	Side       *string  `json:"side"`
	Type       *string  `json:"type"`
	Quantity   *float64 `json:"quantity"`
	LimitPrice *float64 `json:"limit_price"`
}

type replayRequest struct {
	Symbol *string `json:"symbol"`
}

type consistency struct {
	Consistent            bool   `json:"consistent"`
	Symbol                string `json:"symbol"`
	EventCount            int    `json:"event_count"`
	BookMatched           bool   `json:"book_matched"`
	OrdersMatched         bool   `json:"orders_matched"`
	TradesMatched         bool   `json:"trades_matched"`
	BidCount              int    `json:"bid_level_count"`
	AskCount              int    `json:"ask_level_count"`
	OpenOrderCount        int    `json:"open_order_count"`
	RebuiltOpenOrderCount int    `json:"rebuilt_open_order_count"`
	TradeCount            int    `json:"trade_count"`
	RebuiltTradeCount     int    `json:"rebuilt_trade_count"`
}

func (a *API) route(response http.ResponseWriter, request *http.Request) {
	segments := splitPath(request.URL.Path)
	if segments == nil {
		a.writeError(response, invalidf("request path must not contain empty segments"))
		return
	}
	switch {
	case request.Method == http.MethodGet && len(segments) == 1 && segments[0] == "health":
		a.writeJSON(response, http.StatusOK, map[string]any{"status": "ok"})
	case request.Method == http.MethodPost && len(segments) == 1 && segments[0] == "symbols":
		a.writeResult(response, a.createSymbol(request))
	case request.Method == http.MethodGet && len(segments) == 1 && segments[0] == "symbols":
		body := map[string]any{"symbols": a.engine.Symbols()}
		a.writeJSON(response, http.StatusOK, body)
	case request.Method == http.MethodPost && len(segments) == 1 && segments[0] == "orders":
		a.writeResult(response, a.createOrder(request))
	case len(segments) == 2 && segments[0] == "orders" && request.Method == http.MethodDelete:
		a.writeResult(response, a.cancelOrder(request, segments[1]))
	case len(segments) == 1 && segments[0] == "book" && request.Method == http.MethodGet:
		a.writeResult(response, a.getBook(request))
	case len(segments) == 1 && segments[0] == "trades" && request.Method == http.MethodGet:
		a.writeResult(response, a.getTrades(request))
	case len(segments) == 1 && segments[0] == "replay" && request.Method == http.MethodPost:
		a.writeResult(response, a.replay(request))
	default:
		if isKnownPath(segments) {
			response.Header().Set("Allow", allowedMethods(segments, request))
			a.writeError(response, &apiError{
				code:    "method_not_allowed",
				message: fmt.Sprintf("%s is not allowed on this route", request.Method),
				status:  http.StatusMethodNotAllowed,
			})
			return
		}
		a.writeError(response, notFoundf("route %s was not found", request.URL.Path))
	}
}

// isKnownPath reports whether a path exists regardless of the method used.
func isKnownPath(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	switch segments[0] {
	case "health":
		return len(segments) == 1
	case "symbols", "book", "trades", "replay":
		return len(segments) == 1
	case "orders":
		return len(segments) == 1 || len(segments) == 2
	default:
		return false
	}
}

func allowedMethods(segments []string, request *http.Request) string {
	switch segments[0] {
	case "health", "book", "trades":
		return "GET"
	case "symbols":
		return "GET, POST"
	case "orders":
		if len(segments) == 2 {
			return "DELETE"
		}
		return "POST"
	case "replay":
		return "POST"
	default:
		return "GET"
	}
}

// splitPath returns the path segments, an empty slice for "/", or nil when the
// path contains empty segments.
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	if path == "/" {
		return []string{}
	}
	if strings.HasSuffix(path, "/") {
		return nil
	}
	trimmed := strings.TrimPrefix(path, "/")
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if segment == "" {
			return nil
		}
	}
	decoded := make([]string, 0, len(segments))
	for _, segment := range segments {
		value, err := url.PathUnescape(segment)
		if err != nil {
			return nil
		}
		decoded = append(decoded, value)
	}
	return decoded
}

func (a *API) createSymbol(request *http.Request) callResult {
	raw, err := readBody(request)
	if err != nil {
		return callResult{err: err}
	}
	var document symbolRequest
	if err := decodeDocument(raw, &document); err != nil {
		return callResult{err: err}
	}
	if document.Symbol == nil || strings.TrimSpace(*document.Symbol) == "" {
		return callResult{err: invalidf("symbol must be a non-empty string")}
	}
	if document.PriceScale == nil {
		return callResult{err: invalidf("price_scale is required")}
	}
	scale, err := engine.AsInt64(*document.PriceScale, "price_scale")
	if err != nil {
		return callResult{err: err}
	}
	key, err := a.idempotencyKey(request)
	if err != nil {
		return callResult{err: err}
	}
	symbol := strings.TrimSpace(*document.Symbol)
	created, err := engine.Remember(a.engine, key, "create-symbol:"+symbol, func() (*engine.Symbol, error) {
		return a.engine.AddSymbol(symbol, scale)
	})
	if err != nil {
		return callResult{err: err}
	}
	return callResult{status: http.StatusCreated, body: created}
}

func (a *API) createOrder(request *http.Request) callResult {
	raw, err := readBody(request)
	if err != nil {
		return callResult{err: err}
	}
	var document orderRequest
	if err := decodeDocument(raw, &document); err != nil {
		return callResult{err: err}
	}
	if document.Symbol == nil || strings.TrimSpace(*document.Symbol) == "" {
		return callResult{err: invalidf("symbol must be a non-empty string")}
	}
	if document.Side == nil {
		return callResult{err: invalidf("side is required")}
	}
	if document.Type == nil {
		return callResult{err: invalidf("type is required")}
	}
	if document.Quantity == nil {
		return callResult{err: invalidf("quantity is required")}
	}
	symbol := strings.TrimSpace(*document.Symbol)
	side := strings.TrimSpace(*document.Side)
	orderType := strings.TrimSpace(*document.Type)
	if side != engine.SideBuy && side != engine.SideSell {
		return callResult{err: invalidf("side must be buy or sell")}
	}
	if orderType != engine.TypeLimit && orderType != engine.TypeMarket {
		return callResult{err: invalidf("type must be limit or market")}
	}
	quantity, err := engine.AsInt64(*document.Quantity, "quantity")
	if err != nil {
		return callResult{err: err}
	}
	var limitPrice int64
	switch {
	case document.LimitPrice == nil:
		if orderType == engine.TypeLimit {
			return callResult{err: invalidf("limit_price is required for a limit order")}
		}
	case orderType == engine.TypeMarket:
		return callResult{err: invalidf("limit_price must be omitted for a market order")}
	default:
		parsed, err := engine.AsInt64(*document.LimitPrice, "limit_price")
		if err != nil {
			return callResult{err: err}
		}
		limitPrice = parsed
	}
	identifier := ""
	if document.ID != nil {
		identifier = strings.TrimSpace(*document.ID)
	}
	key, err := a.idempotencyKey(request)
	if err != nil {
		return callResult{err: err}
	}
	view, err := engine.Remember(a.engine, key, "place-order:"+symbol, func() (engine.OrderView, error) {
		return a.engine.PlaceOrder(identifier, symbol, side, orderType, quantity, limitPrice)
	})
	if err != nil {
		return callResult{err: err}
	}
	return callResult{status: http.StatusCreated, body: view}
}

func (a *API) cancelOrder(request *http.Request, id string) callResult {
	if _, err := readBody(request); err != nil {
		return callResult{err: err}
	}
	key, err := a.idempotencyKey(request)
	if err != nil {
		return callResult{err: err}
	}
	view, err := engine.Remember(a.engine, key, "cancel-order:"+id, func() (engine.OrderView, error) {
		return a.engine.CancelOrder(id)
	})
	if err != nil {
		return callResult{err: err}
	}
	return callResult{status: http.StatusOK, body: view}
}

func (a *API) getBook(request *http.Request) callResult {
	query, err := queryValues(request.URL.RawQuery, []string{"symbol", "depth"})
	if err != nil {
		return callResult{err: err}
	}
	symbol := strings.TrimSpace(query.Get("symbol"))
	if symbol == "" {
		return callResult{err: invalidf("symbol query parameter is required")}
	}
	depth, err := engine.ParseDepth(query.Get("depth"))
	if err != nil {
		return callResult{err: err}
	}
	book, err := a.engine.Book(symbol, depth)
	if err != nil {
		return callResult{err: err}
	}
	return callResult{status: http.StatusOK, body: book}
}

func (a *API) getTrades(request *http.Request) callResult {
	query, err := queryValues(request.URL.RawQuery, []string{"symbol", "limit", "offset"})
	if err != nil {
		return callResult{err: err}
	}
	symbol := strings.TrimSpace(query.Get("symbol"))
	if symbol == "" {
		return callResult{err: invalidf("symbol query parameter is required")}
	}
	limit, offset, err := engine.ParsePage(query.Get("limit"), query.Get("offset"))
	if err != nil {
		return callResult{err: err}
	}
	page, err := a.engine.Trades(symbol, offset, limit)
	if err != nil {
		return callResult{err: err}
	}
	return callResult{status: http.StatusOK, body: page}
}

// replay rebuilds every book from the event log and compares it with the live
// state field by field.
func (a *API) replay(request *http.Request) callResult {
	raw, err := readBody(request)
	if err != nil {
		return callResult{err: err}
	}
	document := replayRequest{}
	if len(raw) > 0 {
		if err := decodeDocument(raw, &document); err != nil {
			return callResult{err: err}
		}
	}
	rebuilt, err := a.engine.Rebuild()
	if err != nil {
		return callResult{err: err}
	}
	live := a.engine.Events()
	results := make([]consistency, 0)
	symbols := map[string]bool{}
	for _, symbol := range a.engine.Symbols() {
		symbols[symbol.Name] = true
	}
	for name := range rebuilt.Symbols {
		symbols[name] = true
	}
	names := make([]string, 0, len(symbols))
	for name := range symbols {
		names = append(names, name)
	}
	sort.Strings(names)
	if document.Symbol != nil && !symbols[*document.Symbol] {
		return callResult{err: notFoundf("symbol %s was not found", *document.Symbol)}
	}
	for _, name := range names {
		if document.Symbol != nil && *document.Symbol != name {
			continue
		}
		results = append(results, a.compareSymbol(name, rebuilt))
	}
	consistent := len(live) == len(rebuilt.Events)
	for _, result := range results {
		if !result.Consistent {
			consistent = false
		}
	}
	body := map[string]any{
		"consistent":       consistent,
		"live_sequence":    len(live),
		"rebuilt_sequence": len(rebuilt.Events),
		"symbol_count":     len(results),
		"books":            results,
	}
	return callResult{status: http.StatusOK, body: body}
}

func (a *API) compareSymbol(name string, rebuilt *engine.State) consistency {
	result := consistency{Symbol: name, EventCount: len(rebuilt.Events)}
	liveBook, liveErr := a.engine.Book(name, 100)
	rebuiltBook, rebuiltErr := rebuildBookView(rebuilt, name)
	result.BookMatched = liveErr == nil && rebuiltErr == nil && equalBook(liveBook, rebuiltBook)
	result.BidCount = rebuiltBook.BidCount
	result.AskCount = rebuiltBook.AskCount
	result.OpenOrderCount = a.engine.CountOpenOrders(name)
	result.RebuiltOpenOrderCount = rebuiltBook.OpenOrders
	result.OrdersMatched = result.OpenOrderCount == result.RebuiltOpenOrderCount
	liveTrades, liveTradeErr := a.engine.Trades(name, 0, 1000)
	rebuiltTrades, rebuiltTradeErr := rebuiltTradePage(rebuilt, name)
	if liveTradeErr == nil && rebuiltTradeErr == nil {
		result.TradeCount = liveTrades.Total
		result.RebuiltTradeCount = rebuiltTrades.Total
		result.TradesMatched = result.TradeCount == result.RebuiltTradeCount
	}
	result.Consistent = result.BookMatched && result.OrdersMatched && result.TradesMatched
	return result
}

func equalBook(live engine.BookView, rebuilt engine.BookView) bool {
	return live.Symbol == rebuilt.Symbol &&
		live.PriceScale == rebuilt.PriceScale &&
		live.Depth == rebuilt.Depth &&
		equalStringPtr(live.BestBid, rebuilt.BestBid) &&
		equalStringPtr(live.BestAsk, rebuilt.BestAsk) &&
		equalStringPtr(live.LastTrade, rebuilt.LastTrade) &&
		live.BidCount == rebuilt.BidCount &&
		live.AskCount == rebuilt.AskCount &&
		live.OpenOrders == rebuilt.OpenOrders &&
		live.Crossed == rebuilt.Crossed &&
		equalLevels(live.Bids, rebuilt.Bids) &&
		equalLevels(live.Asks, rebuilt.Asks)
}

func equalStringPtr(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalLevels(left, right []engine.Level) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// rebuildBookView derives a book snapshot from replayed state only. It must
// agree with engine.Book field for field, otherwise replay is not consistent.
func rebuildBookView(state *engine.State, symbol string) (engine.BookView, error) {
	book := state.Books[symbol]
	meta := state.Symbols[symbol]
	if book == nil || meta == nil {
		return engine.BookView{}, fmt.Errorf("rebuilt state has no book for %s", symbol)
	}
	view := engine.BookView{
		Symbol:     symbol,
		PriceScale: meta.PriceScale,
		Depth:      100,
		Bids:       levelsFrom(book.Bids, 100),
		Asks:       levelsFrom(book.Asks, 100),
		BidCount:   len(book.Bids),
		AskCount:   len(book.Asks),
		OpenOrders: len(book.Index),
		Crossed:    engine.BookCrossed(book),
	}
	if len(book.Bids) > 0 {
		price := engine.ScalePrice(book.Bids[0].Price, meta.PriceScale)
		view.BestBid = &price
	}
	if len(book.Asks) > 0 {
		price := engine.ScalePrice(book.Asks[0].Price, meta.PriceScale)
		view.BestAsk = &price
	}
	for i := len(state.Trades) - 1; i >= 0; i-- {
		if state.Trades[i].Symbol == symbol {
			price := engine.ScalePrice(state.Trades[i].Price, meta.PriceScale)
			view.LastTrade = &price
			break
		}
	}
	return view, nil
}

func levelsFrom(levels []*engine.LevelState, depth int) []engine.Level {
	out := []engine.Level{}
	for i, level := range levels {
		if i >= depth {
			break
		}
		quantity := int64(0)
		for _, order := range level.Orders {
			quantity += order.Remaining
		}
		out = append(out, engine.Level{Price: level.Price, Quantity: quantity, OrderCount: len(level.Orders)})
	}
	return out
}

func rebuiltTradePage(state *engine.State, symbol string) (engine.TradePage, error) {
	meta, ok := state.Symbols[symbol]
	if !ok {
		return engine.TradePage{}, fmt.Errorf("rebuilt state has no symbol %s", symbol)
	}
	total := 0
	for i := range state.Trades {
		if state.Trades[i].Symbol == symbol {
			total++
		}
	}
	return engine.TradePage{Symbol: symbol, PriceScale: meta.PriceScale, Total: total, Limit: 1000}, nil
}

func (a *API) idempotencyKey(request *http.Request) (string, error) {
	return engine.ValidateIdempotencyKey(request.Header.Get("Idempotency-Key"))
}

func readBody(request *http.Request) ([]byte, error) {
	if request.Body == nil || request.ContentLength == 0 {
		return nil, nil
	}
	if request.ContentLength > maxBodyBytes {
		return nil, invalidf("request body must not exceed %d bytes", maxBodyBytes)
	}
	mediaType := request.Header.Get("Content-Type")
	if index := strings.Index(mediaType, ";"); index >= 0 {
		mediaType = mediaType[:index]
	}
	if strings.TrimSpace(strings.ToLower(mediaType)) != "application/json" {
		return nil, invalidf("Content-Type must be application/json")
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, invalidf("request body could not be read")
	}
	if len(raw) > maxBodyBytes {
		return nil, invalidf("request body must not exceed %d bytes", maxBodyBytes)
	}
	return raw, nil
}

func decodeDocument(raw []byte, target any) error {
	if len(raw) == 0 {
		return invalidf("request body must not be empty")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalidf("request body must be a valid JSON object: %s", err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return invalidf("request body must contain exactly one JSON object")
	}
	return nil
}

// queryValues rejects unknown or repeated query parameters.
func queryValues(rawQuery string, allowed []string) (url.Values, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, invalidf("query string is not valid")
	}
	for name, entries := range values {
		known := false
		for _, candidate := range allowed {
			if name == candidate {
				known = true
			}
		}
		if !known {
			return nil, invalidf("unknown query parameter %s", name)
		}
		if len(entries) != 1 {
			return nil, invalidf("query parameter %s must appear exactly once", name)
		}
	}
	return values, nil
}

func (a *API) writeResult(response http.ResponseWriter, result callResult) {
	if result.err != nil {
		a.writeError(response, result.err)
		return
	}
	a.writeJSON(response, result.status, result.body)
}

// callResult is what every routed handler returns.
type callResult struct {
	status int
	body   any
	err    error
}

func (a *API) writeJSON(response http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		a.writeError(response, fmt.Errorf("response could not be encoded: %w", err))
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	if _, err := response.Write(encoded); err != nil {
		log.Printf("response write failed: %v", err)
	}
}

func (a *API) writeError(response http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "internal server error"
	switch {
	case errors.Is(err, engine.ErrValidation):
		status, code, message = http.StatusBadRequest, "validation_error", err.Error()
	case errors.Is(err, engine.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", err.Error()
	case errors.Is(err, engine.ErrConflict):
		status, code, message = http.StatusConflict, "conflict", err.Error()
	default:
		var typed *apiError
		if errors.As(err, &typed) {
			status, code, message = typed.status, typed.code, typed.message
		}
	}
	a.writeJSON(response, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// apiError is a transport level error that is not part of the engine domain.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func invalidf(format string, args ...any) error {
	return &engine.ValidationError{Message: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) error {
	return &engine.NotFoundError{Message: fmt.Sprintf(format, args...)}
}

// Listen binds the configured address and returns the listener so the caller can
// print the listening line only once the port is actually held.
func Listen(host string, port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
}

// Serve runs the HTTP server until it fails.
func Serve(listener net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.Serve(listener)
}
