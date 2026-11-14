package rolebots

// Stateless draws. Every per-pilot taste, noise and salted pick in the module
// is a pure function of (seed, id) through one finalizer, so nothing needs
// per-entity storage, nothing depends on visit order, and the same pair always
// answers the same number.

// noiseGolden is the odd 64-bit constant every stateless draw in this module
// strides its second argument by: 2^64/phi, splitmix64's own increment. It is defined once so
// every draw stays comparable with the others.
const noiseGolden = 0x9E3779B97F4A7C15

// splitmix64 is the finalizer, on its own so every draw provably shares one
// avalanche instead of copies that can drift. Pure; no state, no seeding
// ritual, no allocation.
func splitmix64(z uint64) uint64 {
	z ^= z >> 30
	z *= 0xBF58476D1CE4E5B9
	z ^= z >> 27
	z *= 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// splitmixUnit is splitmix64 read as a uniform draw in [-1, 1). The shift of
// 11 keeps the top 53 bits, the mantissa of a float64, so every value in the range is
// reachable once.
func splitmixUnit(z uint64) float64 {
	return float64(splitmix64(z)>>11)/float64(1<<52) - 1
}
