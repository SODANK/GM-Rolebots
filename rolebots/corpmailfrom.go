package rolebots

// Who writes a corporation's letter: the officer whose business it is (the payslip from the Treasurer, the
// patrol from Security, the welcome from Personnel), signed with the title that officer wears
// (character_.title, which the title vocabulary names by race and culture). It falls back to the CEO when the
// post is empty or held by a person: a capsuleer's name is never put to a letter the module wrote.

import "context"

// The topics a letter can be about.
const (
	mailFromChair          = "chair"          // bulletins, the weekly report, war: the CEO
	mailFromSecurity       = "security"       // patrols, the Armory, ship replacement
	mailFromIndustry       = "industry"       // build work, the Production Board
	mailFromCommerce       = "commerce"       // deliveries into the corporation's bids
	mailFromInfrastructure = "infrastructure" // tower fuel
	mailFromTreasury       = "treasury"       // payslips, shares, dividends
	mailFromPersonnel      = "personnel"      // welcome, postings, ranks
	mailFromResearch       = "research"       // the library
)

// mailFromDefaultHat is the hat an office signs with when its holder carries
// no title text.
var mailFromDefaultHat = map[string]string{
	mailFromSecurity:       "Director of Security",
	mailFromIndustry:       corpIndustryDirector,
	mailFromCommerce:       "Director of Commerce",
	mailFromInfrastructure: "Director of Infrastructure",
	mailFromTreasury:       "Treasurer",
	mailFromPersonnel:      "Director of Personnel",
	mailFromResearch:       "Director of Research",
}

// letterFrom is the writer of a letter about topic, and the hat they sign
// with. The CEO when nobody the module flies holds the office.
func (m *Mod) letterFrom(ctx context.Context, c *corpCEO, topic string) (int64, string) {
	ceoHat := "Chief Executive Officer"
	if c == nil {
		return 0, ceoHat
	}
	// A human chair's c.CharID is the bot acting for it: an officer like any other, and one that
	// never signs as the CEO.
	human := c.Origin == ceoOriginHuman
	if human {
		ceoHat = "for the Chief Executive"
		ctx = m.withChairStaff(ctx, c)
	}
	s := m.staffFor(ctx, c)
	pick := func(h *staffMember, hat string) (int64, string, bool) {
		if h == nil || h.Human || !h.Enabled || (h.CharID == c.CharID && !human) {
			return 0, "", false
		}
		return h.CharID, m.writerHat(ctx, h.CharID, hat), true
	}
	var id int64
	var hat string
	ok := false
	switch topic {
	case mailFromSecurity:
		id, hat, ok = pick(s.head(portSecurity), portfolioTitle(portSecurity))
	case mailFromInfrastructure:
		id, hat, ok = pick(s.head(portInfrastructure), portfolioTitle(portInfrastructure))
	case mailFromResearch:
		id, hat, ok = pick(s.head(portResearch), armHeadTitle(armResearch))
	case mailFromCommerce:
		if id, hat, ok = pick(s.head(portCommerce), portfolioTitle(portCommerce)); !ok {
			id, hat, ok = pick(s.Treasurer, mailFromDefaultHat[mailFromTreasury])
		}
	case mailFromTreasury:
		id, hat, ok = pick(s.Treasurer, mailFromDefaultHat[mailFromTreasury])
	case mailFromPersonnel:
		if id, hat, ok = pick(s.head(portHR), armHeadTitle(armHR)); !ok && len(s.Personnel) > 0 {
			id, hat, ok = pick(s.Personnel[0], "Recruitment Officer")
		}
	case mailFromIndustry:
		if p, err := m.loadCorpPolicy(ctx, c.CorpID); err == nil && p.DirectorID != 0 &&
			(p.DirectorID != c.CharID || human) && m.isSynthetic(p.DirectorID) {
			id, hat, ok = p.DirectorID, m.writerHat(ctx, p.DirectorID, corpIndustryDirector), true
		}
	}
	if !ok {
		return c.CharID, ceoHat
	}
	if hat == "" {
		hat = mailFromDefaultHat[topic]
	}
	return id, hat
}

// writerHat is the title an officer wears, else the office's own name.
func (m *Mod) writerHat(ctx context.Context, charID int64, fallback string) string {
	var t string
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(title, '') FROM character_ WHERE characterID = ?`, charID).Scan(&t)
	if t != "" {
		return t
	}
	return fallback
}
