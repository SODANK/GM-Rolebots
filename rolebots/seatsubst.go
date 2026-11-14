package rolebots

// seatSubstitutesCover drops from a seat check's missing list every planned module whose
// place on the ship is taken by another module of the same group in the same rack band:
// a Light Neutron Blaster I where the plan now names the Modal variant, a different signal
// amplifier in the low rack. A planned fit moves up a meta level as the pilot's skills and
// wallet grow, and the ship keeps flying its older modules until procurement swaps them;
// for the seat they do the job. A covered gun's charge line goes with it. Ship modules that
// are already the plan's exact type and flag cover nothing else, and each fitted module
// covers one line. ship maps slot flag to module typeID (shipSlotModules); groupOf returns
// a module type's groupID, 0 when unknown. Pure; procurement still reads the full list.
func seatSubstitutesCover(missing []ShopItem, ship map[int64]int64, fit *Fit, groupOf func(int64) int64) []ShopItem {
	if len(missing) == 0 || len(ship) == 0 || fit == nil || groupOf == nil {
		return missing
	}
	exact := map[[2]int64]bool{}
	for _, mod := range fit.Modules {
		exact[[2]int64{mod.TypeID, mod.Flag}] = true
	}
	// spare counts the ship's off-plan modules by (band, group).
	spare := map[[2]int64]int{}
	for flag, typeID := range ship {
		if exact[[2]int64{typeID, flag}] {
			continue
		}
		if g := groupOf(typeID); g != 0 {
			spare[[2]int64{bandOf(flag), g}]++
		}
	}
	if len(spare) == 0 {
		return missing
	}
	covered := map[int64]bool{} // flags of covered module lines, for their charge lines
	out := make([]ShopItem, 0, len(missing))
	for _, it := range missing {
		if it.Dest == "module" {
			k := [2]int64{bandOf(it.Flag), groupOf(it.TypeID)}
			if k[1] != 0 && spare[k] > 0 {
				spare[k]--
				covered[it.Flag] = true
				continue
			}
		}
		out = append(out, it)
	}
	kept := out[:0]
	for _, it := range out {
		if it.Dest == "charge" && covered[it.Flag] {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}
