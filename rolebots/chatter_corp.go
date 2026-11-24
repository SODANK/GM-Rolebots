package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Corp chat. Every enabled bot has been in its corporation's channel since
// presence.go joined it there, and none of them ever said a word: the
// department desks posted orders to a table, a member under fire fled in
// silence, and a corporation with a human in it read like a list of names.
//
// The channel is the corporation ID. Nothing here runs unless a human member
// is online to read it (hearable), and nothing here blocks its caller: the
// beats run on their own goroutine.

const (
	corpBeatOpen  = 1 * time.Second
	corpBeatReply = 6 * time.Second
	corpBeatLast  = 5 * time.Second
)

// armHeadCharID is a corporation's arm head from the index, 0 for none.
func armHeadCharID(corpID int64, a corpArm) int64 {
	p := armIndex.Load()
	if p == nil {
		return 0
	}
	for id, s := range *p {
		if s.CorpID == corpID && s.Arm == a && s.Tier == armTierHead {
			return id
		}
	}
	return 0
}

// armMembersOf is the rank and file of an arm (heads excluded), from the index.
func armMembersOf(corpID int64, a corpArm) []int64 {
	p := armIndex.Load()
	if p == nil {
		return nil
	}
	var out []int64
	for id, s := range *p {
		if s.CorpID == corpID && s.Arm == a && s.Tier != armTierHead {
			out = append(out, id)
		}
	}
	return out
}

// corpParty is a corporation member as a party, nil when it is not a bot the
// module may speak for.
func (m *Mod) corpParty(ctx context.Context, charID, corpID int64) *chatParty {
	if charID == 0 || !m.isSynthetic(charID) {
		return nil
	}
	if lp := liveProjection(charID); lp != nil {
		return partyFromEmbodied(lp)
	}
	name := m.charNameOr(ctx, charID)
	tn, _ := m.tuneFor(ctx, charID)
	p := &chatParty{CharID: charID, Name: name, CorpID: corpID, RaceID: raceOf(charID),
		BloodlineID: bloodlineOf(charID), Phobia: tn.Xenophobia, Philia: tn.Xenophilia, Bot: true}
	p.Voice = voiceFor(charID, p.RaceID, tn, false)
	return p
}

var corpHelpAt = newMotiveMemo()

// chatterCorpHelp is a member under fire telling its corporation, and the Security arm
// answering: "on our way" only when a security order points the wing at that system. When
// the corporation cannot answer and flies in an alliance, the member asks the alliance as
// well (chatter_alliance.go).
func (m *Mod) chatterCorpHelp(victim, attacker *chatParty, sysID int64, where string) {
	if victim == nil || !victim.Bot || victim.CorpID == 0 || !Chatter() {
		return
	}
	corpHears := chatterCorpOn.Load() && m.hearable(victim.CorpID, true)
	allyMaybe := chatterAllianceOn.Load()
	if !corpHears && !allyMaybe {
		return
	}
	if corpHelpAt.get(victim.CorpID, victim.CharID) != motiveNone {
		return
	}
	corpHelpAt.note(victim.CorpID, victim.CharID, motiveDefense)
	corpID := victim.CorpID
	goSafe("chatter corp help", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		extra := chatSlots{"system": m.nameOfSystem(ctx, sysID)}
		if where != "" {
			extra["where"] = where
		}
		head := armHeadCharID(corpID, armSecurity)
		if head == 0 || head == victim.CharID || !presentIn(corpID, head) {
			head = 0
			mates := armMembersOf(corpID, armSecurity)
			rand.Shuffle(len(mates), func(i, j int) { mates[i], mates[j] = mates[j], mates[i] })
			for _, id := range mates {
				if id != victim.CharID && presentIn(corpID, id) {
					head = id
					break
				}
			}
		}
		answered := false
		var resp *chatParty
		ev := evCorpHelpNo
		if resp = m.corpParty(ctx, head, corpID); resp != nil && resp.CharID != victim.CharID {
			if o := m.corpFleetOrderFor(ctx, corpID, portSecurity); o != nil && o.SystemID == sysID {
				ev = evCorpHelpYes
			} else if liveProjection(head) != nil && liveProjection(head).systemID == sysID {
				ev = evCorpHelpYes
			}
			answered = ev == evCorpHelpYes
		} else {
			resp = nil
		}
		if corpHears {
			// The victim speaks from its OWN corp party, which carries the
			// live Alive check if it is on grid.
			beats := []beat{{after: corpBeatOpen, speaker: victim, about: attacker, ev: evCorpHelp, extra: extra, urgent: true}}
			if resp != nil {
				beats = append(beats, beat{after: corpBeatReply, speaker: resp, about: victim, ev: ev, extra: extra,
					odds: 80})
			}
			m.runExchange("corp help", corpID, true, beats)
		}
		if !answered {
			m.chatterAllyHelp(victim, attacker, sysID, extra)
		}
	})
}

