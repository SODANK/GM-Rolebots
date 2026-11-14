package rolebots

import (
	"context"
	"hash/crc32"
	"math/rand"
	"regexp"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The alliance channel. presence.go seats every bot in its alliance's channel; this is what
// they say in it. Two halves:
//   - Support. A member under fire whose corporation cannot answer asks the alliance, and a
//     fleet's help call or a reinforced tower's call for defence is posted there. The answer
//     is "coming" only when a member fleet really is pointed at that system (an adopted help
//     call, a hunt, a defend order). A person asking for help gets the same truthful answer.
//   - Small talk between corporations: the alliance's politics as the alliance layer decides
//     them, one corporation remarking on another's loss or kill, and shop talk.
// The channel is the allianceID, so exchanges run through chatSay's corp-shaped path with the
// alliance's own ceiling (chatterLimitsIn) and switch. chatSay drops the {slur} slot there.

// ── events ──────────────────────────────────────────────────────────────────

const (
	// Support.
	evAllyHelp      chatEvent = "allyhelp"      // a member under fire asks the alliance
	evAllyHelpYes   chatEvent = "allyhelpyes"   // a member fleet is really there
	evAllyHelpNo    chatEvent = "allyhelpno"    // nobody is
	evAllyCall      chatEvent = "allycall"      // a fleet's help call, why = the call's kind
	evAllyCallOmw   chatEvent = "allycallomw"   // another corporation's fleet adopted it
	evAllyDefend    chatEvent = "allydefend"    // a reinforced tower calls the alliance
	evAllyDefendYes chatEvent = "allydefendyes" // a member corporation sends its wing
	evAllyHumanYes  chatEvent = "allyhumanyes"  // a person asked; a fleet is already there
	evAllyHumanNo   chatEvent = "allyhumanno"   // a person asked; the nearest wing is {jumps} out
	evAllyHumanNone chatEvent = "allyhumannone" // a person asked; nobody is out

	// Politics: an opener and a reply from another corporation.
	evAllyJoin        chatEvent = "allyjoin"        // the newcomer says hello
	evAllyJoinBack    chatEvent = "allyjoinback"    // a member answers, why = tone
	evAllyAccept      chatEvent = "allyaccept"      // the executor accepted an application
	evAllyAcceptBack  chatEvent = "allyacceptback"  // why = tone
	evAllyLeave       chatEvent = "allyleave"       // a corporation leaves
	evAllyLeaveBack   chatEvent = "allyleaveback"   // why = tone
	evAllyKick        chatEvent = "allykick"        // the executor expels one
	evAllyKickBack    chatEvent = "allykickback"    // why = tone
	evAllyVote        chatEvent = "allyvote"        // a corporation backs a candidate
	evAllyVoteBack    chatEvent = "allyvoteback"    // why = tone
	evAllyExec        chatEvent = "allyexec"        // congratulations to a new executor, why = tone
	evAllyExecBack    chatEvent = "allyexecback"    // the new executor answers
	evAllyDues        chatEvent = "allydues"        // a member pays its dues
	evAllyDuesBack    chatEvent = "allyduesback"    // why = tone
	evAllyTithe       chatEvent = "allytithe"       // a member pays its tithe
	evAllyTitheBack   chatEvent = "allytitheback"   // the executor answers
	evAllyRefuse      chatEvent = "allyrefuse"      // the executor names a member that could not pay
	evAllyRefuseBack  chatEvent = "allyrefuseback"  // the member answers
	evAllyRel         chatEvent = "allyrel"         // the executor names an outsider, why = relWhy
	evAllyRelBack     chatEvent = "allyrelback"     // why = tone toward the executor
	evAllyWar         chatEvent = "allywar"         // the executor declares war
	evAllyWarBack     chatEvent = "allywarback"     // why = eager | wary
	evAllyWarReq      chatEvent = "allywarreq"      // a member asks for war
	evAllyWarReqBack  chatEvent = "allywarreqback"  // the executor answers
	evAllyWarHold     chatEvent = "allywarhold"     // the executor holds the requests
	evAllyWarHoldBack chatEvent = "allywarholdback" // a member grumbles
	evAllySovGain     chatEvent = "allysovgain"     // a system is ours
	evAllySovLoss     chatEvent = "allysovloss"     // a system is lost
	evAllySovBack     chatEvent = "allysovback"     // why = gain | loss
	evAllyLossBig     chatEvent = "allylossbig"     // condolences on a costly loss, why = tone
	evAllyLossSmall   chatEvent = "allylosssmall"   // ribbing over a cheap one, why = tone
	evAllyLossBack    chatEvent = "allylossback"    // the loser answers
	evAllyKill        chatEvent = "allykill"        // praise for another corporation's kill, why = tone
	evAllyKillBack    chatEvent = "allykillback"    // the killer answers
	evAllyShopAsk     chatEvent = "allyshopask"     // "anyone in the alliance making {item}?"
	evAllyShopYes     chatEvent = "allyshopyes"     // a member corporation that really builds it
	evAllyShopAd      chatEvent = "allyshopad"      // a listing mentioned to the alliance
	evAllyShopAdBack  chatEvent = "allyshopadback"  // a member notes it
	evAllyCareer      chatEvent = "allycareer"      // shop talk about one's own work, why = career motive
	evAllyCareerBack  chatEvent = "allycareerback"  // why = tone
)

// The reply's tone, a motive cell. Never a reason to shoot: how one member
// corporation's pilot takes another's news.
const (
	toneWarm    motive = "warm"
	tonePolite  motive = "polite"
	toneCold    motive = "cold"
	toneCurious motive = "curious" // banter only: a xenophile asking
)

// War and relationship cells.
const (
	whyEager    motive = "eager"
	whyWary     motive = "wary"
	whyFriendly motive = "friendly"
	whyHostile  motive = "hostile"
	whyWithdraw motive = "withdrawn"
	whyGain     motive = "gain"
	whyLoss     motive = "loss"
)

// toneToward is how self takes news from other: cold toward a corporation
// it holds in poor standing or a people its prejudice shuts out, warm toward
// one it likes or a people it is curious about, polite otherwise. Map reads
// only.
func toneToward(self, other *chatParty) motive {
	if self == nil || other == nil {
		return tonePolite
	}
	st := relCorpStanding(self.CorpID, other.CorpID)
	if st <= StandingDisliked {
		return toneCold
	}
	foreign := self.RaceID != 0 && other.RaceID != 0 && self.RaceID != other.RaceID
	switch {
	case foreign && xenoPhobeStage(self.Phobia) >= xenoStageOwnRace:
		return toneCold
	case !foreign && xenoBloodlineBars(self.Phobia, self.RaceID, self.BloodlineID, other.RaceID, other.BloodlineID):
		return toneCold
	case foreign && self.Philia >= xenoWary:
		return toneWarm
	case st >= StandingLiked:
		return toneWarm
	}
	return tonePolite
}

// ── exchange kinds (mutes and cooldowns) ────────────────────────────────────

// chatterExchangeKind is a family of exchange lines the panel can mute. These are chatPools
// events, not the queued catalogue (chatterKinds), so they sit in their own map with an
// event to render the panel's test line from.
type chatterExchangeKind struct {
	Kind, Label, Class string
	Cooldown           time.Duration // per (channel, kind), on the opening line only
	Event              chatEvent
	SampleWhy          motive
}

const (
	chatterClassAlliance = "alliance"
	chatterClassBanter   = "banter"
)

var chatterExchangeKinds = map[string]chatterExchangeKind{
	"a.help":   {Kind: "a.help", Label: "Member asks the alliance for help", Class: chatterClassAlliance, Event: evAllyHelp},
	"a.call":   {Kind: "a.call", Label: "Fleet help call / answer", Class: chatterClassAlliance, Event: evAllyCall, SampleWhy: motive(helpOutnumbered)},
	"a.defend": {Kind: "a.defend", Label: "Reinforced tower / defenders", Class: chatterClassAlliance, Event: evAllyDefend},
	"a.human":  {Kind: "a.human", Label: "Answer to a person asking for help", Class: chatterClassAlliance, Event: evAllyHumanNone},
	"a.join":   {Kind: "a.join", Label: "New member / acceptance", Class: chatterClassAlliance, Cooldown: 30 * time.Minute, Event: evAllyJoin},
	"a.leave":  {Kind: "a.leave", Label: "Member leaves / expelled", Class: chatterClassAlliance, Cooldown: 30 * time.Minute, Event: evAllyLeave},
	"a.vote":   {Kind: "a.vote", Label: "Executor vote", Class: chatterClassAlliance, Cooldown: 2 * time.Hour, Event: evAllyVote},
	"a.exec":   {Kind: "a.exec", Label: "New executor", Class: chatterClassAlliance, Cooldown: time.Hour, Event: evAllyExec, SampleWhy: tonePolite},
	"a.dues":   {Kind: "a.dues", Label: "Dues paid", Class: chatterClassAlliance, Cooldown: 12 * time.Hour, Event: evAllyDues},
	"a.tithe":  {Kind: "a.tithe", Label: "Tithe paid / refused", Class: chatterClassAlliance, Cooldown: 3 * time.Hour, Event: evAllyTithe},
	"a.rel":    {Kind: "a.rel", Label: "Alliance relationship", Class: chatterClassAlliance, Cooldown: time.Hour, Event: evAllyRel, SampleWhy: whyFriendly},
	"a.war":    {Kind: "a.war", Label: "Alliance war / request / hold", Class: chatterClassAlliance, Cooldown: time.Hour, Event: evAllyWar},
	"a.sov":    {Kind: "a.sov", Label: "Sovereignty won / lost", Class: chatterClassAlliance, Cooldown: time.Hour, Event: evAllySovGain},
	"a.rib":    {Kind: "a.rib", Label: "Remark on another member's loss", Class: chatterClassAlliance, Cooldown: 30 * time.Minute, Event: evAllyLossSmall, SampleWhy: tonePolite},
	"a.kill":   {Kind: "a.kill", Label: "Praise for another member's kill", Class: chatterClassAlliance, Cooldown: 30 * time.Minute, Event: evAllyKill, SampleWhy: tonePolite},
	"a.shop":   {Kind: "a.shop", Label: "Shop talk across the alliance", Class: chatterClassAlliance, Cooldown: 20 * time.Minute, Event: evAllyShopAsk},
	"a.banter": {Kind: "a.banter", Label: "Culture banter (alliance)", Class: chatterClassBanter, Event: banterOpen(raceAmarr)},
	"l.banter": {Kind: "l.banter", Label: "Culture banter (Local)", Class: chatterClassBanter, Event: banterOpen(raceCaldari)},
}

// chatterExchangeSlotNames is every slot the panel's test line fills.
var chatterExchangeSlotNames = []string{
	"self", "myrace", "mybloodline", "target", "name", "race", "bloodline", "ship", "hull",
	"where", "system", "corp", "othercorp", "jumps", "when", "rel", "first", "isk", "item", "units", "station",
}

// allyEventKind is which family each alliance event (opener and reply) is
// muted and cooled under.
var allyEventKind = map[chatEvent]string{
	evAllyHelp: "a.help", evAllyHelpYes: "a.help", evAllyHelpNo: "a.help",
	evAllyCall: "a.call", evAllyCallOmw: "a.call",
	evAllyDefend: "a.defend", evAllyDefendYes: "a.defend",
	evAllyHumanYes: "a.human", evAllyHumanNo: "a.human", evAllyHumanNone: "a.human",
	evAllyJoin: "a.join", evAllyJoinBack: "a.join", evAllyAccept: "a.join", evAllyAcceptBack: "a.join",
	evAllyLeave: "a.leave", evAllyLeaveBack: "a.leave", evAllyKick: "a.leave", evAllyKickBack: "a.leave",
	evAllyVote: "a.vote", evAllyVoteBack: "a.vote",
	evAllyExec: "a.exec", evAllyExecBack: "a.exec",
	evAllyDues: "a.dues", evAllyDuesBack: "a.dues",
	evAllyTithe: "a.tithe", evAllyTitheBack: "a.tithe", evAllyRefuse: "a.tithe", evAllyRefuseBack: "a.tithe",
	evAllyRel: "a.rel", evAllyRelBack: "a.rel",
	evAllyWar: "a.war", evAllyWarBack: "a.war", evAllyWarReq: "a.war", evAllyWarReqBack: "a.war",
	evAllyWarHold: "a.war", evAllyWarHoldBack: "a.war",
	evAllySovGain: "a.sov", evAllySovLoss: "a.sov", evAllySovBack: "a.sov",
	evAllyLossBig: "a.rib", evAllyLossSmall: "a.rib", evAllyLossBack: "a.rib",
	evAllyKill: "a.kill", evAllyKillBack: "a.kill",
	evAllyShopAsk: "a.shop", evAllyShopYes: "a.shop", evAllyShopAd: "a.shop", evAllyShopAdBack: "a.shop",
	evAllyCareer: "a.shop", evAllyCareerBack: "a.shop",
}

// exchangeKindFor is the mutable kind of an exchange line, "" for the older
// exchanges that keep their x.<event> kind.
func exchangeKindFor(ev chatEvent, ally bool) string {
	if k, ok := allyEventKind[ev]; ok {
		return k
	}
	if isBanterEvent(ev) {
		if ally {
			return "a.banter"
		}
		return "l.banter"
	}
	return ""
}

// ── who speaks ──────────────────────────────────────────────────────────────

// allianceChatOK is the cheap gate every producer asks first: the switches,
// and a person live in that alliance's channel.
func allianceChatOK(aid int64) bool {
	return aid != 0 && Chatter() && chatterAllianceOn.Load() && chatterWatched(aid)
}

// allySeated is the bots of one corporation (0 = any) seated in an alliance
// channel, shuffled.
func allySeated(aid, corpID, notCorp int64) []chatterSpeaker {
	var out []chatterSpeaker
	for _, b := range chatterPresentBots(aid) {
		if (corpID == 0 || b.CorpID == corpID) && (notCorp == 0 || b.CorpID != notCorp) {
			out = append(out, b)
		}
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// allyVoiceOf is a corporation's voice in the alliance channel: the pilot
// asked for first when it is seated there (the officer who did the thing),
// then the CEO, then a security or industry head, then any of its bots.
func (m *Mod) allyVoiceOf(ctx context.Context, aid, corpID int64, prefer ...int64) *chatParty {
	if corpID == 0 {
		return nil
	}
	cands := append([]int64{}, prefer...)
	if ceo, ok := managedCEO(corpID); ok {
		cands = append(cands, ceo)
	}
	cands = append(cands, armHeadCharID(corpID, armSecurity), armHeadCharID(corpID, armIndustry))
	for _, id := range cands {
		if id == 0 {
			continue
		}
		if sp, ok := chatterSpeakerFor(aid, id); ok && sp.CorpID == corpID {
			if p := m.corpParty(ctx, sp.CharID, sp.CorpID); p != nil {
				return p
			}
		}
	}
	for _, b := range allySeated(aid, corpID, 0) {
		if p := m.corpParty(ctx, b.CharID, b.CorpID); p != nil {
			return p
		}
	}
	return nil
}

// allyOther is some bot of a member corporation other than notCorp (and not charID not),
// seated in the alliance channel: the second voice of an exchange, so the reply comes from
// across a corporate line.
func (m *Mod) allyOther(ctx context.Context, aid, notCorp, not int64) *chatParty {
	for _, b := range allySeated(aid, 0, notCorp) {
		if b.CharID == not {
			continue
		}
		if p := m.corpParty(ctx, b.CharID, b.CorpID); p != nil {
			return p
		}
	}
	return nil
}

// ── support ─────────────────────────────────────────────────────────────────

var allyHelpAt = newMotiveMemo()

// chatterAllyHelp is a member under fire asking the wider alliance, after its
// own corporation could not answer (chatterCorpHelp). The answer is "yes" only
// when a fleet of another member corporation is pointed at that system now.
func (m *Mod) chatterAllyHelp(victim, attacker *chatParty, sysID int64, extra chatSlots) {
	if victim == nil || !victim.Bot || victim.CorpID == 0 || !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	goSafe("chatter ally help", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		aid := m.corpAllianceID(ctx, victim.CorpID)
		if !allianceChatOK(aid) || allyHelpAt.get(aid, victim.CharID) != motiveNone {
			return
		}
		allyHelpAt.note(aid, victim.CharID, motiveDefense)
		s := chatSlots{"corp": m.corpNameOf(ctx, victim.CorpID)}
		for k, v := range extra {
			s[k] = v
		}
		// The corporation's own call goes out first, in its own channel; the
		// alliance hears it a few seconds after, as a second appeal.
		beats := []beat{{after: 12 * time.Second, speaker: victim, about: attacker, ev: evAllyHelp, extra: s, urgent: true}}
		if f, ok := m.allyFleetIn(ctx, aid, sysID, victim.CorpID); ok {
			if resp := m.allyVoiceOf(ctx, aid, f.CorpID, f.LeaderID); resp != nil {
				s2 := chatSlots{"corp": m.corpNameOf(ctx, f.CorpID)}
				for k, v := range s {
					if k != "corp" {
						s2[k] = v
					}
				}
				beats = append(beats, beat{after: corpBeatReply, speaker: resp, about: victim, ev: evAllyHelpYes, extra: s2})
			}
		} else if resp := m.allyOther(ctx, aid, victim.CorpID, victim.CharID); resp != nil {
			beats = append(beats, beat{after: corpBeatReply, speaker: resp, about: victim, ev: evAllyHelpNo,
				why: toneToward(resp, victim), extra: s, odds: 50})
		}
		m.runExchange("ally help", aid, true, beats)
	})
}

// allyFleet is a member fleet in a system.
type allyFleet struct {
	GangID, CorpID, LeaderID int64
}

// allyFleetIn finds a member corporation's combat or security fleet whose sweep is pointed
// at sysID now, answering a call there or hunting it; notCorp is excluded (0 = none). It is
// the coming test: only a fleet already on its way says yes.
func (m *Mod) allyFleetIn(ctx context.Context, aid, sysID, notCorp int64) (allyFleet, bool) {
	var f allyFleet
	if aid == 0 || sysID == 0 || m.db == nil {
		return f, false
	}
	err := m.db.QueryRowContext(ctx, `
		SELECT g.gangID, g.corpID, g.leaderCharID
		  FROM rolebot_gangs g
		  JOIN corporation c ON c.corporationID = g.corpID
		 WHERE g.enabled = 1 AND c.allianceID = ? AND g.corpID <> ?
		   AND g.activity IN (?, ?)
		   AND JSON_VALUE(g.state, '$.huntSystemID') = ?
		 ORDER BY g.gangID LIMIT 1`,
		aid, notCorp, ActCombat, ActSecurity, sysID).Scan(&f.GangID, &f.CorpID, &f.LeaderID)
	return f, err == nil
}

// allyFleetSystems is every member fleet's sweep system, for "how far is the
// nearest wing" when nothing is close.
func (m *Mod) allyFleetSystems(ctx context.Context, aid int64) []int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT CAST(JSON_VALUE(g.state, '$.huntSystemID') AS SIGNED)
		  FROM rolebot_gangs g
		  JOIN corporation c ON c.corporationID = g.corpID
		 WHERE g.enabled = 1 AND c.allianceID = ? AND g.activity IN (?, ?)
		   AND JSON_VALUE(g.state, '$.huntSystemID') IS NOT NULL
		 LIMIT 64`, aid, ActCombat, ActSecurity)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// allyCallWhy is the cell a help call's kind is voiced in; "" for a kind the
// alliance is not told about (site calls are a fleet's own business).
func allyCallWhy(kind string) motive {
	switch kind {
	case helpOutnumbered, helpRetreat, helpSiege, helpCallToArms:
		return motive(kind)
	}
	return ""
}

// chatterAllyCall is a new help call (postHelpCall) said in the caller's
// alliance channel by the fleet's commander, or the corporation's voice for
// a call to arms. The answer is chatterAllyOmw, when a fleet adopts it.
func (m *Mod) chatterAllyCall(ctx context.Context, gangID, corpID, sysID int64, kind string) {
	why := allyCallWhy(kind)
	if why == "" || !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	aid := m.corpAllianceID(ctx, corpID)
	if !allianceChatOK(aid) {
		return
	}
	var leader int64
	if gangID != 0 {
		if g, _ := getGang(ctx, m.db, gangID); g != nil {
			leader = g.LeaderCharID
		}
	}
	sp := m.allyVoiceOf(ctx, aid, corpID, leader)
	if sp == nil {
		return
	}
	s := chatSlots{"system": m.nameOfSystem(ctx, sysID), "corp": m.corpNameOf(ctx, corpID)}
	m.runExchange("ally call", aid, true, []beat{{after: 2 * time.Second, speaker: sp, ev: evAllyCall, why: why, extra: s, urgent: true}})
}

var allyOmwAt = newMotiveMemo()

// chatterAllyOmw is a fleet of one member corporation taking up another's
// help call (adoptHelpCall): the commander says it is coming, because it is.
func (m *Mod) chatterAllyOmw(ctx context.Context, g *Gang, call *helpCall) {
	if g == nil || call == nil || g.CorpID == 0 || call.CorpID == g.CorpID || !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	aid := m.corpAllianceID(ctx, call.CorpID)
	if aid == 0 || aid != m.corpAllianceID(ctx, g.CorpID) || !allianceChatOK(aid) {
		return // a mutual ally outside the alliance has no seat in its channel
	}
	if allyOmwAt.get(call.CallID, g.GangID) != motiveNone {
		return
	}
	allyOmwAt.note(call.CallID, g.GangID, motiveFleetCall)
	sp := m.allyVoiceOf(ctx, aid, g.CorpID, g.LeaderCharID)
	if sp == nil {
		return
	}
	s := chatSlots{"system": m.nameOfSystem(ctx, call.SystemID), "corp": m.corpNameOf(ctx, call.CorpID)}
	if call.jumps > 0 {
		s["jumps"] = plural(call.jumps, "jump", "jumps")
	}
	m.runExchange("ally omw", aid, true, []beat{{after: 2 * time.Second, speaker: sp, ev: evAllyCallOmw, extra: s, urgent: true}})
}

// chatterAllyDefend is a reinforced tower calling the alliance
// (allianceRaiseDefend): the owner corporation's voice asks.
func (m *Mod) chatterAllyDefend(ctx context.Context, aid, ownerCorp, sysID, exitAt int64) {
	if !allianceChatOK(aid) {
		return
	}
	sp := m.allyVoiceOf(ctx, aid, ownerCorp, armHeadCharID(ownerCorp, armInfrastructure))
	if sp == nil {
		return
	}
	s := chatSlots{"system": m.nameOfSystem(ctx, sysID), "corp": m.corpNameOf(ctx, ownerCorp)}
	if left := exitAt - clock.Unix(); left > 0 {
		s["when"] = etaWords(left)
	}
	m.runExchange("ally defend", aid, true, []beat{{after: 2 * time.Second, speaker: sp, ev: evAllyDefend, extra: s, urgent: true}})
}

// chatterAllyDefendYes is a member corporation answering a defend call
// (allianceAnswerDefend): its security wing really is ordered there.
func (m *Mod) chatterAllyDefendYes(ctx context.Context, aid, corpID int64, r allianceRequest) {
	if !allianceChatOK(aid) {
		return
	}
	sp := m.allyVoiceOf(ctx, aid, corpID, armHeadCharID(corpID, armSecurity))
	if sp == nil {
		return
	}
	s := chatSlots{"system": m.nameOfSystem(ctx, r.SystemID), "corp": m.corpNameOf(ctx, r.FromCorp)}
	m.runExchange("ally defend yes", aid, true, []beat{{after: 4 * time.Second, speaker: sp, ev: evAllyDefendYes, extra: s}})
}

// ── a person asking for help ────────────────────────────────────────────────

var (
	reHelpAsk   = regexp.MustCompile(`\b(help|backup|back up|support|tackle|reinforcements|under attack|being attacked|need ships|need a fleet|need guns|i'm pointed|im pointed|got pointed|scrammed)\b`)
	reHelpWhere = regexp.MustCompile(`\b(?:in|at|to) ([a-z0-9][a-z0-9-]{1,30})\b`)
	reHelpNot   = regexp.MustCompile(`^(thanks|thank you|ty|thx|cheers)\b|\bthanks for\b`)
)

// parseHelpAsk reports whether a line asks the alliance for help, and the
// system it names, if any ("" when it names none). Pure.
func parseHelpAsk(raw string) (bool, string) {
	text, _ := extractChatLinks(raw)
	words := strings.ToLower(normaliseChat(text))
	words = strings.Trim(words, " .!?,")
	if words == "" || len(strings.Fields(words)) > 16 || reHelpNot.MatchString(words) {
		return false, ""
	}
	if !reHelpAsk.MatchString(words) {
		return false, ""
	}
	where := ""
	for _, m := range reHelpWhere.FindAllStringSubmatch(words, -1) {
		if !helpWhereFiller[m[1]] {
			where = m[1]
			break
		}
	}
	return true, where
}

// helpWhereFiller is what follows "in"/"at"/"to" without naming a system.
var helpWhereFiller = map[string]bool{
	"the": true, "a": true, "an": true, "my": true, "our": true, "this": true, "that": true, "me": true,
	"us": true, "here": true, "home": true, "gate": true, "station": true, "belt": true, "space": true,
	"once": true, "all": true, "anyone": true, "least": true,
}

// chatterAllyHumanHelp answers a person asking for help in the alliance
// channel. Talk only: nothing is posted or ordered for them. A member fleet
// already in the system says so; otherwise the nearest wing's distance, or
// that nobody is out.
func (m *Mod) chatterAllyHumanHelp(ctx context.Context, aid, humanID int64, first, whereWord string) {
	if !allianceChatOK(aid) {
		return
	}
	sysID := int64(0)
	if whereWord != "" {
		_ = m.db.QueryRowContext(ctx, `SELECT solarSystemID FROM mapSolarSystems WHERE solarSystemName = ? LIMIT 1`,
			whereWord).Scan(&sysID)
	}
	if sysID == 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(solarSystemID, 0) FROM character_ WHERE characterID = ?`,
			humanID).Scan(&sysID)
	}
	if sysID == 0 {
		return
	}
	human := partyFromID(humanID, first, 0, 0, false)
	s := chatSlots{"system": m.nameOfSystem(ctx, sysID), "first": first}
	ev := evAllyHumanNone
	var sp *chatParty
	if f, ok := m.allyFleetIn(ctx, aid, sysID, 0); ok {
		ev = evAllyHumanYes
		sp = m.allyVoiceOf(ctx, aid, f.CorpID, f.LeaderID)
		s["corp"] = m.corpNameOf(ctx, f.CorpID)
	} else if u, err := m.universe(ctx); err == nil {
		near := hopsWithin(u, sysID, 15)
		best := -1
		for _, sys := range m.allyFleetSystems(ctx, aid) {
			if j, ok := near[sys]; ok && (best < 0 || j < best) {
				best = j
			}
		}
		if best > 0 {
			ev = evAllyHumanNo
			s["jumps"] = plural(best, "jump", "jumps")
		}
	}
	if sp == nil {
		if g, ok := m.chatterGreeter(ctx, aid, nil); ok {
			sp = m.corpParty(ctx, g.CharID, g.CorpID)
		}
	}
	if sp == nil {
		return
	}
	m.chatSay(ctx, aid, true, sp, human, ev, motiveNone, s, true, false)
}

// ── politics ────────────────────────────────────────────────────────────────

var allyPoliticsAt = newMotiveMemo()

// chatterAllianceEvent is ceoLog's second listener (corpceo.go): the alliance layer's
// decisions, said in the alliance channel by the corporation that made them and answered by
// a pilot of another member corporation. ceoLog is synchronous in a CEO's think, so this
// only checks the gate and hands off.
func (m *Mod) chatterAllianceEvent(ctx context.Context, c *corpCEO, actorID int64, power, verb string, subjectID int64, amount float64) {
	if c == nil || !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	switch power {
	case powerAlliance:
		switch verb {
		case "joined", "accept", "leave", "kick", "support", "executor", "dues", "tithe", "refuse",
			"allrel", "declare", "warhold", "sov-gain", "sov-loss":
		default:
			return
		}
	case powerWar:
		if verb != "request" {
			return
		}
	default:
		return
	}
	corpID := c.CorpID
	goSafe("chatter alliance "+verb, func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m.sayAllianceEvent(cctx, corpID, actorID, verb, subjectID, amount)
	})
}

func (m *Mod) sayAllianceEvent(ctx context.Context, corpID, actorID int64, verb string, subjectID int64, amount float64) {
	aid := m.corpAllianceID(ctx, corpID)
	switch verb {
	case "joined", "leave":
		// The alliance is the subject: the cached membership may be stale
		// either way on the pass that changed it.
		aid = subjectID
	}
	if !allianceChatOK(aid) {
		return
	}
	// One exchange per (alliance, verb, subject) a while: a vote re-cast on
	// every think is not news twice.
	key := int64(crc32.ChecksumIEEE([]byte(verb)))<<32 ^ subjectID
	if allyPoliticsAt.get(aid, key) != motiveNone {
		return
	}
	actor := m.allyVoiceOf(ctx, aid, corpID, actorID)
	exec := m.allianceExecutor(ctx, aid)
	s := chatSlots{"corp": m.corpNameOf(ctx, corpID)}
	var opener, replier *chatParty
	var ev, back chatEvent
	var why motive
	var replyWhy func() motive
	toneFrom := func() motive { return toneToward(replier, opener) }
	switch verb {
	case "joined":
		opener, ev, back = actor, evAllyJoin, evAllyJoinBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "accept":
		s["othercorp"] = m.corpNameOf(ctx, subjectID)
		opener, ev, back = actor, evAllyAccept, evAllyAcceptBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "leave":
		opener, ev, back = actor, evAllyLeave, evAllyLeaveBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "kick":
		s["othercorp"] = m.corpNameOf(ctx, subjectID)
		opener, ev, back = actor, evAllyKick, evAllyKickBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "support":
		s["othercorp"] = ownerNameOf(ctx, m.db, subjectID)
		opener, ev, back = actor, evAllyVote, evAllyVoteBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "executor":
		// Somebody else congratulates the new executor; the executor answers.
		newExec := m.allyVoiceOf(ctx, aid, subjectID)
		s["corp"] = m.corpNameOf(ctx, subjectID)
		opener = m.allyOther(ctx, aid, subjectID, 0)
		replier = newExec
		ev, back = evAllyExec, evAllyExecBack
		why = toneToward(opener, newExec)
	case "dues":
		opener, ev, back = actor, evAllyDues, evAllyDuesBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "tithe":
		if amount > 0 {
			s["isk"] = iskWords(amount)
		}
		opener, ev, back = actor, evAllyTithe, evAllyTitheBack
		replier = m.allyVoiceOf(ctx, aid, subjectID)
	case "refuse":
		// The executor names the debtor, which answers for itself.
		if amount > 0 {
			s["isk"] = iskWords(amount)
		}
		opener = m.allyVoiceOf(ctx, aid, subjectID)
		replier = actor
		ev, back = evAllyRefuse, evAllyRefuseBack
	case "allrel":
		s["othercorp"] = ownerNameOf(ctx, m.db, subjectID)
		switch int64(amount) {
		case 0:
			why = whyWithdraw
		case evedb.AllianceRelationshipNAP, evedb.AllianceRelationshipFriend:
			why = whyFriendly
		default:
			why = whyHostile
		}
		if why != whyWithdraw {
			s["rel"] = strings.ToLower(evedb.AllianceRelationshipName(int64(amount)))
		}
		opener, ev, back = actor, evAllyRel, evAllyRelBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = toneFrom
	case "declare":
		s["othercorp"] = ownerNameOf(ctx, m.db, subjectID)
		opener, ev, back = actor, evAllyWar, evAllyWarBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = func() motive { return m.warTemper(ctx, replier) }
	case "request":
		s["othercorp"] = ownerNameOf(ctx, m.db, subjectID)
		opener, ev, back = actor, evAllyWarReq, evAllyWarReqBack
		if exec != corpID {
			replier = m.allyVoiceOf(ctx, aid, exec)
		}
	case "warhold":
		opener, ev, back = actor, evAllyWarHold, evAllyWarHoldBack
		replier = m.allyOther(ctx, aid, corpID, 0)
	case "sov-gain", "sov-loss":
		s["system"] = m.nameOfSystem(ctx, subjectID)
		ev, why = evAllySovGain, whyGain
		if verb == "sov-loss" {
			ev, why = evAllySovLoss, whyLoss
		}
		opener, back = actor, evAllySovBack
		replier = m.allyOther(ctx, aid, corpID, 0)
		replyWhy = func() motive {
			if verb == "sov-loss" {
				return whyLoss
			}
			return whyGain
		}
	}
	if opener == nil {
		return
	}
	allyPoliticsAt.note(aid, key, motiveWar)
	beats := []beat{{after: 3 * time.Second, speaker: opener, ev: ev, why: why, extra: s}}
	if replier != nil && replier.CharID != opener.CharID {
		rw := motiveNone
		if replyWhy != nil {
			rw = replyWhy()
		}
		beats = append(beats, beat{after: corpBeatReply, speaker: replier, about: opener, ev: back, why: rw, extra: s, odds: 75})
	}
	m.runExchange("alliance "+verb, aid, true, beats)
}

// warTemper is how a pilot takes the news of a war: eager when aggressive, wary when
// cautious, eager by default.
func (m *Mod) warTemper(ctx context.Context, p *chatParty) motive {
	if p == nil {
		return whyEager
	}
	t := m.chatterTraitsOf(ctx, p.CharID)
	if t[TraitCaution] > t[TraitAggression] {
		return whyWary
	}
	return whyEager
}

// ── ribbing ─────────────────────────────────────────────────────────────────

// allyLossBigISK is where a loss stops being a joke.
const allyLossBigISK = 50_000_000

var allyRibAt = newMotiveMemo()

// chatterAllyLoss is a member corporation's loss (killboard.go) remarked on
// by a pilot of another member corporation: condolences when it was costly,
// teasing when it was cheap, and the loser sometimes answers. Pods are
// nobody's business.
func (m *Mod) chatterAllyLoss(ctx context.Context, victimID, corpID int64, ship, system string, value float64, isPod bool) {
	if isPod || corpID == 0 || !Chatter() || !chatterAllianceOn.Load() || !chance(&chatterRibPct) {
		return
	}
	aid := m.corpAllianceID(ctx, corpID)
	if !allianceChatOK(aid) || allyRibAt.get(aid, corpID) != motiveNone {
		return
	}
	victim := m.corpParty(ctx, victimID, corpID)
	if victim == nil {
		victim = partyFromID(victimID, m.charNameOr(ctx, victimID), corpID, 0, false)
	}
	other := m.allyOther(ctx, aid, corpID, victimID)
	if other == nil {
		return
	}
	allyRibAt.note(aid, corpID, motiveNone+"rib")
	s := chatSlots{"ship": ship, "system": system, "corp": m.corpNameOf(ctx, corpID)}
	if value > 0 {
		s["isk"] = iskWords(value)
	}
	ev := evAllyLossSmall
	if value >= allyLossBigISK {
		ev = evAllyLossBig
	}
	tone := toneToward(other, victim)
	beats := []beat{{after: 20 * time.Second, speaker: other, about: victim, ev: ev, why: tone, extra: s}}
	if victim.Bot {
		if _, seated := chatterSpeakerFor(aid, victim.CharID); seated {
			beats = append(beats, beat{after: corpBeatReply, speaker: victim, about: other, ev: evAllyLossBack, why: tone,
				extra: s, odds: 50, optional: true})
		}
	}
	m.runExchange("ally loss", aid, true, beats)
}

// chatterAllyKill is a member's kill of an outsider praised by another member
// corporation's pilot (chatterKill). The victim must not be an ally: a kill
// inside the alliance is a different conversation, and not one to cheer.
func (m *Mod) chatterAllyKill(killer, victim *chatParty) {
	if killer == nil || victim == nil || !killer.Bot || killer.CorpID == 0 || !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	if !chance(&chatterRibPct) {
		return
	}
	goSafe("chatter ally kill", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		aid := m.corpAllianceID(ctx, killer.CorpID)
		if !allianceChatOK(aid) || (victim.CorpID != 0 && m.corpAllianceID(ctx, victim.CorpID) == aid) {
			return
		}
		if allyRibAt.get(aid, -killer.CorpID) != motiveNone {
			return
		}
		other := m.allyOther(ctx, aid, killer.CorpID, killer.CharID)
		if other == nil {
			return
		}
		allyRibAt.note(aid, -killer.CorpID, motiveNone+"kill")
		s := chatSlots{"corp": m.corpNameOf(ctx, killer.CorpID)}
		tone := toneToward(other, killer)
		beats := []beat{{after: 15 * time.Second, speaker: other, about: killer, ev: evAllyKill, why: tone, extra: s}}
		if _, seated := chatterSpeakerFor(aid, killer.CharID); seated {
			beats = append(beats, beat{after: corpBeatReply, speaker: killer, about: other, ev: evAllyKillBack,
				extra: s, odds: 60, optional: true})
		}
		m.runExchange("ally kill", aid, true, beats)
	})
}

// ── shop talk ───────────────────────────────────────────────────────────────

// chatterAllyAd is a listing (sayMarketAd's facts) mentioned to the alliance
// as well, at the shop dial's odds; a member sometimes notes it.
func (m *Mod) chatterAllyAd(ctx context.Context, sp *chatParty, s chatSlots) {
	if sp == nil || sp.CorpID == 0 || !chance(&chatterShopPct) {
		return
	}
	aid := m.corpAllianceID(ctx, sp.CorpID)
	if !allianceChatOK(aid) {
		return
	}
	if _, seated := chatterSpeakerFor(aid, sp.CharID); !seated {
		return
	}
	beats := []beat{{after: 5 * time.Second, speaker: sp, ev: evAllyShopAd, extra: s}}
	if other := m.allyOther(ctx, aid, sp.CorpID, sp.CharID); other != nil {
		beats = append(beats, beat{after: corpBeatReply, speaker: other, about: sp, ev: evAllyShopAdBack,
			why: toneToward(other, sp), extra: s, odds: 40, optional: true})
	}
	m.runExchange("ally ad", aid, true, beats)
}

// allyAdFor is sayMarketAd's alliance half: the same facts, said where the
// lister's alliance can hear it, whether or not anybody is in Local.
func (m *Mod) allyAdFor(ctx context.Context, charID, typeID, stationID, sysID int64, typeName string, price float64, vol int64) {
	if !Chatter() || !chatterAllianceOn.Load() {
		return
	}
	sp := m.seatedParty(ctx, charID)
	if sp == nil {
		return
	}
	if typeName == "" {
		typeName = m.typeNameOr(ctx, typeID)
	}
	s := chatSlots{
		"item":  typeLink(typeID, typeName),
		"units": unitsWords(vol),
		"isk":   iskWords(price),
		"total": iskWords(price * float64(vol)),
	}
	m.marketPlaceSlots(ctx, s, stationID, sysID)
	m.chatterAllyAd(ctx, sp, s)
}

// seatedParty is a present bot as a party of its own corporation, nil when
// it is not present.
func (m *Mod) seatedParty(ctx context.Context, charID int64) *chatParty {
	presenceMu.Lock()
	r, ok := presenceApplied[charID]
	presenceMu.Unlock()
	if !ok || r.CorpID == 0 {
		return nil
	}
	return m.corpParty(ctx, charID, r.CorpID)
}

// chatterAllyShopAsk is a bot that walked away from a gouged price asking whether anyone in
// the alliance makes the thing. Only a member corporation that posted that product on its
// work board in the last fortnight answers; with none, nothing is asked.
func (m *Mod) chatterAllyShopAsk(ctx context.Context, sp *chatParty, typeID int64, item string) {
	if sp == nil || sp.CorpID == 0 || typeID == 0 || !chance(&chatterShopPct) {
		return
	}
	aid := m.corpAllianceID(ctx, sp.CorpID)
	if !allianceChatOK(aid) {
		return
	}
	if _, seated := chatterSpeakerFor(aid, sp.CharID); !seated {
		return
	}
	maker := m.allyMakerOf(ctx, aid, sp.CorpID, typeID)
	if maker == 0 {
		return
	}
	resp := m.allyVoiceOf(ctx, aid, maker, armHeadCharID(maker, armIndustry))
	if resp == nil {
		return
	}
	s := chatSlots{"item": item, "corp": m.corpNameOf(ctx, maker)}
	m.runExchange("ally shop", aid, true, []beat{
		{after: 5 * time.Second, speaker: sp, ev: evAllyShopAsk, extra: s},
		{after: corpBeatReply, speaker: resp, about: sp, ev: evAllyShopYes, why: toneToward(resp, sp), extra: s},
	})
}

// allyMakerOf is a member corporation (not notCorp) whose work board carried
// a build of typeID in the last fourteen days, 0 for none.
func (m *Mod) allyMakerOf(ctx context.Context, aid, notCorp, typeID int64) int64 {
	var corpID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT w.corporationID FROM rolebot_corp_work w
		  JOIN corporation c ON c.corporationID = w.corporationID
		 WHERE c.allianceID = ? AND w.corporationID <> ? AND w.kind = ? AND w.productTypeID = ?
		   AND w.postedAt > ?
		 ORDER BY w.postedAt DESC LIMIT 1`,
		aid, notCorp, workKindBuild, typeID, clock.Unix()-14*86400).Scan(&corpID)
	return corpID
}

// allyCareerSay is the banter lane's shop-talk turn in an alliance channel
// (chatter_banter.go): a pilot says what its own work is like this week, in
// its career's cell, and a pilot of another corporation answers.
func (m *Mod) allyCareerSay(ctx context.Context, aid int64) bool {
	bots := allySeated(aid, 0, 0)
	for _, b := range bots {
		sp := m.corpParty(ctx, b.CharID, b.CorpID)
		if sp == nil {
			continue
		}
		st, err := loadState(ctx, m.db, b.CharID)
		if err != nil || st == nil {
			continue
		}
		why := careerMotive(st)
		if why == motiveNone {
			continue
		}
		other := m.allyOther(ctx, aid, b.CorpID, b.CharID)
		if other == nil {
			return false
		}
		s := chatSlots{"corp": m.corpNameOf(ctx, b.CorpID)}
		m.runExchange("ally career", aid, true, []beat{
			{after: 2 * time.Second, speaker: sp, ev: evAllyCareer, why: why, extra: s},
			{after: corpBeatReply, speaker: other, about: sp, ev: evAllyCareerBack, why: toneToward(other, sp), extra: s, odds: 70},
		})
		return true
	}
	return false
}

func init() {
	registerSweep("chatterAlliance", func() {
		now := clock.Now()
		allyHelpAt.sweep(now)
		allyOmwAt.sweep(now)
		allyPoliticsAt.sweep(now)
		allyRibAt.sweep(now)
	})
}
