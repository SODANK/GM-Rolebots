package modapi

import (
	"gurimoon/gang"
	evenet "gurimoon/net"
)

// GangHooks is the player-fleet seam. The fleet registry is a package-main global
// set in main(), and the join notifications are unexported, so main installs entry
// points from init() through InstallGang. Every field may be nil.
type GangHooks struct {
	// Registry returns the process fleet registry. It is a func because globalGangReg
	// is assigned in main(), after init() runs.
	Registry func() *gang.Registry

	// ConnFor resolves a character to its live connection and that character's own
	// accountID. A nil conn means not connected, which is always true for a bot.
	ConnFor func(charID int64) (*evenet.Conn, int64)

	// SysNotify broadcasts a membership change to everyone in the character's solar
	// system (OnCharGangChange, OnGangCorpChange). gangID 0 means left.
	SysNotify func(charID, gangID, corpID int64)

	// PushMail rings a player's inbox for a mail already inserted, since SendEveMail
	// only writes the row. msgID 0 means nothing was written and must not be pushed.
	PushMail func(charID, msgID, senderID int64, subject string, created int64)

	// LockedTargets returns the entity IDs the pilot has locked, copied under the
	// connection state's lock. nil for a pilot who is docked, offline or locking
	// nothing.
	LockedTargets func(charID int64) []uint32
}

var gangHooks GangHooks

// InstallGang publishes the core's fleet entry points. Package main calls it once
// from init(); a later call overwrites.
func InstallGang(h GangHooks) { gangHooks = h }

// Gangs returns the installed hooks. Every field may be nil.
func Gangs() GangHooks { return gangHooks }
