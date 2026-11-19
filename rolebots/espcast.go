package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// ── /rolebots: Main Cast ────────────────────────────────────────────────────
//
// The operator's own characters: built by hand in the client, given a personality and handed to
// the sim. The generated population lives on the other two tabs and is excluded here
// unconditionally; no query parameter lifts it.
//
// The exclusion is two tests: a rolebot_generated row, or a RoleBot<N> account. A run interrupted
// mid-purge leaves a character on a RoleBot account whose marker is gone, which one test alone
// would show. The account test goes through botSeqOf (botgen.go), never a SQL REGEXP, so the
// generator, the wipe range and this filter agree on what a RoleBot account is.

// castRow is one member of the Main Cast, as the cards render it: the roster row plus where the pilot lives,
// where it builds, what it is worth and what it is like.
type castRow struct {
	CharacterID   int64
	CharacterName string
	CorpID        int64
	CorpName      string
	Enabled       bool
	StationName   string
	SystemName    string
	Added         string
	SimActivity   string
	SimPhase      string
	Halted        bool
	LastError     string
	// Offline is the online cap holding this pilot off the server
	// (popcap.go): possessed and enabled, but not connected, so its activity
	// is a paused state rather than something it is doing.
	Offline bool
	// HomeName is the medical clone station (character_.homeStationID), the address every other is
	// measured from. HomePinned is the operator's leave-this-where-it-is flag (rolebot_profiles).
	HomeName   string
	HomePinned bool
	// FactoryName is the pilot's other address: where it builds.
	FactoryName string
	// Balance is preformatted (fmtISK, gangreport.go) because module pages get no funcMap:
	// TestESPPagesParse parses each page with none.
	Balance string
	// Traits is the character sketch: the poles this pilot leans on hardest, from traitHighlights.
	// It is empty for a pilot with no strong opinions.
	Traits []string
}

// allCharRow is one row of the every-character browser: any player character
// on the server that is not part of the generated population, with its cast
// standing if it has one.
type allCharRow struct {
	CharacterID   int64
	CharacterName string
	AccountID     int64
	AccountName   string
	CorpName      string
	OnRoster      bool
	Enabled       bool
	// Search is the pre-lowered haystack the client-side filter matches
	// against: name, characterID, account name/ID, corporation.
	Search string
}

func (m *Mod) handleCast(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleCastPost(w, r, ctx)
		return
	}

	rows, err := m.listCast(ctx)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	allChars, err := listAllCharacters(ctx, m.db)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	espRender(w, "rolebots_cast.html", map[string]any{
		"Page":     "rolebots",
		"Tab":      "cast",
		"Bots":     rows,
		"AllChars": allChars,
		"MasterOn": Master(),
		"Msg":      r.URL.Query().Get("msg"),
		"Err":      r.URL.Query().Get("err"),
	})
}

