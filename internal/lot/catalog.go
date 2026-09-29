package lot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// Display states of a published lot, computed by the database clock: the
// stored status alone cannot distinguish "the bidding is running" from "the
// deadline has passed and the result is being determined".
const (
	DisplayStateActive      = "active"      // active before the deadline: the bidding is running
	DisplayStateDetermining = "determining" // active after the deadline: the result is being determined
	DisplayStateFinished    = "finished"    // the worker has recorded the result
)

// Page sizes of the participant reads. The catalog and the bid history fetch
// one extra row to detect the next page without a separate COUNT query.
const (
	CatalogPageSize    = 20
	BidHistoryPageSize = 10
)

// displayStates is the closed set of the catalog filter values; anything else
// is treated as "no state filter".
var displayStates = map[string]struct{}{
	DisplayStateActive:      {},
	DisplayStateDetermining: {},
	DisplayStateFinished:    {},
}

// NormalizeState maps a submitted state filter to the closed set of display
// states: an unknown or empty value filters nothing.
func NormalizeState(state string) string {
	if _, ok := displayStates[state]; ok {
		return state
	}

	return ""
}

// The catalog and the public lot page never show drafts: a participant sees
// only published lots, an administrator manages drafts through the
// administrative section. The state expressions are computed from the
// database clock (now()), so every replica answers with the same states.
const (
	catalogStateSQL = "CASE WHEN l.status = 'finished' THEN 'finished'" +
		" WHEN l.ends_at <= now() THEN 'determining' ELSE 'active' END"

	listCatalogSQL = "SELECT l.id, l.title, l.category_id, c.name, l.start_price," +
		" GREATEST(l.start_price, COALESCE((SELECT MAX(b.amount) FROM bids b WHERE b.lot_id = l.id), 0))," +
		" " + catalogStateSQL + ", l.ends_at" +
		" FROM lots l JOIN categories c ON c.id = l.category_id" +
		" WHERE l.status <> 'draft'" +
		" AND ($1 = 0 OR l.category_id = $1)" +
		" AND ($2 = '' OR ($2 = 'active' AND l.status = 'active' AND l.ends_at > now())" +
		" OR ($2 = 'determining' AND l.status = 'active' AND l.ends_at <= now())" +
		" OR ($2 = 'finished' AND l.status = 'finished'))" +
		" ORDER BY CASE WHEN l.status = 'active' AND l.ends_at > now() THEN 0" +
		" WHEN l.status = 'active' THEN 1 ELSE 2 END, l.ends_at, l.id" +
		" LIMIT $3 OFFSET $4"

	// One statement answers the whole current state: the lot fields, the
	// maximal accepted bid, the stored result and the database time share one
	// snapshot, so the answer never mixes the price from before a bid with
	// history from after it.
	selectPublicLotSQL = "SELECT l.id, l.title, l.description, l.category_id, c.name, l.start_price," +
		" l.status, l.ends_at," +
		" GREATEST(l.start_price, COALESCE(b.max_amount, 0)), COALESCE(b.max_amount, 0)," +
		" l.winning_bid_id, COALESCE(w.amount, 0), COALESCE(u.login, '')," +
		" " + catalogStateSQL + ", now()" +
		" FROM lots l" +
		" JOIN categories c ON c.id = l.category_id" +
		" LEFT JOIN LATERAL (SELECT MAX(amount) AS max_amount FROM bids WHERE lot_id = l.id) b ON TRUE" +
		" LEFT JOIN bids w ON w.id = l.winning_bid_id" +
		" LEFT JOIN users u ON u.id = w.user_id" +
		" WHERE l.id = $1 AND l.status <> 'draft'"

	// The history is immutable and is read newest first; the pagination is a
	// plain LIMIT/OFFSET over the fixed order.
	listBidsSQL = "SELECT b.id, u.login, b.amount, b.accepted_at" +
		" FROM bids b JOIN users u ON u.id = b.user_id" +
		" WHERE b.lot_id = $1" +
		" ORDER BY b.accepted_at DESC, b.id DESC" +
		" LIMIT $2 OFFSET $3"
)

// CatalogFilter carries the participant catalog filters: an all-categories
// filter is CategoryID 0, an all-states filter is an empty State.
type CatalogFilter struct {
	CategoryID int64
	State      string
	Page       int
}

// CatalogItem is one row of the participant catalog: the current price is
// already the start price or the maximal accepted bid, whichever is greater.
type CatalogItem struct {
	ID           int64
	Title        string
	CategoryID   int64
	CategoryName string
	StartPrice   int64
	CurrentPrice int64
	State        string
	EndsAt       time.Time
}

