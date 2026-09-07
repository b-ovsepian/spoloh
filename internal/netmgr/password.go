package netmgr

import (
	"crypto/rand"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
)

// passwordLength is the length of a generated access point password. WPA2
// only requires 8, but that is the floor for accepting a password, not for
// choosing one: 12 is still short enough to type off a screen while leaving
// a wide margin over the minimum.
const passwordLength = 12

// passwordAlphabet is letters and digits with the characters a person
// squinting at a screen in a corridor could confuse for one another removed:
// lowercase l (looks like digit 1 or uppercase I), uppercase I, uppercase O,
// digit 0, and digit 1. It deliberately keeps uppercase L, lowercase i, and
// lowercase o — only the five characters above are the actual confusable
// pairs.
//
// Its length (57) does not divide 256 evenly (256 = 4*57 + 28), so an
// unmodified `crypto/rand` byte reduced with % would draw the first 28
// letters of the alphabet slightly more often than the rest. GenerateAPPassword
// corrects for that with rejection sampling below rather than picking a
// different-length alphabet, so the alphabet stays exactly "letters and
// digits minus the five confusable characters" as decided.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// GenerateAPPassword draws a random access point password from
// passwordAlphabet using crypto/rand. It rejects any drawn byte that would
// introduce modulo bias (see passwordAlphabet) and redraws instead, so every
// character of the result is uniformly distributed over the alphabet.
func GenerateAPPassword() (string, error) {
	// maxUnbiased is the largest multiple of len(passwordAlphabet) that fits
	// in a byte. A drawn byte at or above it is discarded and redrawn; every
	// byte below it maps onto the alphabet with exactly the same number of
	// possible source values, which is what makes the reduction below exact.
	const maxUnbiased = 256 - 256%len(passwordAlphabet)

	pw := make([]byte, passwordLength)
	buf := make([]byte, 1)
	for i := range pw {
		for {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			if int(buf[0]) >= maxUnbiased {
				continue
			}
			pw[i] = passwordAlphabet[int(buf[0])%len(passwordAlphabet)]
			break
		}
	}
	return string(pw), nil
}

// EnsureAPPassword returns the access point password from store, generating
// and persisting one first if the field is still empty — the "not generated
// yet" state that config.Default() ships and config.Validate allows. This
// covers a device installed before Plan 3b's installer exists to print a
// password at install time, and it guarantees the field is never blank by
// the time the supervisor needs it for StartHotspot.
//
// It goes through store.Update so the write is atomic and validated like any
// other configuration change, and it re-checks the field inside the mutator:
// two callers racing to ensure a password at once (a supervisor tick and a
// dashboard request, say) each generate their own candidate, but only the
// first to actually persist under the store's lock wins — the loser's
// mutator finds the field already set and leaves it alone, so every caller
// converges on the same one password rather than one clobbering another's
// already-live hotspot credential.
func EnsureAPPassword(store *configstore.Store) (string, error) {
	if pw := store.Get().Network.APPassword; pw != "" {
		return pw, nil
	}

	pw, err := GenerateAPPassword()
	if err != nil {
		return "", err
	}

	if err := store.Update(func(c *config.Config) error {
		if c.Network.APPassword == "" {
			c.Network.APPassword = pw
		}
		return nil
	}); err != nil {
		return "", err
	}

	// Read back rather than assuming pw won: a concurrent caller may have
	// persisted its own candidate first, and every caller must report the
	// one password that actually ended up on disk.
	return store.Get().Network.APPassword, nil
}
