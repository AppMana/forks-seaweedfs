//go:build race

package needle

// The race detector turns off the compiler's elision of
// append([]byte(nil), make([]byte, n)...) inside bytes.Buffer.Grow, so under
// -race every Grow allocates its size twice and allocation counts are not
// the production ones.
const raceEnabled = true