// chatterCorpConsole is a corpmate answering a member's loss, a few seconds
// after the queued pers.loss line went out (chatter.go chatterPass). Not
// every loss: a corporation that answers every pod with "o7" is a script.
func (m *Mod) chatterCorpConsole(ctx context.Context, corpID int64, lost chatterSpeaker, system string) {
	if !chatterCorpOn.Load() || corpID == 0 || rand.Intn(100) >= 60 {
		return
	}
	victim := m.corpParty(ctx, lost.CharID, corpID)
	mate := m.corpmateFor(ctx, corpID, lost.CharID)
	if victim == nil || mate == nil {
		return
	}
	extra := chatSlots{}
	if system != "" {
		extra["system"] = system
	}
	// The loss line opened the exchange; this beat follows it.
	goSafe("chatter corp console", func() {
		time.Sleep(jitter(corpBeatReply))
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.chatSay(cctx, corpID, true, mate, victim, evCorpConsole, motiveNone, extra, false, true)
	})
}

// corpmateFor is some other bot sitting in the corporation's channel now to answer a
// member; a bot that is not there cannot speak in it.
func (m *Mod) corpmateFor(ctx context.Context, corpID, not int64) *chatParty {
	bots := chatterPresentBots(corpID)
	rand.Shuffle(len(bots), func(i, j int) { bots[i], bots[j] = bots[j], bots[i] })
	for _, b := range bots {
		if b.CharID == not {
			continue
		}
		if party := m.corpParty(ctx, b.CharID, corpID); party != nil {
			return party
		}
	}
	return nil
}

// presentIn reports whether a bot sits in a corporation's channel.
func presentIn(corpID, charID int64) bool {
	_, ok := chatterSpeakerFor(corpID, charID)
	return ok
}

// chatterCorpOrder is a department head calling a member out on a NEW order
// (postFleetOrder decides what is new). The poster speaks only if it is a
// bot; a human chair's order from the Corporate Portal is still acknowledged.
func (m *Mod) chatterCorpOrder(ctx context.Context, o corpFleetOrder) {
	if !chatterCorpOn.Load() || o.CorpID == 0 || !m.hearable(o.CorpID, true) {
		return
	}
	arm := armOfPortfolio(o.Dept)
	if arm == "" {
		return
	}
	members := armMembersOf(o.CorpID, arm)
	rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	var member *chatParty
	for _, id := range members {
		if id == o.PostedBy || !presentIn(o.CorpID, id) {
			continue
		}
		if member = m.corpParty(ctx, id, o.CorpID); member != nil {
			break
		}
	}
	if member == nil {
		return
	}
	head := m.corpParty(ctx, o.PostedBy, o.CorpID)
	if head == nil {
		// A human posted it. Name them for the ack all the same.
		head = &chatParty{CharID: o.PostedBy, Name: m.charNameOr(ctx, o.PostedBy), CorpID: o.CorpID,
			RaceID: raceOf(o.PostedBy), BloodlineID: bloodlineOf(o.PostedBy)}
	}
	// A bot director not sitting in the channel cannot open the exchange;
	// the member answers the board instead, as for a human's order.
	headSpeaks := head.Bot && presentIn(o.CorpID, head.CharID)
	extra := chatSlots{
		"member": member.link(),
		"title":  armHeadTitle(arm), "dept": strings.ToLower(armLabel(arm)),
	}
	if head.CharID != 0 && head.Name != "" {
		extra["head"] = head.link()
	}
	if o.Place() != 0 {
		extra["system"] = m.placeName(ctx, o.Place())
	}
	if len(o.OreNames) > 0 {
		extra["ore"] = joinWords(o.OreNames)
	}
	if o.Reason != "" {
		extra["reason"] = o.Reason
	}
	why := motive(o.Dept) // the department is the motive cell for orders
	ack := evCorpAck
	if rand.Intn(100) < 20 {
		ack = evCorpGrumble
	}
	var beats []beat
	if headSpeaks {
		beats = append(beats, beat{after: corpBeatOpen, speaker: head, about: member, ev: evCorpOrder, why: why,
			extra: extra, urgent: true})
	}
	aboutHead := head
	if head.CharID == 0 || head.Name == "" {
		aboutHead = nil
	}
	beats = append(beats, beat{after: corpBeatReply, speaker: member, about: aboutHead, ev: ack, why: why, extra: extra})
	if headSpeaks {
		beats = append(beats, beat{after: corpBeatLast, speaker: head, about: member, ev: evCorpFollow,
			why: motive(ack), extra: extra, odds: 40})
	}
	m.runExchange("corp order", o.CorpID, true, beats)
}

