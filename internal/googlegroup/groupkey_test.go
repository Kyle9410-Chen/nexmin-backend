package googlegroup

import (
	"errors"
	"fmt"
	"testing"
)

func domainService(domain string) *Service {
	return &Service{domain: domain}
}

func TestQualifyGroupKey(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		key    string
		want   string
	}{
		{"bare name gains the domain", "sdc.nycu.club", "general", "general@sdc.nycu.club"},
		// A full address is somebody's own choice of domain; never rewrite it.
		{"full address is untouched", "sdc.nycu.club", "general@sdc.nycu.club", "general@sdc.nycu.club"},
		{"other domain is untouched", "sdc.nycu.club", "team@example.com", "team@example.com"},
		{"no domain configured", "", "general", "general"},
		{"empty key", "sdc.nycu.club", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domainService(tt.domain).qualifyGroupKey(tt.key); got != tt.want {
				t.Fatalf("qualifyGroupKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestShortGroupKey(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		email  string
		want   string
	}{
		{"own domain is stripped", "sdc.nycu.club", "general@sdc.nycu.club", "general"},
		// Group listing spans every domain in the account, so a group elsewhere keeps
		// its address rather than becoming a key that would not resolve.
		{"other domain is kept whole", "sdc.nycu.club", "team@example.com", "team@example.com"},
		// A suffix match is not enough; the domain must be the whole thing after the @.
		{"lookalike domain is kept whole", "nycu.club", "general@sdc.nycu.club", "general@sdc.nycu.club"},
		{"no domain configured", "", "general@sdc.nycu.club", "general@sdc.nycu.club"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domainService(tt.domain).shortGroupKey(tt.email); got != tt.want {
				t.Fatalf("shortGroupKey(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

// Whatever comes back from ListGroups has to be usable as a group_key on the next
// request, which is only true if shortening and qualifying are exact inverses.
func TestGroupKeyRoundTrips(t *testing.T) {
	s := domainService("sdc.nycu.club")

	for _, email := range []string{"general@sdc.nycu.club", "team@example.com"} {
		if got := s.qualifyGroupKey(s.shortGroupKey(email)); got != email {
			t.Fatalf("round trip of %q produced %q", email, got)
		}
	}
}

func TestShortGroupKeysLeavesNilAlone(t *testing.T) {
	if got := domainService("sdc.nycu.club").shortGroupKeys(nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestWithGroupKeyQualifiesInOneCall(t *testing.T) {
	s := domainService("sdc.nycu.club")

	var seen []string
	got, err := withGroupKey(s, "general", func(key string) (string, error) {
		seen = append(seen, key)
		return key, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "general@sdc.nycu.club" {
		t.Fatalf("got %q, want the qualified key", got)
	}
	if len(seen) != 1 {
		t.Fatalf("expected a single call, got %v", seen)
	}
}

// An immutable group ID has no "@" either, so it gets qualified first and only the
// retry can tell the two apart.
func TestWithGroupKeyRetriesWithTheRawKey(t *testing.T) {
	s := domainService("sdc.nycu.club")
	const immutableID = "03x8ty12abcd"

	var seen []string
	got, err := withGroupKey(s, immutableID, func(key string) (string, error) {
		seen = append(seen, key)
		if key != immutableID {
			return "", fmt.Errorf("%w: %s", ErrGroupNotFound, key)
		}
		return key, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != immutableID {
		t.Fatalf("got %q, want %q", got, immutableID)
	}
	if len(seen) != 2 || seen[0] != immutableID+"@sdc.nycu.club" || seen[1] != immutableID {
		t.Fatalf("expected a qualified attempt then the raw key, got %v", seen)
	}
}

// Member operations report an unknown group as ErrMemberNotFound, because a 404 from
// members.insert cannot say which of the two keys was wrong. The retry has to cover it.
func TestWithGroupKeyRetriesOnMemberNotFound(t *testing.T) {
	s := domainService("sdc.nycu.club")

	calls := 0
	_, err := withGroupKey(s, "03x8ty12abcd", func(key string) (string, error) {
		calls++
		return "", fmt.Errorf("%w: %s", ErrMemberNotFound, key)
	})
	if !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("got %v, want it to wrap ErrMemberNotFound", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// Anything other than "not found" is the real answer; retrying would double the damage
// report and hide the first failure.
func TestWithGroupKeyDoesNotRetryOtherErrors(t *testing.T) {
	s := domainService("sdc.nycu.club")

	calls := 0
	_, err := withGroupKey(s, "general", func(_ string) (string, error) {
		calls++
		return "", ErrInsufficientPermission
	})
	if !errors.Is(err, ErrInsufficientPermission) {
		t.Fatalf("got %v, want ErrInsufficientPermission", err)
	}
	if calls != 1 {
		t.Fatalf("expected a single attempt, got %d", calls)
	}
}

// With no domain configured the qualified and raw keys are identical, so a failure must
// not be retried against the same key.
func TestWithGroupKeyDoesNotRetryWithoutDomain(t *testing.T) {
	s := domainService("")

	calls := 0
	_, err := withGroupKey(s, "general", func(_ string) (string, error) {
		calls++
		return "", ErrGroupNotFound
	})
	if !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("got %v, want ErrGroupNotFound", err)
	}
	if calls != 1 {
		t.Fatalf("expected a single attempt, got %d", calls)
	}
}

// "domain" reads naturally either with or without the leading "@", and neither spelling
// is worth a startup failure. The unconfigured path still has to record it, since the
// service is built before anyone knows whether a key was supplied.
func TestNewServiceNormalizesDomain(t *testing.T) {
	for _, configured := range []string{"sdc.nycu.club", "@sdc.nycu.club", "  sdc.nycu.club  "} {
		s, err := NewService(testLogger(t), Config{Domain: configured})
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", configured, err)
		}
		if s.domain != "sdc.nycu.club" {
			t.Fatalf("domain %q parsed as %q", configured, s.domain)
		}
	}
}

// cachedService is marked configured but has no Google client, so any call that misses
// the cache would panic. That is the point: these tests prove the answers come from
// memory, which is what keeps a cache expiry from costing a burst of Google calls.
//
// The roster reads every group's member list at once, so this is the hot path.
func cachedService(t *testing.T) *Service {
	t.Helper()

	s, err := NewService(testLogger(t), Config{Domain: "sdc.nycu.club"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s.configured = true

	return s
}

// The roster reads every group's member list at once, so a cache expiry there would be
// multiplied by both the group count and the number of callers without a flight.
func TestListMembersServesFromCache(t *testing.T) {
	s := cachedService(t)

	s.memberCache.set("general@sdc.nycu.club", s.memberCache.begin(), []Member{{Email: "alice@example.com", Type: "USER"}})

	got, err := s.ListMembers(t.Context(), "general")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Email != "alice@example.com" {
		t.Fatalf("got %v, want the cached members", got)
	}
}

func TestInvalidateCachesDropsMembers(t *testing.T) {
	s := cachedService(t)

	s.memberCache.set("general@sdc.nycu.club", s.memberCache.begin(), []Member{{Email: "alice@example.com"}})
	s.invalidateCaches()

	if _, ok := s.memberCache.get("general@sdc.nycu.club"); ok {
		t.Fatal("expected the member cache to be cleared")
	}
}
