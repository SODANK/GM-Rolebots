package rolebots

import (
	"math/rand"
	"strings"
)

// What a xenophobe calls people: EVE's own history only (the Amarr slave trade, the
// Minmatar Rebellion, the Caldari secession, the Reclaiming). Harshness is the speaker's
// stage (tolerance.go), not the line's:
//   - prefer, own+allied: no epithet. These pilots dislike a people and say so plainly
//     (the motive lines without a {slur}).
//   - own race: the epithet for any foreign people, harsh for a people the speaker's empire
//     stands against, condescending for an ally.
//   - bloodline: as own race, plus the digs kept for the wrong blood of its own people.
// slurFor is the only reader, through the {slur} slot, which the race switch
// (rolebots.chatter.race) removes outright.

// hostileEpithets are for a people the speaker's empire stands against.
var hostileEpithets = map[[2]int64][]string{
	{raceAmarr, raceMinmatar}:   {"slave", "heathen", "Matari dog", "runaway slave", "savage", "chattel", "rust-picker"},
	{raceAmarr, raceGallente}:   {"Federation libertine", "Fed degenerate", "godless Gallentean", "heretic", "hedonist"},
	{raceMinmatar, raceAmarr}:   {"slaver", "Holder's dog", "Holder", "God-Emperor's lapdog", "whip-hand", "zealot"},
	{raceMinmatar, raceCaldari}: {"corporate drone", "wage-slave", "State machine", "megacorp serf"},
	{raceCaldari, raceGallente}: {"Fed degenerate", "Federation parasite", "Gallentean hedonist", "Fed", "decadent"},
	{raceCaldari, raceMinmatar}: {"rust-bucket", "Matari scrap-picker", "tribal"},
	{raceGallente, raceCaldari}: {"State drone", "corporate slave", "Caldari automaton", "megacorp serf", "Kaalakiota clone"},
	{raceGallente, raceAmarr}:   {"slaver", "theocrat", "Holder trash", "God-Emperor's lapdog"},
}

// alliedEpithets are for a people the speaker's empire is friendly with: a final-stage
// xenophobe dislikes them too and is patronising about it.
var alliedEpithets = map[[2]int64][]string{
	{raceAmarr, raceCaldari}:     {"bean-counter", "faithless clerk", "State functionary"},
	{raceCaldari, raceAmarr}:     {"preacher", "scripture-thumper", "zealot"},
	{raceMinmatar, raceGallente}: {"soft Gallentean", "Fed fop", "pretty-boy"},
	{raceGallente, raceMinmatar}: {"tribal cousin", "Matari", "rust-bucket cousin"},
	{raceAmarr, raceJove}:        {"Jove freak"},
	{raceCaldari, raceJove}:      {"Jove freak"},
	{raceGallente, raceJove}:     {"Jove freak"},
	{raceMinmatar, raceJove}:     {"Jove freak"},
}

// bloodlineEpithets are a final-stage xenophobe's names for the other
// bloodlines of its own race, keyed by the TARGET's bloodline.
var bloodlineEpithets = map[int64][]string{
	5:  {"True Amarr snob", "Holder's pet", "Throne-polisher"},    // Amarr
	6:  {"Ni-Kunni mongrel", "half-breed", "Ni-Kunni trader-rat"}, // Ni-Kunni
	13: {"Khanid turncoat", "Khanid secessionist", "Khanid upstart"},
	4:  {"Brutor brute", "Brutor muscle", "knuckle-dragger"},
	3:  {"Sebiestor tinker-rat", "wrench-turner", "grease-monkey"},
	14: {"Vherokior peddler", "Vherokior drifter", "charm-seller"},
	1:  {"Deteis paper-pusher", "Deteis desk-jockey"},
	2:  {"Civire hothead", "Civire brawler"},
	11: {"Achura mystic", "Achura hermit"},
	7:  {"Luminaire snob", "Garoun dandy"},
	8:  {"Intaki dreamer", "Intaki navel-gazer"},
	12: {"Jin-Mei caste-climber", "Jin-Mei status-chaser"},
}

// slurFor is what the speaker calls the party, empty for nothing: below the own-race stage,
// for its own people (unless bloodline-stage and the wrong blood), or for a race the tables
// do not know.
func slurFor(speaker, about *chatParty) string {
	if speaker == nil || about == nil || speaker.RaceID == 0 || about.RaceID == 0 {
		return ""
	}
	stage := xenoPhobeStage(speaker.Phobia)
	if stage < xenoStageOwnRace {
		return ""
	}
	if speaker.RaceID == about.RaceID {
		if stage >= xenoStageBloodline && speaker.BloodlineID != 0 && about.BloodlineID != 0 &&
			speaker.BloodlineID != about.BloodlineID {
			return pickAny(bloodlineEpithets[about.BloodlineID])
		}
		return ""
	}
	pair := [2]int64{speaker.RaceID, about.RaceID}
	if l := hostileEpithets[pair]; len(l) > 0 {
		return pickAny(l)
	}
	return pickAny(alliedEpithets[pair])
}

func pickAny(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[rand.Intn(len(l))]
}

// epithetsAgainst is every name any table calls a race (and its bloodlines), lower-cased,
// for matching a player who calls an Amarr bot a slaver (chatter_retort.go).
func epithetsAgainst(race int64) []string {
	var out []string
	seen := map[string]bool{}
	add := func(l []string) {
		for _, e := range l {
			e = strings.ToLower(e)
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	for pair, l := range hostileEpithets {
		if pair[1] == race {
			add(l)
		}
	}
	for pair, l := range alliedEpithets {
		if pair[1] == race {
			add(l)
		}
	}
	for bl, l := range bloodlineEpithets {
		if raceOfBloodline(bl) == race {
			add(l)
		}
	}
	return out
}

// raceOfBloodline is chrBloodlines.raceID for the playable bloodlines.
func raceOfBloodline(bl int64) int64 {
	switch bl {
	case 1, 2, 11:
		return raceCaldari
	case 3, 4, 14:
		return raceMinmatar
	case 5, 6, 13:
		return raceAmarr
	case 7, 8, 12:
		return raceGallente
	case 9, 10:
		return raceJove
	}
	return 0
}
