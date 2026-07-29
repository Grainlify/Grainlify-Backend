package handlers

import (
	"context"
	"sync"

	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// maxConcurrentRepoFetches bounds how many GitHub repo-metadata requests
// fetchReposConcurrently issues in parallel for a single caller, so a user
// with many owned/led projects doesn't fan out an unbounded number of
// simultaneous upstream requests.
const maxConcurrentRepoFetches = 5

// repoFetchResult is one fullName's github.Client.GetRepo outcome.
type repoFetchResult struct {
	repo github.Repo
	err  error
}

// fetchReposConcurrently fetches GitHub repo metadata for every entry in
// fullNames concurrently (bounded by maxConcurrentRepoFetches), reusing gh's
// existing rate-limited/retrying transport for every call so this doesn't
// bypass the per-token rate-limit handling a sequential loop would already
// respect. Returns a map keyed by full name so callers can look up each
// row's result after re-iterating their own DB rows, regardless of fetch
// completion order.
//
// Callers with N project rows should collect every row's full name first,
// call this once with the complete list, then do a second pass over their
// rows to build the response -- replacing the old one-fetch-per-row-inline
// pattern that made latency and GitHub rate-limit consumption scale
// linearly with N (see issue #290).
func fetchReposConcurrently(ctx context.Context, gh *github.Client, accessToken string, fullNames []string) map[string]repoFetchResult {
	results := make(map[string]repoFetchResult, len(fullNames))
	if len(fullNames) == 0 {
		return results
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentRepoFetches)

	for _, fullName := range fullNames {
		wg.Add(1)
		go func(fullName string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			repo, err := gh.GetRepo(ctx, accessToken, fullName)

			mu.Lock()
			results[fullName] = repoFetchResult{repo: repo, err: err}
			mu.Unlock()
		}(fullName)
	}
	wg.Wait()

	return results
}
