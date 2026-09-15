//go:build !race

package bench

// raceDetector is false in an ordinary build; see race_on_test.go.
const raceDetector = false
