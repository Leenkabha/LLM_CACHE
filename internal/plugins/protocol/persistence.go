package protocol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/leenkabha/llm_cache/internal/persistence"
)

// Persistence contract (v1):
//
//	PUT    /v1/entries/{id}   entry JSON {"id","prompt","reply","vector":[..],"created_at"} -> 200/201/204
//	GET    /v1/entries/{id}   -> 200 entry | 404 {"error":{"code":"not_found"}} | 422 {"error":{"code":"invalid_data"}}
//	GET    /v1/entries?cursor=&limit=  -> {"entries":[entry...],"next_cursor":""}
//	DELETE /v1/entries/{id}   -> 200/204 (deleting an unknown id is not an error)
//	GET    /v1/size           -> {"size":N}
//	POST   /v1/flush          -> 200/204
//	GET    /health
//
// Every field of an entry is persisted: the vector store and the eviction
// policy are rebuilt from these entries (ordered by created_at) after a restart.
// The three failure classes are distinct: 404 not_found, 422 invalid_data and
// any 5xx/transport failure (backend failure).

const (
	listPageLimit   = 500
	maxPersistPages = 100_000
)

// Persistence is the remote adapter for persistence.Store.
type Persistence struct{ c *Client }

var (
	_ persistence.Store          = (*Persistence)(nil)
	_ persistence.DetailedLoader = (*Persistence)(nil)
)

func NewPersistence(c *Client) *Persistence {
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > 32<<20 {
		cc.MaxResponse = 32 << 20
	}
	return &Persistence{c: &cc}
}

func (p *Persistence) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.c.timeout())
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func validateEntry(e persistence.Entry) error {
	if !ValidID(e.ID) {
		return errors.New("entry id is blank or not safe as a URL segment")
	}
	for _, x := range e.Vector {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("entry vector contains a non-finite value")
		}
	}
	return nil
}

func (p *Persistence) Save(e persistence.Entry) error {
	if err := validateEntry(e); err != nil {
		return fmt.Errorf("%w: %v", persistence.ErrInvalidData, err)
	}
	ctx, cancel := p.ctx()
	defer cancel()
	if _, err := p.c.Do(ctx, http.MethodPut, "/v1/entries/"+url.PathEscape(e.ID), e, nil); err != nil {
		return fmt.Errorf("%w: save: %v", persistence.ErrBackend, err)
	}
	return nil
}

// LoadDetailed distinguishes not-found, invalid data and backend failure.
func (p *Persistence) LoadDetailed(id string) (persistence.Entry, error) {
	if !ValidID(id) {
		return persistence.Entry{}, persistence.ErrNotFound
	}
	ctx, cancel := p.ctx()
	defer cancel()
	var e persistence.Entry
	_, err := p.c.Do(ctx, http.MethodGet, "/v1/entries/"+url.PathEscape(id), nil, &e)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			switch {
			case se.Status == http.StatusNotFound:
				return persistence.Entry{}, persistence.ErrNotFound
			case se.Status == http.StatusUnprocessableEntity || se.Code == "invalid_data":
				return persistence.Entry{}, persistence.ErrInvalidData
			}
		}
		if errors.Is(err, ErrMalformed) {
			return persistence.Entry{}, persistence.ErrInvalidData
		}
		return persistence.Entry{}, fmt.Errorf("%w: %v", persistence.ErrBackend, err)
	}
	if e.ID != id || validateEntry(e) != nil {
		return persistence.Entry{}, persistence.ErrInvalidData
	}
	return e, nil
}

// Load implements persistence.Store. The orchestrator only needs "usable entry
// or not"; the failure class is logged so an operator can tell them apart.
func (p *Persistence) Load(id string) (persistence.Entry, bool) {
	e, err := p.LoadDetailed(id)
	switch {
	case err == nil:
		return e, true
	case errors.Is(err, persistence.ErrNotFound):
	case errors.Is(err, persistence.ErrInvalidData):
		log.Printf("persistence plugin: entry %s has invalid stored data", id)
	default:
		log.Printf("persistence plugin: load %s failed: %v", id, err)
	}
	return persistence.Entry{}, false
}

type listResponse struct {
	Entries    []persistence.Entry `json:"entries"`
	NextCursor string              `json:"next_cursor"`
}

func (p *Persistence) List() ([]persistence.Entry, error) {
	var all []persistence.Entry
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maxPersistPages; page++ {
		ctx, cancel := p.ctx()
		path := "/v1/entries?limit=" + strconv.Itoa(listPageLimit)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out listResponse
		_, err := p.c.Do(ctx, http.MethodGet, path, nil, &out)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%w: list: %v", persistence.ErrBackend, err)
		}
		for _, e := range out.Entries {
			if err := validateEntry(e); err != nil {
				return nil, fmt.Errorf("%w: list returned entry %q: %v", persistence.ErrInvalidData, e.ID, err)
			}
			if seen[e.ID] {
				return nil, fmt.Errorf("%w: list returned duplicate id %q", persistence.ErrInvalidData, e.ID)
			}
			seen[e.ID] = true
			all = append(all, e)
		}
		if out.NextCursor == "" {
			return all, nil
		}
		if out.NextCursor == cursor {
			return nil, fmt.Errorf("%w: list cursor did not advance", persistence.ErrBackend)
		}
		cursor = out.NextCursor
	}
	return nil, fmt.Errorf("%w: list exceeded %d pages", persistence.ErrBackend, maxPersistPages)
}

func (p *Persistence) Size() (int, error) {
	ctx, cancel := p.ctx()
	defer cancel()
	var out struct {
		Size *int `json:"size"`
	}
	if _, err := p.c.Do(ctx, http.MethodGet, "/v1/size", nil, &out); err != nil {
		return 0, fmt.Errorf("%w: size: %v", persistence.ErrBackend, err)
	}
	if out.Size == nil || *out.Size < 0 {
		return 0, fmt.Errorf("%w: size: %v", persistence.ErrInvalidData, ErrMalformed)
	}
	return *out.Size, nil
}

func (p *Persistence) Delete(id string) error {
	if !ValidID(id) {
		return nil // cannot exist
	}
	ctx, cancel := p.ctx()
	defer cancel()
	if _, err := p.c.Do(ctx, http.MethodDelete, "/v1/entries/"+url.PathEscape(id), nil, nil); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil // idempotent
		}
		return fmt.Errorf("%w: delete: %v", persistence.ErrBackend, err)
	}
	return nil
}

func (p *Persistence) Flush() error {
	ctx, cancel := p.ctx()
	defer cancel()
	if _, err := p.c.Do(ctx, http.MethodPost, "/v1/flush", struct{}{}, nil); err != nil {
		return fmt.Errorf("%w: flush: %v", persistence.ErrBackend, err)
	}
	return nil
}

func (p *Persistence) Health() error {
	ctx, cancel := p.ctx()
	defer cancel()
	return p.c.Health(ctx)
}
