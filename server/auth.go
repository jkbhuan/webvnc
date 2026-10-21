package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"
)

// AuthConfig configures the password gate.
type AuthConfig struct {
	// Password is the shared secret. An empty password disables the gate; the
	// caller is responsible for warning the operator.
	Password string
	// SessionTTL is how long a login session stays valid.
	SessionTTL time.Duration
	// MaxFailures is the number of consecutive failed attempts from one IP
	// before it is temporarily locked out. Zero disables lockout.
	MaxFailures int
	// LockoutDuration is how long a locked-out IP must wait.
	LockoutDuration time.Duration
}

// Authenticator validates the shared password and issues short-lived session
// tokens so the password itself does not have to travel on every WebSocket
// upgrade.
type Authenticator struct {
	cfg AuthConfig

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	attempts map[string]*attempt  // remote IP -> failure state
}

type attempt struct {
	failures int
	blocked  time.Time
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(cfg AuthConfig) *Authenticator {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 5
	}
	if cfg.LockoutDuration <= 0 {
		cfg.LockoutDuration = 30 * time.Second
	}
	return &Authenticator{
		cfg:      cfg,
		sessions: make(map[string]time.Time),
		attempts: make(map[string]*attempt),
	}
}

// Enabled reports whether a password is required.
func (a *Authenticator) Enabled() bool { return a.cfg.Password != "" }

// Login verifies a password and, on success, returns a fresh session token.
// remaining is the lockout time left when the attempt is rejected as locked.
//
// remoteAddr is the client's address as reported by the transport; it is
// normalized by lockoutKey before any accounting.
func (a *Authenticator) Login(remoteAddr, password string) (token string, expires time.Time, remaining time.Duration, ok bool) {
	if !a.Enabled() {
		// No password configured: hand out a session anyway so the client flow
		// is identical either way.
		t, exp := a.issue()
		return t, exp, 0, true
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.pruneLocked()

	key := lockoutKey(remoteAddr)
	if st, found := a.attempts[key]; found && time.Now().Before(st.blocked) {
		return "", time.Time{}, time.Until(st.blocked), false
	}

	if !equalSecret(password, a.cfg.Password) {
		st := a.attempts[key]
		if st == nil {
			st = &attempt{}
			a.attempts[key] = st
		}
		st.failures++
		if st.failures >= a.cfg.MaxFailures {
			st.blocked = time.Now().Add(a.cfg.LockoutDuration)
			st.failures = 0
		}
		return "", time.Time{}, 0, false
	}

	delete(a.attempts, key)
	t, exp := a.issue()
	return t, exp, 0, true
}

// lockoutKey normalizes a client address into the key used for failure
// accounting. Two normalizations matter, and both are about not handing an
// attacker extra buckets to work with:
//
// An IPv4 client on a dual-stack listener may arrive as either "192.168.1.5" or
// its IPv4-mapped form "::ffff:192.168.1.5". They are the same host, so they
// must share one bucket rather than getting two.
//
// An IPv6 client holds an entire /64 or larger. Keying on the single address
// would make the lockout decorative -- four guesses, then move to the next
// address in your own prefix and continue. The whole /64 is locked instead.
func lockoutKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		// Not a form we can classify; key on it verbatim rather than lumping
		// every such client into one shared bucket.
		return addr
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}
	prefix, err := ip.Prefix(64)
	if err != nil {
		return ip.String()
	}
	return prefix.String()
}

// issue mints a session token. The caller must hold a.mu.
func (a *Authenticator) issue() (string, time.Time) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not recoverable in any useful way.
		panic("webvnc: crypto/rand unavailable: " + err.Error())
	}
	tok := hex.EncodeToString(buf)
	exp := time.Now().Add(a.cfg.SessionTTL)
	a.sessions[tok] = exp
	return tok, exp
}

// Validate reports whether a token may open a WebSocket.
//
// A raw password is accepted as well, which is what makes
// `http://host:8339/?token=<password>` work for quick access; it is otherwise
// equivalent to session tokens and is documented as the less safe option.
func (a *Authenticator) Validate(token string) bool {
	if !a.Enabled() {
		return true
	}
	if token == "" {
		return false
	}
	if equalSecret(token, a.cfg.Password) {
		return true
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	exp, found := a.sessions[token]
	if !found {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	return true
}

// pruneLocked drops expired sessions and stale lockout state. The caller must
// hold a.mu.
func (a *Authenticator) pruneLocked() {
	now := time.Now()
	for tok, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, tok)
		}
	}
	for ip, st := range a.attempts {
		if st.failures == 0 && now.After(st.blocked) {
			delete(a.attempts, ip)
		}
	}
}

// equalSecret compares two secrets without leaking length or content through
// timing: both sides are hashed so the comparison always runs over 32 bytes.
func equalSecret(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// GeneratePassword returns a random, human-typeable password used when the
// operator does not supply one.
func GeneratePassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		panic("webvnc: crypto/rand unavailable: " + err.Error())
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}
