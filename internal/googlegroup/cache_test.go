package googlegroup

import (
	"testing"
	"time"
)

func TestCacheReturnsStoredMembers(t *testing.T) {
	c := newCache[Member](time.Minute)
	want := []Member{{Email: "a@example.com", Role: "MEMBER"}}

	c.set("group@example.com", c.begin(), want)

	got, ok := c.get("group@example.com")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 1 || got[0].Email != "a@example.com" {
		t.Fatalf("unexpected members: %+v", got)
	}
}

func TestCacheMissForUnknownKey(t *testing.T) {
	c := newCache[Member](time.Minute)
	c.set("group@example.com", c.begin(), []Member{{Email: "a@example.com"}})

	if _, ok := c.get("other@example.com"); ok {
		t.Fatal("expected cache miss for a different group key")
	}
}

func TestCacheExpiresAndEvicts(t *testing.T) {
	c := newCache[Member](-time.Second) // already expired on write
	c.set("group@example.com", c.begin(), []Member{{Email: "a@example.com"}})

	if _, ok := c.get("group@example.com"); ok {
		t.Fatal("expected expired entry to miss")
	}

	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	if n != 0 {
		t.Fatalf("expected expired entry to be evicted, %d remain", n)
	}
}

// The cache must not hand out the array it is holding: callers sort what they get -- the
// group list is put into organizational order in place before it is rendered -- and a
// caller that reorders the cache reorders it underneath everyone else. That is what made
// the roster attribute people to the wrong mailing list.
func TestCacheCopiesOnTheWayOut(t *testing.T) {
	c := newCache[Member](time.Minute)
	c.set("group@example.com", c.begin(), []Member{{Email: "a@example.com"}, {Email: "b@example.com"}})

	got, _ := c.get("group@example.com")
	got[0] = Member{Email: "clobbered@example.com"}

	again, ok := c.get("group@example.com")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if again[0].Email != "a@example.com" {
		t.Fatalf("a caller mutating its own result changed the cache: %+v", again)
	}
}

// The fetcher returns the same slice it stored, so the cache has to copy on the way in
// too -- otherwise the very first caller still shares the cache's array.
func TestCacheCopiesOnTheWayIn(t *testing.T) {
	c := newCache[Member](time.Minute)
	stored := []Member{{Email: "a@example.com"}, {Email: "b@example.com"}}

	c.set("group@example.com", c.begin(), stored)
	stored[0] = Member{Email: "clobbered@example.com"}

	got, ok := c.get("group@example.com")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got[0].Email != "a@example.com" {
		t.Fatalf("the fetcher mutating its own slice changed the cache: %+v", got)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := newCache[Member](time.Minute)
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				c.set("group@example.com", c.begin(), []Member{{Email: "a@example.com"}})
				c.get("group@example.com")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// The cache is generic, so the same expiry and eviction behaviour must hold for the
// group list, which uses a fixed key rather than a group address.
func TestCacheWorksForGroups(t *testing.T) {
	c := newCache[Group](time.Minute)
	c.set(allGroupsCacheKey, c.begin(), []Group{{Email: "team@example.com", DirectMembersCount: 3}})

	got, ok := c.get(allGroupsCacheKey)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 1 || got[0].DirectMembersCount != 3 {
		t.Fatalf("unexpected groups: %+v", got)
	}
}

func TestCacheExpiresGroups(t *testing.T) {
	c := newCache[Group](-time.Second)
	c.set(allGroupsCacheKey, c.begin(), []Group{{Email: "team@example.com"}})

	if _, ok := c.get(allGroupsCacheKey); ok {
		t.Fatal("expected expired group entry to miss")
	}
}

func TestCacheClearDropsEveryKey(t *testing.T) {
	c := newCache[Member](time.Minute)
	c.set("team@example.com", c.begin(), []Member{{Email: "a@example.com"}})
	c.set("00000000", c.begin(), []Member{{Email: "b@example.com"}})

	c.clear()

	// Both keys must go: a write can name a group by either address, so clearing only
	// the one it saw would leave the other serving a stale roster.
	if _, ok := c.get("team@example.com"); ok {
		t.Fatal("expected the email-keyed entry to be gone")
	}
	if _, ok := c.get("00000000"); ok {
		t.Fatal("expected the id-keyed entry to be gone")
	}
}
