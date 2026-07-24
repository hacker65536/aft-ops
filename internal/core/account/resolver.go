package account

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hacker65536/aft-ops/internal/cache"
	"github.com/hacker65536/aft-ops/internal/core/model"
)

const cacheKey = "accounts"

// Resolver answers id<->name lookups over the loaded account set.
type Resolver struct {
	accounts []model.Account
	byID     map[string]int

	FetchedAt time.Time // when the data was originally fetched
	FromCache bool      // true when served from cache (surface staleness!)
	Source    string
}

// Load returns a Resolver, from cache when fresh, otherwise fetching from
// src and refilling the cache. refresh forces a fetch.
func Load(ctx context.Context, src Source, store cache.Store, ttl time.Duration, refresh bool) (*Resolver, error) {
	if !refresh {
		if accounts, at, ok := cache.Get[[]model.Account](store, cacheKey, ttl); ok {
			return newResolver(accounts, at, true, src.Name()), nil
		}
	}
	accounts, err := src.Fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch accounts from %s: %w", src.Name(), err)
	}
	if err := cache.Put(store, cacheKey, accounts); err != nil {
		// Non-fatal: the fetched data is still usable this run.
		fmt.Fprintln(os.Stderr, "warning: failed to write account cache:", err)
	}
	return newResolver(accounts, time.Now(), false, src.Name()), nil
}

func newResolver(accounts []model.Account, at time.Time, cached bool, source string) *Resolver {
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	byID := make(map[string]int, len(accounts))
	for i, a := range accounts {
		byID[a.ID] = i
	}
	return &Resolver{
		accounts:  accounts,
		byID:      byID,
		FetchedAt: at,
		FromCache: cached,
		Source:    source,
	}
}

// All returns every known account (sorted by name).
func (r *Resolver) All() []model.Account { return r.accounts }

// ByID returns the account for a 12-digit id, or nil.
func (r *Resolver) ByID(id string) *model.Account {
	if i, ok := r.byID[id]; ok {
		return &r.accounts[i]
	}
	return nil
}

// Match returns accounts whose id equals, or name contains
// (case-insensitive), the query. Exact name matches win over substring
// matches.
func (r *Resolver) Match(query string) []model.Account {
	if a := r.ByID(query); a != nil {
		return []model.Account{*a}
	}
	q := strings.ToLower(query)
	var exact, partial []model.Account
	for _, a := range r.accounts {
		name := strings.ToLower(a.Name)
		switch {
		case name == q:
			exact = append(exact, a)
		case strings.Contains(name, q):
			partial = append(partial, a)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}