func (m *Mod) handleCastPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {
	case "master":
		on := r.FormValue("state") == "on"
		val := "0"
		if on {
			val = "1"
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingMaster, val); err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		setMaster(on)
		m.syncPresence()
		if on {
			redirect(w, r, "/rolebots", "RoleBot master switch ON — enabled characters are live", "")
		} else {
			redirect(w, r, "/rolebots", "RoleBot master switch OFF — all rolebot presence withdrawn", "")
		}
	case "rehome":
		// Both halves: the academy a pilot was created in (moveout.go) and the employer it has signed
		// with (corphome.go) are two reasons it stands where it does.
		settled := reconsiderAllCorpHomes()
		reconsiderAllMoveOuts()
		redirect(w, r, "/rolebots", fmt.Sprintf(
			"Every pilot will re-check where it lives and builds on its next planning pass "+
				"(%d already settled with an employer).", settled), "")
	case "add":
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if charID == 0 {
			redirect(w, r, "/rolebots", "", "invalid character ID")
			return
		}
		// The browser never offers a generated pilot, but this is a POST with a caller-supplied ID, so
		// refuse one here too.
		gen, err := isGeneratedBot(ctx, m.db, charID)
		if err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		if gen {
			redirect(w, r, "/rolebots", "", "that Capsuleer belongs to the generated population — "+
				"hand it to a player account on RoleBot Generation first, and it becomes yours to cast")
			return
		}
		if err := addRolebot(ctx, m.db, charID); err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		// Nothing seeds an identity any more: a pilot's politics are its
		// employer's and are read from the employer (outlook.go).
		seededNote := ""
		// Mend a sheet that holds skills without their prerequisites
		// before anything scores against it (seed.go).
		if n, err := m.repairSkillPrereqs(ctx, charID); err != nil {
			seededNote += " (skill repair failed: " + err.Error() + ")"
		} else if n > 0 {
			seededNote += fmt.Sprintf(" — granted %d missing prerequisite skill(s)", n)
		}
		redirect(w, r, "/rolebots", fmt.Sprintf("character %d joined the Main Cast%s", charID, seededNote), "")
	case "enable", "disable":
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if charID == 0 {
			redirect(w, r, "/rolebots", "", "invalid character ID")
			return
		}
		on := r.FormValue("action") == "enable"
		// Same guard as the profile page: never possess a character a player is logged in on.
		if on && m.sessions != nil {
			if sess := m.sessions.FindByCharID(charID); sess != nil {
				redirect(w, r, "/rolebots", "", fmt.Sprintf(
					"a player is currently logged in on this character (%s) — log them out first", sess.CharName))
				return
			}
		}
		if err := setRolebotEnabled(ctx, m.db, charID, on); err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		m.syncPresence()
		m.syncCEOSeat(ctx, charID)
		if on {
			redirect(w, r, "/rolebots", fmt.Sprintf("rolebot %d activated — RoleBot has taken possession", charID), "")
		} else {
			redirect(w, r, "/rolebots", fmt.Sprintf("rolebot %d deactivated — players may log in on it again", charID), "")
		}
	case "reboot":
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if err := m.rebootAndLog(ctx, charID); err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		redirect(w, r, "/rolebots", fmt.Sprintf("rolebot %d rebooted — it re-plans on the next sim pass", charID), "")
	case "remove":
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if err := removeRolebot(ctx, m.db, charID); err != nil {
			redirect(w, r, "/rolebots", "", err.Error())
			return
		}
		m.syncPresence()
		redirect(w, r, "/rolebots", fmt.Sprintf("character %d left the Main Cast", charID), "")
	default:
		redirect(w, r, "/rolebots", "", "unknown action")
	}
}