// chatterCorpClaim is a member taking work off the corporation's board, and
// the head of the arm that posted it noticing.
func (m *Mod) chatterCorpClaim(_ context.Context, w corpWork, charID int64, runs int) {
	if !chatterCorpOn.Load() || w.CorpID == 0 || runs <= 0 || !m.hearable(w.CorpID, true) {
		return
	}
	// One line in five: a busy board would otherwise fill the channel.
	if rand.Intn(5) != 0 {
		return
	}
	// Off the planning pass that claimed it: the names are queries.
	goSafe("chatter corp claim", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.sayCorpClaim(ctx, w, charID, runs)
	})
}

func (m *Mod) sayCorpClaim(ctx context.Context, w corpWork, charID int64, runs int) {
	member := m.corpParty(ctx, charID, w.CorpID)
	if member == nil {
		return
	}
	// A build names its product; research and copies name the print, which is
	// the thing the work is done to (their orders carry no product).
	itemType := w.ProductTypeID
	if w.Kind != workKindBuild || itemType == 0 {
		itemType = w.BPTypeID
	}
	extra := chatSlots{
		"n":    fmt.Sprintf("%d", runs),
		"item": m.typeNameOr(ctx, itemType),
		"work": workWords(w.Kind, runs),
	}
	if w.StationID != 0 {
		extra["station"] = m.stationNameOr(ctx, w.StationID)
	}
	why := workMotive(w.Kind)
	beats := []beat{{after: corpBeatOpen, speaker: member, ev: evCorpClaim, why: why, extra: extra}}
	if head := m.corpParty(ctx, armHeadCharID(w.CorpID, armIndustry), w.CorpID); head != nil && head.CharID != charID {
		extra["member"] = member.link()
		beats = append(beats, beat{after: corpBeatReply, speaker: head, about: member, ev: evCorpClaimOK,
			why: why, extra: extra, odds: 60})
	}
	m.runExchange("corp claim", w.CorpID, true, beats)
}

// joinWords is "a, b and c".
func joinWords(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// workKindOfActivity is the work kind a ramJobs activity is.
func workKindOfActivity(activityID int) string {
	switch int64(activityID) {
	case ramActivityMEResearch:
		return workKindME
	case ramActivityPEResearch:
		return workKindPE
	case ramActivityCopying:
		return workKindCopy
	}
	return workKindBuild
}

// chatterCorpJob narrates a corporation job going on the line, in the words its kind is
// counted in. product is the build's output; printName is the corporation's blueprint,
// which research and copies are done to.
func (m *Mod) chatterCorpJob(ctx context.Context, charID int64, kind string, n int, product, printName string, jobID int64) {
	switch kind {
	case workKindME, workKindPE:
		m.chatterNoteBot(ctx, charID, "ind.corpresearch", map[string]string{
			"what": printName, "activity": activityLineNoun(int64(workActivity(kind))),
			"levels": plural(n, "level", "levels")}, jobID)
	case workKindCopy:
		m.chatterNoteBot(ctx, charID, "ind.corpcopy", map[string]string{
			"what": printName, "copies": plural(n, "copy", "copies")}, jobID)
	default:
		if product == "" {
			product = printName
		}
		m.chatterNoteBot(ctx, charID, "ind.corpjob", map[string]string{
			"work": plural(n, "run", "runs"), "what": product}, jobID)
	}
}
