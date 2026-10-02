package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LogFileName is the append-only event log written inside the data directory.
const LogFileName = "events.jsonl"

// Event is one recorded command or market consequence. Sequence is the 1-based
// position of the event in the log. Data holds the event payload.
type Event struct {
	Sequence int64          `json:"-"`
	At       int64          `json:"at"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data"`
}

// logEntry is the on-disk representation of an event.
type logEntry struct {
	Sequence int64          `json:"sequence"`
	At       int64          `json:"at"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data"`
}

// rawEvent is the payload-only view used by replay, which must never read the
// wall clock or the live sequence counters.
type rawEvent struct {
	At   int64          `json:"at"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// State is the materialised market state derived from an ordered event stream.
type State struct {
	Symbols map[string]*Symbol
	Books   map[string]*Book
	Orders  map[string]*Order
	Trades  []Trade
	Events  []Event
}

// Book is a single symbol's limit order book: price levels on each side plus an
// index of every live (resting) order.
type Book struct {
	Symbol string
	Bids   []*LevelState // descending price
	Asks   []*LevelState // ascending price
	Index  map[string]*Order
}

// LevelState is one price level, holding its resting orders in FIFO order.
type LevelState struct {
	Price  int64
	Orders []*Order
}

// NewState returns an empty market state.
func NewState() *State {
	return &State{
		Symbols: map[string]*Symbol{},
		Books:   map[string]*Book{},
		Orders:  map[string]*Order{},
		Trades:  []Trade{},
		Events:  []Event{},
	}
}

func (s *State) book(symbol string) *Book {
	if book, ok := s.Books[symbol]; ok {
		return book
	}
	book := &Book{Symbol: symbol, Index: map[string]*Order{}}
	s.Books[symbol] = book
	return book
}

// Apply replays a single already-stamped event. It is used by Rebuild and by
// every live mutation, which funnels through the log so that live state and
// replayed state cannot drift apart.
func (s *State) Apply(event rawEvent) error {
	switch event.Type {
	case EventSymbolAdded:
		name, err := stringField(event.Data, "symbol")
		if err != nil {
			return err
		}
		scale, err := intField(event.Data, "price_scale")
		if err != nil {
			return err
		}
		if _, exists := s.Symbols[name]; exists {
			return fmt.Errorf("event log adds symbol %s twice", name)
		}
		s.Symbols[name] = &Symbol{Name: name, PriceScale: int(scale)}
		// A symbol always owns a book, even before its first order, so that the
		// event log alone determines how many books exist.
		s.book(name)
		return nil
	case EventOrderAccepted:
		return s.applyOrderAccepted(event)
	case EventOrderUpdated:
		return s.applyOrderUpdated(event)
	case EventOrderFilled:
		return s.applyOrderFilled(event)
	case EventOrderCancelled:
		return s.applyOrderCancelled(event)
	case EventOrderRejected:
		return s.applyOrderRejected(event)
	case EventTradeExecuted:
		return s.applyTradeExecuted(event)
	default:
		return fmt.Errorf("event log contains unknown event type %q", event.Type)
	}
}

func (s *State) applyOrderAccepted(event rawEvent) error {
	id, err := stringField(event.Data, "order_id")
	if err != nil {
		return err
	}
	symbol, err := stringField(event.Data, "symbol")
	if err != nil {
		return err
	}
	side, err := stringField(event.Data, "side")
	if err != nil {
		return err
	}
	orderType, err := stringField(event.Data, "type")
	if err != nil {
		return err
	}
	quantity, err := intField(event.Data, "quantity")
	if err != nil {
		return err
	}
	if _, exists := s.Orders[id]; exists {
		return fmt.Errorf("event log accepts order %s twice", id)
	}
	if _, exists := s.Symbols[symbol]; !exists {
		return fmt.Errorf("event log accepts order %s for unknown symbol %s", id, symbol)
	}
	price, err := optionalIntField(event.Data, "limit_price")
	if err != nil {
		return err
	}
	order := &Order{
		ID:         id,
		Symbol:     symbol,
		Side:       side,
		Type:       orderType,
		LimitPrice: price,
		Quantity:   quantity,
		Remaining:  quantity,
		Status:     StatusOpen,
		Trades:     []string{},
		Sequence:   int64(len(s.Events) + 1),
		CreatedAt:  event.At,
		UpdatedAt:  event.At,
	}
	s.Orders[id] = order
	book := s.book(symbol)
	book.add(order)
	return nil
}

// applyOrderUpdated and applyOrderFilled consume the quantities recorded in the
// event itself, never the live order, so replay reproduces exactly the values
// that were reported at the time of the fill.
func (s *State) applyOrderUpdated(event rawEvent) error {
	id, err := stringField(event.Data, "order_id")
	if err != nil {
		return err
	}
	filled, err := intField(event.Data, "filled_quantity")
	if err != nil {
		return err
	}
	remaining, err := intField(event.Data, "remaining_quantity")
	if err != nil {
		return err
	}
	order, ok := s.Orders[id]
	if !ok {
		return fmt.Errorf("event log updates unknown order %s", id)
	}
	if filled < 0 || remaining < 0 {
		return fmt.Errorf("event log updates order %s with negative quantities", id)
	}
	order.Filled = filled
	order.Remaining = remaining
	order.Status = StatusOpen
	order.UpdatedAt = event.At
	if remaining == 0 {
		s.book(order.Symbol).remove(order)
	}
	return nil
}

func (s *State) applyOrderFilled(event rawEvent) error {
	id, err := stringField(event.Data, "order_id")
	if err != nil {
		return err
	}
	filled, err := intField(event.Data, "filled_quantity")
	if err != nil {
		return err
	}
	order, ok := s.Orders[id]
	if !ok {
		return fmt.Errorf("event log fills unknown order %s", id)
	}
	order.Filled = filled
	order.Remaining = 0
	order.Status = StatusFilled
	order.UpdatedAt = event.At
	s.book(order.Symbol).remove(order)
	return nil
}

func (s *State) applyOrderCancelled(event rawEvent) error {
	id, err := stringField(event.Data, "order_id")
	if err != nil {
		return err
	}
	filled, err := intField(event.Data, "filled_quantity")
	if err != nil {
		return err
	}
	order, ok := s.Orders[id]
	if !ok {
		return fmt.Errorf("event log cancels unknown order %s", id)
	}
	order.Filled = filled
	if filled < order.Quantity {
		order.Status = StatusCancelled
	}
	order.UpdatedAt = event.At
	s.book(order.Symbol).remove(order)
	return nil
}

func (s *State) applyOrderRejected(event rawEvent) error {
	id, err := stringField(event.Data, "order_id")
	if err != nil {
		return err
	}
	filled, err := intField(event.Data, "filled_quantity")
	if err != nil {
		return err
	}
	order, ok := s.Orders[id]
	if !ok {
		return fmt.Errorf("event log rejects unknown order %s", id)
	}
	order.Filled = filled
	order.Status = StatusRejected
	order.UpdatedAt = event.At
	s.book(order.Symbol).remove(order)
	return nil
}

func (s *State) applyTradeExecuted(event rawEvent) error {
	tradeID, err := stringField(event.Data, "trade_id")
	if err != nil {
		return err
	}
	makerID, err := stringField(event.Data, "maker_order_id")
	if err != nil {
		return err
	}
	takerID, err := stringField(event.Data, "taker_order_id")
	if err != nil {
		return err
	}
	price, err := intField(event.Data, "price")
	if err != nil {
		return err
	}
	quantity, err := intField(event.Data, "quantity")
	if err != nil {
		return err
	}
	maker, ok := s.Orders[makerID]
	if !ok {
		return fmt.Errorf("trade %s references unknown maker %s", tradeID, makerID)
	}
	taker, ok := s.Orders[takerID]
	if !ok {
		return fmt.Errorf("trade %s references unknown taker %s", tradeID, takerID)
	}
	s.Trades = append(s.Trades, Trade{
		ID:         tradeID,
		Sequence:   int64(len(s.Trades) + 1),
		Symbol:     maker.Symbol,
		Price:      price,
		Quantity:   quantity,
		MakerID:    makerID,
		TakerID:    takerID,
		MakerSide:  maker.Side,
		ExecutedAt: event.At,
	})
	maker.Trades = append(maker.Trades, tradeID)
	taker.Trades = append(taker.Trades, tradeID)
	return nil
}

func stringField(data map[string]any, name string) (string, error) {
	value, ok := data[name]
	if !ok {
		return "", fmt.Errorf("event data is missing %s", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("event data field %s must be a string", name)
	}
	return text, nil
}

// numericField extracts an integer from a decoded payload. Values that reached
// the engine through json.Unmarshal arrive as float64; values fed straight from
// the live code path keep their Go integer type.
func numericField(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		if number != float64(int64(number)) {
			return 0, false
		}
		return int64(number), true
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func intField(data map[string]any, name string) (int64, error) {
	value, ok := data[name]
	if !ok {
		return 0, fmt.Errorf("event data is missing %s", name)
	}
	number, ok := numericField(value)
	if !ok {
		return 0, fmt.Errorf("event data field %s must be an integer", name)
	}
	return number, nil
}

func optionalIntField(data map[string]any, name string) (int64, error) {
	value, ok := data[name]
	if !ok || value == nil {
		return 0, nil
	}
	number, ok := numericField(value)
	if !ok {
		return 0, fmt.Errorf("event data field %s must be an integer", name)
	}
	return number, nil
}

// add appends an order to its price level, keeping price order and FIFO order
// within a level.
func (b *Book) add(order *Order) {
	if order.Remaining <= 0 {
		return
	}
	levels, ascending := &b.Asks, true
	if order.Side == SideBuy {
		levels, ascending = &b.Bids, false
	}
	for _, level := range *levels {
		if level.Price == order.LimitPrice {
			level.Orders = append(level.Orders, order)
			b.Index[order.ID] = order
			return
		}
	}
	level := &LevelState{Price: order.LimitPrice, Orders: []*Order{order}}
	*levels = append(*levels, level)
	sortLevels(*levels, ascending)
	b.Index[order.ID] = order
}

func sortLevels(levels []*LevelState, ascending bool) {
	sort.SliceStable(levels, func(i, j int) bool {
		if ascending {
			return levels[i].Price < levels[j].Price
		}
		return levels[i].Price > levels[j].Price
	})
}

// remove takes an order out of the book. Removing an order that is not resting
// is a no-op.
func (b *Book) remove(order *Order) {
	delete(b.Index, order.ID)
	levels := &b.Asks
	if order.Side == SideBuy {
		levels = &b.Bids
	}
	for i, level := range *levels {
		if level.Price != order.LimitPrice {
			continue
		}
		for j, resting := range level.Orders {
			if resting.ID == order.ID {
				level.Orders = append(level.Orders[:j], level.Orders[j+1:]...)
				break
			}
		}
		if len(level.Orders) == 0 {
			*levels = append((*levels)[:i], (*levels)[i+1:]...)
		}
		return
	}
}

// clearLevel empties a whole price level.
func (b *Book) clearLevel(price int64, side string) {
	levels := &b.Asks
	if side == SideBuy {
		levels = &b.Bids
	}
	for i, level := range *levels {
		if level.Price != price {
			continue
		}
		for _, order := range level.Orders {
			delete(b.Index, order.ID)
		}
		*levels = append((*levels)[:i], (*levels)[i+1:]...)
		return
	}
}

// bestAsk and bestBid return the top of book, or nil when that side is empty.
func (b *Book) bestAsk() *LevelState {
	if len(b.Asks) == 0 {
		return nil
	}
	return b.Asks[0]
}

func (b *Book) bestBid() *LevelState {
	if len(b.Bids) == 0 {
		return nil
	}
	return b.Bids[0]
}

// Snapshot returns up to depth levels of each side, best price first.
func (b *Book) Snapshot(depth int) ([]Level, []Level) {
	build := func(levels []*LevelState) []Level {
		out := []Level{}
		for i, level := range levels {
			if i >= depth {
				break
			}
			quantity := int64(0)
			for _, order := range level.Orders {
				quantity += order.Remaining
			}
			out = append(out, Level{Price: level.Price, Quantity: quantity, OrderCount: len(level.Orders)})
		}
		return out
	}
	return build(b.Bids), build(b.Asks)
}

// ledgerFileName stores the Idempotency-Key ledger next to the event log.
const ledgerFileName = "idempotency.json"

// Engine is the live matching service: it owns the derived state, the
// append-only event log, and the idempotency ledger.
type Engine struct {
	state *State
	clock func() int64
	file  *os.File
	path  string

	keys       map[string]idempotent
	ledgerPath string
}

// idempotent remembers the response returned for an Idempotency-Key.
type idempotent struct {
	Operation string          `json:"operation"`
	Response  json.RawMessage `json:"response"`
}

// NewEngine opens (and creates if needed) the event log inside dataDir.
func NewEngine(dataDir string, clock func() int64) (*Engine, error) {
	if clock == nil {
		return nil, fmt.Errorf("clock must not be nil")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	engine := &Engine{
		state:      NewState(),
		clock:      clock,
		path:       filepath.Join(dataDir, LogFileName),
		keys:       map[string]idempotent{},
		ledgerPath: filepath.Join(dataDir, ledgerFileName),
	}
	if _, err := os.Stat(engine.path); err == nil {
		if err := engine.replayFile(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(engine.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	engine.file = file
	if err := engine.OpenLedger(engine.ledgerPath); err != nil {
		return nil, err
	}
	return engine, nil
}

// replayFile rebuilds in-memory state from the on-disk event log.
func (e *Engine) replayFile() error {
	state, err := rebuildFromFile(e.path)
	if err != nil {
		return err
	}
	e.state = state
	return nil
}

// rebuildFromFile replays the event log from the very beginning and returns the
// state it produces. It touches no live counter and reads no clock, so the
// result is a pure function of the log.
func rebuildFromFile(path string) (*State, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(), nil
		}
		return nil, err
	}
	defer file.Close()
	state := NewState()
	if err := scanLog(file, state); err != nil {
		return nil, err
	}
	return state, nil
}

// scanLog applies every line of an event log to state in order, rejecting gaps.
func scanLog(file *os.File, state *State) error {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sequence := int64(0)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("event log line %d is not valid JSON: %w", sequence+1, err)
		}
		sequence++
		if entry.Sequence != sequence {
			return fmt.Errorf("event log is not contiguous at line %d", sequence)
		}
		raw := rawEvent{At: entry.At, Type: entry.Type, Data: entry.Data}
		if err := state.Apply(raw); err != nil {
			return fmt.Errorf("event log line %d: %w", sequence, err)
		}
		state.Events = append(state.Events, Event{
			Sequence: entry.Sequence,
			At:       entry.At,
			Type:     entry.Type,
			Data:     entry.Data,
		})
	}
	return scanner.Err()
}