// isGeneratedBot reports whether a character belongs to the generated population, by its marker
// row or by its account: the same two tests the cast lists apply, so a hand-built POST cannot get
// past what the page refuses to show.
func isGeneratedBot(ctx context.Context, db *sql.DB, charID int64) (bool, error) {
	var marker int64
	var acctName sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT CASE WHEN g.characterID IS NULL THEN 0 ELSE 1 END, a.accountName
		  FROM character_ c
		  LEFT JOIN rolebot_generated g ON g.characterID = c.characterID
		  LEFT JOIN account a ON a.accountID = c.accountID
		 WHERE c.characterID = ?`, charID).Scan(&marker, &acctName)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("character %d not found", charID)
	}
	if err != nil {
		return false, fmt.Errorf("isGeneratedBot: %w", err)
	}
	if marker != 0 {
		return true, nil
	}
	_, isBotAcct := botSeqOf(acctName.String)
	return isBotAcct, nil
}

// listCast returns the Main Cast: the characters the operator adopted, and
// nothing the generator made. See the exclusion note at the top of the file.
func (m *Mod) listCast(ctx context.Context) ([]castRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.characterName,
		       COALESCE(c.corporationID, 0), COALESCE(corp.corporationName, ''),
		       b.enabled, b.addedDate,
		       COALESCE(st.stationName, ''), COALESCE(ms.solarSystemName, ''),
		       COALESCE(s.activity, ''), COALESCE(s.phase, ''),
		       COALESCE(s.halted, 0), COALESCE(s.lastError, ''),
		       COALESCE(hs.stationName, ''), COALESCE(p.homePinned, 0),
		       COALESCE(fs.stationName, ''), COALESCE(c.balance, 0),
		       COALESCE(a.accountName, '')
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN account a ON a.accountID = c.accountID
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN staStations st ON st.stationID = c.stationID
		LEFT JOIN staStations hs ON hs.stationID = c.homeStationID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		LEFT JOIN rolebot_profiles p ON p.characterID = b.characterID
		LEFT JOIN staStations fs ON fs.stationID = p.industryStationID
		LEFT JOIN rolebot_generated g ON g.characterID = b.characterID
		WHERE g.characterID IS NULL
		ORDER BY c.characterName`)
	if err != nil {
		return nil, fmt.Errorf("listCast: %w", err)
	}
	defer rows.Close()

	var out []castRow
	var ids []int64
	for rows.Next() {
		var cr castRow
		var en, added, halted, pinned int64
		var balance float64
		var acctName string
		if err := rows.Scan(&cr.CharacterID, &cr.CharacterName, &cr.CorpID, &cr.CorpName,
			&en, &added, &cr.StationName, &cr.SystemName,
			&cr.SimActivity, &cr.SimPhase, &halted, &cr.LastError,
			&cr.HomeName, &pinned, &cr.FactoryName, &balance, &acctName); err != nil {
			return nil, fmt.Errorf("listCast scan: %w", err)
		}
		cr.Balance = fmtISK(balance)
		// The second half of the exclusion, in Go because botSeqOf is the one
		// authority on what a RoleBot account is.
		if _, isBotAcct := botSeqOf(acctName); isBotAcct {
			continue
		}
		cr.Enabled = en != 0
		cr.Halted = halted != 0
		cr.HomePinned = pinned != 0
		cr.Added = time.Unix(added, 0).UTC().Format("2006-01-02")
		cr.Offline = cr.Enabled && popIsOffline(cr.CharacterID)
		out = append(out, cr)
		ids = append(ids, cr.CharacterID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listCast: %w", err)
	}

	// One bulk load for the whole cast rather than a query per card.
	traits, err := loadTraitsBulk(ctx, m.db, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Traits = traitHighlights(traits[out[i].CharacterID], 3)
	}
	return out, nil
}

// listAllCharacters is the browser the operator picks a cast member out of:
// every account-backed character on the server EXCEPT the generated
// population, with its cast standing if it has one.
func listAllCharacters(ctx context.Context, db *sql.DB) ([]allCharRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName,
		       COALESCE(c.accountID, 0), COALESCE(a.accountName, ''),
		       COALESCE(corp.corporationName, ''),
		       COALESCE(b.characterID, 0), COALESCE(b.enabled, 0)
		FROM character_ c
		LEFT JOIN account a ON a.accountID = c.accountID
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN rolebot_characters b ON b.characterID = c.characterID
		LEFT JOIN rolebot_generated g ON g.characterID = c.characterID
		WHERE c.accountID > 0 AND g.characterID IS NULL
		ORDER BY c.characterName`)
	if err != nil {
		return nil, fmt.Errorf("listAllCharacters: %w", err)
	}
	defer rows.Close()
	var out []allCharRow
	for rows.Next() {
		var ac allCharRow
		var rosterID, en int64
		if err := rows.Scan(&ac.CharacterID, &ac.CharacterName, &ac.AccountID,
			&ac.AccountName, &ac.CorpName, &rosterID, &en); err != nil {
			return nil, fmt.Errorf("listAllCharacters scan: %w", err)
		}
		if _, isBotAcct := botSeqOf(ac.AccountName); isBotAcct {
			continue
		}
		ac.OnRoster = rosterID != 0
		ac.Enabled = en != 0
		ac.Search = strings.ToLower(fmt.Sprintf("%s %d %s %d %s",
			ac.CharacterName, ac.CharacterID, ac.AccountName, ac.AccountID, ac.CorpName))
		out = append(out, ac)
	}
	return out, rows.Err()
}
