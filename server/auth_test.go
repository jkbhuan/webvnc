package server

import (
	"testing"
	"time"
)

func TestLockoutKey(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
	}{
		{"plain IPv4", "192.168.1.5", "192.168.1.5"},
		{"IPv4-mapped IPv6 is the same host", "::ffff:192.168.1.5", "192.168.1.5"},
		{"IPv4 loopback", "127.0.0.1", "127.0.0.1"},
		{
			"global IPv6 collapses to its /64",
			"2001:db8:abcd:1234:a954:c9e2:741a:1847",
			"2001:db8:abcd:1234::/64",
		},
		{
			"a different address in the same /64 shares the key",
			"2001:db8:abcd:1234:5b7a:ef83:cf16:55ba",
			"2001:db8:abcd:1234::/64",
		},
		{
			"an address in a different /64 does not",
			"2001:db8:abcd:1235::1",
			"2001:db8:abcd:1235::/64",
		},
		{"ULA collapses too", "fd00:1234:5678:9abc::5", "fd00:1234:5678:9abc::/64"},
		{"IPv6 loopback", "::1", "::/64"},
		{"a scope zone is stripped", "fe80::1%16", "fe80::/64"},
		{"unparseable input is passed through", "not-an-address", "not-an-address"},
		{"empty input is passed through", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := lockoutKey(c.in); got != c.want {
				t.Errorf("lockoutKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The point of the prefix key: rotating to another address inside your own /64
// must not buy a fresh set of attempts.
func TestLockoutSurvivesRotationWithinAnIPv6Prefix(t *testing.T) {
	auth := NewAuthenticator(AuthConfig{
		Password: "right", MaxFailures: 2, LockoutDuration: time.Minute,
	})

	if _, _, _, ok := auth.Login("2001:db8:abcd:1234:1111::1", "wrong"); ok {
		t.Fatal("a wrong password was accepted")
	}
	if _, _, _, ok := auth.Login("2001:db8:abcd:1234:1111::1", "wrong"); ok {
		t.Fatal("a wrong password was accepted")
	}

	// Two failures from one address have now locked the prefix, so a correct
	// password from a *different* address in the same prefix must be refused.
	_, _, remaining, ok := auth.Login("2001:db8:abcd:1234:2222::2", "right")
	if ok {
		t.Error("rotating to another address in the same /64 dodged the lockout")
	}
	if remaining <= 0 {
		t.Error("rejected without reporting a lockout window")
	}

	// An address in a different /64 is unaffected.
	if _, _, _, ok := auth.Login("2001:db8:abcd:1235::1", "right"); !ok {
		t.Error("an unrelated prefix was locked out too")
	}
}

// The same host must not get two buckets just because one connection arrived
// over IPv4 and another over the dual-stack listener's mapped form.
func TestLockoutTreatsMappedIPv4AsOneHost(t *testing.T) {
	auth := NewAuthenticator(AuthConfig{
		Password: "right", MaxFailures: 2, LockoutDuration: time.Minute,
	})

	auth.Login("192.168.1.5", "wrong")
	auth.Login("::ffff:192.168.1.5", "wrong")

	if _, _, _, ok := auth.Login("192.168.1.5", "right"); ok {
		t.Error("failures from the mapped form did not count against the plain form")
	}
}
