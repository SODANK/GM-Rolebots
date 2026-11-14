package rolebots

import "runtime/debug"

// goSafe is the panic guard for every detached goroutine this module launches.
// Projections run warps, mining sessions and teardown off the guarded lanes, where
// an entity or system can go nil; an unrecovered panic would take the server down,
// so it is recovered and the stack logged. It takes no context or return.
func goSafe(name string, f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rlogf("rolebots: %s panicked: %v\n%s", name, r, debug.Stack())
			}
		}()
		f()
	}()
}

// goSafeBot is goSafe for work that belongs to one pilot: the panic lands in that
// pilot's own log as well as the console.
func (m *Mod) goSafeBot(charID int64, name string, f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rlogf("rolebots: %s (char %d) panicked: %v\n%s", name, charID, r, debug.Stack())
				m.logBot(charID, logError, "sim",
					"panicked during %s: %v — see the console for the stack", name, r)
			}
		}()
		f()
	}()
}
