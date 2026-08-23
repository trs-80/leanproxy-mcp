//go:build race

package bouncer

// raceDetectorEnabled lets expensive sweep tests shrink their input space
// when running under -race, where each iteration is ~20x slower.
const raceDetectorEnabled = true
