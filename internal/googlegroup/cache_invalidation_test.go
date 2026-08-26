package googlegroup

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

// A fetch that started before a write must not write its result back afterwards.
// Without the generation check the pre-write list lands in the just-cleared cache and
// is served for a full TTL -- which reads to the user as the change they just made
// being undone.
func TestSetDiscardsResultOfAFetchThatPredatesTheClear(t *testing.T) {
	c := newCache[Member](5 * time.Minute)
	c.suppress = 0 // isolate the generation check from the suppression window

	gen := c.begin()
	c.clear() // a write lands while the fetch is in flight
	c.set("general", gen, []Member{{Email: "pre-write@example.com"}})

	if _, ok := c.get("general"); ok {
		t.Fatal("a pre-write fetch must not repopulate the cache")
	}
}

// Even a fetch that starts after the write is not trustworthy straight away: Google's
// Directory API is eventually consistent and may still answer with the old list.
func TestSetIsSuppressedRightAfterAClear(t *testing.T) {
	c := newCache[Member](5 * time.Minute)
	c.suppress = 50 * time.Millisecond

	c.clear()

	// A fetch begun after the clear, so its generation is current.
	c.set("general", c.begin(), []Member{{Email: "maybe-stale@example.com"}})
	if _, ok := c.get("general"); ok {
		t.Fatal("results must not be stored inside the suppression window")
	}

	time.Sleep(60 * time.Millisecond)

	c.set("general", c.begin(), []Member{{Email: "settled@example.com"}})
	got, ok := c.get("general")
	if !ok {
		t.Fatal("caching must resume once the window closes")
	}
	if got[0].Email != "settled@example.com" {
		t.Fatalf("got %v", got)
	}
}

// Two writes in quick succession extend the window rather than reopening the cache.
func TestClearExtendsTheSuppressionWindow(t *testing.T) {
	c := newCache[Member](5 * time.Minute)
	c.suppress = 80 * time.Millisecond

	c.clear()
	time.Sleep(50 * time.Millisecond)
	c.clear()
	time.Sleep(50 * time.Millisecond) // past the first window, inside the second

	c.set("general", c.begin(), []Member{{Email: "x@example.com"}})
	if _, ok := c.get("general"); ok {
		t.Fatal("the second write must extend the window")
	}
}

// singleflight hands every joiner the in-flight call's result, so a read arriving after
// a write must not be attached to the call that was already running.
func TestInvalidateCachesStartsAFreshFlight(t *testing.T) {
	s := &Service{
		memberCache:  newCache[Member](time.Minute),
		groupCache:   newCache[Group](time.Minute),
		memberFlight: &singleflight.Group{},
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 2)

	wg.Add(1)
	go func() { // arrives before the write
		defer wg.Done()
		v, _, _ := s.flight().Do("general", func() (any, error) {
			close(started)
			<-release
			return "pre-write", nil
		})
		results[0] = v.(string)
	}()
	<-started

	s.invalidateCaches()

	wg.Add(1)
	go func() { // arrives after the write
		defer wg.Done()
		v, _, _ := s.flight().Do("general", func() (any, error) { return "post-write", nil })
		results[1] = v.(string)
	}()

	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()

	if results[1] != "post-write" {
		t.Fatalf("a read arriving after the write got %q; it must not join the pre-write flight", results[1])
	}
}
