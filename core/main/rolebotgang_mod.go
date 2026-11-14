package main

import (
	"gurimoon/gang"
	"gurimoon/modapi"
	evenet "gurimoon/net"
	"gurimoon/server/gangsta"
)

// init installs the player-fleet seam (modapi.GangHooks, server/lsc/chathook_mod.go).
// Each entry point is a thin closure over package-main state. It installs twice:
// modapi.InstallGang for the module, and gangsta.InstallModuleGang, because gangsta
// cannot import package main.
func init() {
	modapi.InstallGang(modapi.GangHooks{
		Registry:      func() *gang.Registry { return globalGangReg },
		ConnFor:       charConnLookup,
		SysNotify:     modGangSysNotify,
		PushMail:      pushMailNotification,
		LockedTargets: modLockedTargets,
	})

	gangsta.InstallModuleGang(gangsta.ModuleGangDeps{
		Registry:   func() *gang.Registry { return globalGangReg },
		ConnLookup: gangsta.ConnLookup(charConnLookup),
		SysNotify:  gangsta.SysNotify(modGangSysNotify),
	})
}

// modGangSysNotify is broadcastGangSysChange with main's system manager. Nil-safe
// before main wires it: brackets are not repainted.
func modGangSysNotify(charID, gangID, corpID int64) {
	if globalSysMgr == nil {
		return
	}
	broadcastGangSysChange(globalSysMgr, charID, gangID, corpID)
}

// modLockedTargets returns the entity IDs a pilot has locked. The list is
// destinyConn.targets, reachable only through Entity.PlayerState. The copy is taken
// under targetMu, the lock the CONCORD and ball-removal paths hold when they edit it.
func modLockedTargets(charID int64) []uint32 {
	if globalSysMgr == nil || charID <= 0 {
		return nil
	}
	for _, ss := range globalSysMgr.All() {
		e := ss.GetPlayerByCharID(charID)
		if e == nil || e.PlayerState == nil {
			continue
		}
		ds, ok := e.PlayerState.(*destinyConn)
		if !ok || ds == nil {
			continue
		}
		ds.targetMu.Lock()
		out := make([]uint32, len(ds.targets))
		copy(out, ds.targets)
		ds.targetMu.Unlock()
		return out
	}
	return nil
}

// Compile-time check that charConnLookup matches the func shape both seams declare.
// The two hooks use identical funcs under different named types.
var _ func(int64) (*evenet.Conn, int64) = charConnLookup
