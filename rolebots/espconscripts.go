package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/server/gangsta"
)

// The operator's view of players commanding bots. It answers three questions: is this switched on,
// who is borrowing whom, and can a pilot be taken back. The card lives at the bottom of Gangs
// (espgangs.go); this file holds the rows and the writes.

// conscriptPage is where every write on this card answers to: the card is
// drawn at the bottom of the Gangs page and has no page of its own.
const conscriptPage = "/rolebots/gangs"

// conscriptRow is one line of the table.
type conscriptRow struct {
	CharID    int64
	Name      string
	BossID    int64
	BossName  string
	GangID    int64
	Role      string
	Duty      string
	Doing     string
	Order     string
	ServedFor string
	LastOrder string
	FleetLive bool
}

func (m *Mod) conscriptRows(ctx context.Context) ([]conscriptRow, error) {
	list, err := loadConscripts(ctx, m.db)
	if err != nil {
		return nil, err
	}
	now := clock.Unix()
	out := make([]conscriptRow, 0, len(list))
	for _, k := range list {
		row := conscriptRow{
			CharID: k.CharID, BossID: k.BossCharID, GangID: k.GangID,
			Role:      k.Role,
			ServedFor: humanSince(now - k.JoinedAt),
			LastOrder: humanSince(now - k.LastOrderAt),
			// The registry is the truth about whether anybody is still flying this fleet; the row records only
			// what was true when written, and the release sweep runs every five seconds.
			FleetLive: len(gangsta.HumanMembersOnline(k.GangID)) > 0,
		}
		row.Name, _ = m.charNameCorp(ctx, k.CharID)
		row.BossName, _ = m.charNameCorp(ctx, k.BossCharID)
		if k.DutyStationID != 0 {
			row.Duty = m.stationName(ctx, k.DutyStationID)
		}
		if st, err := loadState(ctx, m.db, k.CharID); err == nil {
			row.Doing = phaseLabel(st)
			row.Order = st.Data.OrderVerb
		}
		out = append(out, row)
	}
	return out, nil
}

// handleConscriptsPost serves the player-fleet card's three writes. The form posts to /rolebots/gangs
// and every answer redirects back there.
func (m *Mod) handleConscriptsPost(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirect(w, r, conscriptPage, "", err.Error())
		return
	}
	switch r.FormValue("action") {
	case "conscript":
		on := r.FormValue("enabled") == "on"
		defend := r.FormValue("defend") == "on"
		voice := r.FormValue("voice") == "on"
		num := func(field string, def int) int {
			n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
			if err != nil || n < 0 {
				return def
			}
			return n
		}
		maxFleet := num("maxFleet", defaultConscriptMaxFleet)
		maxTotal := num("maxTotal", defaultConscriptMaxTotal)
		idleTTL := num("idleTTL", defaultConscriptIdleTTL)
		voiceGap := num("voiceGap", defaultVoiceGapSec)
		voiceRate := num("voicePerMin", defaultVoicePerMin)

		bit := func(b bool) string {
			if b {
				return "1"
			}
			return "0"
		}
		for _, kv := range [][2]string{
			{settingConscript, bit(on)},
			{settingConscriptDefence, bit(defend)},
			{settingConscriptVoice, bit(voice)},
			{settingConscriptMaxFleet, strconv.Itoa(maxFleet)},
			{settingConscriptMaxTotal, strconv.Itoa(maxTotal)},
			{settingConscriptIdleTTL, strconv.Itoa(idleTTL)},
			{settingConscriptVoiceGap, strconv.Itoa(voiceGap)},
			{settingConscriptVoicePerMin, strconv.Itoa(voiceRate)},
		} {
			if err := evedb.SetServerSetting(ctx, m.db, kv[0], kv[1]); err != nil {
				redirect(w, r, conscriptPage, "", err.Error())
				return
			}
		}
		// Written and applied in one go, like the security-law settings: the values live in atomics that
		// only the loader reads, so a save that stopped at the database would wait for a restart.
		setConscription(on)
		setConscriptDefence(defend)
		setConscriptVoice(voice)
		setConscriptMaxFleet(maxFleet)
		setConscriptMaxTotal(maxTotal)
		setConscriptIdleTTL(idleTTL)
		setConscriptVoiceGap(voiceGap)
		setConscriptVoiceRate(voiceRate)
		if on {
			redirect(w, r, conscriptPage,
				"players may now invite rolebots into their own fleets", "")
			return
		}
		redirect(w, r, conscriptPage,
			"conscription is off — pilots already serving are released on the next sweep", "")

	case "conscriptRelease":
		charID, err := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if err != nil || charID == 0 {
			redirect(w, r, conscriptPage, "", "no pilot named")
			return
		}
		m.release(ctx, charID, "released by an operator")
		redirect(w, r, conscriptPage, "pilot released", "")

	case "conscriptReleaseAll":
		n := 0
		for _, k := range conscriptSnapshot() {
			m.release(ctx, k.CharID, "released by an operator")
			n++
		}
		redirect(w, r, conscriptPage, fmt.Sprintf("released %d pilot(s)", n), "")

	default:
		redirect(w, r, conscriptPage, "", "unknown action")
	}
}

// stationName resolves a station for display, falling back to its ID so a
// missing row reads as a number rather than as an empty cell.
func (m *Mod) stationName(ctx context.Context, stationID int64) string {
	if name := m.stationNameCached(ctx, stationID); name != "" {
		return name
	}
	return strconv.FormatInt(stationID, 10)
}

// humanSince renders an age the way a person reads one.
func humanSince(sec int64) string {
	switch {
	case sec < 0:
		return "—"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dd", sec/86400)
}