// PublicLot is the current state of one published lot for the participant
// page and state API. DatabaseNow is the database clock of the snapshot;
// CurrentPrice is GREATEST(start_price, max accepted bid). The result fields
// are zero until the lot is finished with a recorded winning bid.
type PublicLot struct {
	ID                 int64
	Title              string
	Description        string
	CategoryID         int64
	CategoryName       string
	StartPrice         int64
	Status             string
	State              string
	EndsAt             time.Time
	DatabaseNow        time.Time
	CurrentPrice       int64
	MaxBid             int64
	WinningBidID       *int64
	WinningAmount      int64
	WinningParticipant string
}

// MinimumNextBid reports the smallest acceptable next bid and whether a next
// bid is possible at all. Before the first bid it is the start price (the
// first accepted bid may equal it); afterwards it is the current price plus
// one, except when that sum would overflow int64. Closed states never accept
// a next bid.
func (p PublicLot) MinimumNextBid() (int64, bool) {
	if p.State != DisplayStateActive {
		return 0, false
	}
	if p.MaxBid == 0 {
		return p.StartPrice, true
	}
	if p.CurrentPrice == math.MaxInt64 {
		return 0, false
	}

	return p.CurrentPrice + 1, true
}

// CanBid reports whether the state accepts a next bid at all; the final
// decision stays with the bid transaction.
func (p PublicLot) CanBid() bool {
	_, ok := p.MinimumNextBid()

	return ok
}

// Bid is one accepted bid of the public history: only the display fields,
// never the request key or any session data.
type Bid struct {
	ID          int64
	Participant string
	Amount      int64
	AcceptedAt  time.Time
}

// Catalog returns one page of the published lots ordered by urgency: the
// running auctions with the nearest deadlines first, the result determination
// next, the finished lots last. The second return value reports whether a
// next page exists.
func (s *Service) Catalog(ctx context.Context, filter CatalogFilter) ([]CatalogItem, bool, error) {
	page := filter.Page
	if page < 1 {
		page = 1
	}

	rows, err := s.pool.Query(ctx, listCatalogSQL,
		filter.CategoryID, NormalizeState(filter.State), CatalogPageSize+1, (page-1)*CatalogPageSize)
	if err != nil {
		return nil, false, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()

	items := []CatalogItem{}
	for rows.Next() {
		var item CatalogItem
		if err := rows.Scan(&item.ID, &item.Title, &item.CategoryID, &item.CategoryName,
			&item.StartPrice, &item.CurrentPrice, &item.State, &item.EndsAt); err != nil {
			return nil, false, fmt.Errorf("scan catalog lot: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read catalog: %w", err)
	}

	hasMore := len(items) > CatalogPageSize
	if hasMore {
		items = items[:CatalogPageSize]
	}

	return items, hasMore, nil
}

// GetPublicLot loads the current state of one published lot; a missing ID or
// a draft is ErrNotFound, so the direct access answers like any missing lot
// without revealing the draft's existence.
func (s *Service) GetPublicLot(ctx context.Context, id int64) (PublicLot, error) {
	var p PublicLot
	err := s.pool.QueryRow(ctx, selectPublicLotSQL, id).
		Scan(&p.ID, &p.Title, &p.Description, &p.CategoryID, &p.CategoryName,
			&p.StartPrice, &p.Status, &p.EndsAt,
			&p.CurrentPrice, &p.MaxBid,
			&p.WinningBidID, &p.WinningAmount, &p.WinningParticipant,
			&p.State, &p.DatabaseNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicLot{}, ErrNotFound
	}
	if err != nil {
		return PublicLot{}, fmt.Errorf("load public lot: %w", err)
	}

	return p, nil
}

// ListBids returns one page of the bid history, newest first; the second
// return value reports whether a next page exists.
func (s *Service) ListBids(ctx context.Context, lotID int64, page int) ([]Bid, bool, error) {
	if page < 1 {
		page = 1
	}

	rows, err := s.pool.Query(ctx, listBidsSQL, lotID, BidHistoryPageSize+1, (page-1)*BidHistoryPageSize)
	if err != nil {
		return nil, false, fmt.Errorf("list bids: %w", err)
	}
	defer rows.Close()

	bids := []Bid{}
	for rows.Next() {
		var bid Bid
		if err := rows.Scan(&bid.ID, &bid.Participant, &bid.Amount, &bid.AcceptedAt); err != nil {
			return nil, false, fmt.Errorf("scan bid: %w", err)
		}
		bids = append(bids, bid)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read bids: %w", err)
	}

	hasMore := len(bids) > BidHistoryPageSize
	if hasMore {
		bids = bids[:BidHistoryPageSize]
	}

	return bids, hasMore, nil
}
